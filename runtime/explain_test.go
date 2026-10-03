package runtime

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestExplainStatement(t *testing.T) {
	const sql = "\nSELECT 1 FROM t WHERE id = $1;\n"
	cases := []struct {
		d    ExplainDialect
		opts ExplainOptions
		want string
	}{
		{ExplainPostgres, ExplainOptions{}, "EXPLAIN " + sql},
		{ExplainPostgres, ExplainOptions{Format: ExplainJSON}, "EXPLAIN (FORMAT JSON) " + sql},
		{ExplainPostgres, ExplainOptions{Analyze: true}, "EXPLAIN (ANALYZE) " + sql},
		{ExplainPostgres, ExplainOptions{Analyze: true, Format: ExplainJSON}, "EXPLAIN (ANALYZE, FORMAT JSON) " + sql},
		{ExplainMySQL, ExplainOptions{}, "EXPLAIN FORMAT=TREE " + sql},
		{ExplainMySQL, ExplainOptions{Format: ExplainJSON}, "EXPLAIN FORMAT=JSON " + sql},
		{ExplainMySQL, ExplainOptions{Analyze: true}, "EXPLAIN ANALYZE " + sql},
		{ExplainMySQL, ExplainOptions{Analyze: true, Format: ExplainJSON}, "EXPLAIN ANALYZE FORMAT=JSON " + sql},
		{ExplainSQLite, ExplainOptions{}, "EXPLAIN QUERY PLAN " + sql},
		{ExplainSQLite, ExplainOptions{Format: ExplainJSON}, "EXPLAIN QUERY PLAN " + sql},
	}
	for _, c := range cases {
		got, err := ExplainStatement(c.d, c.opts, sql)
		if err != nil {
			t.Fatalf("%v %+v: %v", c.d, c.opts, err)
		}
		if got != c.want {
			t.Errorf("%v %+v: got %q, want %q", c.d, c.opts, got, c.want)
		}
	}
}

func TestExplainStatementRefusals(t *testing.T) {
	cases := []struct {
		name string
		d    ExplainDialect
		opts ExplainOptions
	}{
		{"sqlite analyze", ExplainSQLite, ExplainOptions{Analyze: true}},
		{"sqlite analyze json", ExplainSQLite, ExplainOptions{Analyze: true, Format: ExplainJSON}},
		{"unknown format pg", ExplainPostgres, ExplainOptions{Format: ExplainFormat(7)}},
		{"unknown format mysql", ExplainMySQL, ExplainOptions{Format: ExplainFormat(2)}},
		{"unknown format sqlite", ExplainSQLite, ExplainOptions{Format: ExplainFormat(255)}},
		{"unset dialect", ExplainDialect(0), ExplainOptions{}},
		{"unknown dialect", ExplainDialect(9), ExplainOptions{}},
	}
	for _, c := range cases {
		got, err := ExplainStatement(c.d, c.opts, "SELECT 1")
		if !errors.Is(err, ErrExplainUnsupported) {
			t.Errorf("%s: err = %v, want ErrExplainUnsupported", c.name, err)
		}
		if got != "" {
			t.Errorf("%s: statement %q returned alongside a refusal", c.name, got)
		}
	}
}

func TestSQLitePlanText(t *testing.T) {
	rows := []SQLitePlanRow{
		{ID: 2, Parent: 0, Detail: "SCAN u"},
		{ID: 5, Parent: 0, Detail: "CORRELATED SCALAR SUBQUERY 1"},
		{ID: 9, Parent: 5, Detail: "SEARCH a USING INDEX ix (actor_id=?)"},
		{ID: 12, Parent: 9, Detail: "USE TEMP B-TREE FOR ORDER BY"},
		{ID: 20, Parent: 0, Detail: "USE TEMP B-TREE FOR ORDER BY"},
	}
	got, err := SQLitePlan(rows, ExplainText)
	if err != nil {
		t.Fatal(err)
	}
	want := "SCAN u\n" +
		"CORRELATED SCALAR SUBQUERY 1\n" +
		"  SEARCH a USING INDEX ix (actor_id=?)\n" +
		"    USE TEMP B-TREE FOR ORDER BY\n" +
		"USE TEMP B-TREE FOR ORDER BY"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// A parent id that never appeared (engine output we do not model)
// renders at the top level rather than being dropped or panicking.
func TestSQLitePlanTextUnknownParent(t *testing.T) {
	got, err := SQLitePlan([]SQLitePlanRow{{ID: 3, Parent: 42, Detail: "SCAN x"}}, ExplainText)
	if err != nil {
		t.Fatal(err)
	}
	if got != "SCAN x" {
		t.Errorf("got %q", got)
	}
}

func TestSQLitePlanJSON(t *testing.T) {
	rows := []SQLitePlanRow{
		{ID: 2, Parent: 0, Detail: "SCAN u"},
		{ID: 4, Parent: 2, Detail: `SEARCH "ユーザー" USING INDEX`},
	}
	got, err := SQLitePlan(rows, ExplainJSON)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":2,"parent":0,"detail":"SCAN u"},{"id":4,"parent":2,"detail":"SEARCH \"ユーザー\" USING INDEX"}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	var back []SQLitePlanRow
	if err := json.Unmarshal([]byte(got), &back); err != nil || len(back) != 2 || back[1].Detail != rows[1].Detail {
		t.Errorf("round-trip: %v %+v", err, back)
	}
	// Empty plan is an empty JSON array, never "null".
	empty, err := SQLitePlan(nil, ExplainJSON)
	if err != nil || empty != "[]" {
		t.Errorf("empty: %q %v", empty, err)
	}
	if _, err := SQLitePlan(rows, ExplainFormat(3)); !errors.Is(err, ErrExplainUnsupported) {
		t.Errorf("unknown format: %v", err)
	}
}
