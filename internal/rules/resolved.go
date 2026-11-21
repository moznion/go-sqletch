package rules

import (
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// CheckResolved runs the catalog-dependent rule pass on the parsed
// maximal rendering: R3 (resolution-based guarded scope), R2's star
// expansion, ambiguity detection, and the planner-sensitive
// combination table. See docs/design/03-structural-rules.md §6–7.
//
// Qualified column references are resolved innermost-first (design 03
// §6): a qualifier introduced by an enclosing subquery scope
// (ColRef.ScopeAliases) shadows a same-named top-level relation, so the
// reference binds locally and the top-level guard is not re-derived for
// it; a correlated qualified reference (qualifier absent from every
// enclosing subquery FROM) is still checked. UNqualified references
// inside subquery scopes remain skipped — resolving a bare column
// against nested scopes is not modeled — with the per-shape EXPLAIN
// property test as the mechanical backstop.
func CheckResolved(profile dialect.LexerProfile, q *template.QueryTemplate, maxR ast.Rendering,
	maxTree dialect.Tree, cat *cache.Catalog) []diagnostics.Diagnostic {

	var diags []diagnostics.Diagnostic
	res := newResolver(profile, q, maxR, maxTree, cat)

	// R3: every column reference resolving into an optional join must
	// be guarded at least as strongly as the join.
	for _, cr := range maxTree.ColumnRefs() {
		if cr.Star || len(cr.Fields) == 0 {
			continue
		}
		var cands []*relInfo
		if len(cr.Fields) >= 2 {
			qualifier := cr.Fields[len(cr.Fields)-2]
			// Innermost-first resolution: a qualifier introduced by a
			// nearer subquery scope shadows a same-named top-level
			// relation, so this reference does not touch the top-level
			// join and its guard must not be re-derived here (design 03
			// §6). A correlated reference whose qualifier is absent from
			// every enclosing subquery FROM is not shadowed and is
			// checked as before, preserving the correlated-ref guarantee.
			//
			// Only a two-field ref (alias.column) can be alias-shadowed: a
			// three-or-more-field ref is schema-qualified
			// (schema.table.column) and a bare FROM alias — which carries
			// no schema — can never shadow it, so such a reference is
			// always resolved and checked against the top-level relation.
			//
			// Shadowing SKIPS the check, so it needs a name the engine
			// certainly binds to the inner scope ("must" key); the
			// relations the qualifier is checked against are every one it
			// MAY name under some server configuration ("may" key) — a
			// guard on any of them must cover the reference (fail closed).
			if len(cr.Fields) == 2 && res.scopeContains(cr.ScopeAliases, qualifier) {
				continue
			}
			cands = res.byMay[res.relMay(qualifier)]
		} else {
			if cr.InSubquery {
				continue // conservative skip; see doc comment
			}
			name := cr.Fields[0]
			// Output aliases are visible ONLY in ORDER BY (a bare
			// output-column name binds to the alias, universally across
			// dialects); in WHERE/HAVING/ON the reference binds to a real
			// column, so the guard must still be resolved even when the
			// name equals an output alias. Skipping on name equality alone
			// masked R3 for a guarded-column reference sharing an alias's
			// spelling (F2).
			if res.outputAliases[res.colFold(name)] && res.inOrderBy[cr.Loc] {
				continue
			}
			cands = res.columnCandidates(name)
			if len(cands) > 1 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeAmbiguousRef,
					res.spanAt(cr.Loc, len(name)),
					"unqualified column %q matches %d relations; qualify it", name, len(cands)))
				continue
			}
		}
		refGuards := res.guardsAt(cr.Loc)
		for _, rel := range cands {
			if len(rel.guards) == 0 || supersetAtoms(refGuards, rel.guards) {
				continue
			}
			field := cr.Fields[len(cr.Fields)-1]
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeScopeViolation,
				res.spanAt(cr.Loc, len(field)),
				"%q resolves to the optional join %q guarded by %s, but this reference is not covered by that guard (R3)",
				field, rel.name(), atomsString(rel.guards)).
				WithHint("move this reference inside @if-present(%s)", atomsParamList(rel.guards)))
			break
		}
	}

	// R2: SELECT * must not expand into optional-join columns.
	for _, ti := range maxTree.TargetItems() {
		if !ti.Star {
			continue
		}
		if ti.Qualifier == "" {
			for _, rel := range res.rels {
				if len(rel.guards) > 0 {
					diags = append(diags, diagnostics.Errorf(diagnostics.CodeStarExpansion,
						res.spanAt(ti.Loc, 1),
						"SELECT * would include columns of the optional join %q, changing the result shape per shape (R2)", rel.name()).
						WithHint("list the skeleton columns explicitly or use a qualified star"))
					break
				}
			}
			continue
		}
		for _, rel := range res.byMay[res.relMay(ti.Qualifier)] {
			if len(rel.guards) > 0 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeStarExpansion,
					res.spanAt(ti.Loc, len(ti.Qualifier)),
					"%s.* projects columns of an optional join; optional joins may not contribute result columns (R2)", ti.Qualifier))
				break
			}
		}
	}

	// R7 companion warning: an optional NOT NULL column without a
	// default fails at execution time in shapes that omit it —
	// prepare-level verification cannot see that (spec: known limits).
	if maxTree.Kind() == dialect.StmtInsert && len(q.InsertColGuards) > 0 && cat != nil {
		if rels := maxTree.Relations(); len(rels) > 0 {
			if tbl := cat.Lookup(rels[0].Table); tbl != nil {
				for _, gi := range q.InsertColGuards {
					name := strings.Trim(gi.Name, `"`)
					// Fold both sides like R2/R3: a mixed-case NOT NULL
					// column resolves at runtime on a case-insensitive
					// dialect, so an exact-string lookup would silently miss
					// it (F3).
					if c := res.col(tbl, name); c != nil && c.NotNull && !c.HasDefault {
						diags = append(diags, diagnostics.Warnf(diagnostics.CodeOptionalInsertNotNull, gi.Span,
							"column %q is NOT NULL without a default; shapes omitting it will fail at execution time", name).
							WithHint("add a database default, or make the column unconditional"))
					}
				}
			}
		}
	}

	// Planner-sensitive combinations: FOR UPDATE/SHARE cannot lock the
	// nullable side of an outer join — with a guarded LEFT JOIN this
	// fails only in guard-on shapes, invisibly to prepare (spec:
	// Verification Model known limits).
	if maxTree.HasLockingClause() {
		for _, rel := range res.rels {
			if len(rel.guards) > 0 && rel.Join == dialect.JoinLeft {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodePlannerSensitive,
					rel.fragSpan(q),
					"FOR UPDATE/SHARE combined with an optional LEFT JOIN fails at plan time in shapes where the join is active").
					WithHint("use an INNER join, an EXISTS predicate, or drop the locking clause"))
			}
		}
	}
	return diags
}

