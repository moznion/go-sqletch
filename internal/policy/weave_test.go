package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

func tenantPolicy() Policy {
	return Policy{
		Name:      "tenant_scope",
		Tables:    []string{"orders", "order_items"},
		Predicate: "{}.tenant_id = :tenant_id",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
}

func scanOne(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	if len(f.Queries) != 1 {
		t.Fatalf("got %d queries", len(f.Queries))
	}
	return f.Queries[0]
}

func weaveOne(t *testing.T, src string, pols ...Policy) Result {
	t.Helper()
	q := scanOne(t, src)
	return Weave(postgres.Profile{}, postgres.Frontend{}, pols, q)
}

func renderSQL(t *testing.T, q *template.QueryTemplate) string {
	t.Helper()
	r, err := ast.Render(postgres.Profile{}, q, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return strings.TrimSpace(r.SQL)
}

func noDiags(t *testing.T, res Result) {
	t.Helper()
	if len(res.Diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diags)
	}
}

// Golden woven renderings for each insertion case (design 14 §8).
func TestWeave_Golden(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // maximal rendering of the woven template, trimmed
	}{
		{
			name: "existing WHERE, alias bound",
			src:  "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n",
			want: "SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND o.status = $2",
		},
		{
			name: "no WHERE, before ORDER BY",
			src:  "-- name: Q :many\nSELECT id FROM orders ORDER BY id\n",
			want: "SELECT id FROM orders WHERE (orders.tenant_id = $1) ORDER BY id",
		},
		{
			name: "bare DELETE",
			src:  "-- name: Q :exec\nDELETE FROM orders\n",
			want: "DELETE FROM orders WHERE (orders.tenant_id = $1)",
		},
		{
			name: "UPDATE with WHERE",
			src:  "-- name: Q :exec\nUPDATE orders SET status = :s WHERE id = :id\n",
			want: "UPDATE orders SET status = $1 WHERE (orders.tenant_id = $2) AND id = $3",
		},
		{
			name: "self-join: one conjunct per occurrence",
			src:  "-- name: Q :many\nSELECT a.id FROM orders a JOIN orders b ON a.id = b.parent_id WHERE a.ok\n",
			want: "SELECT a.id FROM orders a JOIN orders b ON a.id = b.parent_id WHERE (a.tenant_id = $1) AND (b.tenant_id = $1) AND a.ok",
		},
		{
			name: "two designated tables",
			src:  "-- name: Q :many\nSELECT o.id FROM orders o JOIN order_items i ON i.order_id = o.id WHERE o.ok\n",
			want: "SELECT o.id FROM orders o JOIN order_items i ON i.order_id = o.id WHERE (o.tenant_id = $1) AND (i.tenant_id = $1) AND o.ok",
		},
		{
			name: "undesignated table untouched",
			src:  "-- name: Q :many\nSELECT id FROM users WHERE id = :id\n",
			want: "SELECT id FROM users WHERE id = $1",
		},
		{
			name: "INSERT VALUES into designated table untouched",
			src:  "-- name: Q :exec\nINSERT INTO orders (id) VALUES (:id)\n",
			want: "INSERT INTO orders (id) VALUES ($1)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			noDiags(t, res)
			if got := renderSQL(t, res.Query); got != tc.want {
				t.Errorf("woven rendering:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestWeave_MultiplePoliciesInDeclarationOrder(t *testing.T) {
	soft := Policy{Name: "soft_delete", Tables: []string{"orders"}, Predicate: "{}.deleted_at IS NULL"}
	res := weaveOne(t, "-- name: Q :many\nSELECT id FROM orders WHERE ok\n", tenantPolicy(), soft)
	noDiags(t, res)
	want := "SELECT id FROM orders WHERE (orders.tenant_id = $1) AND (orders.deleted_at IS NULL) AND ok"
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("got: %s\nwant: %s", got, want)
	}
	if len(res.Woven) != 2 || res.Woven[0].Policy.Name != "tenant_scope" || res.Woven[1].Policy.Name != "soft_delete" {
		t.Errorf("Woven = %+v", res.Woven)
	}
}

// Idempotence: a hand-scoped query is not double-woven, and the
// template is returned unmodified (identity, no copy).
func TestWeave_Idempotence(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND o.status = :status\n"
	q := scanOne(t, src)
	res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{tenantPolicy()}, q)
	noDiags(t, res)
	if res.Query != q {
		t.Errorf("hand-scoped query was rewritten")
	}
	// Coverage still records the policy as satisfied.
	if len(res.Woven) != 1 || len(res.Woven[0].Conjuncts) != 1 {
		t.Fatalf("Woven = %+v", res.Woven)
	}
	// Case/whitespace variants still match.
	src2 := "-- name: Q :many\nSELECT o.id FROM orders o WHERE O.Tenant_ID   =  :tenant_id AND o.ok\n"
	q2 := scanOne(t, src2)
	res2 := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{tenantPolicy()}, q2)
	noDiags(t, res2)
	if res2.Query != q2 {
		t.Errorf("normalized-equal conjunct was not recognized")
	}
}

// A guarded copy of a policy's conjunct must NOT satisfy the policy:
// it vanishes in guard-off shapes, so the weaver still adds the
// unconditional one (idempotence keys on skeleton conjuncts only). A
// paramless policy is used so the property is tested without a
// parameter-name collision — see TestWeave_RejectsCollisionWith*.
func TestWeave_GuardedCopyDoesNotCount(t *testing.T) {
	soft := Policy{Name: "soft", Tables: []string{"orders"}, Predicate: "{}.deleted_at IS NULL"}
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.ok\n@if-present(inc_deleted)\nAND o.deleted_at IS NULL\n@endif\n"
	res := weaveOne(t, src, soft)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	if !strings.Contains(got, "WHERE (o.deleted_at IS NULL) AND o.ok") {
		t.Errorf("guarded copy suppressed weaving:\n%s", got)
	}
}

// A policy must not re-bind a name the query already declares as an
// optional @if-present parameter: the woven conjunct is unconditional,
// so a caller passing None would send NULL in every shape and silently
// empty the result set. Reject with SQLETCH125 instead of weaving a
// copy the SQLETCH124 enforcement pass would then wrongly accept (M13).
func TestWeave_RejectsCollisionWithOptionalParam(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.ok\n@if-present(tenant_id)\nAND o.tenant_id = :tenant_id\n@endif\n"
	res := weaveOne(t, src, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
	if got := renderSQL(t, res.Query); strings.Contains(got, "WHERE o.tenant_id = $") {
		t.Errorf("collision must not be woven unconditionally:\n%s", got)
	}
}

// The same rejection for a name the query binds inside a @filter-tree
// @predicate (a constructor argument, not a params-struct value).
func TestWeave_RejectsCollisionWithFilterTreeParam(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders AS o\nWHERE TRUE\n  AND @filter-tree(scope)\n@predicate(tenant)\no.tenant_id = :tenant_id\n@end\n"
	res := weaveOne(t, src, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
}

// A plain, always-required value parameter of the same name is the D3a
// safe case: the policy binds the same required value, so weaving
// proceeds with no diagnostic.
func TestWeave_AllowsCollisionWithRequiredValueParam(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.owner_id = :tenant_id AND o.ok\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	if got := renderSQL(t, res.Query); !strings.Contains(got, "o.tenant_id = $") {
		t.Errorf("policy should weave on a plain required-value collision:\n%s", got)
	}
}

func TestWeave_Determinism(t *testing.T) {
	src := "-- name: Q :many\nSELECT a.id FROM orders a JOIN order_items b ON b.order_id = a.id WHERE a.ok\n"
	pols := []Policy{tenantPolicy(), {Name: "soft", Tables: []string{"orders"}, Predicate: "{}.deleted_at IS NULL"}}
	first := ""
	for i := 0; i < 5; i++ {
		res := weaveOne(t, src, pols...)
		noDiags(t, res)
		sql := renderSQL(t, res.Query)
		if i == 0 {
			first = sql
		} else if sql != first {
			t.Fatalf("weave output differs across runs:\n%s\nvs\n%s", first, sql)
		}
	}
}

func TestWeave_ParamRegistration(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT id FROM orders WHERE ok\n", tenantPolicy())
	noDiags(t, res)
	q := res.Query
	p, ok := q.Params["tenant_id"]
	if !ok || p.GuardBit != -1 {
		t.Fatalf("woven param not registered: %+v", q.Params)
	}
	found := false
	for _, n := range q.ParamOrder {
		if n == "tenant_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("tenant_id missing from ParamOrder %v", q.ParamOrder)
	}
	if h, ok := q.TypeHints["tenant_id"]; !ok || h.SQLType != "bigint" {
		t.Errorf("policy param type hint not injected: %+v", q.TypeHints)
	}
}

func TestWeave_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantMsg string
	}{
		{
			name:    "USING join on the nullable side",
			src:     "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders USING (user_id) WHERE u.ok\n",
			wantMsg: "no ON expression",
		},
		{
			name:    "NATURAL join on the nullable side",
			src:     "-- name: Q :many\nSELECT u.id FROM users u NATURAL LEFT JOIN orders WHERE u.ok\n",
			wantMsg: "no ON expression",
		},
		{
			name:    "subquery-only occurrence",
			src:     "-- name: Q :many\nSELECT id FROM reports WHERE order_id IN (SELECT id FROM orders)\n",
			wantMsg: "subquery",
		},
		{
			name:    "CTE body occurrence",
			src:     "-- name: Q :many\nWITH r AS (SELECT * FROM orders) SELECT * FROM r\n",
			wantMsg: "subquery",
		},
		{
			name:    "guarded join",
			src:     "-- name: Q :many\nSELECT u.id FROM users u\n@if-present(with_orders)\nJOIN orders o ON o.user_id = u.id AND :with_orders\n@endif\nWHERE u.ok\n",
			wantMsg: "guarded",
		},
		{
			name:    "INSERT ... SELECT reading a designated table",
			src:     "-- name: Q :exec\nINSERT INTO archive SELECT * FROM orders\n",
			wantMsg: "INSERT's SELECT body",
		},
		{
			name:    "set operation hides the table from the weaver",
			src:     "-- name: Q :many\nSELECT id FROM orders UNION SELECT id FROM archive\n",
			wantMsg: "set-operation",
		},
		{
			name:    "conflicting param hint",
			src:     "-- name: Q :many\n-- @param tenant_id: text\nSELECT id FROM orders WHERE name = :tenant_id\n",
			wantMsg: "policy declares",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, tc.src)
			res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{tenantPolicy()}, q)
			if len(res.Diags) != 1 {
				t.Fatalf("diags = %+v, want exactly one SQLETCH125", res.Diags)
			}
			d := res.Diags[0]
			if d.Code != diagnostics.CodePolicyUnweavable {
				t.Errorf("code = %s, want SQLETCH125", d.Code)
			}
			if !strings.Contains(d.Message, tc.wantMsg) {
				t.Errorf("message %q does not mention %q", d.Message, tc.wantMsg)
			}
			if res.Query != q {
				t.Errorf("rejected query must not be woven")
			}
		})
	}
}

