package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// PostgreSQL downcases an unquoted identifier over ASCII only (verified
// on PG 16: unquoted `É` names the alias `"É"`; `Éa` does NOT name
// `"éa"`). The F7 check therefore keys on dialect.ASCIILower: a quoted
// `"A"` still folds to `a` and is refused, `"É"` does not fold at all.
func TestUnquotedFoldChanges_ASCIIOnly(t *testing.T) {
	for name, want := range map[string]bool{"A": true, "Orders": true, "a": false, "É": false, "éA": true} {
		if got := unquotedFoldChanges(postgres.Profile{}, name); got != want {
			t.Errorf("unquotedFoldChanges(%q) = %v, want %v", name, got, want)
		}
	}
}

// A non-bare (here: non-ASCII) bound name is refused by the weaver
// (SQLETCH125, §11.3), and Enforce must never credit a hand-written
// conjunct built from it either: the two passes agree. Before, Enforce
// refused `"É"` on PostgreSQL only by accident — with the false rationale
// that PostgreSQL folds it to `é` — and credited `é` on MySQL/SQLite.
func TestEnforce_NonBareBoundNameNotCredited(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    dialectKit
		body string
	}{
		{"postgres quoted", pgKit, `SELECT "É".id FROM orders AS "É" WHERE É.tenant_id = :tenant_id LIMIT 10`},
		{"mysql", mysqlKit, "SELECT é.id FROM orders AS é WHERE é.tenant_id = :tenant_id LIMIT 10"},
		{"sqlite", sqliteKit, "SELECT é.id FROM orders AS é WHERE é.tenant_id = :tenant_id LIMIT 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Weave(tc.k.profile, tc.k.fe, []Policy{caseTenant()}, tc.k.scan(t, tc.body))
			if !hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
				t.Fatalf("weaver: want SQLETCH125, got %+v", res.Diags)
			}
			diags := tc.k.enforce(t, tc.k.scan(t, tc.body), caseTenant())
			if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
				t.Fatalf("Enforce: want exactly one SQLETCH124, got %+v", diags)
			}
			if !strings.Contains(diags[0].Message, "not a bare identifier") {
				t.Errorf("message must give the real reason: %q", diags[0].Message)
			}
		})
	}
}
