package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
)

// Security finding F7 (CWE-178; owner decision 2026-10-21, design 14
// §11.3): on PostgreSQL the bound name is substituted UNQUOTED (no
// quoting facility exists), and PostgreSQL folds an unquoted identifier
// to lower case. A quoted mixed-case alias `"A"` therefore turns into a
// reference to `a` — which may be ANOTHER relation in scope. The woven
// conjunct then filters that relation, and every tenant's rows of the
// designated table leak. A bound name that is not already in its folded
// form must be refused (SQLETCH125), and enforcement must never credit
// a conjunct built from one (SQLETCH124).

const pgCaseLeakQuery = "-- name: Q :many\n" +
	`SELECT "A".id, "A".secret FROM orders AS "A" JOIN users AS a ON a.id = :uid` + "\n"

func TestWeave_PGQuotedMixedCaseAliasRefused(t *testing.T) {
	res := weaveOne(t, pgCaseLeakQuery, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
	d := res.Diags[0]
	if !strings.Contains(d.Message, `"A"`) || !strings.Contains(d.Message, "fold") {
		t.Errorf("message must name the bound name and the folding rationale: %q", d.Message)
	}
	if !strings.Contains(d.Hint, "lower-case unquoted alias") {
		t.Errorf("hint must show the compliant rewrite: %q", d.Hint)
	}
	if res.Query.Name != "Q" {
		t.Fatalf("query lost: %+v", res.Query)
	}
	if got := renderSQL(t, res.Query); strings.Contains(got, "tenant_id") {
		t.Errorf("refused query must not be woven, got %q", got)
	}
}

// The mixed-case name need not be an alias: a quoted mixed-case table
// name with no alias is bound as-is and folds the same way.
func TestWeave_PGQuotedMixedCaseTableRefused(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT id FROM \"Orders\"\n", tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
}

// A quoted alias already in folded form means the same identifier
// unquoted: accepted, and the woven conjunct scopes the right relation.
func TestWeave_PGQuotedLowerCaseAliasAccepted(t *testing.T) {
	src := "-- name: Q :many\n" +
		`SELECT "a".id FROM orders AS "a" JOIN users AS u ON u.id = :uid` + "\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	if !strings.Contains(got, "WHERE (a.tenant_id = $2)") {
		t.Errorf("woven rendering = %q", got)
	}
	if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
		t.Errorf("enforcement rejected the weaver's own output: %+v", diags)
	}
}

// Enforcement re-derives independently: a hand-written conjunct whose
// tokens match the (unquoted) bound name must not be credited, because
// on PostgreSQL `A.tenant_id` reads `a.tenant_id` — the users relation.
func TestEnforce_PGQuotedMixedCaseAliasNotCredited(t *testing.T) {
	for _, src := range []string{
		"-- name: Q :many\n" +
			`SELECT "A".id FROM orders AS "A" JOIN users AS a ON a.id = :uid WHERE A.tenant_id = :tenant_id` + "\n",
		// Even the correctly quoted spelling is refused: the weaver can
		// never produce it (§11.3), so crediting it would make the two
		// passes disagree on what "scoped" means.
		"-- name: Q :many\n" +
			`SELECT "A".id FROM orders AS "A" WHERE "A".tenant_id = :tenant_id` + "\n",
	} {
		q := scanOne(t, src)
		diags := enforceOn(t, q, tenantPolicy())
		if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
			t.Fatalf("%s: want exactly one SQLETCH124, got %+v", src, diags)
		}
		if !strings.Contains(diags[0].Message, "fold") {
			t.Errorf("message must state the folding rationale: %q", diags[0].Message)
		}
	}
}

// A predicate with no `{}` placeholder never substitutes the bound
// name, so the folding hazard cannot arise: weave and enforce both
// accept it (the two passes must agree).
func TestWeave_PGQuotedMixedCaseAliasNoPlaceholder(t *testing.T) {
	pol := Policy{Name: "tenant", Tables: []string{"orders"}, Predicate: "current_setting('app.tenant') = '1'"}
	res := weaveOne(t, pgCaseLeakQuery, pol)
	noDiags(t, res)
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejected the weaver's own output: %+v", diags)
	}
}

// Scope guard: MySQL resolves aliases case-insensitively, so a
// mixed-case alias there means the same relation and keeps weaving
// (case-variant matching on MySQL/SQLite is a separate finding, F13).
func TestWeave_MySQLMixedCaseAliasUnaffected(t *testing.T) {
	k := dialectKit{mysql.Profile{}, mysql.Frontend{}}
	q := k.scan(t, "SELECT A.id FROM orders AS A")
	res := Weave(k.profile, k.fe, []Policy{literalTenant()}, q)
	noDiags(t, res)
	if got, _ := k.render(t, res.Query); !strings.Contains(got, "A.tenant_id = 1") {
		t.Errorf("woven rendering = %q", got)
	}
}
