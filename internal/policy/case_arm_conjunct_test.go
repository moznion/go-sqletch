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

// Security finding F14: exprScan (and joinOn) split conjunct segments at
// every paren-depth-0 AND without modeling CASE … END as nesting, so a
// policy conjunct written inside a CASE arm became its own "top-level"
// segment. The weaver then skipped the splice (idempotence) and Enforce
// credited it, while the conjunct actually scoped only the arm it sits
// in: `mine = false` returned every tenant's public orders. A CASE is a
// single operand, exactly like a parenthesized expression.

// The finding's exact query: the weaver must splice the conjunct at the
// top level (it is NOT already present there).
func TestWeave_CaseArmConjunct_IsWoven(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"finding F14",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN :mine THEN o.owner_id = :uid AND o.tenant_id = :tenant_id AND o.active ELSE o.is_public END LIMIT 50\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND CASE WHEN $2 THEN o.owner_id = $3 AND o.tenant_id = $1 AND o.active ELSE o.is_public END LIMIT 50",
		},
		{
			"conjunct in the ELSE arm",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN :mine THEN o.active ELSE o.is_public AND o.tenant_id = :tenant_id END\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND CASE WHEN $2 THEN o.active ELSE o.is_public AND o.tenant_id = $1 END",
		},
		{
			"conjunct in a WHEN condition",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN o.active AND o.tenant_id = :tenant_id THEN true ELSE o.is_public END\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND CASE WHEN o.active AND o.tenant_id = $1 THEN true ELSE o.is_public END",
		},
		{
			"nested CASE",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN :mine THEN CASE WHEN o.active THEN true ELSE o.is_public AND o.tenant_id = :tenant_id END ELSE o.is_public END AND o.ok\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND CASE WHEN $2 THEN CASE WHEN o.active THEN true ELSE o.is_public AND o.tenant_id = $1 END ELSE o.is_public END AND o.ok",
		},
		{
			"CASE inside parentheses",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE (CASE WHEN :mine THEN o.active AND o.tenant_id = :tenant_id ELSE o.is_public END)\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND (CASE WHEN $2 THEN o.active AND o.tenant_id = $1 ELSE o.is_public END)",
		},
		{
			"simple CASE form",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE o.kind WHEN 1 THEN o.active AND o.tenant_id = :tenant_id ELSE o.is_public END\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND CASE o.kind WHEN 1 THEN o.active AND o.tenant_id = $1 ELSE o.is_public END",
		},
		{
			// An OR inside a CASE arm is not a top-level OR: the CASE is
			// one operand, so no wrap is needed (and none is emitted).
			"OR inside an arm does not wrap",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN o.a OR o.b THEN true ELSE false END AND o.ok\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND CASE WHEN o.a OR o.b THEN true ELSE false END AND o.ok",
		},
		{
			// …but a depth-0 OR after the CASE still wraps.
			"OR after the CASE wraps",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN o.a THEN o.tenant_id = :tenant_id AND o.b ELSE false END OR o.ok\n",
			"SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND (CASE WHEN o.a THEN o.tenant_id = $1 AND o.b ELSE false END OR o.ok)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			noDiags(t, res)
			if got := renderSQL(t, res.Query); got != tc.want {
				t.Errorf("CASE-arm conjunct credited as top-level:\n got: %s\nwant: %s", got, tc.want)
			}
			if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
				t.Errorf("enforcement rejects the woven output: %+v", diags)
			}
		})
	}
}

// Enforce re-derives presence independently: a hand-written conjunct
// that exists ONLY inside a CASE arm does not scope the statement.
func TestEnforce_CaseArmConjunct_Fails(t *testing.T) {
	srcs := []string{
		"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN :mine THEN o.owner_id = :uid AND o.tenant_id = :tenant_id AND o.active ELSE o.is_public END LIMIT 50\n",
		"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN :mine THEN CASE WHEN o.a THEN o.b AND o.tenant_id = :tenant_id ELSE false END ELSE o.is_public END\n",
		"-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND CASE WHEN :mine THEN o.active AND o.tenant_id = :tenant_id ELSE o.is_public END WHERE u.ok\n",
	}
	for _, src := range srcs {
		q := scanOne(t, src)
		diags := enforceOn(t, q, tenantPolicy())
		if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
			t.Errorf("CASE-arm-only conjunct must be SQLETCH124 for %q, got %+v", src, diags)
		}
	}
}

