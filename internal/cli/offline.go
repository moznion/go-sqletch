package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/rules"
	"github.com/moznion/go-sqletch/internal/template"
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
	// pols are the compiled policies (empty when the config declares
	// none or the set failed validation); polDiags carry the one-time
	// SQLETCH303s, surfaced against the config path on every Check.
	// Weaving is offline by construction (§4.1: render + parse, no
	// catalog), so the LSP runs it too.
	pols     []policy.Policy
	polDiags []diagnostics.Diagnostic

	// catMemo caches the loaded catalog across Checks so a keystroke
	// does not re-glob, re-read, and re-hash every schema file (and
	// re-parse the catalog JSON) when nothing changed. It is validated
	// by a cheap stat signature of the schema files plus the catalog
	// file, so a schema edit or a `generate` run (which rewrites the
	// catalog) refreshes it; the per-query oracle Descs are still read
	// fresh every Check, so a cold→warm cache transition is picked up
	// with no stale-diagnostic risk.
	catMemo *catalogMemo
	// schemaReads counts full schema-file reads (test seam for the
	// memo: an unchanged Check must not read schema files again).
	schemaReads int
}

// statSig is a file's cheap change signature: size + mod time, or
// absent. Used only for in-process memo invalidation, never emitted.
type statSig struct {
	size   int64
	modNs  int64
	exists bool
}

func statOf(path string) statSig {
	fi, err := os.Stat(path)
	if err != nil {
		return statSig{}
	}
	return statSig{size: fi.Size(), modNs: fi.ModTime().UnixNano(), exists: true}
}

type schemaStat struct {
	path string
	sig  statSig
}

type catalogMemo struct {
	schema  []schemaStat
	catPath string
	catSig  statSig
	cat     *cache.Catalog
	store   *cache.Store
	fp      string
	ok      bool
}

func statSchema(paths []string) []schemaStat {
	out := make([]schemaStat, len(paths))
	for i, p := range paths {
		out[i] = schemaStat{path: p, sig: statOf(p)}
	}
	return out
}

