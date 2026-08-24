package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Audit-12 M10 (owner-authorized fail-closed fix, 2026-08-21): an
// INSERT ... ON CONFLICT DO UPDATE on a designated table is silently
// outside every policy today — no weave, no SQLETCH125, no SQLETCH124 —
// yet its DO UPDATE arm modifies rows and can overwrite another tenant's
// row on a cross-tenant unique-key collision. An upsert cannot carry a
// woven WHERE that scopes the conflict, so the weaver must REFUSE it
// (SQLETCH125).
func TestWeaveUpsert_ConflictDoUpdateRefused(t *testing.T) {
	src := "-- name: Q :exec\nINSERT INTO orders (id, v) VALUES (:id, :v) ON CONFLICT (id) DO UPDATE SET v = excluded.v\n"
	res := weaveOne(t, src, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
	if !strings.Contains(res.Diags[0].Message, "CONFLICT") && !strings.Contains(res.Diags[0].Message, "upsert") {
		t.Errorf("message should name the upsert construct: %q", res.Diags[0].Message)
	}
}

// The enforcement pass is the weaver-regression backstop: if a regressed
// weaver let the upsert through, Enforce must still flag it (SQLETCH124).
func TestEnforceUpsert_ConflictDoUpdateFlagged(t *testing.T) {
	src := "-- name: Q :exec\nINSERT INTO orders (id, v) VALUES (:id, :v) ON CONFLICT (id) DO UPDATE SET v = excluded.v\n"
	q := scanOne(t, src)
	diags := enforceOn(t, q, tenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
	}
}

// A plain INSERT ... VALUES (no upsert) into a designated table stays a
// non-target per spec: no diagnostic, template untouched.
func TestWeaveUpsert_PlainInsertValuesUntouched(t *testing.T) {
	src := "-- name: Q :exec\nINSERT INTO orders (id, v) VALUES (:id, :v)\n"
	q := scanOne(t, src)
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	if res.Query != q && renderSQL(t, res.Query) != "INSERT INTO orders (id, v) VALUES ($1, $2)" {
		t.Errorf("plain INSERT VALUES was altered: %s", renderSQL(t, res.Query))
	}
	if diags := enforceOn(t, q, tenantPolicy()); len(diags) != 0 {
		t.Errorf("enforcement flagged a plain INSERT VALUES: %+v", diags)
	}
}

// ON CONFLICT DO NOTHING modifies no rows, so it is NOT an upsert-update
// and stays a non-target (no diagnostic).
func TestWeaveUpsert_ConflictDoNothingUntouched(t *testing.T) {
	src := "-- name: Q :exec\nINSERT INTO orders (id, v) VALUES (:id, :v) ON CONFLICT (id) DO NOTHING\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
}
