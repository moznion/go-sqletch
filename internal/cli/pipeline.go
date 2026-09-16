// Package cli implements the sqletch commands as testable functions;
// cmd/sqletch is thin cobra wiring over these. The pipeline here is
// the cache-aware flow of docs/design/04-type-oracle.md §4.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/codegen"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/nullability"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/rules"
	"github.com/moznion/go-sqletch/internal/shape"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
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
	// NativePlan: exhaustive mode ran on the native backend, whose
	// Plan is describe-validation only — the printed summary must not
	// claim EXPLAIN coverage (design 15 D2).
	NativePlan bool
}

// versionPinDiag converts a dev-database version-pin mismatch into
// SQLETCH200. It is a user mistake in sqletch.yaml, not an environment
// failure, so it belongs in the diagnostic stream — coded, attached to
// the config file, and carried by `--json` to editors.
func versionPinDiag(cfg config.Config, err error) (diagnostics.Diagnostic, bool) {
	var vme *devdb.VersionMismatchError
	if !errors.As(err, &vme) {
		return diagnostics.Diagnostic{}, false
	}
	d := diagnostics.Errorf(diagnostics.CodeServerVersionMismatch,
		diagnostics.Span{File: cfg.Path}, "%v", vme)
	d.Hint = fmt.Sprintf("set `server_version: \"%s\"`, or point database.dsn at a matching server", vme.Actual)
	return d, true
}

// destructiveResetDiag converts devdb's refusal to reset a
// user-supplied database into SQLETCH204 against the config's
// database.dsn. sqletch drops the schema before applying it, so a cold
// run against a DSN the repo chose could wipe a database the developer
// cares about — this is a fail-closed safety guard (H1), not an
// environment failure, so it belongs in the coded diagnostic stream and
// reaches `--json`/editors. The escape hatch is the --allow-destructive
// flag, deliberately not a config key: a repo-controlled key could
// disarm the guard invisibly.
func destructiveResetDiag(cfg config.Config, err error) (diagnostics.Diagnostic, bool) {
	var dre *devdb.DestructiveResetError
	if !errors.As(err, &dre) {
		return diagnostics.Diagnostic{}, false
	}
	d := diagnostics.Errorf(diagnostics.CodeDestructiveReset,
		diagnostics.Span{File: cfg.Path},
		"refusing to reset the %s database at database.dsn: sqletch drops the schema before applying it, which destroys data unless the database is disposable",
		dre.Server)
	d.Hint = "leave database.dsn empty to use a disposable container, or point it at a throwaway database and pass --allow-destructive to confirm sqletch may drop and recreate its schema"
	return d, true
}