type relInfo struct {
	dialect.RelRef
	guards []template.GuardAtom
	// table is the catalog table the relation names exactly (nil when
	// none); mayTables is every catalog table it MAY name under some
	// server configuration (the "may" relation key) — the candidates an
	// unqualified column reference is checked against.
	table     *cache.Table
	mayTables []*cache.Table
	frag      *template.IfPresent
}

func (r *relInfo) name() string {
	if r.Alias != "" {
		return r.Alias
	}
	return r.Table
}

func (r *relInfo) fragSpan(q *template.QueryTemplate) diagnostics.Span {
	if r.frag != nil {
		return r.frag.Span
	}
	return q.HeaderSpan
}

type resolver struct {
	q    *template.QueryTemplate
	maxR ast.Rendering
	rels []*relInfo
	// byMay maps a relation name's (alias else table) "may" key to every
	// relation carrying it: a qualifier is checked against all relations
	// it could name under some server configuration (F3; MySQL's
	// lower_case_table_names). byMust is the same under the "must" key.
	byMay  map[string][]*relInfo
	byMust map[string][]*relInfo
	// outputAliases is keyed by column-folded alias name.
	outputAliases map[string]bool
	// inOrderBy holds the parsed-SQL byte offset of every statement-level
	// ORDER BY item, the only clause where a bare output-alias name binds
	// to the alias rather than a table column (F2).
	inOrderBy map[int]bool
	// colFold is the engine's column-name key; relMay/relMust are its
	// relation-name keys (dialect.IdentFolds).
	colFold, relMay, relMust func(string) string
}

