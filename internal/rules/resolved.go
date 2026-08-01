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
// Known limitation (documented in design 03 §6): unqualified column
// references *inside subquery scopes* are skipped — innermost-first
// resolution is not modeled in v0.1. Qualified references are checked
// everywhere. The per-shape EXPLAIN property test is the mechanical
// backstop for the skipped corner.
func CheckResolved(q *template.QueryTemplate, maxR ast.Rendering,
	maxTree dialect.Tree, cat *cache.Catalog) []diagnostics.Diagnostic {

	var diags []diagnostics.Diagnostic
	res := newResolver(q, maxR, maxTree, cat)

	// R3: every column reference resolving into an optional join must
	// be guarded at least as strongly as the join.
	for _, cr := range maxTree.ColumnRefs() {
		if cr.Star || len(cr.Fields) == 0 {
			continue
		}
		var rel *relInfo
		if len(cr.Fields) >= 2 {
			rel = res.byName[cr.Fields[len(cr.Fields)-2]]
		} else {
			if cr.InSubquery {
				continue // conservative skip; see doc comment
			}
			name := cr.Fields[0]
			if res.outputAliases[name] {
				continue
			}
			cands := res.columnCandidates(name)
			if len(cands) == 0 {
				continue
			}
			if len(cands) > 1 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeAmbiguousRef,
					res.spanAt(cr.Loc, len(name)),
					"unqualified column %q matches %d relations; qualify it", name, len(cands)))
				continue
			}
			rel = cands[0]
		}
		if rel == nil || len(rel.guards) == 0 {
			continue
		}
		refGuards := res.guardsAt(cr.Loc)
		if !supersetAtoms(refGuards, rel.guards) {
			field := cr.Fields[len(cr.Fields)-1]
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeScopeViolation,
				res.spanAt(cr.Loc, len(field)),
				"%q resolves to the optional join %q guarded by %s, but this reference is not covered by that guard (R3)",
				field, rel.name(), atomsString(rel.guards)).
				WithHint("move this reference inside @if-present(%s)", atomsParamList(rel.guards)))
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
		if rel := res.byName[ti.Qualifier]; rel != nil && len(rel.guards) > 0 {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeStarExpansion,
				res.spanAt(ti.Loc, len(ti.Qualifier)),
				"%s.* projects columns of an optional join; optional joins may not contribute result columns (R2)", ti.Qualifier))
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
					if c := tbl.Col(name); c != nil && c.NotNull && !c.HasDefault {
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
	table  *cache.Table
	frag   *template.IfPresent
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
	q             *template.QueryTemplate
	maxR          ast.Rendering
	rels          []*relInfo
	byName        map[string]*relInfo
	outputAliases map[string]bool
}

func newResolver(q *template.QueryTemplate, maxR ast.Rendering,
	maxTree dialect.Tree, cat *cache.Catalog) *resolver {

	res := &resolver{q: q, maxR: maxR, byName: map[string]*relInfo{}, outputAliases: map[string]bool{}}
	for _, rr := range maxTree.Relations() {
		ri := &relInfo{RelRef: rr}
		if frag := res.fragAt(rr.Loc); frag != nil {
			ri.guards = frag.Guards
			ri.frag = frag
		}
		if cat != nil && rr.Table != "" {
			ri.table = cat.Lookup(rr.Table)
		}
		res.rels = append(res.rels, ri)
		if n := ri.name(); n != "" {
			res.byName[n] = ri
		}
	}
	for _, ti := range maxTree.TargetItems() {
		if ti.Name != "" {
			res.outputAliases[ti.Name] = true
		}
	}
	return res
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

func (res *resolver) columnCandidates(col string) []*relInfo {
	var out []*relInfo
	for _, rel := range res.rels {
		if rel.table != nil && rel.table.Col(col) != nil {
			out = append(out, rel)
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
