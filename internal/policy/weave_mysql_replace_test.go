package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Security finding F10 (CWE-863): MySQL `REPLACE` parses as an
// InsertStmt (IsReplace) and, on a unique-key collision, DELETES the
// colliding row — any tenant's — before inserting. It used to pass as a
// plain INSERT … VALUES (a policy non-target), so a request-supplied id
// let tenant B replace tenant A's row. REPLACE is a row-modifying
// conflict arm like ON DUPLICATE KEY UPDATE and is refused under the
// same audit-12 M10 rule — when the policy covers `update` (the row is
// rewritten) OR `delete` (the old row is deleted outright).

// mysqlKit is declared in conjunct_case_test.go.

func mysqlWeave(t *testing.T, body string, pols ...Policy) Result {
	t.Helper()
	return Weave(mysqlKit.profile, mysqlKit.fe, pols, mysqlKit.scan(t, body))
}

func kindsTenant(kinds ...dialect.StmtKind) Policy {
	p := literalTenant()
	p.Kinds = kinds
	return p
}

const (
	replaceValues = "REPLACE INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)"
	replaceSet    = "REPLACE INTO orders SET id = :id, tenant_id = :tenant_id, note = :note"
	odku          = "INSERT INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note) ON DUPLICATE KEY UPDATE note = VALUES(note)"
)

func TestWeaveMySQLReplace_Refused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		pol  Policy
	}{
		{"values/default kinds", replaceValues, literalTenant()},
		{"set/default kinds", replaceSet, literalTenant()},
		{"values/update only", replaceValues, kindsTenant(dialect.StmtUpdate)},
		// REPLACE deletes the colliding row: a delete-only policy must
		// bite too, not just one covering update.
		{"values/delete only", replaceValues, kindsTenant(dialect.StmtDelete)},
		{"set/delete only", replaceSet, kindsTenant(dialect.StmtDelete)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := mysqlWeave(t, tc.body, tc.pol)
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
			}
			if !strings.Contains(res.Diags[0].Message, "REPLACE") {
				t.Errorf("message should name REPLACE: %q", res.Diags[0].Message)
			}
		})
	}
}

// Enforce is the weaver-regression backstop: the unwoven REPLACE must be
// flagged SQLETCH124 under the same coverage rule.
func TestEnforceMySQLReplace_Flagged(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		pol  Policy
	}{
		{"values/default kinds", replaceValues, literalTenant()},
		{"set/default kinds", replaceSet, literalTenant()},
		{"values/update only", replaceValues, kindsTenant(dialect.StmtUpdate)},
		{"values/delete only", replaceValues, kindsTenant(dialect.StmtDelete)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags := mysqlKit.enforce(t, mysqlKit.scan(t, tc.body), tc.pol)
			if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
				t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
			}
			if !strings.Contains(diags[0].Message, "REPLACE") {
				t.Errorf("message should name REPLACE: %q", diags[0].Message)
			}
		})
	}
}

// A select-only policy does not govern row modification: REPLACE (whose
// target is not a read) stays outside it, like any INSERT … VALUES.
func TestWeaveMySQLReplace_SelectOnlyPolicyUntouched(t *testing.T) {
	pol := kindsTenant(dialect.StmtSelect)
	noDiags(t, mysqlWeave(t, replaceValues, pol))
	if diags := mysqlKit.enforce(t, mysqlKit.scan(t, replaceValues), pol); len(diags) != 0 {
		t.Errorf("enforcement flagged REPLACE under a select-only policy: %+v", diags)
	}
}

// ON DUPLICATE KEY UPDATE keeps its audit-12 M10 rule exactly: refused
// when the policy covers update, and NOT under a delete-only policy (its
// arm never deletes) — the delete widening is REPLACE's alone.
func TestWeaveMySQLOnDuplicateKeyUpdate_CoverageUnchanged(t *testing.T) {
	res := mysqlWeave(t, odku, literalTenant())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("default kinds: want exactly one SQLETCH125, got %+v", res.Diags)
	}
	if diags := mysqlKit.enforce(t, mysqlKit.scan(t, odku), literalTenant()); len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("default kinds: want exactly one SQLETCH124, got %+v", diags)
	}
	delOnly := kindsTenant(dialect.StmtDelete)
	noDiags(t, mysqlWeave(t, odku, delOnly))
	if diags := mysqlKit.enforce(t, mysqlKit.scan(t, odku), delOnly); len(diags) != 0 {
		t.Errorf("delete-only policy flagged ON DUPLICATE KEY UPDATE: %+v", diags)
	}
}

// Must-stay: a plain INSERT (VALUES, SET, or IGNORE — none of which
// touch an existing row) into a designated table remains a non-target:
// no diagnostic, template untouched, Enforce silent.
func TestWeaveMySQLPlainInsert_Untouched(t *testing.T) {
	for _, body := range []string{
		"INSERT INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)",
		"INSERT INTO orders SET id = :id, tenant_id = :tenant_id, note = :note",
		"INSERT IGNORE INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)",
	} {
		t.Run(body, func(t *testing.T) {
			q := mysqlKit.scan(t, body)
			before, _ := mysqlKit.render(t, q)
			res := Weave(mysqlKit.profile, mysqlKit.fe, []Policy{literalTenant()}, q)
			noDiags(t, res)
			if after, _ := mysqlKit.render(t, res.Query); after != before {
				t.Errorf("plain INSERT altered:\n before %s\n after  %s", before, after)
			}
			if diags := mysqlKit.enforce(t, q, literalTenant()); len(diags) != 0 {
				t.Errorf("enforcement flagged a plain INSERT: %+v", diags)
			}
		})
	}
}
