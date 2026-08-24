package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// Audit-16: the audit-15 (#119) prevDot guard fixed a DOTTED keyword-column
// (`o.group`), but a BARE non-reserved keyword-column had no guard. In the
// Tier-2 dialects several tailKeywords are non-reserved and legal as bare
// column names (MySQL: offset, returning; SQLite: offset, fetch, for,
// window), so `WHERE offset = 1 OR active` ended the scan at `offset`,
// missed the depth-0 OR, and wove an unwrapped conjunct
// (`(tenant AND offset=1) OR active`) — every `active` row for all tenants.
// Valid SQL that PREPAREs; Enforce shares the scanner. The fix classifies a
// keyword only in OPERATOR position (afterOperand); in OPERAND position it
// is a column. This test drives the real Tier-2 frontends.
func TestWeave_BareKeywordColumn_Wraps(t *testing.T) {
	tenant := func() Policy {
		return Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	}
	tenantSQLite := func() Policy {
		return Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "int"}
	}
	cases := []struct {
		name  string
		prof  dialect.LexerProfile
		fe    dialect.Frontend
		ph    string // placeholder as rendered
		pol   Policy
		col   string
		phTyp string
	}{
		{"mysql-offset", mysql.Profile{}, mysql.Frontend{}, "?", tenant(), "offset", ""},
		{"mysql-returning", mysql.Profile{}, mysql.Frontend{}, "?", tenant(), "returning", ""},
		{"sqlite-offset", sqlite.Profile{}, sqlite.Frontend{}, "?", tenantSQLite(), "offset", ""},
		{"sqlite-window", sqlite.Profile{}, sqlite.Frontend{}, "?", tenantSQLite(), "window", ""},
		{"sqlite-for", sqlite.Profile{}, sqlite.Frontend{}, "?", tenantSQLite(), "for", ""},
		{"sqlite-fetch", sqlite.Profile{}, sqlite.Frontend{}, "?", tenantSQLite(), "fetch", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "-- name: Q :many\nSELECT id FROM orders WHERE " + tc.col + " = 1 OR active\n"
			f, diags := template.NewScanner(tc.prof).ScanFile("t.sql", []byte(src))
			if diagnostics.HasErrors(diags) {
				t.Fatalf("scan: %+v", diags)
			}
			q := f.Queries[0]
			res := Weave(tc.prof, tc.fe, []Policy{tc.pol}, q)
			if diagnostics.HasErrors(res.Diags) {
				t.Fatalf("unexpected diags: %+v", res.Diags)
			}
			r, err := ast.Render(tc.prof, res.Query, nil)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			got := strings.TrimSpace(r.SQL)
			want := "SELECT id FROM orders WHERE (tenant_id = " + tc.ph + ") AND (" + tc.col + " = 1 OR active)"
			if got != want {
				t.Errorf("bare keyword-column %q leaked (OR escaped scope):\n got: %s\nwant: %s", tc.col, got, want)
			}
		})
	}
}

// The join-ON scanner has the same operand-position guard: a bare
// keyword-column inside an ON before a depth-0 OR must not truncate the
// scan. SQLite `window` on a LEFT JOIN's nullable side weaves into the ON.
func TestWeave_BareKeywordColumnInJoinOn(t *testing.T) {
	src := "-- name: Q :many\nSELECT a.id FROM acc a LEFT JOIN orders o ON o.k = a.g AND window = 1 OR o.h = 2\n"
	f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("t.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "{}.tenant_id = :tid", ParamName: "tid", ParamType: "int"}
	res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diags: %+v", res.Diags)
	}
	r, err := ast.Render(sqlite.Profile{}, res.Query, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := strings.TrimSpace(r.SQL)
	// The whole ON disjunction must be wrapped and the conjunct inside the ON.
	if !strings.Contains(got, "ON (o.k = a.g AND window = 1 OR o.h = 2) AND (o.tenant_id = ?)") {
		t.Fatalf("joinOn bare keyword-column leaked or malformed:\n%s", got)
	}
}

// afterOperand must not break ordinary expressions: infix keyword-operators
// (IS/LIKE/IN/BETWEEN) keep the following keyword-column classified as an
// operand, and `IS NOT NULL OR` still detects the OR.
func TestWeave_OperandTracking_Controls(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			// IS NOT NULL then OR: OR must be seen → wrapped.
			"-- name: Q :many\nSELECT id FROM orders WHERE a IS NOT NULL OR b\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (a IS NOT NULL OR b)",
		},
		{
			// LIKE with a keyword-column RHS then OR: OR must be seen.
			"-- name: Q :many\nSELECT id FROM orders WHERE name LIKE offset OR b\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (name LIKE offset OR b)",
		},
		{
			// A real trailing OFFSET (operator position) still bounds WHERE.
			"-- name: Q :many\nSELECT id FROM orders WHERE a = 1 OR b = 2 LIMIT 10 OFFSET 5\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (a = 1 OR b = 2) LIMIT 10 OFFSET 5",
		},
	}
	for _, tc := range cases {
		f, _ := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(tc.src))
		q := f.Queries[0]
		res := Weave(mysql.Profile{}, mysql.Frontend{}, []Policy{pol}, q)
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("src %q diags %+v", tc.src, res.Diags)
		}
		r, err := ast.Render(mysql.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got := strings.TrimSpace(r.SQL); got != tc.want {
			t.Errorf("got:  %s\nwant: %s", got, tc.want)
		}
	}
}

// The naturalIntroducesRight prevDot consistency fix: a dotted column named
// `natural` must not spuriously refuse a gate-able LEFT JOIN.
func TestWeave_DottedNaturalColumn_NoSpuriousRefusal(t *testing.T) {
	src := "-- name: Q :many\nSELECT a.id FROM a JOIN b ON b.z = a.natural LEFT JOIN orders o ON o.w = a.w\n"
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "{}.tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{pol}, q)
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("spurious refusal from a dotted `natural` column: %+v", res.Diags)
	}
	r, err := ast.Render(postgres.Profile{}, res.Query, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.TrimSpace(r.SQL); !strings.Contains(got, "ON o.w = a.w AND (o.tenant_id = $1)") {
		t.Fatalf("orders not woven into its own ON:\n%s", got)
	}
}
