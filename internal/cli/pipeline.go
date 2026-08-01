// Package cli implements the sqletch commands as testable functions;
// cmd/sqletch is thin cobra wiring over these. The pipeline here is
// the cache-aware flow of docs/design/04-type-oracle.md §4.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/cache"
	"github.com/moznion/sqletch/internal/codegen"
	"github.com/moznion/sqletch/internal/config"
	"github.com/moznion/sqletch/internal/devdb"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/nullability"
	"github.com/moznion/sqletch/internal/rules"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
)

// Mode selects how much of the pipeline runs.
type Mode int

const (
	ModeCheck Mode = iota
	ModeGenerate
	ModeCheckExhaustive
)

// Result carries what the commands report to the user.
type Result struct {
	Diags       []diagnostics.Diagnostic
	Sources     map[string][]byte // template path -> content (for rendering diags)
	OracleHits  int
	OracleMiss  int
	Offline     bool
	QueryCount  int
	ShapesTotal int // exhaustive mode: shapes verified against the DB
}

type compiledQuery struct {
	q          *template.QueryTemplate
	rs         []ast.Rendering
	descs      []dialect.Desc
	maxTree    dialect.Tree
	paramTypes map[string]dialect.TypeRef
	nullable   []bool
}

// Run executes the pipeline. Diagnostics are user mistakes; the error
// return is environmental (unreadable files, DB unreachable).
func Run(ctx context.Context, cfg config.Config, mode Mode) (*Result, error) {
	res := &Result{Sources: map[string][]byte{}}
	profile := postgres.Profile{}
	frontend := postgres.Frontend{}

	// ---- schema fingerprint (offline) -----------------------------------
	schemaPaths, err := cfg.ExpandGlobs(cfg.Schema.Files)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	var schemaFiles []cache.SchemaFile
	var schemaSQL []string
	for _, p := range schemaPaths {
		content, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(cfg.Dir, p)
		schemaFiles = append(schemaFiles, cache.SchemaFile{Path: rel, Content: content})
		schemaSQL = append(schemaSQL, string(content))
	}
	fp := cache.Fingerprint(cfg.Dialect, cfg.ServerVersion, schemaFiles)
	store := cache.NewStore(cfg.Abs(cfg.Cache.Path))

	// ---- scan + catalog-free checks -------------------------------------
	queryPaths, err := cfg.ExpandGlobs(cfg.Queries)
	if err != nil {
		return nil, fmt.Errorf("queries: %w", err)
	}
	scanner := template.NewScanner(profile)
	var queries []*compiledQuery
	names := map[string]string{}
	for _, p := range queryPaths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		res.Sources[p] = src
		file, diags := scanner.ScanFile(p, src)
		res.Diags = append(res.Diags, diags...)
		for _, q := range file.Queries {
			if prev, dup := names[q.Name]; dup {
				res.Diags = append(res.Diags, diagnostics.Errorf(diagnostics.CodeDuplicateQueryName,
					q.HeaderSpan, "query %q already defined in %s", q.Name, prev))
				continue
			}
			names[q.Name] = p
			queries = append(queries, &compiledQuery{q: q})
		}
	}
	res.QueryCount = len(queries)
	for _, cq := range queries {
		res.Diags = append(res.Diags, rules.CheckLexical(profile, cq.q)...)
		rs, err := ast.Renderings(profile, cq.q)
		if err != nil {
			return nil, err
		}
		cq.rs = rs
		res.Diags = append(res.Diags, rules.CheckR1(profile, frontend, cq.q, rs)...)
	}
	if diagnostics.HasErrors(res.Diags) {
		return res, nil
	}

	// ---- oracle (cache-aware) -------------------------------------------
	cat, haveCat := store.LoadCatalog(fp)
	type miss struct {
		cq *compiledQuery
		ri int
	}
	var misses []miss
	for _, cq := range queries {
		cq.descs = make([]dialect.Desc, len(cq.rs))
		for i, r := range cq.rs {
			if e, ok := store.LoadOracle(fp, r.SQL); ok {
				cq.descs[i] = entryToDesc(e)
				res.OracleHits++
			} else {
				misses = append(misses, miss{cq, i})
				res.OracleMiss++
			}
		}
	}
	res.Offline = haveCat && len(misses) == 0 && mode != ModeCheckExhaustive

	var oracle dialect.Oracle
	var oracleCleanup func()
	acquireOracle := func() (dialect.Oracle, error) {
		if oracle != nil {
			return oracle, nil
		}
		conn, cleanup, err := devdb.Acquire(ctx, devdb.Config{
			DSN:           cfg.Database.DSN,
			ServerVersion: cfg.ServerVersion,
			SchemaSQL:     schemaSQL,
		})
		if err != nil {
			return nil, err
		}
		oracleCleanup = cleanup
		oracle = postgres.NewOracle(conn)
		return oracle, nil
	}
	defer func() {
		if oracleCleanup != nil {
			oracleCleanup()
		}
	}()

	if !haveCat || len(misses) > 0 {
		o, err := acquireOracle()
		if err != nil {
			return nil, err
		}
		if !haveCat {
			cat, err = o.Snapshot(ctx)
			if err != nil {
				return nil, err
			}
			cat.SchemaFP = fp
			if err := store.SaveCatalog(cat); err != nil {
				return nil, err
			}
		}
		for _, m := range misses {
			r := m.cq.rs[m.ri]
			desc, err := o.Describe(ctx, r.SQL)
			if err != nil {
				res.Diags = append(res.Diags, oracleDiag(m.cq.q, r, err))
				continue
			}
			m.cq.descs[m.ri] = desc
			if err := store.SaveOracle(descToEntry(fp, r.SQL, desc)); err != nil {
				return nil, err
			}
		}
	}
	if diagnostics.HasErrors(res.Diags) {
		return res, nil
	}

	// ---- catalog-dependent checks, types, nullability -------------------
	for _, cq := range queries {
		tree, err := frontend.Parse(cq.rs[0].SQL)
		if err != nil {
			return nil, fmt.Errorf("internal: maximal rendering re-parse: %w", err)
		}
		cq.maxTree = tree
		res.Diags = append(res.Diags, rules.CheckResolved(cq.q, cq.rs[0], tree, cat)...)
		res.Diags = append(res.Diags, rules.CheckTypeAgreement(cq.q, cq.rs, cq.descs)...)
		types, d := rules.ResolveParamTypes(cq.q, cq.rs, cq.descs)
		res.Diags = append(res.Diags, d...)
		cq.paramTypes = map[string]dialect.TypeRef{}
		for _, pt := range types {
			cq.paramTypes[pt.Name] = pt.Type
		}
		nullable, err := nullability.AnalyzeAll(frontend, cq.rs, cq.descs, cat, cfg.NullOverridesFor(cq.q.Name))
		if err != nil {
			return nil, err
		}
		cq.nullable = nullable
	}
	if diagnostics.HasErrors(res.Diags) {
		return res, nil
	}

	// ---- exhaustive: prepare + plan every shape -------------------------
	if mode == ModeCheckExhaustive {
		o, err := acquireOracle()
		if err != nil {
			return nil, err
		}
		for _, cq := range queries {
			keys, truncated := shape.Enumerate(cq.q, 4096)
			if truncated {
				res.Diags = append(res.Diags, diagnostics.Errorf(diagnostics.CodeTooManyGuards,
					cq.q.HeaderSpan, "%s exceeds the exhaustive-check cap of 4096 shapes", cq.q.Name))
				continue
			}
			for _, k := range keys {
				r, err := ast.RenderShape(profile, cq.q, k.Guards, k.Selection())
				if err != nil {
					return nil, err
				}
				if _, err := o.Describe(ctx, r.SQL); err != nil {
					res.Diags = append(res.Diags, oracleDiag(cq.q, r, err))
					continue
				}
				if err := o.Plan(ctx, r.SQL); err != nil {
					res.Diags = append(res.Diags, oracleDiag(cq.q, r, err))
					continue
				}
				res.ShapesTotal++
			}
		}
		if diagnostics.HasErrors(res.Diags) {
			return res, nil
		}
	}

	// ---- codegen ---------------------------------------------------------
	var inputs []codegen.QueryInput
	for _, cq := range queries {
		inputs = append(inputs, codegen.QueryInput{
			Q:          cq.q,
			Frags:      codegen.BuildFrags(profile, cq.q),
			ParamTypes: cq.paramTypes,
			Columns:    cq.descs[0].Columns,
			Nullable:   cq.nullable,
		})
	}
	files, diags := codegen.Generate(codegen.Options{Package: cfg.Output.Package}, postgres.TypeMap{}, inputs)
	res.Diags = append(res.Diags, diags...)
	if diagnostics.HasErrors(res.Diags) || mode != ModeGenerate {
		return res, nil
	}
	outDir := cfg.Abs(cfg.Output.Path)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	for name, content := range files {
		path := filepath.Join(outDir, name)
		if prev, err := os.ReadFile(path); err == nil && string(prev) == string(content) {
			continue // unchanged: keep mtime for build systems
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, content, 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
	}
	if err := writeExplainData(cfg, queries); err != nil {
		return nil, err
	}
	return res, nil
}

