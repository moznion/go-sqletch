package sqlite

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

func findScope(refs []dialect.ColRef, fields ...string) (dialect.ColRef, bool) {
	for _, r := range refs {
		if len(r.Fields) != len(fields) {
			continue
		}
		match := true
		for i := range fields {
			if r.Fields[i] != fields[i] {
				match = false
				break
			}
		}
		if match {
			return r, true
		}
	}
	return dialect.ColRef{}, false
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestColumnRefs_ScopeAliases(t *testing.T) {
	sql := "SELECT u.id FROM users u JOIN orgs o ON o.id = u.org_id " +
		"WHERE EXISTS (SELECT 1 FROM audits o WHERE o.user_id = u.id)"
	refs := parse(t, sql).ColumnRefs()

	inner, ok := findScope(refs, "o", "user_id")
	if !ok {
		t.Fatalf("no o.user_id ref in %+v", refs)
	}
	if !hasName(inner.ScopeAliases, "o") {
		t.Errorf("o.user_id ScopeAliases=%v, want the shadowing inner alias o", inner.ScopeAliases)
	}

	corr, ok := findScope(refs, "u", "id")
	if !ok {
		t.Fatalf("no u.id ref in %+v", refs)
	}
	if hasName(corr.ScopeAliases, "u") {
		t.Errorf("correlated u.id ScopeAliases=%v must not contain u", corr.ScopeAliases)
	}

	if top, ok := findScope(refs, "o", "id"); ok && len(top.ScopeAliases) != 0 {
		t.Errorf("top-level o.id ScopeAliases=%v, want empty", top.ScopeAliases)
	}
}

func TestColumnRefs_ScopeAliases_NestedTwoLevel(t *testing.T) {
	// Innermost alias o (from a two-level-deep subquery) must appear in
	// the union of enclosing subquery FROMs.
	sql := "SELECT u.id FROM users u JOIN orgs o ON o.id = u.org_id " +
		"WHERE EXISTS (SELECT 1 FROM audits a WHERE EXISTS " +
		"(SELECT 1 FROM organization_users o WHERE o.user_id = a.id))"
	inner, ok := findScope(parse(t, sql).ColumnRefs(), "o", "user_id")
	if !ok {
		t.Fatalf("no o.user_id ref")
	}
	if !hasName(inner.ScopeAliases, "o") {
		t.Errorf("nested o.user_id ScopeAliases=%v, want o", inner.ScopeAliases)
	}
}

// A non-lateral CTE body cannot reference the FROM items of the select
// that uses it: the using-select's FROM name (the CTE name c) must not
// leak into the CTE body's scope. This facade already threads the
// enclosing scope into walkWith; the test pins that behaviour.
func TestColumnRefs_ScopeAliases_CTEBodyExcludesUsingScope(t *testing.T) {
	sql := "SELECT x FROM o " +
		"WHERE EXISTS (WITH c AS (SELECT o.id AS oid FROM t2) SELECT z FROM c)"
	ref, ok := findScope(parse(t, sql).ColumnRefs(), "o", "id")
	if !ok {
		t.Fatalf("no o.id ref in CTE body")
	}
	if hasName(ref.ScopeAliases, "c") {
		t.Errorf("CTE-body o.id ScopeAliases=%v leaks using-select FROM name c", ref.ScopeAliases)
	}
	if !hasName(ref.ScopeAliases, "t2") {
		t.Errorf("CTE-body o.id ScopeAliases=%v, want its own FROM name t2", ref.ScopeAliases)
	}
}
