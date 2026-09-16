//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/rules"
)

const acknowledgedAudit = `-- name: AcknowledgedAudit :many
-- @policy-apply: tenant_scope
SELECT a.id, a.action FROM audit_logs AS a ORDER BY a.id;
`

const unannotatedAudit = `-- name: UnannotatedAudit :many
SELECT a.id, a.action FROM audit_logs AS a ORDER BY a.id;
`

func requiringTenantScopePolicy() policy.Policy {
	p := tenantScopePolicy()
	p.RequireAnnotation = true
	return p
}

// TestPolicyRequireAnnotationEndToEnd proves the two claims of design
// 14 §12 that only a real engine can settle:
//
//  1. an acknowledged query's annotation comment travels into the
//     verified SQL (§12.8) and the server still accepts it — the
//     annotation is a comment in the middle of a statement the weaver
//     has also rewritten, so "obviously fine" is not good enough;
//  2. a query that FAILS the requirement fails SCOPED (§12.2). The
//     diagnostic is raised, and the same rendering the oracle
//     describes still carries the tenant conjunct. An implementation
//     that returned the diagnostic instead of the conjunct would pass
//     every offline test and leak here.
func TestPolicyRequireAnnotationEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_DSN"),
		AllowDestructive: true,
		ServerVersion:    "16",
		SchemaSQL:        []string{schemaSQL},
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer cleanup()

	conn, connCleanup, err := devdb.Acquire(ctx, devdb.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer connCleanup()
	oracle := postgres.NewOracle(conn)

	pols := []policy.Policy{requiringTenantScopePolicy()}
	if d := policy.Validate(postgres.Profile{}, postgres.Frontend{}, pols, "sqletch.yaml"); len(d) != 0 {
		t.Fatalf("policy validation: %+v", d)
	}

	cases := []struct {
		src            string
		wantUnannotate bool
	}{
		{acknowledgedAudit, false},
		{unannotatedAudit, true},
		// An opt-out is the other way to discharge the obligation, and
		// must stay unwoven under the requirement.
		{allAuditBackfill, false},
	}
	for _, tc := range cases {
		q := compile(t, tc.src)
		wres := policy.Weave(postgres.Profile{}, postgres.Frontend{}, pols, q)

		var got127 bool
		for _, d := range wres.Diags {
			if d.Code == diagnostics.CodePolicyUnannotated {
				got127 = true
				continue
			}
			t.Fatalf("%s: unexpected weave diagnostic: %+v", q.Name, d)
		}
		if got127 != tc.wantUnannotate {
			t.Fatalf("%s: SQLETCH127 = %v, want %v (%+v)", q.Name, got127, tc.wantUnannotate, wres.Diags)
		}

		wq := wres.Query
		rs, err := ast.Renderings(postgres.Profile{}, wq)
		if err != nil {
			t.Fatal(err)
		}
		if d := rules.CheckR1(postgres.Profile{}, postgres.Frontend{}, wq, rs); len(d) != 0 {
			t.Fatalf("%s: R1 on woven: %+v", q.Name, d)
		}

		switch q.Name {
		case "AcknowledgedAudit":
			if !strings.Contains(rs[0].SQL, "@policy-apply: tenant_scope") {
				t.Fatalf("acknowledgment excised from the verified SQL:\n%s", rs[0].SQL)
			}
			if !strings.Contains(rs[0].SQL, "WHERE (a.tenant_id = $1)") {
				t.Fatalf("acknowledged query not woven:\n%s", rs[0].SQL)
			}
		case "UnannotatedAudit":
			// The load-bearing assertion of §12.2.
			if !strings.Contains(rs[0].SQL, "WHERE (a.tenant_id = $1)") {
				t.Fatalf("a query failing require_annotation was left UNSCOPED:\n%s", rs[0].SQL)
			}
		case "AllAuditBackfill":
			if strings.Contains(rs[0].SQL, "tenant_id") {
				t.Fatalf("opt-out was woven anyway:\n%s", rs[0].SQL)
			}
		}

		// The server is the judge of whether a woven statement carrying
		// directive comments is well-formed SQL.
		for _, r := range rs {
			if _, err := oracle.Describe(ctx, r.SQL); err != nil {
				t.Fatalf("%s: describe: %v\n%s", q.Name, err, r.SQL)
			}
		}

		tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
		if err != nil {
			t.Fatal(err)
		}
		if d := policy.Enforce(postgres.Profile{}, postgres.Frontend{}, pols, wq, tree, rs[0]); len(d) != 0 {
			t.Fatalf("%s: enforcement rejects the woven template: %+v", q.Name, d)
		}
	}
}
