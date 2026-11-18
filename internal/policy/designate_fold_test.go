package policy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Designation compares table names with the engine's own "may" relation
// key (dialect.RelationFolds, design 01 / design 14 §11.8), never Go's
// Unicode folds. strings.EqualFold under-matched MySQL: under
// lower_case_table_names=1/2 the server lowercases `İTEMS` through
// utf8mb3_general_ci to `items` (verified on MySQL 8.4), but
// EqualFold("İTEMS", "items") is false — the table went undesignated and
// the query shipped completely unscoped. It also over-matched (`ITEMſ`,
// the Kelvin sign), which no engine resolves to the ASCII name.

func itemsTenant() Policy {
	return Policy{Name: "tenant", Tables: []string{"items"}, Predicate: "{}.tenant_id = 1"}
}

func TestWeave_MySQLDesignatesServerFoldedTableName(t *testing.T) {
	k := mysqlKit
	for _, tc := range []struct {
		body  string
		woven bool // false: refused (the bound name `İTEMS` is not bare, §11.3)
	}{
		{"SELECT id FROM İtems AS x LIMIT 10", true},
		{"SELECT id FROM İTEMS LIMIT 10", false},
		{"DELETE FROM İTEMS WHERE id = 1", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			res := Weave(k.profile, k.fe, []Policy{itemsTenant()}, k.scan(t, tc.body))
			if tc.woven {
				if diagnostics.HasErrors(res.Diags) {
					t.Fatalf("diags: %+v", res.Diags)
				}
				if sql, _ := k.render(t, res.Query); !strings.Contains(sql, "x.tenant_id = 1") {
					t.Fatalf("a table lower_case_table_names=1 resolves to `items` was not scoped: %s", sql)
				}
			} else if !hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
				t.Fatalf("want SQLETCH125, got %+v", res.Diags)
			}
			// Enforce, on the UNWOVEN template, must demand the conjunct.
			if diags := k.enforce(t, k.scan(t, tc.body), itemsTenant()); !hasCode(diags, diagnostics.CodePolicyUnscoped) {
				t.Fatalf("Enforce accepted an unscoped read of a case-variant designated table: %+v", diags)
			}
		})
	}
}

func TestWeave_MySQLHiddenCaseVariantDesignatedRead(t *testing.T) {
	k := mysqlKit
	body := "SELECT id FROM users WHERE id IN (SELECT id FROM İTEMS) LIMIT 10"
	res := Weave(k.profile, k.fe, []Policy{itemsTenant()}, k.scan(t, body))
	if !hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
		t.Fatalf("subquery read of a case-variant designated table must be SQLETCH125, got %+v", res.Diags)
	}
}

// The other direction: names Go's folds join but the engine keeps apart
// are not the designated table and are left alone.
func TestWeave_EngineDistinctNamesAreNotDesignated(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    dialectKit
		body string
	}{
		{"mysql long s", mysqlKit, "SELECT id FROM ITEMſ LIMIT 10"},
		{"sqlite long s", sqliteKit, "SELECT id FROM ITEMſ LIMIT 10"},
		{"sqlite dotted I", sqliteKit, "SELECT id FROM İTEMS LIMIT 10"},
		{"postgres quoted", pgKit, `SELECT id FROM "ITEMS" LIMIT 10`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.k.scan(t, tc.body)
			res := Weave(tc.k.profile, tc.k.fe, []Policy{itemsTenant()}, q)
			if len(res.Diags) != 0 || len(res.Woven) != 0 {
				t.Fatalf("an engine-distinct table was treated as designated: diags %+v, woven %+v", res.Diags, res.Woven)
			}
		})
	}
}

// Engine-identical spellings stay designated on every dialect.
func TestWeave_EngineEqualNamesStayDesignated(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    dialectKit
		body string
	}{
		{"mysql ascii", mysqlKit, "SELECT id FROM ITEMS LIMIT 10"},
		{"sqlite ascii", sqliteKit, "SELECT id FROM ITEMS LIMIT 10"},
		{"postgres unquoted", pgKit, "SELECT id FROM ITEMS LIMIT 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Weave(tc.k.profile, tc.k.fe, []Policy{itemsTenant()}, tc.k.scan(t, tc.body))
			if diagnostics.HasErrors(res.Diags) {
				t.Fatalf("diags: %+v", res.Diags)
			}
			if sql, _ := tc.k.render(t, res.Query); !strings.Contains(sql, "tenant_id = 1") {
				t.Fatalf("not scoped: %s", sql)
			}
		})
	}
}

