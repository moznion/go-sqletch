package postgres

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
	// A subquery re-introduces alias o (audits o) shadowing top-level
	// orgs o, and correlates to top-level u.
	sql := "SELECT u.id FROM users u JOIN orgs o ON o.id = u.org_id " +
		"WHERE EXISTS (SELECT 1 FROM audits o WHERE o.user_id = u.id)"
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	refs := tree.ColumnRefs()

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

	// A SubLink test expression stays in the enclosing scope.
	sql2 := "SELECT u.id FROM users u JOIN orgs o ON o.id = u.org_id " +
		"WHERE o.id IN (SELECT x.oid FROM audits o)"
	tree2, err := Frontend{}.Parse(sql2)
	if err != nil {
		t.Fatal(err)
	}
	test, ok := findScope(tree2.ColumnRefs(), "o", "id")
	if !ok {
		t.Fatalf("no o.id in test expression")
	}
	if hasName(test.ScopeAliases, "o") {
		t.Errorf("IN test-expr o.id ScopeAliases=%v must not inherit subquery alias o", test.ScopeAliases)
	}
}

// A join alias hides the inner relation names (PostgreSQL §7.2.1.2):
// a reference inside a subquery over `(a JOIN b) AS j` sees only `j`, so
// the inner names must NOT enter ScopeAliases (over-collecting them would
// wrongly suppress the R3 guard for a same-named top-level relation).
func TestColumnRefs_ScopeAliases_JoinAliasHidesInner(t *testing.T) {
	sql := "SELECT u.id FROM users u JOIN orgs o ON o.id = u.org_id " +
		"WHERE EXISTS (SELECT 1 FROM (audits a JOIN orgs g ON g.id = a.org_id) AS j " +
		"WHERE o.name = u.id)"
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	refs := tree.ColumnRefs()

	ref, ok := findScope(refs, "o", "name")
	if !ok {
		t.Fatalf("no subquery o.name ref in %+v", refs)
	}
	// The subquery's FROM name is the join alias j only.
	if !hasName(ref.ScopeAliases, "j") {
		t.Errorf("subquery o.id ScopeAliases=%v, want the join alias j", ref.ScopeAliases)
	}
	for _, hidden := range []string{"a", "g"} {
		if hasName(ref.ScopeAliases, hidden) {
			t.Errorf("subquery o.id ScopeAliases=%v leaks hidden inner name %q", ref.ScopeAliases, hidden)
		}
	}
}

// A non-lateral CTE body cannot reference the FROM items of the select
// that uses it, so the using-select's FROM names (here the CTE name c)
// must NOT be inherited into the CTE body's scope. Leaking them would
// wrongly shadow a same-named guarded top-level relation for a correlated
// CTE-body reference.
func TestColumnRefs_ScopeAliases_CTEBodyExcludesUsingScope(t *testing.T) {
	sql := "SELECT x FROM o " +
		"WHERE EXISTS (WITH c AS (SELECT o.id AS oid FROM t2) SELECT z FROM c)"
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	refs := tree.ColumnRefs()

	ref, ok := findScope(refs, "o", "id")
	if !ok {
		t.Fatalf("no o.id ref in CTE body: %+v", refs)
	}
	if hasName(ref.ScopeAliases, "c") {
		t.Errorf("CTE-body o.id ScopeAliases=%v leaks using-select FROM name c", ref.ScopeAliases)
	}
	// The CTE body's own FROM name t2 is legitimately in scope.
	if !hasName(ref.ScopeAliases, "t2") {
		t.Errorf("CTE-body o.id ScopeAliases=%v, want its own FROM name t2", ref.ScopeAliases)
	}
}