// Must-stay idempotence: a hand-written TOP-LEVEL conjunct next to a
// CASE is still credited (not double-woven) and passes enforcement.
func TestWeave_CaseArm_IdempotenceMustStay(t *testing.T) {
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN :mine THEN o.active ELSE o.is_public END AND o.tenant_id = :tenant_id\n",
			"SELECT o.id FROM orders o WHERE CASE WHEN $1 THEN o.active ELSE o.is_public END AND o.tenant_id = $2",
		},
		{
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND CASE WHEN :mine THEN o.active AND o.x ELSE o.is_public END LIMIT 5\n",
			"SELECT o.id FROM orders o WHERE o.tenant_id = $1 AND CASE WHEN $2 THEN o.active AND o.x ELSE o.is_public END LIMIT 5",
		},
		{
			"-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND CASE WHEN :mine THEN o.active AND o.x ELSE true END AND o.tenant_id = :tenant_id WHERE u.ok\n",
			"SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND CASE WHEN $1 THEN o.active AND o.x ELSE true END AND o.tenant_id = $2 WHERE u.ok",
		},
	}
	for _, tc := range cases {
		res := weaveOne(t, tc.src, tenantPolicy())
		noDiags(t, res)
		if got := renderSQL(t, res.Query); got != tc.want {
			t.Errorf("top-level conjunct next to a CASE was double-woven:\n got: %s\nwant: %s", got, tc.want)
		}
		if diags := enforceOn(t, scanOne(t, tc.src), tenantPolicy()); len(diags) != 0 {
			t.Errorf("top-level hand-written conjunct not credited for %q: %+v", tc.src, diags)
		}
	}
}

// ON-clause variant: a conjunct inside a CASE in the null-extended
// join's ON is not credited; the weaver extends the ON after the CASE.
func TestWeaveON_CaseArmConjunct_IsWoven(t *testing.T) {
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND CASE WHEN :mine THEN o.active AND o.tenant_id = :tenant_id ELSE o.is_public END WHERE u.ok\n"
	want := "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND CASE WHEN $1 THEN o.active AND o.tenant_id = $2 ELSE o.is_public END AND (o.tenant_id = $2) WHERE u.ok"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("ON CASE-arm conjunct credited:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
		t.Errorf("enforcement rejects the ON weave: %+v", diags)
	}
	// A clause keyword inside the CASE does not end the ON early: the
	// tail keyword-shaped WHERE is the real one after END.
	src2 := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON CASE WHEN o.a OR o.b THEN o.user_id = u.id ELSE false END WHERE u.ok\n"
	want2 := "SELECT u.id FROM users u LEFT JOIN orders o ON CASE WHEN o.a OR o.b THEN o.user_id = u.id ELSE false END AND (o.tenant_id = $1) WHERE u.ok"
	res2 := weaveOne(t, src2, tenantPolicy())
	noDiags(t, res2)
	if got := renderSQL(t, res2.Query); got != want2 {
		t.Errorf("OR inside an ON CASE arm mis-wrapped:\n got: %s\nwant: %s", got, want2)
	}
}

