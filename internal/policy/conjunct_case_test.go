package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// Security F13 (CWE-178): conjunct crediting used to fold identifier
// case with strings.ToLower. On MySQL (lower_case_table_names=0, the
// Linux default) table aliases are case-sensitive, so `A` and `a` are
// different relations: a hand-written `a.tenant_id` scoping `users a`
// was credited to `orders A`, the weaver skipped it, Enforce agreed, and
// every tenant's orders shipped. Owner decision 2026-10-21: fail closed —
// on MySQL and SQLite a case-variant-only identifier match is ABSENT
// (weave anyway: doubling is harmless, skipping leaks); keywords stay
// case-insensitive; folding is ASCII-only on every dialect.

var (
	mysqlKit = dialectKit{mysql.Profile{}, mysql.Frontend{}}
	pgKit    = dialectKit{postgres.Profile{}, postgres.Frontend{}}
)

func caseTenant() Policy {
	return Policy{Name: "tenant", Tables: []string{"orders"}, Predicate: "{}.tenant_id = :tenant_id", ParamName: "tenant_id"}
}

func TestWeave_MySQL_CaseVariantAliasIsNotCredited(t *testing.T) {
	k := mysqlKit
	body := "SELECT A.id, A.secret FROM orders A JOIN users a ON a.id = :uid WHERE a.tenant_id = :tenant_id LIMIT 50"
	q := k.scan(t, body)

	// Enforce must not credit the users conjunct to `orders A`.
	diags := k.enforce(t, q, caseTenant())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("unwoven: want one SQLETCH124, got %+v", diags)
	}

	res := Weave(k.profile, k.fe, []Policy{caseTenant()}, q)
	noDiags(t, res)
	if res.Query == q {
		t.Fatalf("weaver credited a.tenant_id to orders A (skipped weaving)")
	}
	got, _ := k.render(t, res.Query)
	if !strings.Contains(got, "(A.tenant_id = ?)") {
		t.Errorf("orders A not scoped:\n%s", got)
	}
	if diags := k.enforce(t, res.Query, caseTenant()); len(diags) != 0 {
		t.Errorf("enforce rejects the weaver's output: %+v", diags)
	}
}

// A case-variant hand-written copy of the SAME relation's conjunct is
// also treated as absent on MySQL/SQLite (fail-closed; the doubled
// conjunct is harmless). PostgreSQL folds unquoted identifiers (ASCII)
// itself, so there a case variant IS the same identifier and stays
// credited (TestWeave_Idempotence).
func TestWeave_CaseVariantSameRelationWeavesAgain(t *testing.T) {
	for _, k := range []struct {
		name string
		kit  dialectKit
	}{{"mysql", mysqlKit}, {"sqlite", sqliteKit}} {
		t.Run(k.name, func(t *testing.T) {
			q := k.kit.scan(t, "SELECT o.id FROM orders o WHERE O.Tenant_ID = :tenant_id AND o.ok = 1")
			res := Weave(k.kit.profile, k.kit.fe, []Policy{caseTenant()}, q)
			noDiags(t, res)
			if res.Query == q {
				t.Fatalf("case-variant conjunct was credited")
			}
			got, _ := k.kit.render(t, res.Query)
			if !strings.Contains(got, "(o.tenant_id = ?)") {
				t.Errorf("not woven:\n%s", got)
			}
			if diags := k.kit.enforce(t, res.Query, caseTenant()); len(diags) != 0 {
				t.Errorf("enforce rejects the weaver's output: %+v", diags)
			}
		})
	}
}

