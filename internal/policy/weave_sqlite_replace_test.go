package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Security finding F17 (CWE-863): SQLite's REPLACE conflict-resolution
// algorithm — `INSERT OR REPLACE`, `REPLACE INTO`, `UPDATE OR REPLACE` —
// DELETES every existing row that collides on a UNIQUE/PRIMARY KEY
// constraint, whoever owns it. The SQLite facade used to ignore the
// rqlite flags, so `INSERT OR REPLACE INTO orders (id, …) VALUES (:id, …)`
// passed as a plain INSERT … VALUES (a policy non-target) and a
// request-supplied id replaced another tenant's row; `UPDATE OR REPLACE`
// was woven with a tenant-scoped WHERE that cannot reach the OTHER row
// the REPLACE deletes. Both are refused (SQLETCH125, Enforce SQLETCH124)
// under the audit-12 M10 / F10 rule: the policy covers `update` or
// `delete`.

func sqliteReplaceTenant(kinds ...dialect.StmtKind) Policy {
	p := literalTenant()
	p.Kinds = kinds
	return p
}

const (
	sqliteInsertOrReplace = "INSERT OR REPLACE INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)"
	sqliteReplaceInto     = "REPLACE INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)"
	sqliteUpdateOrReplace = "UPDATE OR REPLACE orders SET slug = :slug WHERE id = :id"
	// The scoping conjunct written by hand: what a regressed weaver would
	// ship. Enforce must still refuse it — the WHERE scopes the updated
	// row, never the colliding row the REPLACE deletes.
	sqliteUpdateOrReplaceScoped = "UPDATE OR REPLACE orders SET slug = :slug WHERE (orders.tenant_id = 1) AND id = :id"
)

func TestWeaveSQLiteReplace_Refused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		pol  Policy
	}{
		{"insert or replace/default kinds", sqliteInsertOrReplace, literalTenant()},
		{"replace into/default kinds", sqliteReplaceInto, literalTenant()},
		{"update or replace/default kinds", sqliteUpdateOrReplace, literalTenant()},
		{"update or replace/hand-scoped", sqliteUpdateOrReplaceScoped, literalTenant()},
		{"insert or replace/update only", sqliteInsertOrReplace, sqliteReplaceTenant(dialect.StmtUpdate)},
		// REPLACE deletes the colliding row: a delete-only policy bites.
		{"insert or replace/delete only", sqliteInsertOrReplace, sqliteReplaceTenant(dialect.StmtDelete)},
		{"replace into/delete only", sqliteReplaceInto, sqliteReplaceTenant(dialect.StmtDelete)},
		{"update or replace/delete only", sqliteUpdateOrReplace, sqliteReplaceTenant(dialect.StmtDelete)},
		{"update or replace/update only", sqliteUpdateOrReplace, sqliteReplaceTenant(dialect.StmtUpdate)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Weave(sqliteKit.profile, sqliteKit.fe, []Policy{tc.pol}, sqliteKit.scan(t, tc.body))
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("want exactly one SQLETCH125, got %+v (woven %+v)", res.Diags, res.Woven)
			}
			if !strings.Contains(res.Diags[0].Message, "REPLACE") {
				t.Errorf("message should name REPLACE: %q", res.Diags[0].Message)
			}
		})
	}
}

// Enforce is the weaver-regression backstop: the unwoven statement (and
// the hand-scoped UPDATE OR REPLACE, whose conjunct a conjunct-only check
// would credit) must be SQLETCH124 under the same coverage rule.
func TestEnforceSQLiteReplace_Flagged(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		pol  Policy
	}{
		{"insert or replace/default kinds", sqliteInsertOrReplace, literalTenant()},
		{"replace into/default kinds", sqliteReplaceInto, literalTenant()},
		{"insert or replace/delete only", sqliteInsertOrReplace, sqliteReplaceTenant(dialect.StmtDelete)},
		{"update or replace/hand-scoped", sqliteUpdateOrReplaceScoped, literalTenant()},
		{"update or replace/hand-scoped delete only", sqliteUpdateOrReplaceScoped, sqliteReplaceTenant(dialect.StmtDelete)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags := sqliteKit.enforce(t, sqliteKit.scan(t, tc.body), tc.pol)
			if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
				t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
			}
			if !strings.Contains(diags[0].Message, "REPLACE") {
				t.Errorf("message should name REPLACE: %q", diags[0].Message)
			}
		})
	}
}

