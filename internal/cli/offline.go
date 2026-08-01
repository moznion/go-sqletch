package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/cache"
	"github.com/moznion/sqletch/internal/config"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/rules"
	"github.com/moznion/sqletch/internal/template"
)

// OfflineChecker is the LSP-facing slice of the pipeline
// (docs/design/10-lsp.md §4): scan + lexical + R1 per file, memoized
// by content hash; duplicate-name detection across the workspace; and
// the catalog-dependent pass for exactly those queries whose catalog
// and renderings are all present in the committed cache. It NEVER
// opens a database connection.
type OfflineChecker struct {
	cfg  config.Config
	drv  driver
	memo map[string]*fileMemo
}

type fileMemo struct {
	hash  [sha256.Size]byte
	file  *template.QueryFile
	diags []diagnostics.Diagnostic
	// rends holds per-query renderings for the cache lookup of the
	// catalog-dependent pass; nil when the file has scan errors.
	rends map[string][]ast.Rendering
}

// WorkspaceCheck is one consistent snapshot of the workspace's
// diagnostics, keyed by absolute template path.
type WorkspaceCheck struct {
	Diags   map[string][]diagnostics.Diagnostic
	Files   map[string]*template.QueryFile
	Sources map[string][]byte
}

func NewOfflineChecker(cfg config.Config) *OfflineChecker {
	return &OfflineChecker{cfg: cfg, drv: driverFor(cfg), memo: map[string]*fileMemo{}}
}

