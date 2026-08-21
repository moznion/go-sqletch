package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// Every spliced predicate occurrence is parenthesized (design 14
// §11.6): `(p OR q)` and `(a AND b)` are each ONE depth-0 conjunct, so
// precedence is fixed at the splice site and the idempotence/
// enforcement matchers seek one segment.

// A predicate with a top-level OR must weave as a single parenthesized
// conjunct. Unparenthesized, the query's own WHERE would land under
// the OR's right arm (`WHERE o.tenant_id = $1 OR $1 IS NULL AND
// o.status = $2`) — leak-shaped SQL saved only by the enforcement
// backstop.
func TestWeave_ORPredicateParenthesized(t *testing.T) {
	pol := Policy{
		Name:      "tenant_or_global",
		Tables:    []string{"orders"},
		Predicate: "{}.tenant_id = :tid OR :tid IS NULL",
		ParamName: "tid",
		ParamType: "bigint",
	}
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n"
	res := weaveOne(t, src, pol)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT o.id FROM orders o WHERE (o.tenant_id = $1 OR $1 IS NULL) AND o.status = $2"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
	// The leak shape must not be produced.
	if strings.Contains(got, "IS NULL AND o.status") && !strings.Contains(got, "IS NULL) AND o.status") {
		t.Errorf("OR predicate spliced unparenthesized (leak-shaped):\n%s", got)
	}
	// And the weaver's own output satisfies enforcement.
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the weaver's own OR-predicate output: %+v", diags)
	}
}

// The same predicate on a null-extended occurrence goes into the
// join's ON clause, still parenthesized.
func TestWeaveON_ORPredicateParenthesized(t *testing.T) {
	pol := Policy{
		Name:      "tenant_or_global",
		Tables:    []string{"orders"},
		Predicate: "{}.tenant_id = :tid OR :tid IS NULL",
		ParamName: "tid",
		ParamType: "bigint",
	}
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n"
	res := weaveOne(t, src, pol)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND (o.tenant_id = $1 OR $1 IS NULL) WHERE u.ok"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the weaver's own ON OR-predicate output: %+v", diags)
	}
}

// A predicate containing a depth-0 AND weaves as one parenthesized
// conjunct that enforcement accepts. Before parenthesization the woven
// copy was AND-split into multiple segments while the wanted token
// sequence kept its `and` — a guaranteed SQLETCH124 on the weaver's
// own output.
func TestWeave_ANDPredicateEnforceable(t *testing.T) {
	pol := Policy{
		Name:      "tenant_active",
		Tables:    []string{"orders"},
		Predicate: "{}.tenant_id = :tenant_id AND {}.active",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n"
	res := weaveOne(t, src, pol)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT o.id FROM orders o WHERE (o.tenant_id = $1 AND o.active) AND o.status = $2"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the weaver's own AND-predicate output: %+v", diags)
	}
}

// BETWEEN carries an AND token of its own; the parenthesized splice
// keeps the woven conjunct one segment and enforcement passes.
func TestWeave_BetweenPredicateEnforceable(t *testing.T) {
	pol := Policy{
		Name:      "recent_only",
		Tables:    []string{"orders"},
		Predicate: "{}.created_at BETWEEN :lo AND :hi",
		ParamName: "lo",
		ParamType: "timestamptz",
	}
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n"
	res := weaveOne(t, src, pol)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT o.id FROM orders o WHERE (o.created_at BETWEEN $1 AND $2) AND o.status = $3"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the weaver's own BETWEEN output: %+v", diags)
	}
}

// The ON path with a multi-conjunct predicate: parenthesized, single
// depth-0 segment of the join's ON clause, enforcement passes.
func TestWeaveON_ANDPredicateEnforceable(t *testing.T) {
	pol := Policy{
		Name:      "tenant_active",
		Tables:    []string{"orders"},
		Predicate: "{}.tenant_id = :tenant_id AND {}.active",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n"
	res := weaveOne(t, src, pol)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND (o.tenant_id = $1 AND o.active) WHERE u.ok"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the weaver's own ON AND-predicate output: %+v", diags)
	}
}

// Idempotence with parenthesization (design 14 §11.6):
//   - a hand-written PARENTHESIZED copy still matches (no doubling);
//   - a hand-written UNparenthesized copy of a multi-conjunct
//     predicate no longer matches — the weaver weaves anyway and the
//     doubled output passes enforcement ("doubling is harmless,
//     skipping leaks");
//   - a bare single-conjunct hand-written copy still matches
//     (TestWeave_Idempotence pins that separately).
func TestWeave_IdempotenceParenthesizedCopy(t *testing.T) {
	pol := Policy{
		Name:      "tenant_active",
		Tables:    []string{"orders"},
		Predicate: "{}.tenant_id = :tenant_id AND {}.active",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE (o.tenant_id = :tenant_id AND o.active) AND o.ok\n"
	q := scanOne(t, src)
	res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{pol}, q)
	noDiags(t, res)
	if res.Query != q {
		t.Errorf("hand-written parenthesized copy was not recognized (doubled)")
	}
	if diags := enforceOn(t, q, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the hand-written parenthesized copy: %+v", diags)
	}
}

func TestWeave_UnparenthesizedANDCopyDoublesHarmlessly(t *testing.T) {
	pol := Policy{
		Name:      "tenant_active",
		Tables:    []string{"orders"},
		Predicate: "{}.tenant_id = :tenant_id AND {}.active",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND o.active AND o.ok\n"
	res := weaveOne(t, src, pol)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT o.id FROM orders o WHERE (o.tenant_id = $1 AND o.active) AND o.tenant_id = $1 AND o.active AND o.ok"
	if got != want {
		t.Errorf("expected the doubled (harmless) weave:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the doubled output: %+v", diags)
	}
	if _, err := (postgres.Frontend{}).Parse(got); err != nil {
		t.Errorf("doubled output does not parse: %v", err)
	}
}

// A placeholder-free predicate references no joined columns, so the
// weaver emits a single WHERE conjunct even when every designated
// occurrence sits on a null-extended outer-join side (a WHERE conjunct
// that references no joined columns cannot null-filter the join).
// Enforcement must mirror that emission rule: check WHERE, not the
// join's ON clause.
func TestEnforce_PlaceholderFreePredicateAtNullableOccurrence(t *testing.T) {
	pol := Policy{
		Name:      "session_tenant",
		Tables:    []string{"orders"},
		Predicate: "current_setting('app.tenant')::bigint = :tenant_id",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n"
	res := weaveOne(t, src, pol)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE (current_setting('app.tenant')::bigint = $1) AND u.ok"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforcement rejects the weaver's own placeholder-free output: %+v", diags)
	}
	// Absent entirely, it still fails — and the hint names the WHERE
	// clause, not the join's ON clause.
	unwoven := scanOne(t, src)
	diags := enforceOn(t, unwoven, pol)
	if len(diags) != 1 {
		t.Fatalf("unwoven query must fail enforcement: %+v", diags)
	}
	if !strings.Contains(diags[0].Hint, "WHERE clause") || strings.Contains(diags[0].Hint, "ON clause") {
		t.Errorf("hint must point at the WHERE clause for a placeholder-free predicate: %q", diags[0].Hint)
	}
}