// Regression (#89 follow-up): a SELECT whose JOIN condition uses a bare
// column literally named `conflict` forms the token pair `ON` `CONFLICT`,
// but this is NOT an INSERT upsert. Before the scanner gated ON CONFLICT
// detection on the statement being an INSERT, WhereKwEnd stayed -1 and the
// weaver fell through to the tail/StmtEnd fallback, synthesizing a SECOND
// `WHERE` — producing `... WHERE o.status = $2 WHERE o.tenant_id = $1`,
// which the oracle rejects as a syntax error. The policy conjunct must
// instead weave into the single real WHERE.
func TestWeave_SelectJoinOnConflictColumnSingleWhere(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o JOIN b ON conflict = b.id WHERE o.status = :status\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	if n := strings.Count(got, "WHERE"); n != 1 {
		t.Fatalf("expected exactly one WHERE, got %d:\n%s", n, got)
	}
	want := "SELECT o.id FROM orders o JOIN b ON conflict = b.id WHERE (o.tenant_id = $1) AND o.status = $2"
	if got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
}

// Tail constructs must bound the synthesized-WHERE insertion point: a
// @choose whose cases are GROUP BY/ORDER BY clauses, and a WINDOW
// clause, both start the statement tail, so the woven conjunct lands
// BEFORE them and the woven SQL parses. (Regression: the conjunct was
// spliced after them — `GROUP BY day WHERE …` — invalid SQL caught
// loud as SQLETCH100, a false rejection.)
func TestWeave_TailConstructPlacement(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // maximal rendering of the woven template, trimmed
	}{
		{
			name: "conjunct before a GROUP BY @choose",
			src:  "-- name: Q :many\nSELECT count(*) FROM orders\n@choose(g)\n@case(day)\nGROUP BY day\n@case(week)\nGROUP BY week\n@end\n",
			want: "SELECT count(*) FROM orders\nWHERE (orders.tenant_id = $1) \nGROUP BY day",
		},
		{
			name: "conjunct before an ORDER BY @choose",
			src:  "-- name: Q :many\nSELECT id FROM orders\n@choose(sort)\n@case(new)\nORDER BY id DESC\n@case(old)\nORDER BY id ASC\n@end\n",
			want: "SELECT id FROM orders\nWHERE (orders.tenant_id = $1) \nORDER BY id DESC",
		},
		{
			name: "conjunct before a WINDOW clause",
			src:  "-- name: Q :many\nSELECT id, sum(amount) OVER w FROM orders WINDOW w AS (PARTITION BY id)\n",
			want: "SELECT id, sum(amount) OVER w FROM orders WHERE (orders.tenant_id = $1) WINDOW w AS (PARTITION BY id)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			noDiags(t, res)
			got := renderSQL(t, res.Query)
			if got != tc.want {
				t.Errorf("woven rendering:\n got: %q\nwant: %q", got, tc.want)
			}
			// The woven maximal rendering must parse — the regression
			// produced invalid SQL.
			if _, err := (postgres.Frontend{}).Parse(got); err != nil {
				t.Errorf("woven rendering does not parse: %v\n%s", err, got)
			}
			if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
				t.Errorf("enforcement rejects the woven output: %+v", diags)
			}
		})
	}
}

func TestWeave_AppliesToFiltering(t *testing.T) {
	p := tenantPolicy()
	p.Kinds = []dialect.StmtKind{dialect.StmtSelect}
	res := weaveOne(t, "-- name: Q :exec\nDELETE FROM orders\n", p)
	noDiags(t, res)
	if got := renderSQL(t, res.Query); got != "DELETE FROM orders" {
		t.Errorf("delete was woven despite applies_to [select]: %s", got)
	}
	if len(res.Woven) != 0 {
		t.Errorf("Woven = %+v, want empty", res.Woven)
	}
}
