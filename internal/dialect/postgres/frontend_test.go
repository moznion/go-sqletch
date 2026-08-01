package postgres

import (
	"errors"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

func TestFrontend_Parse_Basics(t *testing.T) {
	fe := Frontend{}
	tree, err := fe.Parse("SELECT u.id FROM users AS u JOIN orgs AS o ON o.id = u.org_id WHERE TRUE AND (u.a = $1) ORDER BY u.id")
	if err != nil {
		t.Fatal(err)
	}
	if tree.StmtCount() != 1 || tree.Kind() != dialect.StmtSelect {
		t.Fatalf("stmts=%d kind=%v", tree.StmtCount(), tree.Kind())
	}
	rels := tree.Relations()
	if len(rels) != 2 {
		t.Fatalf("relations = %+v, want 2", rels)
	}
	if rels[0].Alias != "u" || rels[0].Table != "users" || rels[0].Join != dialect.JoinBase {
		t.Errorf("rels[0] = %+v", rels[0])
	}
	if rels[1].Alias != "o" || rels[1].Table != "orgs" || rels[1].Join != dialect.JoinInner {
		t.Errorf("rels[1] = %+v", rels[1])
	}
	if got := tree.TopConjunctLocs(); len(got) != 2 {
		t.Errorf("conjuncts = %v, want 2 (TRUE and the paren expr)", got)
	}
	if got := tree.OrderByLocs(); len(got) != 1 {
		t.Errorf("order by locs = %v, want 1", got)
	}
}

func TestFrontend_JoinTypes(t *testing.T) {
	fe := Frontend{}
	tests := []struct {
		sql  string
		want dialect.JoinType
	}{
		{"SELECT 1 FROM a JOIN b ON true", dialect.JoinInner},
		{"SELECT 1 FROM a INNER JOIN b ON true", dialect.JoinInner},
		{"SELECT 1 FROM a LEFT JOIN b ON true", dialect.JoinLeft},
		{"SELECT 1 FROM a LEFT OUTER JOIN b ON true", dialect.JoinLeft},
		{"SELECT 1 FROM a RIGHT JOIN b ON true", dialect.JoinRight},
		{"SELECT 1 FROM a FULL JOIN b ON true", dialect.JoinFull},
		{"SELECT 1 FROM a CROSS JOIN b", dialect.JoinCross},
	}
	for _, tt := range tests {
		tree, err := fe.Parse(tt.sql)
		if err != nil {
			t.Fatalf("%s: %v", tt.sql, err)
		}
		rels := tree.Relations()
		if len(rels) != 2 || rels[1].Join != tt.want {
			t.Errorf("%s: rels = %+v, want second %v", tt.sql, rels, tt.want)
		}
	}
}

func TestFrontend_ParseError_Position(t *testing.T) {
	fe := Frontend{}
	_, err := fe.Parse("SELECT FROM WHERE")
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *dialect.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T", err)
	}
	if pe.Pos < 0 || pe.Pos >= len("SELECT FROM WHERE") {
		t.Errorf("pos = %d out of range", pe.Pos)
	}
}

func TestFrontend_DistinctOnAndLocking(t *testing.T) {
	fe := Frontend{}
	tree, _ := fe.Parse("SELECT DISTINCT ON (a) a, b FROM t ORDER BY a")
	if !tree.HasDistinctOn() {
		t.Error("DISTINCT ON not detected")
	}
	tree, _ = fe.Parse("SELECT DISTINCT a FROM t")
	if tree.HasDistinctOn() {
		t.Error("plain DISTINCT misdetected as DISTINCT ON")
	}
	tree, _ = fe.Parse("SELECT a FROM t FOR UPDATE")
	if !tree.HasLockingClause() {
		t.Error("FOR UPDATE not detected")
	}
}

func TestFrontend_ProbeExpr(t *testing.T) {
	fe := Frontend{}
	valid := []string{
		"u.status = $1",
		"u.email LIKE $1 || '%'",
		"u.a = 1 OR u.b = 2", // one node thanks to mandatory parens
		"EXISTS (SELECT 1 FROM x WHERE x.id = u.id)",
		"u.deleted_at IS NULL",
		"u.tags @> $1",
	}
	for _, expr := range valid {
		if err := fe.ProbeExpr(expr); err != nil {
			t.Errorf("ProbeExpr(%q) = %v, want nil", expr, err)
		}
	}
	invalid := []string{
		"u.a = 1 AND",           // incomplete
		"u.a = 1; DROP TABLE t", // second statement
		"u.a = 1 GROUP BY u.a",  // trailing clause: syntax error inside the wrapping parens
		"ORDER BY u.a",          // not an expression
		"",                      // empty
	}
	for _, expr := range invalid {
		if err := fe.ProbeExpr(expr); err == nil {
			t.Errorf("ProbeExpr(%q) = nil, want error", expr)
		}
	}
}

func TestFrontend_ProbeJoinItem(t *testing.T) {
	fe := Frontend{}
	valid := []string{
		"JOIN orgs AS o ON o.id = sqletch_probe_t.id",
		"LEFT JOIN x ON true",
		"JOIN a ON true JOIN b ON true", // left-deep chain: accepted (design 02 §4)
	}
	for _, item := range valid {
		if err := fe.ProbeJoinItem(item); err != nil {
			t.Errorf("ProbeJoinItem(%q) = %v, want nil", item, err)
		}
	}
	invalid := []string{
		"JOIN orgs ON true WHERE 1 = 1", // trailing clause
		", extra_table",                 // comma-joined second FROM item
		"orgs",                          // not a join
		"JOIN orgs ON true; SELECT 1",   // second statement
	}
	for _, item := range invalid {
		if err := fe.ProbeJoinItem(item); err == nil {
			t.Errorf("ProbeJoinItem(%q) = nil, want error", item)
		}
	}
}

func TestFrontend_ProbeOrderBy(t *testing.T) {
	fe := Frontend{}
	valid := []string{
		"ORDER BY u.created_at DESC",
		"ORDER BY u.email ASC, u.id ASC",
		"ORDER BY 1",
	}
	for _, c := range valid {
		if err := fe.ProbeOrderBy(c); err != nil {
			t.Errorf("ProbeOrderBy(%q) = %v, want nil", c, err)
		}
	}
	invalid := []string{
		"ORDER BY u.a LIMIT 5",
		"GROUP BY u.a",
		"ORDER BY u.a FOR UPDATE",
		"",
	}
	for _, c := range invalid {
		if err := fe.ProbeOrderBy(c); err == nil {
			t.Errorf("ProbeOrderBy(%q) = nil, want error", c)
		}
	}
}
