package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// F16: PostgreSQL accepts non-ASCII dollar-quote tags
// (dolq_start [A-Za-z\200-\377_]), so `$é$ AND o.deleted_at IS NULL AND $é$`
// is ONE string literal to the server. A lexer that only knew ASCII tags
// saw a depth-0 AND-split segment `o.deleted_at IS NULL` there, and both
// idempotence (Weave) and Enforce credited it — the query ran unscoped.
const dollarTagSmuggle = "-- name: Q :many\n" +
	"SELECT o.id FROM orders o WHERE o.note <> $é$ AND o.deleted_at IS NULL AND $é$\n"

func softDeletePolicy() Policy {
	return Policy{Name: "soft", Tables: []string{"orders"}, Predicate: "{}.deleted_at IS NULL"}
}

func TestWeave_PredicateInsideNonASCIIDollarQuoteIsNotCredited(t *testing.T) {
	res := weaveOne(t, dollarTagSmuggle, softDeletePolicy())
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	want := "SELECT o.id FROM orders o WHERE (o.deleted_at IS NULL) AND o.note <> $é$ AND o.deleted_at IS NULL AND $é$"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, softDeletePolicy()); len(diags) != 0 {
		t.Errorf("enforcement rejects the weaver's own output: %+v", diags)
	}
}

// The same smuggle through an E-string quote continuation: PostgreSQL
// joins `E'a'<newline>'…'` into one literal whose continued segment
// keeps backslash escapes, so `\'` does not close it.
const eStringContinuationSmuggle = "-- name: Q :many\n" +
	"SELECT o.id FROM orders o WHERE o.note <> E'a'\n'\\' AND o.deleted_at IS NULL AND \\''\n"

func TestWeave_PredicateInsideEStringContinuationIsNotCredited(t *testing.T) {
	q := scanOne(t, eStringContinuationSmuggle)
	res := weaveOne(t, eStringContinuationSmuggle, softDeletePolicy())
	noDiags(t, res)
	if got := renderSQL(t, res.Query); !strings.Contains(got, "WHERE (o.deleted_at IS NULL) AND o.note <> E'a'") {
		t.Errorf("conjunct not woven:\n%s", got)
	}
	if diags := enforceOn(t, q, softDeletePolicy()); len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("Enforce on the unwoven query: want exactly one SQLETCH124, got %+v", diags)
	}
}

func TestEnforce_PredicateInsideNonASCIIDollarQuoteIsNotCredited(t *testing.T) {
	q := scanOne(t, dollarTagSmuggle)
	diags := enforceOn(t, q, softDeletePolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
	}
	if !strings.Contains(diags[0].Message, "soft") {
		t.Errorf("diagnostic should name the policy: %q", diags[0].Message)
	}
}