func oracleDiag(q *template.QueryTemplate, r ast.Rendering, err error) diagnostics.Diagnostic {
	if oe, ok := err.(*dialect.OracleError); ok {
		span := q.HeaderSpan
		if oe.Pos >= 0 {
			tOff, _ := r.Map.ToTemplate(oe.Pos)
			span = diagnostics.Span{File: q.HeaderSpan.File, Start: tOff, End: tOff + 1}
		}
		code := diagnostics.CodeOracleFailure
		d := diagnostics.Errorf(code, span, "database rejects this query: %s", oe.Msg)
		if oe.Indeterminate {
			d.Code = diagnostics.CodeIndeterminateParam
			d = d.WithHint("pin the parameter's type with an explicit cast, e.g. :param::text")
		}
		return d
	}
	return diagnostics.Errorf(diagnostics.CodeOracleFailure, q.HeaderSpan, "oracle failure: %v", err)
}

func entryToDesc(e *cache.OracleEntry) dialect.Desc {
	var d dialect.Desc
	for _, p := range e.Params {
		d.Params = append(d.Params, dialect.TypeRef{OID: p.OID, Name: p.Name})
	}
	for _, c := range e.Columns {
		d.Columns = append(d.Columns, dialect.ColumnDesc{
			Name: c.Name, Type: dialect.TypeRef{OID: c.OID, Name: c.TypeName},
			SrcRel: c.SrcRel, SrcAtt: c.SrcAtt,
		})
	}
	return d
}