// Keywords are recognized ASCII-only, as every engine does (verified:
// MySQL 8.4 and SQLite accept `lımıt`, `unıon`, `offſet` as ordinary
// identifiers, even as table aliases; PostgreSQL's keyword lookup
// downcases ASCII only). strings.ToUpper maps `ı`→`I` and `ſ`→`S`, so
// the scanners read `lımıt` as LIMIT: the weaver spliced its WHERE in
// front of it (unparseable output) and split set operations at `unıon`.
func TestWeave_NonASCIIIdentifierIsNotAKeyword(t *testing.T) {
	for _, k := range []dialectKit{mysqlKit, sqliteKit, pgKit} {
		for _, tc := range []struct{ body, want string }{
			{"SELECT o.id FROM orders o JOIN users u ON u.id = o.uid AND u.x = lımıt OR u.y = 1 LIMIT 10",
				"u.y = 1 WHERE (o.tenant_id = 1) LIMIT 10"},
			{"SELECT o.id FROM orders o JOIN users lımıt ON lımıt.id = o.uid LIMIT 10",
				"lımıt.id = o.uid WHERE (o.tenant_id = 1) LIMIT 10"},
			{"SELECT o.id FROM orders o JOIN users wındow ON wındow.id = o.uid LIMIT 10",
				"wındow.id = o.uid WHERE (o.tenant_id = 1) LIMIT 10"},
			{"SELECT o.id FROM orders o JOIN users offſet ON offſet.id = o.uid LIMIT 10",
				"offſet.id = o.uid WHERE (o.tenant_id = 1) LIMIT 10"},
			// The nullable side's ON clause: the same false SQLETCH125.
			{"SELECT u.id FROM users u LEFT JOIN orders o ON o.uid = u.id AND o.x = caſe OR o.y = 1 LIMIT 10",
				"ON (o.uid = u.id AND o.x = caſe OR o.y = 1) AND (o.tenant_id = 1) LIMIT 10"},
			// `caſe` read as CASE opened a CASE nest: a false SQLETCH125.
			{"SELECT o.id FROM orders o WHERE o.a = caſe OR o.b = 1 LIMIT 10",
				"WHERE (o.tenant_id = 1) AND (o.a = caſe OR o.b = 1) LIMIT 10"},
		} {
			t.Run(fmt.Sprintf("%T/%s", k.profile, tc.body), func(t *testing.T) {
				res := Weave(k.profile, k.fe, []Policy{literalTenant()}, k.scan(t, tc.body))
				if len(res.Diags) != 0 {
					t.Fatalf("diags: %+v", res.Diags)
				}
				sql, _ := k.render(t, res.Query)
				if !strings.Contains(sql, tc.want) {
					t.Fatalf("woven = %s\nwant it to contain %s", sql, tc.want)
				}
				if diags := k.enforce(t, res.Query, literalTenant()); len(diags) != 0 {
					t.Fatalf("Enforce rejected the weaver's output: %+v", diags)
				}
			})
		}
	}
	// A column named `unıon` does not split a set operation's branch.
	for _, k := range []dialectKit{sqliteKit, pgKit} {
		body := "SELECT o.id FROM orders o WHERE o.a = unıon UNION SELECT p.id FROM orders p"
		res := Weave(k.profile, k.fe, []Policy{literalTenant()}, k.scan(t, body))
		if len(res.Diags) != 0 {
			t.Fatalf("%T: diags: %+v", k.profile, res.Diags)
		}
		if sql, _ := k.render(t, res.Query); !strings.Contains(sql, "WHERE (o.tenant_id = 1) AND o.a = unıon UNION SELECT p.id FROM orders p WHERE (p.tenant_id = 1)") {
			t.Fatalf("%T: woven = %s", k.profile, sql)
		}
	}
}
