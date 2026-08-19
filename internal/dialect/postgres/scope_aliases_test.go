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