func descToEntry(fp, sql string, d dialect.Desc) *cache.OracleEntry {
	e := &cache.OracleEntry{SchemaFP: fp, RenderedSQL: sql}
	for _, p := range d.Params {
		e.Params = append(e.Params, cache.EntryType{OID: p.OID, Name: p.Name})
	}
	for _, c := range d.Columns {
		e.Columns = append(e.Columns, cache.EntryColumn{
			Name: c.Name, OID: c.Type.OID, TypeName: c.Type.Name,
			SrcRel: c.SrcRel, SrcAtt: c.SrcAtt,
		})
	}
	return e
}

// explainData is the per-query summary consumed by `sqletch explain`.
type explainData struct {
	Name       string   `json:"name"`
	Guards     []string `json:"guards"`
	Chooses    []string `json:"chooses,omitempty"`
	ShapeCount string   `json:"shape_count"`
	Params     []string `json:"params"`
	Columns    []string `json:"columns"`
	MaximalSQL string   `json:"maximal_sql"`
}

func writeExplainData(cfg config.Config, queries []*compiledQuery) error {
	dir := cfg.Abs(filepath.Join(filepath.Dir(cfg.Cache.Path), "explain"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, cq := range queries {
		d := explainData{Name: cq.q.Name, ShapeCount: shape.Count(cq.q).String(), MaximalSQL: cq.rs[0].SQL}
		for i, g := range cq.q.GuardAtoms {
			d.Guards = append(d.Guards, fmt.Sprintf("bit %d: %s", i, g.Param))
		}
		for _, it := range cq.q.Items {
			if c, ok := it.(*template.Choose); ok {
				cases := ""
				for i, cs := range c.Cases {
					if i > 0 {
						cases += ", "
					}
					cases += cs.Name
				}
				if c.Default != nil {
					cases += ", (default)"
				}
				d.Chooses = append(d.Chooses, fmt.Sprintf("%s: %s", c.Param, cases))
			}
		}
		for _, name := range cq.q.ParamOrder {
			tr := cq.paramTypes[name]
			opt := ""
			if cq.q.Params[name].Optional {
				opt = " (optional)"
			}
			d.Params = append(d.Params, fmt.Sprintf("%s: %s%s", name, tr.Name, opt))
		}
		for i, col := range cq.descs[0].Columns {
			null := "not null"
			if i < len(cq.nullable) && cq.nullable[i] {
				null = "nullable"
			}
			d.Columns = append(d.Columns, fmt.Sprintf("%s: %s (%s)", col.Name, col.Type.Name, null))
		}
		data, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, cq.q.Name+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// PrintDiags renders diagnostics in text or JSON to w.
func PrintDiags(w io.Writer, res *Result, jsonFormat bool) {
	diagnostics.Sort(res.Diags)
	for _, d := range res.Diags {
		if jsonFormat {
			line, col := diagnostics.LineCol(res.Sources[d.Span.File], d.Span.Start)
			enc, _ := json.Marshal(map[string]any{
				"code": d.Code, "severity": d.Severity.String(),
				"file": d.Span.File, "line": line, "col": col,
				"message": d.Message, "hint": d.Hint,
			})
			fmt.Fprintln(w, string(enc))
			continue
		}
		fmt.Fprintln(w, d.Render(res.Sources[d.Span.File]))
	}
}