// A select-only policy governs no row modification: the REPLACE forms
// stay outside it (and UPDATE OR REPLACE is not a read).
func TestWeaveSQLiteReplace_SelectOnlyPolicyUntouched(t *testing.T) {
	pol := sqliteReplaceTenant(dialect.StmtSelect)
	for _, body := range []string{sqliteInsertOrReplace, sqliteReplaceInto, sqliteUpdateOrReplace} {
		t.Run(body, func(t *testing.T) {
			noDiags(t, Weave(sqliteKit.profile, sqliteKit.fe, []Policy{pol}, sqliteKit.scan(t, body)))
			if diags := sqliteKit.enforce(t, sqliteKit.scan(t, body), pol); len(diags) != 0 {
				t.Errorf("enforcement flagged REPLACE under a select-only policy: %+v", diags)
			}
		})
	}
}

// UPDATE OR REPLACE whose TARGET is not designated deletes no designated
// row: a designated table read in its FROM is woven as for any UPDATE …
// FROM, not refused.
func TestWeaveSQLiteUpdateOrReplace_UndesignatedTargetWoven(t *testing.T) {
	body := "UPDATE OR REPLACE audit SET n = o.id FROM orders o WHERE o.id = audit.id"
	res := Weave(sqliteKit.profile, sqliteKit.fe, []Policy{literalTenant()}, sqliteKit.scan(t, body))
	noDiags(t, res)
	got, _ := sqliteKit.render(t, res.Query)
	if !strings.Contains(got, "(o.tenant_id = 1)") {
		t.Errorf("FROM occurrence not woven: %s", got)
	}
}

// Must-stay: the non-REPLACE algorithms touch no existing row. A plain or
// OR IGNORE/ABORT/FAIL/ROLLBACK INSERT stays a non-target, and an
// UPDATE OR IGNORE is woven like any UPDATE.
func TestWeaveSQLiteNonReplace_Untouched(t *testing.T) {
	for _, body := range []string{
		"INSERT INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)",
		"INSERT OR IGNORE INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)",
		"INSERT OR ABORT INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)",
		"INSERT OR FAIL INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)",
		"INSERT OR ROLLBACK INTO orders (id, tenant_id, note) VALUES (:id, :tenant_id, :note)",
	} {
		t.Run(body, func(t *testing.T) {
			q := sqliteKit.scan(t, body)
			before, _ := sqliteKit.render(t, q)
			res := Weave(sqliteKit.profile, sqliteKit.fe, []Policy{literalTenant()}, q)
			noDiags(t, res)
			if after, _ := sqliteKit.render(t, res.Query); after != before {
				t.Errorf("INSERT altered:\n before %s\n after  %s", before, after)
			}
			if diags := sqliteKit.enforce(t, q, literalTenant()); len(diags) != 0 {
				t.Errorf("enforcement flagged a non-REPLACE INSERT: %+v", diags)
			}
		})
	}
	t.Run("update or ignore", func(t *testing.T) {
		res := Weave(sqliteKit.profile, sqliteKit.fe, []Policy{literalTenant()}, sqliteKit.scan(t, "UPDATE OR IGNORE orders SET slug = :slug WHERE id = :id"))
		noDiags(t, res)
		got, _ := sqliteKit.render(t, res.Query)
		if want := "UPDATE OR IGNORE orders SET slug = ? WHERE (orders.tenant_id = 1) AND id = ?"; got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
		if diags := sqliteKit.enforce(t, res.Query, literalTenant()); len(diags) != 0 {
			t.Errorf("enforcement flagged the woven UPDATE OR IGNORE: %+v", diags)
		}
	})
}