func sameSchemaStat(a, b []schemaStat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type fileMemo struct {
	hash  [sha256.Size]byte
	file  *template.QueryFile
	diags []diagnostics.Diagnostic
	// rends holds per-query renderings for the cache lookup of the
	// catalog-dependent pass; nil when the file has scan errors.
	// Renderings are of the WOVEN templates in wovenq.
	rends map[string][]ast.Rendering
	// wovenq maps query name -> woven template; the catalog-dependent
	// pass must consume it, never the scanned original.
	wovenq map[string]*template.QueryTemplate
}

// WorkspaceCheck is one consistent snapshot of the workspace's
// diagnostics, keyed by absolute template path.
type WorkspaceCheck struct {
	Diags   map[string][]diagnostics.Diagnostic
	Files   map[string]*template.QueryFile
	Sources map[string][]byte
}

func NewOfflineChecker(cfg config.Config) *OfflineChecker {
	c := &OfflineChecker{cfg: cfg, drv: driverFor(cfg), memo: map[string]*fileMemo{}}
	c.pols, c.polDiags = compilePolicies(c.drv, cfg)
	return c
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

	// Broken policies degrade to unwoven checking (never a crash),
	// with the SQLETCH303s pinned to the config file on every snapshot.
	if len(c.polDiags) > 0 {
		res.Diags[c.cfg.Path] = append(res.Diags[c.cfg.Path], c.polDiags...)
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
				wq := m.wovenq[q.Name]
				if wq == nil {
					wq = q
				}
				descs, hit := loadDescs(store, fp, rs)
				if !hit {
					continue
				}
				_, d, err := resolvedChecks(c.drv, c.cfg.Dialect, c.pols, wq, rs, descs, cat)
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
	file, diags := scanSource(template.NewScanner(c.drv.profile), path, src)
	m.file = file
	m.diags = diags
	// Rendering a template whose scan already failed risks probing
	// malformed fragments for no user benefit; the scan diagnostics
	// are the actionable ones.
	if !diagnostics.HasErrors(diags) {
		m.rends = map[string][]ast.Rendering{}
		m.wovenq = map[string]*template.QueryTemplate{}
		for _, q := range file.Queries {
			wres, rs, d, err := scanChecks(c.drv, c.pols, q)
			m.diags = append(m.diags, d...)
			if err != nil {
				m.diags = append(m.diags, diagnostics.Errorf(diagnostics.CodeRenderingParse,
					q.HeaderSpan, "internal: rendering failed: %v", err))
				continue
			}
			m.rends[q.Name] = rs
			m.wovenq[q.Name] = wres.Query
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
	schema := statSchema(schemaPaths)
	// Fast path: schema files unchanged AND the catalog file's presence
	// and content are unchanged (a `generate` run rewrites it, and a
	// cold→warm transition makes it appear). Neither reads any schema
	// file nor re-parses the catalog JSON.
	if m := c.catMemo; m != nil && sameSchemaStat(m.schema, schema) && statOf(m.catPath) == m.catSig {
		if !m.ok {
			return nil, nil, "", false
		}
		return m.cat, m.store, m.fp, true
	}

	c.schemaReads += len(schemaPaths)
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
	cacheDir := c.cfg.Abs(c.cfg.Cache.Path)
	store := cache.NewStore(cacheDir)
	catPath := filepath.Join(cacheDir, cache.CatalogFileName(fp))
	cat, ok := store.LoadCatalog(fp)
	// Record the memo AFTER the load so catSig reflects the file we just
	// read (present or absent); a later appearance/rewrite invalidates.
	c.catMemo = &catalogMemo{
		schema:  schema,
		catPath: catPath,
		catSig:  statOf(catPath),
		cat:     cat,
		store:   store,
		fp:      fp,
		ok:      ok,
	}
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
		descs[i] = dialect.DescFromEntry(e)
	}
	return descs, true
}

// resolvedChecks is the catalog-dependent pass shared by pipeline.Run
// and the OfflineChecker: R3/R2/planner-sensitivity (CheckResolved),
// policy enforcement (SQLETCH124/126 — so violations appear live in
// the editor), Tier 1 type agreement and parameter resolution,
// `-- @param` hint validation, and Tier 2 missing-annotation
// enforcement. Offline once the descs are in hand. q must be the
// WOVEN template.
func resolvedChecks(drv driver, dialectName string, pols []policy.Policy, q *template.QueryTemplate, rs []ast.Rendering,
	descs []dialect.Desc, cat *cache.Catalog) (map[string]dialect.TypeRef, []diagnostics.Diagnostic, error) {

	tree, err := drv.frontend.Parse(rs[0].SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("internal: maximal rendering re-parse: %w", err)
	}
	var diags []diagnostics.Diagnostic
	diags = append(diags, rules.CheckResolved(q, rs[0], tree, cat)...)
	diags = append(diags, policy.Enforce(drv.profile, pols, q, tree)...)
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
	// `-- @param` hints supply the parameter types on Tier 2, and on
	// Tier 1 assert the oracle's. Sorted: diagnostics may not depend on
	// map iteration order.
	hintedParams := make([]string, 0, len(q.TypeHints))
	for name := range q.TypeHints {
		hintedParams = append(hintedParams, name)
	}
	sort.Strings(hintedParams)
	for _, name := range hintedParams {
		hint := q.TypeHints[name]
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
		// Tier 1: the oracle already typed this parameter against the
		// real server. An annotation that disagrees would bind at a type
		// the query was never verified with, breaking premise P1 — and
		// the mismatch is invisible to every other phase, because the
		// oracle types the rendering and never sees the annotation.
		if inferred, ok := paramTypes[name]; ok && inferred.OID != tr.OID {
			d := diagnostics.Errorf(diagnostics.CodeParamHintConflict, hint.Span,
				"`-- @param %s: %s` types the parameter as %s, but the oracle infers %s from the query; binding at an unverified type would break the compile-time type guarantee",
				name, hint.SQLType, tr.Name, inferred.Name)
			d.Hint = fmt.Sprintf("remove the annotation (%s infers parameter types)", dialectName)
			if drv.writableName != nil {
				if spelling, ok := drv.writableName(inferred.OID); ok {
					d.Hint += fmt.Sprintf(", or correct it to `-- @param %s: %s`", name, spelling)
				}
			}
			diags = append(diags, d)
			continue // the verified type wins; a rejected hint never reaches codegen
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
	// `-- @column` hints ASSERT against any column type the oracle
	// supplied (SQLETCH216, the SQLETCH213 rule applied to columns):
	// under the native backend hints SUPPLY expression types, so
	// whenever an oracle answer exists to check them against — a
	// server-backed run, or a catalog-typed direct column — a wrong
	// hint must be loud, never silently overridden. Sorted, and the
	// oracle wins.
	colHintNames := make([]string, 0, len(q.ColumnHints))
	for name := range q.ColumnHints {
		colHintNames = append(colHintNames, name)
	}
	sort.Strings(colHintNames)
	for _, name := range colHintNames {
		hint := q.ColumnHints[name]
		tr, ok := drv.typeByName(hint.SQLType)
		if !ok {
			continue // reported above where hints are load-bearing
		}
		for _, col := range descs[0].Columns {
			if col.Name != name || col.Type.OID == 0 || col.Type.OID == tr.OID {
				continue
			}
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeColumnHintConflict,
				hint.Span,
				"@column says %q is %s, but the oracle typed it %s — the oracle wins; fix or drop the annotation",
				name, hint.SQLType, col.Type.Name))
			break
		}
	}
	return paramTypes, diags, nil
}