func newResolver(profile dialect.LexerProfile, q *template.QueryTemplate, maxR ast.Rendering,
	maxTree dialect.Tree, cat *cache.Catalog) *resolver {

	res := &resolver{
		q: q, maxR: maxR,
		byMay:         map[string][]*relInfo{},
		byMust:        map[string][]*relInfo{},
		outputAliases: map[string]bool{},
		inOrderBy:     map[int]bool{},
		colFold:       dialect.ColumnFold(profile),
	}
	res.relMay, res.relMust = dialect.RelationFolds(profile)
	for _, rr := range maxTree.Relations() {
		ri := &relInfo{RelRef: rr}
		if frag := res.fragAt(rr.Loc); frag != nil {
			ri.guards = frag.Guards
			ri.frag = frag
		}
		if cat != nil && rr.Table != "" {
			ri.table = cat.Lookup(rr.Table)
			for i := range cat.Tables {
				t := &cat.Tables[i]
				if res.relMay(t.Name) == res.relMay(rr.Table) &&
					(rr.Schema == "" || res.relMay(t.Schema) == res.relMay(rr.Schema)) {
					ri.mayTables = append(ri.mayTables, t)
				}
			}
		}
		res.rels = append(res.rels, ri)
		if n := ri.name(); n != "" {
			res.byMay[res.relMay(n)] = append(res.byMay[res.relMay(n)], ri)
			res.byMust[res.relMust(n)] = append(res.byMust[res.relMust(n)], ri)
		}
	}
	for _, ti := range maxTree.TargetItems() {
		if ti.Name != "" {
			res.outputAliases[res.colFold(ti.Name)] = true
		}
	}
	for _, loc := range maxTree.OrderByLocs() {
		if loc >= 0 {
			res.inOrderBy[loc] = true
		}
	}
	return res
}

// scopeContains reports whether qualifier certainly names one of the
// enclosing subquery scope names ("must" key: shadowing skips a check,
// so a name that only MAY match must not shadow).
func (res *resolver) scopeContains(scope []string, qualifier string) bool {
	q := res.relMust(qualifier)
	for _, s := range scope {
		if res.relMust(s) == q {
			return true
		}
	}
	return false
}

// fragAt returns the @if-present fragment whose rendered range covers
// the given rendered offset, or nil for skeleton/@choose positions.
func (res *resolver) fragAt(loc int) *template.IfPresent {
	if loc < 0 {
		return nil
	}
	for _, fr := range res.maxR.Frags {
		if loc >= fr.Start && loc < fr.End {
			if ip, ok := fr.Item.(*template.IfPresent); ok {
				return ip
			}
			return nil
		}
	}
	return nil
}

// guardsAt returns the guard set of the fragment enclosing a rendered
// offset. Skeleton and @choose case positions have the empty set.
func (res *resolver) guardsAt(loc int) []template.GuardAtom {
	if frag := res.fragAt(loc); frag != nil {
		return frag.Guards
	}
	return nil
}

// col looks a column up in a catalog table under the dialect's
// identifier folding, so a mixed-case name matches on a
// case-insensitive dialect (F3).
func (res *resolver) col(tbl *cache.Table, name string) *cache.Column {
	folded := res.colFold(name)
	for i := range tbl.Cols {
		if res.colFold(tbl.Cols[i].Name) == folded {
			return &tbl.Cols[i]
		}
	}
	return nil
}

// columnCandidates returns every relation an unqualified column
// reference may bind to: one whose name MAY denote a catalog table
// (mayTables) having the column.
func (res *resolver) columnCandidates(col string) []*relInfo {
	var out []*relInfo
	for _, rel := range res.rels {
		for _, t := range rel.mayTables {
			if res.col(t, col) != nil {
				out = append(out, rel)
				break
			}
		}
	}
	return out
}

// spanAt maps a rendered offset back to a template span of the given
// display length.
func (res *resolver) spanAt(loc, n int) diagnostics.Span {
	if loc < 0 {
		return res.q.HeaderSpan
	}
	tOff, _ := res.maxR.Map.ToTemplate(loc)
	return diagnostics.Span{File: res.q.HeaderSpan.File, Start: tOff, End: tOff + n}
}

func supersetAtoms(super, sub []template.GuardAtom) bool {
	for _, a := range sub {
		if !containsAtom(super, a) {
			return false
		}
	}
	return true
}

func atomsString(atoms []template.GuardAtom) string {
	return "`" + atomsParamList(atoms) + "`"
}

func atomsParamList(atoms []template.GuardAtom) string {
	var s strings.Builder
	for i, a := range atoms {
		if i > 0 {
			s.WriteString(", ")
		}
		s.WriteString(a.Param)
	}
	return s.String()
}
