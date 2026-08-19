package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

func TestHavingConjunctLocs(t *testing.T) {
	fe := Frontend{}
	sql := "SELECT t.a FROM t WHERE t.x = $1 GROUP BY t.a HAVING TRUE AND (count(*) > $2 OR sum(t.b) > $3) AND t.a > $4"
	tree, err := fe.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	locs := tree.HavingConjunctLocs()
	if len(locs) != 3 {
		t.Fatalf("having conjuncts = %v", locs)
	}
	havingStart := strings.Index(sql, "HAVING")
	for i, loc := range locs {
		if loc < havingStart {
			t.Errorf("having conjunct[%d] loc %d before HAVING", i, loc)
		}
	}
	if wl := tree.TopConjunctLocs(); len(wl) != 1 || wl[0] >= havingStart {
		t.Errorf("where conjuncts = %v", wl)
	}
	noHaving, err := fe.Parse("SELECT 1 FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if locs := noHaving.HavingConjunctLocs(); len(locs) != 0 {
		t.Errorf("no-HAVING statement: %v", locs)
	}
}

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

func mustParse(t *testing.T, sql string) dialect.Tree {
	t.Helper()
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestHasOpaqueProvenance(t *testing.T) {
	opaque := []string{
		"SELECT s.a FROM t1 LEFT JOIN (SELECT a FROM t2) s ON s.a = t1.a",
		"SELECT s.a FROM (SELECT a FROM t2) s",
		"WITH s AS (SELECT a FROM t2) SELECT s.a FROM s",
		"SELECT a FROM t1 UNION ALL SELECT b FROM t2",
		"UPDATE t SET a = s.a FROM (SELECT a FROM t2) s RETURNING t.a",
		"WITH s AS (SELECT a FROM t2) DELETE FROM t USING s WHERE t.a = s.a RETURNING t.a",
	}
	for _, sql := range opaque {
		if !mustParse(t, sql).HasOpaqueProvenance() {
			t.Errorf("HasOpaqueProvenance(%q) = false, want true", sql)
		}
	}
	transparent := []string{
		"SELECT t1.a, t2.b FROM t1 LEFT JOIN t2 ON t2.a = t1.a",
		"SELECT t1.a FROM t1 TABLESAMPLE SYSTEM (50)",
		// A sublink is not FROM-reachable: its columns carry no origin.
		"SELECT t1.a FROM t1 WHERE EXISTS (SELECT 1 FROM (SELECT a FROM t2) s)",
		"INSERT INTO t (a) SELECT a FROM (SELECT a FROM t2) s RETURNING t.a",
		"UPDATE t SET a = $1 RETURNING t.a",
	}
	for _, sql := range transparent {
		if mustParse(t, sql).HasOpaqueProvenance() {
			t.Errorf("HasOpaqueProvenance(%q) = true, want false", sql)
		}
	}
}

func TestHasGroupingSets(t *testing.T) {
	grouping := []string{
		"SELECT a, count(*) FROM t GROUP BY ROLLUP(a)",
		"SELECT a, count(*) FROM t GROUP BY CUBE(a)",
		"SELECT a, count(*) FROM t GROUP BY GROUPING SETS ((a), ())",
	}
	for _, sql := range grouping {
		if !mustParse(t, sql).HasGroupingSets() {
			t.Errorf("HasGroupingSets(%q) = false, want true", sql)
		}
	}
	if mustParse(t, "SELECT a, count(*) FROM t GROUP BY a").HasGroupingSets() {
		t.Error("plain GROUP BY reported as grouping sets")
	}
}

func TestRelations_SchemaAndTablesample(t *testing.T) {
	rels := mustParse(t, "SELECT * FROM aux.t1 LEFT JOIN t2 TABLESAMPLE SYSTEM (10) ON TRUE").Relations()
	if len(rels) != 2 {
		t.Fatalf("relations = %+v", rels)
	}
	if rels[0].Table != "t1" || rels[0].Schema != "aux" {
		t.Errorf("rels[0] = %+v, want schema-qualified t1", rels[0])
	}
	if rels[1].Table != "t2" || rels[1].Schema != "" || !rels[1].NullableSide {
		t.Errorf("rels[1] = %+v, want null-extended t2 through TABLESAMPLE", rels[1])
	}
}