// overrideHygieneDiags warns about null_overrides entries that cannot
// do what the user intended: a key naming no result column is dead
// config (a typo or a renamed column), and a key matching several
// same-named result columns forces ALL of them — overrides are
// name-keyed, not position-keyed.
func overrideHygieneDiags(cfg config.Config, query string,
	overrides map[string]bool, desc dialect.Desc) []diagnostics.Diagnostic {

	if len(overrides) == 0 {
		return nil
	}
	count := map[string]int{}
	for _, c := range desc.Columns {
		count[c.Name]++
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	var diags []diagnostics.Diagnostic
	for _, name := range names {
		switch {
		case count[name] == 0:
			diags = append(diags, diagnostics.Warnf(diagnostics.CodeOverrideUnknownColumn,
				diagnostics.Span{File: cfg.Path},
				"null_overrides for query %q names column %q, which is not a result column — the override is dead",
				query, name).
				WithHint("check for a typo or a renamed output alias"))
		case count[name] > 1:
			diags = append(diags, diagnostics.Warnf(diagnostics.CodeOverrideAmbiguousColumn,
				diagnostics.Span{File: cfg.Path},
				"null_overrides for query %q matches %d result columns named %q — overrides are name-keyed and force all of them",
				query, count[name], name).
				WithHint("give the columns distinct output aliases and override the intended one"))
		}
	}
	return diags
}

// RunOptions carries the per-invocation switches that are not
// sqletch.yaml's business — things a user opts into for one command,
// which must stay visible on the command line rather than being
// permanently (and invisibly) disarmed in config.
type RunOptions struct {
	// AllowServerDrift accepts a committed cache whose recorded
	// generation environment disagrees with the connected server,
	// downgrading SQLETCH203 to a warning and adopting the connected
	// server in the record. The result is a cache no single
	// environment produced — deliberate, and never the default.
	AllowServerDrift bool
	// AllowDestructive confirms that the database at a user-supplied
	// database.dsn is disposable, letting sqletch reset (drop and
	// recreate) its schema. Without it a cold run against a
	// user-supplied DSN is refused with SQLETCH204 rather than wiping a
	// database a cloned repo pointed sqletch at (H1). A database
	// sqletch provisioned itself (empty dsn → a disposable container or
	// temp file) is never gated.
	AllowDestructive bool
}

type compiledQuery struct {
	q      *template.QueryTemplate // woven: every phase past scanChecks reads this
	target int                     // index into the run's resolved targets
	woven  []policy.WovenPolicy    // policy coverage (enforcement, explain)
	rs     []ast.Rendering
	// refs name each rendering's committed cache entry, parallel to rs
	// (docs/design/21-cache-layout.md §3).
	refs        []cache.OracleRef
	descs       []dialect.Desc
	paramTypes  map[string]dialect.TypeRef
	nullable    []bool
	nullReasons []string // parallel to nullable; explain's "why"
	// nullableParams are design 20's catalog-derived nullable value
	// parameters (optional.Option[T], None binds NULL).
	nullableParams map[string]bool
}

// Run executes the pipeline. Diagnostics are user mistakes; the error
// return is environmental (unreadable files, DB unreachable).
func Run(ctx context.Context, cfg config.Config, mode Mode, opts RunOptions) (*Result, error) {
	res := &Result{Sources: map[string][]byte{}}
	drv := driverFor(cfg)
	profile := drv.profile
	frontend := drv.frontend

	// ---- schema fingerprint (offline) -----------------------------------
	schemaPaths, err := cfg.ExpandGlobs(cfg.Schema.Files)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	var schemaFiles []cache.SchemaFile // rel paths: the fingerprint inputs
	var schemaAcq []cache.SchemaFile   // as-read paths: oracle construction (diag spans)
	for _, p := range schemaPaths {
		content, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(cfg.Dir, p)
		schemaFiles = append(schemaFiles, cache.SchemaFile{Path: rel, Content: content})
		schemaAcq = append(schemaAcq, cache.SchemaFile{Path: p, Content: content})
	}
	fp := cache.Fingerprint(cfg.Dialect, cfg.ServerVersion, schemaFiles)
	store := cache.NewStore(cfg.Abs(cfg.Cache.Path))

	// ---- scan + catalog-free checks -------------------------------------
	// Targets resolve BEFORE scanning: which file belongs to which
	// generated package decides the scope of duplicate-name detection
	// (two packages may each define GetUser) and, later, which
	// emission each query feeds.
	resolution, targetDiags := cfg.ResolveTargets()
	res.Diags = append(res.Diags, targetDiags...)
	if diagnostics.HasErrors(res.Diags) {
		return res, nil
	}
	scanner := template.NewScanner(profile)
	var queries []*compiledQuery
	queryTargets := map[string][]string{} // query name -> output paths defining it
	for ti, t := range resolution.Targets {
		names := map[string]string{}
		for _, p := range t.Files {
			src, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			res.Sources[p] = src
			file, diags := scanSource(scanner, p, src)
			res.Diags = append(res.Diags, diags...)
			for _, q := range file.Queries {
				if prev, dup := names[q.Name]; dup {
					res.Diags = append(res.Diags, diagnostics.Errorf(diagnostics.CodeDuplicateQueryName,
						q.HeaderSpan, "query %q already defined in %s (within the target %s)", q.Name, prev, t.Path))
					continue
				}
				names[q.Name] = p
				queryTargets[q.Name] = append(queryTargets[q.Name], t.Path)
				queries = append(queries, &compiledQuery{q: q, target: ti})
			}
		}
	}
	res.Diags = append(res.Diags, nameScopeDiags(cfg, queryTargets)...)
	res.QueryCount = len(queries)
	pols, polDiags := compilePolicies(drv, cfg)
	res.Diags = append(res.Diags, polDiags...)
	for _, cq := range queries {
		wres, rs, d, err := scanChecks(drv, pols, cq.q, cfg.Verification.MaxShapes)
		if err != nil {
			return nil, err
		}
		res.Diags = append(res.Diags, d...)
		cq.q = wres.Query
		cq.woven = wres.Woven
		cq.rs = rs
		slug := resolution.Targets[cq.target].Slug()
		cq.refs = make([]cache.OracleRef, len(rs))
		for i, r := range rs {
			cq.refs[i] = oracleRef(slug, cq.q.Name, r)
		}
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
			if e, ok := store.LoadOracle(cq.refs[i], fp, r.SQL); ok {
				cq.descs[i] = dialect.DescFromEntry(e)
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
		var det devdb.Detected
		o, cleanup, err := drv.acquire(ctx, cfg, schemaAcq, opts.AllowDestructive, &det)
		if err != nil {
			return nil, err
		}
		oracleCleanup = cleanup
		// Connecting is the only moment the committed cache's
		// generation environment can be checked at all (a warm offline
		// run never gets here — design 04 §3.1). Do it before a single
		// miss is filled, so a refused drift leaves the tree untouched.
		recorded, _ := store.LoadEnv(fp)
		if d, drifted := serverDriftDiag(cfg, recorded, det.ServerVersion, opts.AllowServerDrift); drifted {
			res.Diags = append(res.Diags, d)
			if d.Severity == diagnostics.Error {
				return nil, errServerDrift
			}
		}
		if err := store.SaveEnv(envRecord(cfg, fp, det.ServerVersion)); err != nil {
			return nil, err
		}
		oracle = o
		return oracle, nil
	}
	defer func() {
		if oracleCleanup != nil {
			oracleCleanup()
		}
	}()

	if !haveCat || len(misses) > 0 {
		o, err := acquireOracle()
		if d, ok := versionPinDiag(cfg, err); ok {
			res.Diags = append(res.Diags, d)
			return res, nil
		}
		if d, ok := destructiveResetDiag(cfg, err); ok {
			res.Diags = append(res.Diags, d)
			return res, nil
		}
		if d, ok := nativeDDLDiag(err); ok {
			res.Diags = append(res.Diags, d)
			return res, nil
		}
		if errors.Is(err, errServerDrift) {
			return res, nil // SQLETCH203 is already in res.Diags
		}
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
			if err := store.SaveOracle(m.cq.refs[m.ri], dialect.EntryFromDesc(fp, r.SQL, desc)); err != nil {
				return nil, err
			}
		}
	}
	if diagnostics.HasErrors(res.Diags) {
		return res, nil
	}

	// ---- catalog-dependent checks, types, nullability -------------------
	for _, cq := range queries {
		types, d, err := resolvedChecks(drv, cfg.Dialect, pols, cq.q, cq.rs, cq.descs, cat)
		if err != nil {
			return nil, err
		}
		res.Diags = append(res.Diags, d...)
		cq.paramTypes = types
		// Design 20: parameters writing only nullable columns become
		// Option[T]. It emits no diagnostics, so it lives beside the P5
		// result analysis rather than in resolvedChecks (whose only
		// product the LSP consumes is diagnostics).
		maxTree, err := frontend.Parse(cq.rs[0].SQL)
		if err != nil {
			return nil, fmt.Errorf("internal: maximal rendering re-parse: %w", err)
		}
		cq.nullableParams = rules.DeriveNullableParams(profile, cq.q, cq.rs[0], maxTree, cat)
		overrides := cfg.NullOverridesFor(cq.q.Name)
		verdicts, err := nullability.AnalyzeAllVerdicts(frontend, cq.rs, cq.descs, cat, overrides)
		if err != nil {
			return nil, err
		}
		cq.nullable = make([]bool, len(verdicts))
		cq.nullReasons = make([]string, len(verdicts))
		for i, v := range verdicts {
			cq.nullable[i] = v.Nullable
			cq.nullReasons[i] = v.Reason
		}
		res.Diags = append(res.Diags, overrideHygieneDiags(cfg, cq.q.Name, overrides, cq.descs[0])...)
	}
	if diagnostics.HasErrors(res.Diags) {
		return res, nil
	}

	// ---- exhaustive: prepare + plan every shape -------------------------
	if mode == ModeCheckExhaustive {
		res.NativePlan = cfg.NativeOracle()
		o, err := acquireOracle()
		if d, ok := versionPinDiag(cfg, err); ok {
			res.Diags = append(res.Diags, d)
			return res, nil
		}
		if d, ok := destructiveResetDiag(cfg, err); ok {
			res.Diags = append(res.Diags, d)
			return res, nil
		}
		if d, ok := nativeDDLDiag(err); ok {
			res.Diags = append(res.Diags, d)
			return res, nil
		}
		if errors.Is(err, errServerDrift) {
			return res, nil // SQLETCH203 is already in res.Diags
		}
		if err != nil {
			return nil, err
		}
		for _, cq := range queries {
			keys, truncated := shape.EnumerateExpand(cq.q, cfg.Verification.MaxShapes, drv.expandIn)
			if truncated {
				res.Diags = append(res.Diags, diagnostics.Errorf(diagnostics.CodeShapeCapReached,
					cq.q.HeaderSpan, "%s exceeds the exhaustive-check cap of %d shapes",
					cq.q.Name, cfg.Verification.MaxShapes).
					WithHint("raise verification.max_shapes in %s to give this query the budget it needs; "+
						"the cap exists so one query cannot stall the check, not to bound what may be verified",
						cfg.Path))
				continue
			}
			for _, k := range keys {
				r, err := ast.RenderShape(profile, cq.q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
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
	expandedNames := map[string]bool{}
	for _, name := range cfg.Expansion.Queries {
		if _, ok := queryTargets[name]; !ok {
			res.Diags = append(res.Diags, diagnostics.Errorf(diagnostics.CodeConfigInvalid,
				diagnostics.Span{File: cfg.Dir}, "static_expansion.queries lists unknown query %q", name))
		}
		expandedNames[name] = true
	}
	inputsByTarget := make([][]codegen.QueryInput, len(resolution.Targets))
	for _, cq := range queries {
		frags := codegen.BuildFrags(profile, cq.q)
		in := codegen.QueryInput{
			Q:          cq.q,
			Frags:      frags,
			ParamTypes: cq.paramTypes,
			Columns:    cq.descs[0].Columns,
			Nullable:   cq.nullable,

			NullableParams: cq.nullableParams,
		}
		if expandedNames[cq.q.Name] {
			for _, it := range cq.q.Items {
				if _, hasTree := it.(*template.FilterTree); hasTree {
					res.Diags = append(res.Diags, diagnostics.Errorf(diagnostics.CodeExpansionLarge,
						cq.q.HeaderSpan,
						"%s uses @filter-tree, whose tree space is unbounded; it cannot be statically expanded (its audit surface is the predicate vocabulary and caps)", cq.q.Name))
					break
				}
				if _, hasIn := it.(*template.InExpr); hasIn && drv.expandIn {
					res.Diags = append(res.Diags, diagnostics.Errorf(diagnostics.CodeExpansionLarge,
						cq.q.HeaderSpan,
						"%s uses @in, whose arity space is unbounded on %s; it cannot be statically expanded", cq.q.Name, cfg.Dialect))
					break
				}
			}
			if diagnostics.HasErrors(res.Diags) {
				continue
			}
			shapes, d := expandShapes(cq.q, frags, cfg.Expansion.MaxShapes, drv.style)
			res.Diags = append(res.Diags, d...)
			if shapes != nil {
				in.ExpandedShapes = shapes
				if mode == ModeGenerate {
					if err := writeExpandedFiles(cfg, resolution.Targets[cq.target], cq.q.Name, shapes); err != nil {
						return nil, err
					}
				}
			}
		}
		inputsByTarget[cq.target] = append(inputsByTarget[cq.target], in)
	}
	if diagnostics.HasErrors(res.Diags) {
		return res, nil
	}
	for ti, t := range resolution.Targets {
		files, diags := codegen.Generate(codegen.Options{
			Package:  t.Package,
			TreeCaps: runtime.TreeCaps{MaxNodes: cfg.TreeCaps.MaxNodes, MaxDepth: cfg.TreeCaps.MaxDepth},
			Style:    drv.style,
		}, drv.typemap, inputsByTarget[ti])
		res.Diags = append(res.Diags, diags...)
		if diagnostics.HasErrors(res.Diags) || mode != ModeGenerate {
			continue
		}
		outDir := t.Abs(cfg)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, err
		}
		for name, content := range files {
			path := filepath.Join(outDir, name)
			if prev, err := os.ReadFile(path); err == nil && string(prev) == string(content) {
				continue // unchanged: keep mtime for build systems
			}
			if err := cache.WriteFileAtomic(path, content, 0o644); err != nil {
				return nil, err
			}
		}
		d, err := removeStaleGenerated(cfg, t, files)
		if err != nil {
			return nil, err
		}
		res.Diags = append(res.Diags, d...)
	}
	if diagnostics.HasErrors(res.Diags) || mode != ModeGenerate {
		return res, nil
	}
	// The committed tree is now exactly the live set: every entry this
	// run wrote or hit, and nothing else (doc 21 §4).
	live := map[string]bool{cache.CatalogFile: true, cache.EnvFile: true}
	for _, cq := range queries {
		for _, ref := range cq.refs {
			live[cache.OracleFileName(ref)] = true
		}
	}
	pruneDiags, err := pruneCache(cfg, live)
	if err != nil {
		return nil, err
	}
	res.Diags = append(res.Diags, pruneDiags...)
	if err := writeExplainData(cfg, resolution, queries); err != nil {
		return nil, err
	}
	return res, nil
}

// nativeDDLDiag converts a native catalog-builder refusal into
// SQLETCH215 with a span into the schema file — a user's schema shape
// outside the modeled subset, not an environment failure (the same
// distinction versionPinDiag draws for SQLETCH200).
func nativeDDLDiag(err error) (diagnostics.Diagnostic, bool) {
	var ue *mysql.UnsupportedDDLError
	if !errors.As(err, &ue) {
		return diagnostics.Diagnostic{}, false
	}
	d := diagnostics.Errorf(diagnostics.CodeNativeDDL,
		diagnostics.Span{File: ue.File, Start: ue.Pos, End: ue.Pos + 1},
		"the native oracle cannot model this schema statement: %s", ue.Msg)
	return d, true
}

func oracleDiag(q *template.QueryTemplate, r ast.Rendering, err error) diagnostics.Diagnostic {
	var ne *dialect.NativeUnsupportedError
	if errors.As(err, &ne) {
		span := q.HeaderSpan
		if ne.Pos >= 0 {
			tOff, _ := r.Map.ToTemplate(ne.Pos)
			span = diagnostics.Span{File: q.HeaderSpan.File, Start: tOff, End: tOff + 1}
		}
		d := diagnostics.Errorf(diagnostics.CodeNativeUnsupported, span,
			"the native oracle refuses %s: outside its modeled subset, and it never guesses", ne.Construct)
		hint := ne.Hint
		if hint == "" {
			hint = "switch to database.oracle: \"server\""
		}
		return d.WithHint("%s", hint)
	}
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

// expandShapes precomposes every reachable shape via the SAME runtime
// composer generated code uses, so the expansion is byte-identical to
// what hybrid composition would produce.
func expandShapes(q *template.QueryTemplate, frags []runtime.Frag,
	maxShapes int, style runtime.Style) (map[string]runtime.Expanded, []diagnostics.Diagnostic) {

	keys, truncated := shape.Enumerate(q, maxShapes)
	if truncated {
		return nil, []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeExpansionLarge,
			q.HeaderSpan,
			"%s reaches more than %d shapes; raise static_expansion.max_shapes or drop the query from static expansion",
			q.Name, maxShapes)}
	}
	out := make(map[string]runtime.Expanded, len(keys))
	for _, k := range keys {
		sqlText, argIdx := runtime.ComposeStyle(style, frags, runtime.ShapeKey{Guards: k.Guards, Choices: k.Choices, Orders: k.Orders})
		out[k.String()] = runtime.Expanded{SQL: sqlText, ArgIdx: argIdx}
	}
	return out, nil
}

// isControlOnlyParam reports a parameter with no bind occurrences —
// pure @when/@choose/@order-by/@filter-tree control, never sent to the
// database.
func isControlOnlyParam(q *template.QueryTemplate, name string) bool {
	p := q.Params[name]
	return p == nil || len(p.Occurrences) == 0
}

func paramSpan(q *template.QueryTemplate, name string) diagnostics.Span {
	if p := q.Params[name]; p != nil && len(p.Occurrences) > 0 {
		return p.Occurrences[0].Span
	}
	return q.HeaderSpan
}

// writeExpandedFiles materializes the audit surface: one .sql file per
// shape under .sqletch/expanded/<target>/<query>/. The target slug is
// part of the path because a query name is unique only within its
// target — without it two packages' same-named queries would overwrite
// each other's audit surface.
func writeExpandedFiles(cfg config.Config, t config.ResolvedTarget, query string, shapes map[string]runtime.Expanded) error {
	dir := cfg.Abs(filepath.Join(filepath.Dir(cfg.Cache.Path), "expanded", filepath.FromSlash(t.Slug()), query))
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for key, e := range shapes {
		name := shapeFileName(key) + ".sql"
		if err := cache.WriteFileAtomic(filepath.Join(dir, name), []byte(e.SQL+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// shapeFileName makes a canonical shape key filename-safe:
// "g=3;c=1,0" -> "g3_c1-0".
func shapeFileName(key string) string {
	r := strings.NewReplacer("=", "", ";", "_", ",", "-")
	return r.Replace(key)
}

// explainData is the per-query summary consumed by `sqletch explain`.
type explainData struct {
	Name       string           `json:"name"`
	Guards     []string         `json:"guards"`
	Chooses    []string         `json:"chooses,omitempty"`
	ShapeCount string           `json:"shape_count"`
	Params     []string         `json:"params"`
	Columns    []string         `json:"columns"`
	Policies   []policyCoverage `json:"policies,omitempty"`
	MaximalSQL string           `json:"maximal_sql"`
}

// policyCoverage is one applied policy in the explain report (§6.3):
// per query, which policies bite and whether each is woven or opted
// out — "which queries are unscoped, and why" as a command's output.
type policyCoverage struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"` // "woven" | "opted_out"
	Reason    string   `json:"reason,omitempty"`
	Conjuncts []string `json:"conjuncts,omitempty"`
}

func policyCoverageOf(cq *compiledQuery) []policyCoverage {
	var out []policyCoverage
	for _, wp := range cq.woven {
		pc := policyCoverage{Name: wp.Policy.Name}
		if wp.OptedOut {
			pc.Status = "opted_out"
			pc.Reason = wp.OptOutReason
		} else {
			pc.Status = "woven"
			pc.Conjuncts = wp.Conjuncts
		}
		out = append(out, pc)
	}
	return out
}

func writeExplainData(cfg config.Config, resolution config.Resolution, queries []*compiledQuery) error {
	root := cfg.Abs(filepath.Join(filepath.Dir(cfg.Cache.Path), "explain"))
	drv := driverFor(cfg)
	for _, cq := range queries {
		d := explainData{Name: cq.q.Name, ShapeCount: shape.CountExpand(cq.q, drv.expandIn).String(),
			Policies: policyCoverageOf(cq), MaximalSQL: cq.rs[0].SQL}
		for i, g := range cq.q.GuardAtoms {
			d.Guards = append(d.Guards, fmt.Sprintf("bit %d: %s", i, g.Param))
		}
		for _, it := range cq.q.Items {
			if c, ok := it.(*template.Choose); ok {
				var cases strings.Builder
				for i, cs := range c.Cases {
					if i > 0 {
						cases.WriteString(", ")
					}
					cases.WriteString(cs.Name)
				}
				if c.Default != nil {
					cases.WriteString(", (default)")
				}
				d.Chooses = append(d.Chooses, fmt.Sprintf("%s: %s", c.Param, cases.String()))
			}
		}
		for _, name := range cq.q.ParamOrder {
			tr := cq.paramTypes[name]
			var kinds []string
			if cq.q.Params[name].Optional {
				kinds = append(kinds, "omittable")
			}
			if cq.nullableParams[name] {
				kinds = append(kinds, "nullable")
			}
			opt := ""
			if len(kinds) > 0 {
				opt = " (" + strings.Join(kinds, ", ") + ")"
			}
			d.Params = append(d.Params, fmt.Sprintf("%s: %s%s", name, tr.Name, opt))
		}
		for i, col := range cq.descs[0].Columns {
			null := "not null"
			if i < len(cq.nullable) && cq.nullable[i] {
				null = "nullable"
			}
			why := ""
			if i < len(cq.nullReasons) && cq.nullReasons[i] != "" {
				why = "; " + cq.nullReasons[i]
			}
			d.Columns = append(d.Columns, fmt.Sprintf("%s: %s (%s%s)", col.Name, col.Type.Name, null, why))
		}
		data, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		dir := filepath.Join(root, filepath.FromSlash(resolution.Targets[cq.target].Slug()))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, cq.q.Name+".json")
		if err := cache.WriteFileAtomic(path, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// nameScopeDiags warns when a query name that `overrides` or
// `static_expansion.queries` references exists in more than one target.
// Both key on a NAME, which is unique only within a target since
// design 19, so such an entry silently applies to several queries —
// visible here rather than as a surprise in the generated code.
func nameScopeDiags(cfg config.Config, queryTargets map[string][]string) []diagnostics.Diagnostic {
	referenced := map[string]string{} // name -> which config key names it
	for _, o := range cfg.Overrides {
		if o.Query != "" {
			referenced[o.Query] = "overrides"
		}
	}
	for _, n := range cfg.Expansion.Queries {
		referenced[n] = "static_expansion.queries"
	}
	names := make([]string, 0, len(referenced))
	for n := range referenced {
		names = append(names, n)
	}
	sort.Strings(names)
	var diags []diagnostics.Diagnostic
	for _, n := range names {
		paths := queryTargets[n]
		if len(paths) < 2 {
			continue
		}
		diags = append(diags, diagnostics.Warnf(diagnostics.CodeTargetNameSpan,
			diagnostics.Span{File: cfg.Path},
			"%s names query %q, which is defined in %d targets (%s): the entry applies to all of them",
			referenced[n], n, len(paths), strings.Join(paths, ", ")).
			WithHint("rename one of the queries if only one was meant"))
	}
	return diags
}

// removeStaleGenerated deletes the *.gen.go files in a target's output
// directory that this run did not write — a deleted or renamed query
// otherwise leaves a stale method compiled into the package forever.
//
// Only `*.gen.go` is ever removed: every file sqletch emits ends in
// that suffix (pinned by a codegen test), so a hand-written file in the
// same directory is never touched, and neither is a subdirectory. An
// output directory OUTSIDE the project (the absolute path config.Load
// only warns about) is skipped with a warning rather than swept:
// deleting files outside the repository sqletch was pointed at is not
// something a committed config gets to ask for.
func removeStaleGenerated(cfg config.Config, t config.ResolvedTarget, written map[string][]byte) ([]diagnostics.Diagnostic, error) {
	dir := t.Abs(cfg)
	rel, err := filepath.Rel(cfg.Dir, dir)
	outside := err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	if filepath.IsAbs(t.Path) || outside {
		return []diagnostics.Diagnostic{diagnostics.Warnf(diagnostics.CodePathEscape,
			diagnostics.Span{File: cfg.Path},
			"output %q is outside the project directory, so stale generated files there are not removed", t.Path).
			WithHint("move the output inside the project so sqletch can keep it consistent, or delete obsolete *.gen.go files yourself")}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gen.go") {
			continue
		}
		if _, ok := written[e.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// PrintDiags renders diagnostics in text or JSON to w.
func PrintDiags(w io.Writer, res *Result, jsonFormat bool) {
	diagnostics.Sort(res.Diags)
	// One line index per source file, reused across all of that file's
	// diagnostics: byte offset → (line, col) is then O(log lines) rather
	// than an O(offset) rescan per diagnostic (a file with many
	// diagnostics would otherwise be O(n·d) to render).
	lineMaps := map[string]*diagnostics.LineMap{}
	lineMapFor := func(file string) *diagnostics.LineMap {
		lm, ok := lineMaps[file]
		if !ok {
			lm = diagnostics.NewLineMap(res.Sources[file])
			lineMaps[file] = lm
		}
		return lm
	}
	for _, d := range res.Diags {
		lm := lineMapFor(d.Span.File)
		if jsonFormat {
			line, col := lm.LineCol(d.Span.Start)
			enc, _ := json.Marshal(map[string]any{
				"code": d.Code, "severity": d.Severity.String(),
				"file": d.Span.File, "line": line, "col": col,
				"message": d.Message, "hint": d.Hint,
			})
			fmt.Fprintln(w, string(enc))
			continue
		}
		fmt.Fprintln(w, d.RenderExcerptWith(res.Sources[d.Span.File], lm))
	}
}