// Keyword-column edge cases: `end` is a legal bare column on MySQL and
// SQLite (non-reserved / ID-fallback); in OPERAND position it is a
// column and must neither close nor unbalance the CASE. A dotted `o.end`
// or `o.case` is always a column. `case`/`when`/`then`/`else` are
// reserved on all three dialects.
func TestWeave_CaseArm_KeywordColumns(t *testing.T) {
	pol := func(typ string) Policy {
		return Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: typ}
	}
	cases := []struct {
		name string
		prof dialect.LexerProfile
		fe   dialect.Frontend
		typ  string
		src  string
		want string
	}{
		{
			"mysql bare end in WHEN",
			mysql.Profile{}, mysql.Frontend{}, "bigint",
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN end = 1 THEN tenant_id = :tid AND a = 1 ELSE b = 1 END\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND CASE WHEN end = 1 THEN tenant_id = ? AND a = 1 ELSE b = 1 END",
		},
		{
			"mysql bare end in THEN",
			mysql.Profile{}, mysql.Frontend{}, "bigint",
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a = 1 THEN end ELSE tenant_id = :tid AND b = 1 END\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND CASE WHEN a = 1 THEN end ELSE tenant_id = ? AND b = 1 END",
		},
		{
			"mysql bare end column outside any CASE stays credited",
			mysql.Profile{}, mysql.Frontend{}, "bigint",
			"-- name: Q :many\nSELECT id FROM orders WHERE end = 1 AND tenant_id = :tid\n",
			"SELECT id FROM orders WHERE end = 1 AND tenant_id = ?",
		},
		{
			// F2/F3 × F14: XOR / `||` inside a CASE arm are not top-level
			// OR-class operators (the CASE is one operand) — no wrap.
			"mysql XOR and || inside an arm do not wrap",
			mysql.Profile{}, mysql.Frontend{}, "bigint",
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a XOR b THEN c || d ELSE false END AND e = 1\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND CASE WHEN a XOR b THEN c || d ELSE false END AND e = 1",
		},
		{
			// …but a depth-0 XOR / `||` after the CASE still wraps.
			"mysql XOR after the CASE wraps",
			mysql.Profile{}, mysql.Frontend{}, "bigint",
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a = 1 THEN b ELSE c END XOR d\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (CASE WHEN a = 1 THEN b ELSE c END XOR d)",
		},
		{
			"mysql || after the CASE wraps",
			mysql.Profile{}, mysql.Frontend{}, "bigint",
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a = 1 THEN b ELSE c END || d\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (CASE WHEN a = 1 THEN b ELSE c END || d)",
		},
		{
			"sqlite bare end in WHEN",
			sqlite.Profile{}, sqlite.Frontend{}, "int",
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN end = 1 THEN tenant_id = :tid AND a = 1 ELSE b = 1 END\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND CASE WHEN end = 1 THEN tenant_id = ? AND a = 1 ELSE b = 1 END",
		},
		{
			"postgres dotted o.end and o.case",
			postgres.Profile{}, postgres.Frontend{}, "bigint",
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN o.end THEN o.case AND tenant_id = :tid ELSE false END\n",
			"SELECT o.id FROM orders o WHERE (tenant_id = $1) AND CASE WHEN o.end THEN o.case AND tenant_id = $1 ELSE false END",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, diags := template.NewScanner(tc.prof).ScanFile("t.sql", []byte(tc.src))
			if diagnostics.HasErrors(diags) {
				t.Fatalf("scan: %+v", diags)
			}
			q := f.Queries[0]
			r0, err := ast.Render(tc.prof, q, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tc.fe.Parse(r0.SQL); err != nil {
				t.Fatalf("fixture must be valid SQL for the frontend: %v", err)
			}
			res := Weave(tc.prof, tc.fe, []Policy{pol(tc.typ)}, q)
			if diagnostics.HasErrors(res.Diags) {
				t.Fatalf("unexpected diags: %+v", res.Diags)
			}
			r, err := ast.Render(tc.prof, res.Query, nil)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got := strings.TrimSpace(r.SQL); got != tc.want {
				t.Errorf("\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// Fail closed: a WHERE/ON expression whose CASE … END nesting the
// lexical scan cannot balance is unreadable (lexOK=false) — never a
// guess at segment boundaries.
func TestClauseScan_UnbalancedCaseFailsClosed(t *testing.T) {
	where := []string{
		"SELECT id FROM orders WHERE CASE WHEN a THEN b AND tenant_id = 1",            // never closed
		"SELECT id FROM orders WHERE a = 1 AND b END AND tenant_id = 1",               // END with no CASE, operator position
		"SELECT id FROM orders WHERE a THEN tenant_id = 1",                            // arm keyword outside CASE
		"SELECT id FROM orders WHERE CASE WHEN a THEN b END ELSE c AND tenant_id = 1", // arm after close
	}
	for _, sql := range where {
		q := &template.QueryTemplate{Items: []template.Item{&template.Skeleton{Text: sql, Span: diagnostics.Span{Start: 0, End: len(sql)}}}}
		if cs := whereClause(postgres.Profile{}, q); cs.lexOK {
			t.Errorf("unbalanced CASE read as lexOK: %q segs=%v", sql, cs.segs)
		}
	}
	on := "SELECT u.id FROM users u LEFT JOIN orders o ON CASE WHEN a THEN o.tenant_id = 1 AND b WHERE u.ok"
	q := &template.QueryTemplate{Items: []template.Item{&template.Skeleton{Text: on, Span: diagnostics.Span{Start: 0, End: len(on)}}}}
	off := strings.Index(on, "orders")
	if res := joinOn(postgres.Profile{}, q, off, dialect.JoinLeft); res.cs.lexOK {
		t.Errorf("unbalanced CASE in ON read as lexOK: segs=%v", res.cs.segs)
	}
	// `end` after `IS DISTINCT FROM` (SQLite ≥ 3.39; rqlite/sql cannot
	// parse it, so this is lexical only) is an operand: FROM is modeled as
	// operand-introducing, so the bare `end` neither closes the CASE nor
	// lets the following AND split the arm.
	distinct := "SELECT id FROM orders WHERE CASE WHEN a IS DISTINCT FROM end AND tenant_id = 1 THEN 1 ELSE 0 END = 1"
	qd := &template.QueryTemplate{Items: []template.Item{&template.Skeleton{Text: distinct, Span: diagnostics.Span{Start: 0, End: len(distinct)}}}}
	if cs := whereClause(sqlite.Profile{}, qd); !cs.lexOK || len(cs.segs) != 1 {
		t.Errorf("IS DISTINCT FROM end: lexOK=%v segs=%v", cs.lexOK, cs.segs)
	}
	// Balanced controls stay readable.
	ok := "SELECT id FROM orders WHERE CASE WHEN a THEN b ELSE c END AND tenant_id = 1"
	qok := &template.QueryTemplate{Items: []template.Item{&template.Skeleton{Text: ok, Span: diagnostics.Span{Start: 0, End: len(ok)}}}}
	if cs := whereClause(postgres.Profile{}, qok); !cs.lexOK || len(cs.segs) != 2 {
		t.Errorf("balanced CASE: lexOK=%v segs=%v", cs.lexOK, cs.segs)
	}
}

// A construct inside a CASE arm leaves the scan unable to tell where the
// arm ends: the weaver refuses (SQLETCH125) instead of weaving blind.
func TestWeave_ConstructInsideCase_Refused(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN o.a THEN o.b\n@if-present(x)\nAND o.x = :x\n@endif\nELSE o.c END\n"
	res := weaveOne(t, src, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
}

// The ON-clause variant of the fail-closed refusal: a construct inside a
// CASE in the null-extended join's ON ends the ON scan mid-CASE.
func TestWeaveON_ConstructInsideCase_Refused(t *testing.T) {
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND CASE WHEN o.a THEN o.b\n@if-present(x)\nAND o.x = :x\n@endif\nELSE o.c END WHERE u.ok\n"
	res := weaveOne(t, src, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable ||
		!strings.Contains(res.Diags[0].Message, "CASE") {
		t.Fatalf("want exactly one SQLETCH125 naming the CASE, got %+v", res.Diags)
	}
}

// Set-operation branch cores share exprScan: a CASE-arm conjunct in a
// branch's WHERE is not credited for that branch.
func TestWeaveSetOp_CaseArmConjunct_IsWoven(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE CASE WHEN :mine THEN o.active AND o.tenant_id = :tenant_id ELSE o.is_public END\nUNION ALL\nSELECT i.id FROM order_items i WHERE i.tenant_id = :tenant_id\n"
	want := "SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND CASE WHEN $2 THEN o.active AND o.tenant_id = $1 ELSE o.is_public END\nUNION ALL\nSELECT i.id FROM order_items i WHERE i.tenant_id = $1"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("set-op branch CASE-arm conjunct credited:\n got: %s\nwant: %s", got, want)
	}
	if diags := enforceOn(t, scanOne(t, src), tenantPolicy()); len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Errorf("unwoven set-op branch with CASE-arm-only conjunct must be SQLETCH124, got %+v", diags)
	}
}