// Must-stay: an exact-case hand-written conjunct (including a mixed-case
// alias on the Tier-2 dialects, and lower-case keywords against an
// upper-case policy) is still credited — idempotence — on all three
// dialects, by both the weaver and Enforce.
func TestWeave_ExactCaseConjunctStillCredited(t *testing.T) {
	soft := Policy{Name: "soft", Tables: []string{"orders"}, Predicate: "{}.deleted_at IS NOT NULL"}
	cases := []struct {
		name string
		kit  dialectKit
		body string
		pol  Policy
	}{
		{"pg", pgKit, "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND o.ok", caseTenant()},
		{"mysql", mysqlKit, "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND o.ok = 1", caseTenant()},
		{"sqlite", sqliteKit, "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND o.ok = 1", caseTenant()},
		{"mysql-mixed-alias", mysqlKit, "SELECT O.id FROM orders O WHERE O.tenant_id = :tenant_id AND O.ok = 1", caseTenant()},
		{"sqlite-mixed-alias", sqliteKit, "SELECT O.id FROM orders O WHERE O.tenant_id = :tenant_id AND O.ok = 1", caseTenant()},
		{"pg-keywords", pgKit, "SELECT o.id FROM orders o WHERE o.deleted_at is not null AND o.ok", soft},
		{"mysql-keywords", mysqlKit, "SELECT o.id FROM orders o WHERE o.deleted_at Is Not Null AND o.ok = 1", soft},
		{"sqlite-keywords", sqliteKit, "SELECT o.id FROM orders o WHERE o.deleted_at is not null AND o.ok = 1", soft},
		{"mysql-parenthesized", mysqlKit, "SELECT o.id FROM orders o WHERE (o.tenant_id = :tenant_id) AND o.ok = 1", caseTenant()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.kit.scan(t, tc.body)
			res := Weave(tc.kit.profile, tc.kit.fe, []Policy{tc.pol}, q)
			noDiags(t, res)
			if res.Query != q {
				got, _ := tc.kit.render(t, res.Query)
				t.Errorf("exact-case conjunct not credited; woven:\n%s", got)
			}
			if diags := tc.kit.enforce(t, q, tc.pol); len(diags) != 0 {
				t.Errorf("enforce does not credit the exact-case conjunct: %+v", diags)
			}
		})
	}
}

// strings.ToLower folds non-ASCII: KELVIN SIGN (U+212A) lowers to `k`.
// No dialect folds it (PostgreSQL folds only ASCII under UTF8; MySQL
// treats it as a distinct identifier), so a conjunct on `users <KELVIN>`
// must not be credited to `orders k`. (A non-ASCII name bound to the
// placeholder is already SQLETCH125, and rqlite/sql does not lex it, so
// the hazard is the hand-written side on PostgreSQL and MySQL.)
func TestWeave_KelvinSignIsNotFolded(t *testing.T) {
	const kelvin = "\u212A"
	for _, k := range []struct {
		name string
		kit  dialectKit
		ph   string
	}{{"pg", pgKit, "$"}, {"mysql", mysqlKit, "?"}} {
		t.Run(k.name, func(t *testing.T) {
			body := "SELECT k.id, k.secret FROM orders k JOIN users " + kelvin + " ON " + kelvin + ".id = k.user_id WHERE " +
				kelvin + ".tenant_id = :tenant_id"
			q := k.kit.scan(t, body)
			diags := k.kit.enforce(t, q, caseTenant())
			if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
				t.Fatalf("unwoven: want one SQLETCH124, got %+v", diags)
			}
			res := Weave(k.kit.profile, k.kit.fe, []Policy{caseTenant()}, q)
			noDiags(t, res)
			if res.Query == q {
				t.Fatalf("%s.tenant_id credited to orders k", kelvin)
			}
			got, _ := k.kit.render(t, res.Query)
			if !strings.Contains(got, "(k.tenant_id = "+k.ph) {
				t.Errorf("orders k not scoped:\n%s", got)
			}
			if diags := k.kit.enforce(t, res.Query, caseTenant()); len(diags) != 0 {
				t.Errorf("enforce rejects the weaver's output: %+v", diags)
			}
		})
	}
}

// Unit: the token matcher. Keywords fold (ASCII) everywhere; on the
// case-insensitive-identifier dialects a non-keyword identifier, or a
// keyword-spelled word after a `.` (a MySQL identifier position even
// when reserved), compares exactly.
func TestConjTokMatch(t *testing.T) {
	cases := []struct {
		name    string
		prof    dialect.LexerProfile
		a, b    string
		matched bool
	}{
		{"pg ident folds", postgres.Profile{}, "O.Tenant_ID", "o.tenant_id", true},
		{"pg non-ascii", postgres.Profile{}, "K.x", "k.x", false},
		{"mysql alias exact", mysql.Profile{}, "A.x", "a.x", false},
		{"mysql column exact", mysql.Profile{}, "a.X", "a.x", false},
		{"mysql keyword folds", mysql.Profile{}, "x IS NOT NULL", "x is not null", true},
		{"mysql keyword after dot exact", mysql.Profile{}, "t.ORDER", "t.order", false},
		{"sqlite alias exact", sqlite.Profile{}, "A.x", "a.x", false},
		{"sqlite keyword folds", sqlite.Profile{}, "x IN (1)", "x in (1)", true},
		{"quoted verbatim", mysql.Profile{}, "`A`.x", "`a`.x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := normalizedTokens(tc.prof, tc.a), normalizedTokens(tc.prof, tc.b)
			if a == nil || b == nil {
				t.Fatalf("lex failed")
			}
			if got := conjTokensEqual(tc.prof, a, b); got != tc.matched {
				t.Errorf("match(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.matched)
			}
		})
	}
}