// Check analyzes the workspace with overlay contents (open editor
// buffers) replacing disk. The error return is environmental (an
// unreadable glob-matched file); user mistakes are diagnostics.
func (c *OfflineChecker) Check(overlay map[string][]byte) (WorkspaceCheck, error) {
	res := WorkspaceCheck{
		Diags:   map[string][]diagnostics.Diagnostic{},
		Files:   map[string]*template.QueryFile{},
		Sources: map[string][]byte{},
	}

	// File set: config globs ∪ overlay keys. A glob error (typoed
	// pattern, or simply no template files yet in a fresh project) must
	// not take the whole server down — the open buffers still get
	// checked.
	seen := map[string]bool{}
	var paths []string
	if globbed, err := c.cfg.ExpandGlobs(c.cfg.Queries); err == nil {
		for _, p := range globbed {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range overlay {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	// Per-file phase, memoized by content hash.
	for _, p := range paths {
		src, ok := overlay[p]
		if !ok {
			var err error
			src, err = os.ReadFile(p)
			if err != nil {
				return WorkspaceCheck{}, err
			}
		}
		m := c.analyzeFile(p, src)
		res.Sources[p] = src
		res.Files[p] = m.file
		res.Diags[p] = append([]diagnostics.Diagnostic(nil), m.diags...)
	}

	// Workspace phase: duplicate query names, first definition in
	// sorted-path order wins — the same collation the pipeline gets
	// from ExpandGlobs, so CLI and LSP flag the same file.
	names := map[string]string{}
	dup := map[*template.QueryTemplate]bool{}
	for _, p := range paths {
		for _, q := range res.Files[p].Queries {
			if prev, isDup := names[q.Name]; isDup {
				dup[q] = true
				res.Diags[p] = append(res.Diags[p], diagnostics.Errorf(diagnostics.CodeDuplicateQueryName,
					q.HeaderSpan, "query %q already defined in %s", q.Name, prev))
				continue
			}
			names[q.Name] = p
		}
	}

	// Catalog-dependent phase, cache hits only. A file that already
	// has errors is skipped (the pipeline stops there too); a query
	// with any rendering missing from the cache is skipped entirely —
	// partial oracle data must not produce half-true agreement
	// diagnostics.
	if cat, store, fp, ok := c.loadCatalog(); ok {
		for _, p := range paths {
			m := c.memo[p]
			if m.rends == nil || diagnostics.HasErrors(res.Diags[p]) {
				continue
			}
			for _, q := range m.file.Queries {
				if dup[q] {
					continue
				}
				rs := m.rends[q.Name]
				if rs == nil {
					continue
				}
				descs, hit := loadDescs(store, fp, rs)
				if !hit {
					continue
				}
				_, d, err := resolvedChecks(c.drv, c.cfg.Dialect, q, rs, descs, cat)
				if err != nil {
					continue // internal re-parse failure; the CLI will surface it
				}
				res.Diags[p] = append(res.Diags[p], d...)
			}
		}
	}

	for p := range res.Diags {
		diagnostics.Sort(res.Diags[p])
	}
	return res, nil
}

// analyzeFile runs the per-file offline phase (scan, lexical, R1) and
// memoizes it by content hash.
func (c *OfflineChecker) analyzeFile(path string, src []byte) *fileMemo {
	h := sha256.Sum256(src)
	if m, ok := c.memo[path]; ok && m.hash == h {
		return m
	}
	m := &fileMemo{hash: h}
	file, diags := template.NewScanner(c.drv.profile).ScanFile(path, src)
	m.file = file
	m.diags = diags
	// Rendering a template whose scan already failed risks probing
	// malformed fragments for no user benefit; the scan diagnostics
	// are the actionable ones.
	if !diagnostics.HasErrors(diags) {
		m.rends = map[string][]ast.Rendering{}
		for _, q := range file.Queries {
			m.diags = append(m.diags, rules.CheckLexical(c.drv.profile, q)...)
			rs, err := ast.Renderings(c.drv.profile, q)
			if err != nil {
				m.diags = append(m.diags, diagnostics.Errorf(diagnostics.CodeRenderingParse,
					q.HeaderSpan, "internal: rendering failed: %v", err))
				continue
			}
			m.rends[q.Name] = rs
			m.diags = append(m.diags, rules.CheckR1(c.drv.profile, c.drv.frontend, q, rs)...)
		}
	}
	c.memo[path] = m
	return m
}

// loadCatalog recomputes the schema fingerprint from disk and loads
// the committed catalog; any gap (unreadable schema, cold cache)
// reports !ok and the caller degrades to the offline-only checks.
func (c *OfflineChecker) loadCatalog() (*cache.Catalog, *cache.Store, string, bool) {
	schemaPaths, err := c.cfg.ExpandGlobs(c.cfg.Schema.Files)
	if err != nil {
		return nil, nil, "", false
	}
	var schemaFiles []cache.SchemaFile
	for _, p := range schemaPaths {
		content, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, "", false
		}
		rel, _ := filepath.Rel(c.cfg.Dir, p)
		schemaFiles = append(schemaFiles, cache.SchemaFile{Path: rel, Content: content})
	}
	fp := cache.Fingerprint(c.cfg.Dialect, c.cfg.ServerVersion, schemaFiles)
	store := cache.NewStore(c.cfg.Abs(c.cfg.Cache.Path))
	cat, ok := store.LoadCatalog(fp)
	if !ok {
		return nil, nil, "", false
	}
	return cat, store, fp, true
}

// loadDescs resolves every rendering through the committed oracle
// cache; a single miss fails the whole query (all-or-nothing).
func loadDescs(store *cache.Store, fp string, rs []ast.Rendering) ([]dialect.Desc, bool) {
	descs := make([]dialect.Desc, len(rs))
	for i, r := range rs {
		e, ok := store.LoadOracle(fp, r.SQL)
		if !ok {
			return nil, false
		}
		descs[i] = entryToDesc(e)
	}
	return descs, true
}

// resolvedChecks is the catalog-dependent pass shared by pipeline.Run
// and the OfflineChecker: R3/R2/planner-sensitivity (CheckResolved),
// Tier 1 type agreement and parameter resolution, `-- @param` hint
// validation, and Tier 2 missing-annotation enforcement. Offline once
// the descs are in hand.
func resolvedChecks(drv driver, dialectName string, q *template.QueryTemplate, rs []ast.Rendering,
	descs []dialect.Desc, cat *cache.Catalog) (map[string]dialect.TypeRef, []diagnostics.Diagnostic, error) {

	tree, err := drv.frontend.Parse(rs[0].SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("internal: maximal rendering re-parse: %w", err)
	}
	var diags []diagnostics.Diagnostic
	diags = append(diags, rules.CheckResolved(q, rs[0], tree, cat)...)
	paramTypes := map[string]dialect.TypeRef{}
	if !drv.annotationsRequired {
		// Tier 1: the oracle types parameters; agreement is checked
		// across renderings.
		diags = append(diags, rules.CheckTypeAgreement(q, rs, descs)...)
		types, d := rules.ResolveParamTypes(q, rs, descs)
		diags = append(diags, d...)
		for _, pt := range types {
			paramTypes[pt.Name] = pt.Type
		}
	}
	// `-- @param` hints override (Tier 1) or supply (Tier 2) the
	// oracle's parameter types.
	for name, hint := range q.TypeHints {
		if _, known := q.Params[name]; !known {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType,
				hint.Span, "@param hint for unknown parameter %q", name))
			continue
		}
		tr, ok := drv.typeByName(hint.SQLType)
		if !ok {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType,
				hint.Span, "unknown SQL type %q in @param hint", hint.SQLType))
			continue
		}
		paramTypes[name] = tr
	}
	if drv.annotationsRequired {
		// Tier 2: the protocol does not type parameters; every
		// non-control parameter needs its annotation.
		for _, name := range q.ParamOrder {
			if _, ok := paramTypes[name]; ok {
				continue
			}
			if isControlOnlyParam(q, name) {
				continue
			}
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType,
				paramSpan(q, name),
				"parameter %q needs a `-- @param %s: <type>` annotation (the %s protocol does not type parameters)",
				name, name, dialectName))
		}
	}
	if drv.columnHintsRequired {
		// SQLite: expression columns have no declared type; fill them
		// from `-- @column` annotations, in every rendering's Desc
		// (the cache stores the raw oracle answer).
		known := map[string]bool{}
		for di := range descs {
			for ci := range descs[di].Columns {
				col := &descs[di].Columns[ci]
				known[col.Name] = true
				if col.Type.OID != 0 {
					continue
				}
				hint, ok := q.ColumnHints[col.Name]
				if !ok {
					if di == 0 { // one diagnostic per column, not per rendering
						diags = append(diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType,
							q.HeaderSpan,
							"result column %q is an expression with no declared type on %s; add `-- @column %s: <type>`",
							col.Name, dialectName, col.Name))
					}
					continue
				}
				tr, ok := drv.typeByName(hint.SQLType)
				if !ok {
					if di == 0 {
						diags = append(diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType,
							hint.Span, "unknown SQL type %q in @column hint", hint.SQLType))
					}
					continue
				}
				col.Type = tr
			}
		}
		var hintNames []string
		for name := range q.ColumnHints {
			if !known[name] {
				hintNames = append(hintNames, name)
			}
		}
		sort.Strings(hintNames)
		for _, name := range hintNames {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeUnsupportedType,
				q.ColumnHints[name].Span, "@column hint for unknown result column %q", name))
		}
	}
	return paramTypes, diags, nil
}
