package rules

import (
	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// DeriveNullableParams reports the parameters whose Go type becomes
// optional.Option[T] because they write SQL NULL-able columns (design 20
// §3): a parameter is nullable iff EVERY one of its bind occurrences is a
// direct value position (dialect.Tree.ValueTargets) of a nullable column
// of the statement's base-table target. The returned map holds only true
// entries; a nil catalog derives nothing.
//
// Every uncertainty resolves to "not nullable" — the parameter stays a
// plain value, which is the pre-design-20 behavior. A wrong "nullable"
// can only surface as a loud constraint error when a caller passes None,
// but nothing here guesses in that direction either.
func DeriveNullableParams(profile dialect.LexerProfile, q *template.QueryTemplate, maxR ast.Rendering,
	maxTree dialect.Tree, cat *cache.Catalog) map[string]bool {

	if cat == nil || maxTree.StmtCount() != 1 {
		return nil
	}
	if k := maxTree.Kind(); k != dialect.StmtInsert && k != dialect.StmtUpdate {
		return nil
	}
	rels := maxTree.Relations()
	if len(rels) == 0 || rels[0].Table == "" {
		return nil
	}
	target := rels[0]
	fold := dialect.FoldIdent(profile)
	tbl := lookupFolded(cat, fold, target.Schema, target.Table)
	// A view column's catalog NOT NULL does not carry its base column's
	// constraint (PostgreSQL attnotnull is always false on a view), so a
	// view cannot vouch for nullability (design 20 §3.3).
	if tbl == nil || tbl.IsView {
		return nil
	}

	// nullableAt maps a template offset (the start of a :name occurrence)
	// to whether the value position there writes a nullable column.
	nullableAt := map[int]bool{}
	for _, vt := range maxTree.ValueTargets() {
		if vt.Qualifier != "" {
			name := target.Table
			if target.Alias != "" {
				name = target.Alias
			}
			if fold(vt.Qualifier) != fold(name) {
				continue
			}
		}
		tOff, synth := maxR.Map.ToTemplate(vt.Loc)
		// Placeholders are always synthesized segments anchored at their
		// :name occurrence; anything else is not an author-written bind.
		if !synth {
			continue
		}
		nullable := false
		for i := range tbl.Cols {
			if fold(tbl.Cols[i].Name) == fold(vt.Column) {
				nullable = !tbl.Cols[i].NotNull
				break
			}
		}
		if prev, seen := nullableAt[tOff]; seen {
			nullable = nullable && prev
		}
		nullableAt[tOff] = nullable
	}
	if len(nullableAt) == 0 {
		return nil
	}

	out := map[string]bool{}
	for _, name := range q.ParamOrder {
		p := q.Params[name]
		if !nullableEligible(q, p) {
			continue
		}
		all := true
		for _, occ := range p.Occurrences {
			// An occurrence absent from the maximal rendering (a
			// non-representative @choose case) maps to no target, which
			// keeps the parameter a plain value.
			if !nullableAt[occ.Span.Start] {
				all = false
				break
			}
		}
		if all {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nullableEligible filters design 20 §3.2's excluded parameter kinds.
func nullableEligible(q *template.QueryTemplate, p *template.Param) bool {
	if p == nil || p.Policy != "" || len(p.Occurrences) == 0 {
		return false
	}
	for _, occ := range p.Occurrences {
		if occ.InFilterTree || occ.InIn {
			return false
		}
	}
	for _, g := range q.GuardAtoms {
		if g.Param == p.Name && g.IsValue() {
			return false // @when control parameter, typed by its literal
		}
	}
	return true
}

// lookupFolded finds a catalog table under the dialect's identifier
// folding, honoring an explicit schema qualifier. Exact matches win so a
// case-sensitive dialect never falls through to a differently-cased
// table.
func lookupFolded(cat *cache.Catalog, fold func(string) string, schema, name string) *cache.Table {
	if t := cat.LookupQualified(schema, name); t != nil {
		return t
	}
	for i := range cat.Tables {
		t := &cat.Tables[i]
		if fold(t.Name) != fold(name) {
			continue
		}
		if schema != "" && fold(t.Schema) != fold(schema) {
			continue
		}
		return t
	}
	return nil
}
