package codegen

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/rules"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

// wovenConformanceCases are templates that a policy actually rewrites.
// Every case must weave (asserted below): a case the weaver leaves
// untouched would silently degrade into the plain conformance corpus.
var wovenConformanceCases = []struct {
	name string
	src  string
}{
	{
		name: "woven WHERE conjunct with if-present guards",
		src: `-- name: WovenGuards :many
SELECT o.id FROM orders AS o
WHERE TRUE
@if-present(status)
  AND o.status = :status
@endif
@if-present(after_id)
  AND o.id < :after_id
@endif
ORDER BY o.id;
`,
	},
	{
		name: "no WHERE clause: weaver inserts one",
		src: `-- name: WovenNoWhere :many
SELECT id FROM orders ORDER BY id LIMIT :limit;
`,
	},
	{
		name: "LEFT JOIN nullable side: conjunct woven into that ON clause",
		src: `-- name: WovenOnClause :many
SELECT u.id FROM users AS u LEFT JOIN orders AS o ON o.user_id = u.id
WHERE TRUE
@if-present(active)
  AND u.active = :active
@endif
;
`,
	},
	{
		name: "choose and order-by alongside a woven conjunct",
		src: `-- name: WovenChooseOrder :many
SELECT
@choose(bucket)
@case(daily)
o.created_day
@case(weekly)
o.created_week
@end
 AS bucket, o.id
FROM orders AS o
WHERE o.owner_id = :owner
@if-present(status)
  AND o.status = :status
@endif
@order-by(sort)
@key(created)
o.created_day
@key(id)
o.id
@default
ORDER BY o.id ASC
@end
LIMIT :limit;
`,
	},
	{
		name: "shared required param bound by skeleton and policy predicate",
		// The D3a safe case (TestWeave_AllowsCollisionWithRequiredValueParam):
		// a plain required value param of the policy's name; the woven
		// conjunct reuses the same bind, so renderer and composer must
		// agree on its single dollar number.
		src: `-- name: WovenSharedParam :many
SELECT o.id FROM orders AS o
WHERE o.owner_id = :tenant_id
@if-present(status)
  AND o.status = :status
@endif
;
`,
	},
}

func wovenTenantPolicy() policy.Policy {
	return policy.Policy{
		Name:      "tenant_scope",
		Tables:    []string{"orders", "order_items"},
		Predicate: "{}.tenant_id = :tenant_id",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
}

// TestWovenComposeConformance extends the compose-conformance invariant
// to POLICY-WOVEN templates: after policy.Weave rewrites a query,
// runtime.Compose over codegen.BuildFrags must still be byte-identical
// to ast.RenderShape for every enumerable shape, with identical bind
// order. The cases follow the pipeline's scan sequence (cli.scanChecks):
// lexical checks on the UNWOVEN template, weave, then renderings and R1
// on the WOVEN result — everything downstream (BuildFrags included)
// consumes the woven template. Render and compose are compared to EACH
// OTHER only; exact SQL bytes are deliberately not pinned, so emission
// changes (e.g. predicate parenthesization) do not invalidate the test.
func TestWovenComposeConformance(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		wovenConformanceOver(t, postgres.Profile{}, postgres.Frontend{}, runtime.StyleDollar)
	})
	t.Run("mysql", func(t *testing.T) {
		wovenConformanceOver(t, mysql.Profile{}, mysql.Frontend{}, runtime.StyleQuestion)
	})
	t.Run("sqlite", func(t *testing.T) {
		wovenConformanceOver(t, sqlite.Profile{}, sqlite.Frontend{}, runtime.StyleQuestion)
	})
}

func wovenConformanceOver(t *testing.T, profile dialect.LexerProfile, frontend dialect.Frontend, style runtime.Style) {
	t.Helper()
	pols := []policy.Policy{wovenTenantPolicy()}
	if d := policy.Validate(profile, frontend, pols, "sqletch.yaml"); diagnostics.HasErrors(d) {
		t.Fatalf("policy must validate: %+v", d)
	}
	for _, tc := range wovenConformanceCases {
		t.Run(tc.name, func(t *testing.T) {
			f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(tc.src))
			if diagnostics.HasErrors(diags) {
				t.Fatalf("scan: %+v", diags)
			}
			q := f.Queries[0]
			// Pipeline order (cli.scanChecks): lexical on the UNWOVEN
			// template — template validity never depends on config.
			if d := rules.CheckLexical(profile, q); diagnostics.HasErrors(d) {
				t.Fatalf("lexical: %+v", d)
			}
			res := policy.Weave(profile, frontend, pols, q)
			if diagnostics.HasErrors(res.Diags) {
				t.Fatalf("weave: %+v", res.Diags)
			}
			if len(res.Woven) == 0 || len(res.Woven[0].Conjuncts) == 0 {
				t.Fatal("case must actually weave a conjunct (else it degrades to the plain corpus)")
			}
			wq := res.Query
			rs, err := ast.Renderings(profile, wq)
			if err != nil {
				t.Fatal(err)
			}
			if d := rules.CheckR1(profile, frontend, wq, rs); diagnostics.HasErrors(d) {
				t.Fatalf("R1 on the woven template: %+v", d)
			}
			// The maximal rendering must show the woven predicate at all
			// (a contains-check, not a byte pin).
			if !strings.Contains(rs[0].SQL, "tenant_id") {
				t.Fatalf("woven rendering lost the policy predicate:\n%s", rs[0].SQL)
			}
			assertComposeConformance(t, profile, style, wq)
		})
	}
}
