package sqlite

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

func parse(t *testing.T, sqlText string) dialect.Tree {
	t.Helper()
	tree, err := Frontend{}.Parse(sqlText)
	if err != nil {
		t.Fatalf("parse %q: %v", sqlText, err)
	}
	return tree
}

func TestParse_KindAndStmtCount(t *testing.T) {
	tests := []struct {
		sql  string
		kind dialect.StmtKind
	}{
		{"SELECT 1", dialect.StmtSelect},
		{"UPDATE t SET a = 1", dialect.StmtUpdate},
		{"INSERT INTO t (a) VALUES (1)", dialect.StmtInsert},
		{"DELETE FROM t WHERE a = 1", dialect.StmtDelete},
	}
	for _, tt := range tests {
		tree := parse(t, tt.sql)
		if tree.StmtCount() != 1 || tree.Kind() != tt.kind {
			t.Errorf("%q: count=%d kind=%v", tt.sql, tree.StmtCount(), tree.Kind())
		}
	}
	if _, err := (Frontend{}).Parse("SELECT 1 FROM"); err == nil {
		t.Error("broken SQL must not parse")
	}
}

func TestParse_ErrorPosition(t *testing.T) {
	src := "SELECT u.id FROM users WHERE AND"
	_, err := Frontend{}.Parse(src)
	if err == nil {
		t.Fatal("expected parse error")
	}
	pe, ok := err.(*dialect.ParseError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if pe.Pos <= 0 || pe.Pos > len(src) {
		t.Errorf("Pos = %d, want inside the statement", pe.Pos)
	}
}

func TestRelations_JoinTypesAndLocations(t *testing.T) {
	sql := "SELECT u.id FROM users AS u LEFT JOIN orgs AS o ON o.id = u.org_id JOIN teams ON teams.id = u.team_id"
	rels := parse(t, sql).Relations()
	if len(rels) != 3 {
		t.Fatalf("relations = %+v", rels)
	}
	if rels[0].Table != "users" || rels[0].Alias != "u" || rels[0].Join != dialect.JoinBase || rels[0].NullableSide {
		t.Errorf("rel[0] = %+v", rels[0])
	}
	if rels[1].Table != "orgs" || rels[1].Alias != "o" || rels[1].Join != dialect.JoinLeft || !rels[1].NullableSide {
		t.Errorf("rel[1] = %+v", rels[1])
	}
	if rels[2].Table != "teams" || rels[2].Join != dialect.JoinInner || rels[2].NullableSide {
		t.Errorf("rel[2] = %+v", rels[2])
	}
	for i, want := range []string{"users", "orgs", "teams"} {
		if rels[i].Loc < 0 || !strings.HasPrefix(sql[rels[i].Loc:], want) {
			t.Errorf("rel[%d].Loc = %d, does not point at %q", i, rels[i].Loc, want)
		}
	}
}

func TestRelations_DerivedAndUpdateInsertDelete(t *testing.T) {
	rels := parse(t, "SELECT 1 FROM t1 JOIN (SELECT id FROM inner_t) AS d ON d.id = t1.id").Relations()
	if len(rels) != 2 || rels[1].Alias != "d" || rels[1].Table != "" {
		t.Fatalf("derived rels = %+v", rels)
	}
	rels = parse(t, "UPDATE users SET a = 1 WHERE id = ?").Relations()
	if len(rels) != 1 || rels[0].Table != "users" {
		t.Fatalf("update rels = %+v", rels)
	}
	rels = parse(t, "INSERT INTO users (a) VALUES (?)").Relations()
	if len(rels) != 1 || rels[0].Table != "users" {
		t.Fatalf("insert rels = %+v", rels)
	}
	rels = parse(t, "DELETE FROM users WHERE id = ?").Relations()
	if len(rels) != 1 || rels[0].Table != "users" {
		t.Fatalf("delete rels = %+v", rels)
	}
}

func TestColumnRefs_SubqueryMarkingAndNamePositions(t *testing.T) {
	sql := "SELECT u.id FROM users AS u WHERE u.status = ? AND EXISTS (SELECT 1 FROM x WHERE x.uid = u.id)"
	refs := parse(t, sql).ColumnRefs()
	var top, sub int
	for _, r := range refs {
		if r.InSubquery {
			sub++
		} else {
			top++
			if r.Loc < 0 || r.Loc >= len(sql) {
				t.Errorf("top-level ref %v has bad loc %d", r.Fields, r.Loc)
			}
		}
	}
	// u.id (proj), u.status (where) top-level; x.uid, u.id inside EXISTS.
	if top != 2 || sub != 2 {
		t.Errorf("top=%d sub=%d, refs=%+v", top, sub, refs)
	}

	// Identifiers in name positions must not read as column refs:
	// table name, alias, function name.
	refs = parse(t, "SELECT count(status) AS n FROM users AS u").ColumnRefs()
	if len(refs) != 1 || refs[0].Fields[0] != "status" {
		t.Errorf("name positions leaked into refs: %+v", refs)
	}
}

func TestTargetItems(t *testing.T) {
	sql := "SELECT u.id, count(*) AS n, u.* FROM users AS u GROUP BY u.id"
	items := parse(t, sql).TargetItems()
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Name != "" || items[0].Star {
		t.Errorf("item[0] = %+v", items[0])
	}
	if items[1].Name != "n" || items[1].FuncName != "count" {
		t.Errorf("item[1] = %+v", items[1])
	}
	if !items[2].Star || items[2].Qualifier != "u" {
		t.Errorf("item[2] = %+v", items[2])
	}
}

func TestTopConjunctLocs(t *testing.T) {
	sql := "SELECT 1 FROM t WHERE 1 AND (t.a = ? OR t.b = ?) AND t.c = ?"
	locs := parse(t, sql).TopConjunctLocs()
	if len(locs) != 3 {
		t.Fatalf("conjuncts = %v", locs)
	}
	orStart := strings.Index(sql, "(t.a")
	orEnd := strings.Index(sql, ") AND t.c")
	if locs[1] < orStart || locs[1] > orEnd {
		t.Errorf("conjunct[1] loc %d outside the OR group [%d,%d]", locs[1], orStart, orEnd)
	}
}

func TestHavingConjunctLocs(t *testing.T) {
	sql := "SELECT t.a FROM t WHERE t.x = ? GROUP BY t.a HAVING 1 AND (count(*) > ? OR sum(t.b) > ?) AND t.a > ?"
	tree := parse(t, sql)
	locs := tree.HavingConjunctLocs()
	if len(locs) != 3 {
		t.Fatalf("having conjuncts = %v", locs)
	}
	havingStart := strings.Index(sql, "HAVING")
	for i, loc := range locs {
		if loc < havingStart {
			t.Errorf("having conjunct[%d] loc %d before HAVING at %d", i, loc, havingStart)
		}
	}
	if wl := tree.TopConjunctLocs(); len(wl) != 1 || wl[0] >= havingStart {
		t.Errorf("where conjuncts = %v", wl)
	}
	if locs := parse(t, "SELECT 1 FROM t").HavingConjunctLocs(); len(locs) != 0 {
		t.Errorf("no-HAVING statement: %v", locs)
	}
}

func TestOrderByLocs(t *testing.T) {
	sql := "SELECT 1 FROM t ORDER BY t.a DESC, t.b"
	locs := parse(t, sql).OrderByLocs()
	if len(locs) != 2 {
		t.Fatalf("order locs = %v", locs)
	}
	if !strings.HasPrefix(sql[locs[0]:], "t.a") || !strings.HasPrefix(sql[locs[1]:], "t.b") {
		t.Errorf("locs %v do not point at sort keys", locs)
	}
}

func TestFlags(t *testing.T) {
	tree := parse(t, "SELECT DISTINCT a FROM t")
	if tree.HasDistinctOn() || tree.HasLockingClause() || tree.HasFetchWithTies() {
		t.Error("SQLite has none of DISTINCT ON / locking clauses / WITH TIES")
	}
}

func TestProbes(t *testing.T) {
	f := Frontend{}
	if err := f.ProbeExpr("t.a = ? AND t.b > 1"); err != nil {
		t.Errorf("valid expr rejected: %v", err)
	}
	for _, bad := range []string{"1; DELETE FROM t", "t.a = 1 ORDER BY t.b"} {
		if err := f.ProbeExpr(bad); err == nil {
			t.Errorf("ProbeExpr(%q) accepted", bad)
		}
	}

	if err := f.ProbeJoinItem("JOIN orgs AS o ON o.id = u.org_id"); err != nil {
		t.Errorf("valid join rejected: %v", err)
	}
	for _, bad := range []string{"orgs", "JOIN o ON o.id = u.id WHERE 1 = 1", "JOIN a ON 1=1 JOIN b ON 2=2"} {
		if err := f.ProbeJoinItem(bad); err == nil {
			t.Errorf("ProbeJoinItem(%q) accepted", bad)
		}
	}

	if err := f.ProbeOrderBy("ORDER BY t.a DESC, t.b"); err != nil {
		t.Errorf("valid order by rejected: %v", err)
	}
	if err := f.ProbeOrderBy("ORDER BY t.a LIMIT 1"); err == nil {
		t.Error("ORDER BY with trailing LIMIT accepted")
	}

	if err := f.ProbeOrderByKey("t.created_at"); err != nil {
		t.Errorf("valid sort key rejected: %v", err)
	}
	if err := f.ProbeOrderByKey("t.a, t.b"); err == nil {
		t.Error("two sort keys accepted as one")
	}

	if err := f.ProbeGroupBy("GROUP BY t.a, t.b"); err != nil {
		t.Errorf("valid group by rejected: %v", err)
	}
	if err := f.ProbeGroupBy("GROUP BY t.a HAVING count(*) > 1"); err == nil {
		t.Error("GROUP BY with HAVING accepted")
	}

	if err := f.ProbeSetItem("email = ?"); err != nil {
		t.Errorf("valid set item rejected: %v", err)
	}
	if err := f.ProbeSetItem("email = ?, nickname = ?"); err == nil {
		t.Error("two set items accepted as one")
	}

	if err := f.ProbeInsertValue("?"); err != nil {
		t.Errorf("valid insert value rejected: %v", err)
	}
	if err := f.ProbeInsertValue("DEFAULT"); err == nil {
		t.Error("SQLite has no per-item DEFAULT; it must be rejected")
	}
	if err := f.ProbeInsertValue("?), (?"); err == nil {
		t.Error("row-splitting insert value accepted")
	}
}

func TestProvenanceFacades(t *testing.T) {
	tr := parse(t, "WITH s AS (SELECT a FROM t2) SELECT s.a, d.b FROM s LEFT JOIN (SELECT b FROM t3) AS d ON TRUE")
	ctes := tr.CTEs()
	if len(ctes) != 1 || ctes[0].Name != "s" || ctes[0].Recursive || ctes[0].Tree == nil {
		t.Fatalf("CTEs = %+v", ctes)
	}
	if rels := ctes[0].Tree.Relations(); len(rels) != 1 || rels[0].Table != "t2" {
		t.Errorf("cte body relations = %+v, want t2", rels)
	}
	dr := tr.DerivedRels()
	if len(dr) != 1 || dr[0].Alias != "d" || !dr[0].NullableSide || dr[0].Tree == nil {
		t.Fatalf("DerivedRels = %+v", dr)
	}
	if rels := dr[0].Tree.Relations(); len(rels) != 1 || rels[0].Table != "t3" {
		t.Errorf("derived body relations = %+v, want t3", rels)
	}
	rec := parse(t, "WITH RECURSIVE r AS (SELECT 1 UNION ALL SELECT n+1 FROM r) SELECT n FROM r").CTEs()
	if len(rec) != 1 || !rec[0].Recursive || rec[0].Tree == nil {
		t.Errorf("recursive CTE = %+v", rec)
	}
	if !parse(t, "SELECT a FROM t1 UNION ALL SELECT b FROM t2").HasSetOperation() {
		t.Error("compound select not reported")
	}
	// Column-origin attribution carries no database qualifier:
	// attached-database references anywhere distrust everything.
	for _, sql := range []string{
		"SELECT t1.a FROM aux.t1",
		"SELECT t1.a FROM t1 WHERE EXISTS (SELECT 1 FROM aux.t2)",
	} {
		if !parse(t, sql).HasUnresolvableProvenance() {
			t.Errorf("HasUnresolvableProvenance(%q) = false, want true", sql)
		}
	}
	if parse(t, "SELECT t1.a, t2.b FROM t1 LEFT JOIN t2 ON t2.a = t1.a").HasUnresolvableProvenance() {
		t.Error("unqualified statement reported unresolvable")
	}
	if parse(t, "SELECT a FROM t").HasGroupingSets() {
		t.Error("SQLite has no grouping sets; must always be false")
	}
}

func TestPrecisionFacades(t *testing.T) {
	tr := parse(t, "SELECT sum(u.org_id) AS s, coalesce(u.nickname, 'anon') AS c, sum(u.id) FILTER (WHERE u.id > 3) AS sf FROM users AS u WHERE u.org_id IS NOT NULL AND (u.bio IS NOT NULL OR u.id = 1) GROUP BY u.status")
	if !tr.HasGroupBy() {
		t.Error("HasGroupBy = false")
	}
	items := tr.TargetItems()
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	if got := items[0].AggArg; len(got) != 2 || got[0] != "u" || got[1] != "org_id" {
		t.Errorf("sum AggArg = %v", got)
	}
	if !items[1].Total {
		t.Error("coalesce with a literal fallback must be Total")
	}
	if items[2].AggArg != nil {
		t.Error("FILTER clause must clear AggArg")
	}
	nn := tr.NotNullConjuncts()
	if len(nn) != 1 || len(nn[0].Fields) != 2 || nn[0].Fields[1] != "org_id" {
		t.Errorf("NotNullConjuncts = %+v, want only the depth-0 u.org_id", nn)
	}
	if parse(t, "SELECT a FROM t").HasGroupBy() {
		t.Error("HasGroupBy without GROUP BY")
	}
}

func TestRelations_SchemaQualifier(t *testing.T) {
	rels := parse(t, "SELECT t1.a FROM aux.t1").Relations()
	if len(rels) != 1 || rels[0].Table != "t1" || rels[0].Schema != "aux" {
		t.Fatalf("relations = %+v, want schema-qualified t1", rels)
	}
}
