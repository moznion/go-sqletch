package mysql

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

func parse(t *testing.T, sql string) dialect.Tree {
	t.Helper()
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
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
	_, err := Frontend{}.Parse("SELECT u.id FROM users WHERE AND")
	if err == nil {
		t.Fatal("expected parse error")
	}
	var pe *dialect.ParseError
	if !asParseErr(err, &pe) {
		t.Fatalf("error type = %T", err)
	}
	if pe.Pos <= 0 || pe.Pos >= len("SELECT u.id FROM users WHERE AND") {
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
	// Locations point at the relation name tokens.
	for i, want := range []string{"users", "orgs", "teams"} {
		if rels[i].Loc < 0 || !strings.HasPrefix(sql[rels[i].Loc:], want) {
			t.Errorf("rel[%d].Loc = %d, does not point at %q", i, rels[i].Loc, want)
		}
	}
}

func TestRelations_LocatorSkipsColumnRefs(t *testing.T) {
	// A bare column named like a later table must not steal its location.
	sql := "SELECT 1 FROM t1 JOIN t2 ON t1.x = flag JOIN flag ON flag.id = t2.y"
	rels := parse(t, sql).Relations()
	if len(rels) != 3 || rels[2].Table != "flag" {
		t.Fatalf("relations = %+v", rels)
	}
	wantLoc := strings.Index(sql, "JOIN flag") + len("JOIN ")
	if rels[2].Loc != wantLoc {
		t.Errorf("flag located at %d, want %d (the FROM-position occurrence)", rels[2].Loc, wantLoc)
	}
}

func TestRelations_BacktickAndSubquery(t *testing.T) {
	sql := "SELECT 1 FROM `wei``rd` JOIN (SELECT id FROM inner_t) AS d ON d.id = `wei``rd`.id"
	rels := parse(t, sql).Relations()
	if len(rels) != 2 {
		t.Fatalf("relations = %+v", rels)
	}
	if rels[0].Table != "wei`rd" || rels[0].Loc != len("SELECT 1 FROM ") {
		t.Errorf("rel[0] = %+v", rels[0])
	}
	if rels[1].Alias != "d" || rels[1].Table != "" {
		t.Errorf("derived table rel = %+v", rels[1])
	}
}

func TestRelations_UpdateInsertDelete(t *testing.T) {
	rels := parse(t, "UPDATE users SET a = 1 WHERE id = ?").Relations()
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

func TestColumnRefs_SubqueryMarking(t *testing.T) {
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

// TestTargetItems_SchemaQualifiedFuncNotWhitelisted is the H2-mysql
// regression: a schema-qualified user function must NOT surface a
// builtin FuncName. The nullability analyzer keys funcWhitelist
// (count/now) and strictAggs (sum/min/max/avg) on TargetItem.FuncName;
// a stored function `mydb.count(x)` returning NULL is only callable
// qualified, so if FuncName came back "count" it would be treated as
// the total builtin and its column generated non-nullable — a NULL
// slipping into a non-null field. A schema-qualified call must leave
// FuncName blank so no whitelist/strictAggs entry can match.
func TestTargetItems_SchemaQualifiedFuncNotWhitelisted(t *testing.T) {
	qualified := []struct{ sql, name string }{
		{"SELECT mydb.count(t.x) AS c FROM t", "count"},
		{"SELECT mydb.now() AS c FROM t", "now"},
		{"SELECT mydb.sum(t.id) AS c FROM t GROUP BY t.id", "sum"},
	}
	for _, tt := range qualified {
		item := parse(t, tt.sql).TargetItems()[0]
		if item.FuncName != "" {
			t.Errorf("%q: FuncName = %q, want \"\" (schema-qualified %s must not impersonate the builtin)",
				tt.sql, item.FuncName, tt.name)
		}
	}
	// Control: unqualified builtin calls still surface their FuncName so
	// the whitelist/strictAggs keep working.
	unqualified := []struct{ sql, name string }{
		{"SELECT count(t.x) AS c FROM t", "count"},
		{"SELECT now() AS c FROM t", "now"},
		{"SELECT sum(t.id) AS c FROM t GROUP BY t.id", "sum"},
	}
	for _, tt := range unqualified {
		item := parse(t, tt.sql).TargetItems()[0]
		if item.FuncName != tt.name {
			t.Errorf("%q: FuncName = %q, want %q", tt.sql, item.FuncName, tt.name)
		}
	}
}

func TestTopConjunctLocs(t *testing.T) {
	sql := "SELECT 1 FROM t WHERE TRUE AND (t.a = ? OR t.b = ?) AND t.c = ?"
	locs := parse(t, sql).TopConjunctLocs()
	if len(locs) != 3 {
		t.Fatalf("conjuncts = %v", locs)
	}
	// The OR group is one conjunct; its loc must sit inside the parens.
	orStart := strings.Index(sql, "(t.a")
	orEnd := strings.Index(sql, ") AND t.c")
	if locs[1] < orStart || locs[1] > orEnd {
		t.Errorf("conjunct[1] loc %d outside the OR group [%d,%d]", locs[1], orStart, orEnd)
	}
}

func TestHavingConjunctLocs(t *testing.T) {
	sql := "SELECT t.a FROM t WHERE t.x = ? GROUP BY t.a HAVING TRUE AND (count(*) > ? OR sum(t.b) > ?) AND t.a > ?"
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
	// WHERE conjuncts stay separate.
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
	if parse(t, "SELECT DISTINCT a FROM t").HasDistinctOn() {
		t.Error("MySQL has no DISTINCT ON; plain DISTINCT must not trip it")
	}
	if !parse(t, "SELECT a FROM t FOR UPDATE").HasLockingClause() {
		t.Error("FOR UPDATE must report a locking clause")
	}
	if parse(t, "SELECT a FROM t").HasLockingClause() {
		t.Error("no locking clause expected")
	}
	if parse(t, "SELECT a FROM t LIMIT 1").HasFetchWithTies() {
		t.Error("MySQL has no WITH TIES")
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
	if err := f.ProbeInsertValue("DEFAULT"); err != nil {
		t.Errorf("DEFAULT insert value rejected: %v", err)
	}
	if err := f.ProbeInsertValue("?), (?"); err == nil {
		t.Error("row-splitting insert value accepted")
	}
}

func asParseErr(err error, out **dialect.ParseError) bool {
	pe, ok := err.(*dialect.ParseError)
	if ok {
		*out = pe
	}
	return ok
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
	rec := parse(t, "WITH RECURSIVE r AS (SELECT 1 AS n UNION ALL SELECT n+1 FROM r) SELECT n FROM r").CTEs()
	if len(rec) != 1 || !rec[0].Recursive || rec[0].Tree == nil {
		t.Errorf("recursive CTE = %+v", rec)
	}
	if !rec[0].Tree.HasSetOperation() {
		t.Error("recursive CTE body's set operation not reported by the sub-facade")
	}
	if parse(t, "SELECT t1.a, t2.b FROM t1 LEFT JOIN t2 ON t2.a = t1.a").HasUnresolvableProvenance() {
		t.Error("unqualified statement reported unresolvable")
	}
	// org_table carries no database qualifier: cross-database
	// references anywhere (subqueries included) distrust everything.
	for _, sql := range []string{
		"SELECT t1.a FROM otherdb.t1",
		"SELECT t1.a, t2.b FROM t1 LEFT JOIN otherdb.t2 ON t2.a = t1.a",
		"SELECT t1.a FROM t1 WHERE EXISTS (SELECT 1 FROM otherdb.t2)",
	} {
		if !parse(t, sql).HasUnresolvableProvenance() {
			t.Errorf("HasUnresolvableProvenance(%q) = false, want true", sql)
		}
	}
}

func TestHasGroupingSets(t *testing.T) {
	if !parse(t, "SELECT a, count(*) FROM t GROUP BY a WITH ROLLUP").HasGroupingSets() {
		t.Error("WITH ROLLUP not reported")
	}
	if parse(t, "SELECT a, count(*) FROM t GROUP BY a").HasGroupingSets() {
		t.Error("plain GROUP BY reported as grouping sets")
	}
}

func TestPrecisionFacades(t *testing.T) {
	tr := parse(t, "SELECT sum(u.org_id) AS s, count(*) AS n, coalesce(u.nickname, 'anon') AS c, u.org_id IS NOT NULL AS p FROM users AS u WHERE u.org_id IS NOT NULL AND (u.bio IS NOT NULL OR u.id = 1) GROUP BY u.status")
	if !tr.HasGroupBy() {
		t.Error("HasGroupBy = false")
	}
	items := tr.TargetItems()
	if len(items) != 4 {
		t.Fatalf("items = %+v", items)
	}
	if got := items[0].AggArg; len(got) != 2 || got[0] != "u" || got[1] != "org_id" {
		t.Errorf("sum AggArg = %v", got)
	}
	if items[0].Total || items[1].Total {
		t.Error("aggregates must not be Total")
	}
	if !items[2].Total {
		t.Error("coalesce with a literal fallback must be Total")
	}
	if !items[3].Total {
		t.Error("IS NOT NULL test must be Total")
	}
	nn := tr.NotNullConjuncts()
	if len(nn) != 1 || len(nn[0].Fields) != 2 || nn[0].Fields[1] != "org_id" {
		t.Errorf("NotNullConjuncts = %+v, want only the depth-0 u.org_id", nn)
	}
	if parse(t, "SELECT a FROM t").HasGroupBy() {
		t.Error("HasGroupBy without GROUP BY")
	}
	// CAST returns NULL on conversion failure in MySQL — never total.
	if parse(t, "SELECT CAST('x' AS SIGNED) AS c FROM t").TargetItems()[0].Total {
		t.Error("MySQL CAST must not be Total")
	}
}

func TestRelations_SchemaQualifier(t *testing.T) {
	rels := parse(t, "SELECT t1.a FROM otherdb.t1").Relations()
	if len(rels) != 1 || rels[0].Table != "t1" || rels[0].Schema != "otherdb" {
		t.Fatalf("relations = %+v, want db-qualified t1", rels)
	}
}
