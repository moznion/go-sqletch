//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"testing"
	"time"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	"github.com/jackc/pgx/v5"
	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/nullability"
)

// TestNullabilitySoundnessAdversarial is the deterministic soundness
// oracle for the nullability analyzer (design 05 §1): if Analyze
// reports a column non-nullable, no execution may return NULL there.
//
// Each case pairs a template with seed data engineered to force NULL
// into the suspect column. The check is one-directional and exact:
// a NULL observed in a claimed-non-nullable column is a proven
// soundness violation (never a flake — schema, data, and queries are
// fixed). The reverse (nullable verdict, no NULL observed) is fine:
// false positives cost a pointer, not a panic.
//
// The adversarial cases target the seam between null-extension
// detection (name-based, over the parse tree's FROM list) and column
// provenance (OID-based, from the wire protocol's resorigtbl, which
// resolves THROUGH views, derived tables, and CTEs to base tables).
const nullSoundSchemaSQL = `
DROP SCHEMA IF EXISTS aux CASCADE;
CREATE SCHEMA aux;
CREATE TABLE orgs (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);
CREATE TABLE members (
    id     bigint PRIMARY KEY,
    email  text NOT NULL,
    org_id bigint
);
CREATE VIEW members_orgs AS
  SELECT m.id AS member_id, m.email, o.name AS org_name
  FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id;
CREATE TABLE aux.orgs (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);
CREATE TABLE inh_parent (x bigint NOT NULL);
CREATE TABLE inh_child () INHERITS (inh_parent);
ALTER TABLE inh_child ALTER COLUMN x DROP NOT NULL;
`

const nullSoundSeedSQL = `
INSERT INTO orgs VALUES (1, 'acme');
INSERT INTO members VALUES (1, 'a@example.com', 1), (2, 'b@example.com', NULL);
-- aux.orgs stays empty: every LEFT JOIN against it misses.
INSERT INTO inh_parent VALUES (1);
INSERT INTO inh_child VALUES (NULL);
`

var nullSoundCases = []struct {
	name string
	src  string
	note string
}{
	{
		name: "control_plain_left_join",
		src: `-- name: ControlLeftJoin :many
SELECT m.email, o.name AS org_name
FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id
ORDER BY m.id;
`,
		note: "sanity: direct LEFT JOIN must already be handled (org_name nullable)",
	},
	{
		name: "view_with_internal_left_join",
		src: `-- name: ViaView :many
SELECT v.member_id, v.email, v.org_name
FROM members_orgs AS v
ORDER BY v.member_id;
`,
		note: "provenance resolves through the view to orgs.name (NOT NULL); the view's internal LEFT JOIN is invisible to Relations()",
	},
	{
		name: "derived_table_on_null_side",
		src: `-- name: ViaDerived :many
SELECT m.email, s.name AS org_name
FROM members AS m LEFT JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "RangeSubselect yields RelRef{Table:\"\"} — never enters nullableSideOIDs; provenance resolves through to orgs.name",
	},
	{
		name: "cte_on_null_side",
		src: `-- name: ViaCTE :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT m.email, s.name AS org_name
FROM members AS m LEFT JOIN s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "CTE name resolves to no catalog table — never enters nullableSideOIDs; provenance may resolve through the CTE",
	},
	{
		name: "schema_qualified_name_collision",
		src: `-- name: ViaOtherSchema :many
SELECT m.email, o.name AS org_name
FROM members AS m LEFT JOIN aux.orgs AS o ON o.id = m.org_id
ORDER BY m.id;
`,
		note: "RelRef.Table drops the schema; Lookup(\"orgs\") prefers public.orgs, marking the wrong OID as null-extended",
	},
	{
		name: "group_by_rollup",
		src: `-- name: Rollup :many
SELECT m.email, count(*) AS n
FROM members AS m
GROUP BY ROLLUP(m.email)
ORDER BY m.email NULLS LAST;
`,
		note: "super-aggregate rows null the grouping column even though members.email is NOT NULL",
	},
	{
		name: "union_all_branch_provenance",
		src: `-- name: UnionBranches :many
SELECT m.id AS v FROM members AS m
UNION ALL
SELECT m2.org_id AS v FROM members AS m2
ORDER BY v NULLS LAST;
`,
		note: "empirical: does the wire protocol attribute UNION output to the first branch's members.id (NOT NULL)?",
	},
	// ---- precision-pack edges (design 05 §3a): each case is the
	// escape hatch of a narrowing rule; if the rule ever over-reaches,
	// the execution oracle catches the NULL. ----
	{
		name: "aggregate_without_group_by",
		src: `-- name: EmptySum :many
SELECT sum(m.id) AS s FROM members AS m WHERE m.id > 1000000;
`,
		note: "no GROUP BY: the empty input yields one NULL row — sum must stay nullable",
	},
	{
		name: "aggregate_with_filter_clause",
		src: `-- name: FilteredSum :many
SELECT m.email, sum(m.id) FILTER (WHERE m.id > 1000000) AS s
FROM members AS m GROUP BY m.email;
`,
		note: "FILTER empties every group's aggregated input — sum must stay nullable",
	},
	{
		name: "is_not_null_on_self_join_instance",
		src: `-- name: SelfJoinFilter :many
SELECT m2.org_id FROM members AS m1
JOIN members AS m2 ON m2.id = m1.id + 1
WHERE m1.org_id IS NOT NULL
ORDER BY m2.id;
`,
		note: "IS NOT NULL filters instance m1; the projected m2.org_id shares (SrcRel, SrcAtt) and can still be NULL",
	},
	{
		name: "is_not_null_narrows_left_join",
		src: `-- name: FilteredJoin :many
SELECT m.email, o.name AS org_name
FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id
WHERE o.name IS NOT NULL
ORDER BY m.id;
`,
		note: "positive: the skeleton IS NOT NULL narrows org_name past the null-extension — execution must agree",
	},
	{
		name: "grouped_strict_aggregate",
		src: `-- name: GroupedSum :many
SELECT m.email, sum(m.id) AS s, max(m.org_id) AS mo
FROM members AS m GROUP BY m.email
ORDER BY m.email;
`,
		note: "positive: sum over NOT NULL id narrows under GROUP BY; max(org_id) has a nullable argument and must not",
	},
	{
		name: "inheritance_parent_scan",
		src: `-- name: ParentScan :many
SELECT p.x FROM inh_parent AS p ORDER BY p.x NULLS LAST;
`,
		note: "a plain-inheritance child dropped the inherited NOT NULL; the parent scan includes its NULL row",
	},
	{
		name: "inheritance_only_scan",
		src: `-- name: ParentOnlyScan :many
SELECT p.x FROM ONLY inh_parent AS p ORDER BY p.x;
`,
		note: "positive: FROM ONLY excludes children — attnotnull holds and narrowing is allowed",
	},
	// ---- recursive provenance (design 05 §2b) ----
	{
		name: "derived_inner_join_narrows",
		src: `-- name: DerivedInner :many
SELECT m.email, s.name AS org_name
FROM members AS m JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "positive: recursion narrows a clean INNER-joined derived table — execution must agree",
	},
	{
		name: "derived_body_left_join",
		src: `-- name: DerivedBodyLeft :many
SELECT s.org_name FROM members AS m
JOIN (SELECT m2.id, o.name AS org_name
      FROM members AS m2 LEFT JOIN orgs AS o ON o.id = m2.org_id) AS s
  ON s.id = m.id
ORDER BY m.id;
`,
		note: "null extension INSIDE the derived body must survive the recursion",
	},
	{
		name: "union_in_derived_with_direct_instance",
		src: `-- name: UnionInDerived :many
SELECT o1.name AS n1, s.v FROM orgs AS o1
JOIN (SELECT o.id AS v FROM orgs AS o UNION ALL SELECT m.org_id FROM members AS m) AS s ON TRUE
ORDER BY o1.id, s.v NULLS LAST;
`,
		note: "empirical: sub-level set-op attribution — poisoning must cover whatever the engine reports",
	},
	{
		name: "recursive_cte",
		src: `-- name: RecCTE :many
WITH RECURSIVE r AS (
  SELECT m.id, m.org_id FROM members AS m
  UNION ALL
  SELECT r.id + 100, r.org_id FROM r WHERE r.id < 100
)
SELECT r.org_id FROM r ORDER BY r.org_id NULLS LAST;
`,
		note: "recursive CTE output must stay conservative",
	},
	{
		name: "cte_inner_join_narrows",
		src: `-- name: CTEInner :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT m.email, s.name AS org_name
FROM members AS m JOIN s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "positive: recursion narrows a clean INNER-joined CTE — execution must agree",
	},
	{
		name: "forward_cte_reference_base_table",
		src: `-- name: ForwardCTE :many
WITH a AS (
  SELECT o.name AS org_name
  FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id
),
orgs AS (SELECT 1 AS one)
SELECT a.org_name
FROM a, public.orgs AS po
WHERE po.id = 1
ORDER BY a.org_name NULLS LAST;
`,
		note: "forward reference: inside `a`, `orgs` binds to the base table (the CTE `orgs` is defined later), null-extended by the LEFT JOIN; analyzing it as the forward CTE drops that hazard and, with the clean public.orgs instance, would unsoundly narrow org_name",
	},
}

func TestNullabilitySoundnessAdversarial(t *testing.T) {
	conn, ctx := acquireWithSchema(t, nullSoundSchemaSQL)
	oracle := postgres.NewOracle(conn)
	if _, err := conn.Exec(ctx, nullSoundSeedSQL); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range nullSoundCases {
		t.Run(tc.name, func(t *testing.T) {
			q := compile(t, tc.src)
			rs, err := ast.Renderings(postgres.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			desc, err := oracle.Describe(ctx, rs[0].SQL)
			if err != nil {
				t.Fatalf("describe: %v", err)
			}
			tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
			if err != nil {
				t.Fatal(err)
			}
			verdict := nullability.Analyze(tree, rs[0], desc, cat, nil)

			for i, c := range desc.Columns {
				t.Logf("column %d %q: SrcRel=%d SrcAtt=%d -> nullable=%v",
					i, c.Name, c.SrcRel, c.SrcAtt, verdict[i])
			}

			// Execute and record, per column, whether any NULL appears.
			rows, err := conn.Query(ctx, rs[0].SQL)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			sawNull := make([]bool, len(desc.Columns))
			nRows := 0
			for rows.Next() {
				vals, err := rows.Values()
				if err != nil {
					t.Fatal(err)
				}
				nRows++
				for i, v := range vals {
					if v == nil {
						sawNull[i] = true
					}
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if nRows == 0 {
				t.Fatal("seed data produced no rows; the case proves nothing")
			}

			for i := range verdict {
				if !verdict[i] && sawNull[i] {
					t.Errorf("SOUNDNESS VIOLATION: column %d %q claimed non-nullable but execution returned NULL (%s)",
						i, desc.Columns[i].Name, tc.note)
				}
			}
		})
	}
}

// ---- MySQL ----------------------------------------------------------------

const mysqlNullSoundSchemaSQL = `
DROP VIEW IF EXISTS members_orgs;
DROP VIEW IF EXISTS members_rollup;
DROP VIEW IF EXISTS members_union;
DROP VIEW IF EXISTS members_empty_max;
CREATE TABLE orgs (
    id   BIGINT PRIMARY KEY,
    name VARCHAR(64) NOT NULL
);
CREATE TABLE members (
    id     BIGINT PRIMARY KEY,
    email  VARCHAR(255) NOT NULL,
    org_id BIGINT
);
CREATE VIEW members_orgs AS
  SELECT m.id AS member_id, m.email, o.name AS org_name
  FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id;
CREATE VIEW members_rollup AS
  SELECT m.email, count(*) AS n
  FROM members AS m GROUP BY m.email WITH ROLLUP;
CREATE VIEW members_union AS
  SELECT m.id AS v FROM members AS m
  UNION ALL
  SELECT m2.org_id FROM members AS m2;
CREATE VIEW members_empty_max AS
  SELECT max(m.id) AS m FROM members AS m WHERE m.id > 1000000;
`

var mysqlNullSoundCases = []struct {
	name string
	src  string
	note string
}{
	{
		name: "control_plain_left_join",
		src: `-- name: ControlLeftJoin :many
SELECT m.email, o.name AS org_name
FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id
ORDER BY m.id;
`,
		note: "sanity: direct LEFT JOIN handling",
	},
	{
		name: "view_with_internal_left_join",
		src: `-- name: ViaView :many
SELECT v.member_id, v.email, v.org_name
FROM members_orgs AS v
ORDER BY v.member_id;
`,
		note: "MERGE views report base tables in org_table",
	},
	{
		name: "derived_table_on_null_side",
		src: `-- name: ViaDerived :many
SELECT m.email, s.name AS org_name
FROM members AS m LEFT JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "derived-table org_table attribution",
	},
	{
		name: "cte_on_null_side",
		src: `-- name: ViaCTE :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT m.email, s.name AS org_name
FROM members AS m LEFT JOIN s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "CTE org_table attribution",
	},
	{
		name: "group_by_with_rollup",
		src: `-- name: Rollup :many
SELECT m.email, count(*) AS n
FROM members AS m
GROUP BY m.email WITH ROLLUP;
`,
		note: "WITH ROLLUP nulls the grouping column",
	},
	// No union case: the MySQL dialect maps TiDB's SetOprStmt to
	// StmtOther, so R1 (SQLETCH103) rejects top-level set operations
	// outright — the vector cannot occur.
	{
		name: "view_body_with_rollup",
		src: `-- name: ViaRollupView :many
SELECT v.email, v.n
FROM members_rollup AS v;
`,
		note: "the view's WITH ROLLUP nulls email in super-aggregate rows; is information_schema is_nullable trustworthy?",
	},
	{
		name: "view_body_with_union",
		src: `-- name: ViaUnionView :many
SELECT v.v
FROM members_union AS v;
`,
		note: "the view's second UNION branch supplies nullable org_id; is information_schema is_nullable trustworthy?",
	},
	{
		name: "view_body_empty_aggregate",
		src: `-- name: ViaEmptyMaxView :many
SELECT v.m
FROM members_empty_max AS v;
`,
		note: "max() over an empty input yields one NULL row; is information_schema is_nullable trustworthy?",
	},
}

func TestMySQLNullabilitySoundnessAdversarial(t *testing.T) {
	conn, ctx := acquireMySQLWithSchema(t, mysqlNullSoundSchemaSQL)
	oracle := mysql.NewOracle(conn)
	for _, stmt := range []string{
		"INSERT INTO orgs VALUES (1, 'acme')",
		"INSERT INTO members VALUES (1, 'a@example.com', 1), (2, 'b@example.com', NULL)",
	} {
		if _, err := conn.Execute(stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range mysqlNullSoundCases {
		t.Run(tc.name, func(t *testing.T) {
			q := compileMySQL(t, tc.src)
			rs, err := ast.Renderings(mysql.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			desc, err := oracle.Describe(ctx, rs[0].SQL)
			if err != nil {
				t.Fatalf("describe: %v", err)
			}
			tree, err := mysql.Frontend{}.Parse(rs[0].SQL)
			if err != nil {
				t.Fatal(err)
			}
			verdict := nullability.Analyze(tree, rs[0], desc, cat, nil)
			for i, c := range desc.Columns {
				t.Logf("column %d %q: SrcRel=%d SrcAtt=%d -> nullable=%v",
					i, c.Name, c.SrcRel, c.SrcAtt, verdict[i])
			}

			res, err := conn.Execute(rs[0].SQL)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			defer res.Close()
			if len(res.Values) == 0 {
				t.Fatal("seed data produced no rows; the case proves nothing")
			}
			sawNull := make([]bool, len(desc.Columns))
			for _, row := range res.Values {
				for i := range row {
					if i < len(sawNull) && row[i].Value() == nil {
						sawNull[i] = true
					}
				}
			}
			for i := range verdict {
				if !verdict[i] && sawNull[i] {
					t.Errorf("SOUNDNESS VIOLATION: column %d %q claimed non-nullable but execution returned NULL (%s)",
						i, desc.Columns[i].Name, tc.note)
				}
			}
		})
	}
}

func acquireMySQLWithSchema(t *testing.T, schema string) (*gomysqlclient.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	conn, cleanup, err := devdb.AcquireMySQL(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
		AllowDestructive: true,
		ServerVersion:    "8.4",
		SchemaSQL:        []string{schema},
	})
	if err != nil {
		t.Fatalf("acquire MySQL dev database: %v", err)
	}
	t.Cleanup(cleanup)
	return conn, ctx
}

// ---- SQLite ----------------------------------------------------------------

const sqliteNullSoundSchemaSQL = `
CREATE TABLE orgs (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE members (
    id     INTEGER PRIMARY KEY,
    email  TEXT NOT NULL,
    org_id INTEGER
);
CREATE VIEW members_orgs AS
  SELECT m.id AS member_id, m.email, o.name AS org_name
  FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id;
`

var sqliteNullSoundCases = []struct {
	name string
	src  string
	note string
}{
	{
		name: "control_plain_left_join",
		src: `-- name: ControlLeftJoin :many
SELECT m.email, o.name AS org_name
FROM members AS m LEFT JOIN orgs AS o ON o.id = m.org_id
ORDER BY m.id;
`,
		note: "sanity: direct LEFT JOIN handling",
	},
	{
		name: "view_with_internal_left_join",
		src: `-- name: ViaView :many
SELECT v.member_id, v.email, v.org_name
FROM members_orgs AS v
ORDER BY v.member_id;
`,
		note: "column-origin attribution resolves through views",
	},
	{
		name: "view_with_direct_instance",
		src: `-- name: ViewPlusDirect :many
SELECT v.org_name, o2.name AS direct_name
FROM members_orgs AS v JOIN orgs AS o2 ON 1 = 1
ORDER BY v.member_id, o2.id;
`,
		note: "view-piercing: v.org_name is attributed to base orgs.name, and the DIRECT orgs o2 instance must NOT vouch for it (the view's internal LEFT JOIN nulls it)",
	},
	{
		name: "view_in_derived_with_direct_instance",
		src: `-- name: ViewInDerivedPlusDirect :many
SELECT s.org_name, o2.name AS direct_name
FROM (SELECT vv.org_name, vv.member_id FROM members_orgs AS vv) AS s
JOIN orgs AS o2 ON 1 = 1
ORDER BY s.member_id, o2.id;
`,
		note: "view piercing survives a flattening derived table: attribution still reaches base orgs.name",
	},
	{
		name: "view_on_null_side_with_direct_instance",
		src: `-- name: ViewOuterPlusDirect :many
SELECT o2.name AS direct_name, v.org_name
FROM orgs AS o2 LEFT JOIN members_orgs AS v ON 1 = 1
ORDER BY o2.id, v.member_id;
`,
		note: "view on the null-extended side: the hazard lands on the view OID, but attribution is base orgs.name — the clean o2 must not vouch",
	},
	{
		name: "derived_table_on_null_side",
		src: `-- name: ViaDerived :many
SELECT m.email, s.name AS org_name
FROM members AS m LEFT JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "flattened-subquery origin attribution",
	},
	{
		name: "cte_on_null_side",
		src: `-- name: ViaCTE :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT m.email, s.name AS org_name
FROM members AS m LEFT JOIN s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "CTE origin attribution",
	},
	{
		name: "union_all_branch_provenance",
		src: `-- name: UnionBranches :many
SELECT m.id AS v FROM members AS m
UNION ALL
SELECT m2.org_id AS v FROM members AS m2;
`,
		note: "compound-select output attribution",
	},
	// ---- recursive provenance (design 05 §2b) ----
	{
		name: "derived_inner_join_narrows",
		src: `-- name: DerivedInner :many
SELECT m.email, s.name AS org_name
FROM members AS m JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "positive: recursion narrows a clean INNER-joined derived table — execution must agree",
	},
	{
		name: "derived_body_left_join",
		src: `-- name: DerivedBodyLeft :many
SELECT s.org_name FROM members AS m
JOIN (SELECT m2.id, o.name AS org_name
      FROM members AS m2 LEFT JOIN orgs AS o ON o.id = m2.org_id) AS s
  ON s.id = m.id
ORDER BY m.id;
`,
		note: "null extension INSIDE the derived body must survive the recursion",
	},
	{
		name: "union_in_derived_with_direct_instance",
		src: `-- name: UnionInDerived :many
SELECT o1.name AS n1, s.v FROM orgs AS o1
JOIN (SELECT o.id AS v FROM orgs AS o UNION ALL SELECT m.org_id FROM members AS m) AS s ON 1
ORDER BY o1.id, s.v NULLS LAST;
`,
		note: "SQLite attributes compound output to the FIRST branch — poisoning must cover it even one level down",
	},
	{
		name: "recursive_cte",
		src: `-- name: RecCTE :many
WITH RECURSIVE r AS (
  SELECT m.id, m.org_id FROM members AS m
  UNION ALL
  SELECT r.id + 100, r.org_id FROM r WHERE r.id < 100
)
SELECT r.org_id FROM r ORDER BY r.org_id NULLS LAST;
`,
		note: "recursive CTE output must stay conservative",
	},
	{
		name: "cte_inner_join_narrows",
		src: `-- name: CTEInner :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT m.email, s.name AS org_name
FROM members AS m JOIN s ON s.id = m.org_id
ORDER BY m.id;
`,
		note: "positive: recursion narrows a clean INNER-joined CTE — execution must agree",
	},
}

func TestSQLiteNullabilitySoundnessAdversarial(t *testing.T) {
	conn, ctx := acquireSQLiteWithSchema(t, sqliteNullSoundSchemaSQL)
	oracle := sqlite.NewOracle(conn)
	if err := conn.Exec(`
INSERT INTO orgs VALUES (1, 'acme');
INSERT INTO members VALUES (1, 'a@example.com', 1), (2, 'b@example.com', NULL);
`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range sqliteNullSoundCases {
		t.Run(tc.name, func(t *testing.T) {
			q := compileSQLite(t, tc.src)
			rs, err := ast.Renderings(sqlite.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			desc, err := oracle.Describe(ctx, rs[0].SQL)
			if err != nil {
				t.Fatalf("describe: %v", err)
			}
			tree, err := sqlite.Frontend{}.Parse(rs[0].SQL)
			if err != nil {
				t.Fatal(err)
			}
			verdict := nullability.Analyze(tree, rs[0], desc, cat, nil)
			for i, c := range desc.Columns {
				t.Logf("column %d %q: SrcRel=%d SrcAtt=%d -> nullable=%v",
					i, c.Name, c.SrcRel, c.SrcAtt, verdict[i])
			}

			stmt, _, err := conn.Prepare(rs[0].SQL)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			defer func() { _ = stmt.Close() }()
			sawNull := make([]bool, len(desc.Columns))
			nRows := 0
			for stmt.Step() {
				nRows++
				for i := range sawNull {
					if stmt.ColumnType(i) == sqlite3.NULL {
						sawNull[i] = true
					}
				}
			}
			if err := stmt.Err(); err != nil {
				t.Fatal(err)
			}
			if nRows == 0 {
				t.Fatal("seed data produced no rows; the case proves nothing")
			}
			for i := range verdict {
				if !verdict[i] && sawNull[i] {
					t.Errorf("SOUNDNESS VIOLATION: column %d %q claimed non-nullable but execution returned NULL (%s)",
						i, desc.Columns[i].Name, tc.note)
				}
			}
		})
	}
}

// TestSQLiteNullabilityGuardedMultibyteOffset pins the F1a invariant
// ("nullability never narrows from a GUARDED fragment") for SQLite in
// the presence of multibyte skeleton text. rqlite/sql reports byte
// positions as rune counts; the frontend translates them to bytes, so
// a guarded `col IS NOT NULL` that sits after multibyte skeleton text
// still resolves to a location INSIDE its guard fragment. Without the
// translation the location is reported left of its true byte offset and
// lands in skeleton space, so the analyzer treats the guarded predicate
// as unconditional and narrows a column that is NULL in every guard-off
// shape — a silent soundness hole that no oracle backstops.
//
// The check is at the analyzer level (not execution) because the
// maximal rendering executed by the table-driven suite carries the
// guard ON, which filters the NULL row away; the unsoundness only
// surfaces in a guard-off shape at runtime. Asserting the per-query
// verdict stays nullable catches it deterministically for both shapes.
func TestSQLiteNullabilityGuardedMultibyteOffset(t *testing.T) {
	conn, ctx := acquireSQLiteWithSchema(t, sqliteNullSoundSchemaSQL)
	oracle := sqlite.NewOracle(conn)
	if err := conn.Exec(`
INSERT INTO orgs VALUES (1, 'acme');
INSERT INTO members VALUES (1, 'a@example.com', 1), (2, 'b@example.com', NULL);
`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A long multibyte literal in the SELECT skeleton pushes the guarded
	// `m.org_id IS NOT NULL` many bytes past its rune index. The @when
	// value guard is a pure control fragment (no bind, one conjunct), so
	// it satisfies R9/R1 while keeping the IS NOT NULL GUARDED — it must
	// not narrow.
	src := "-- name: GuardedMultibyte :many\n" +
		"-- @column pad: text\n" +
		"SELECT m.email, m.org_id, 'あいうえおかきくけこさしすせそたちつてと' AS pad\n" +
		"FROM members AS m\n" +
		"WHERE TRUE\n" +
		"@when(mode = 1)\n" +
		"  AND m.org_id IS NOT NULL\n" +
		"@end\n" +
		"ORDER BY m.id;\n"

	q := compileSQLite(t, src)
	rs, err := ast.Renderings(sqlite.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := oracle.Describe(ctx, rs[0].SQL)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	tree, err := sqlite.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	verdict := nullability.Analyze(tree, rs[0], desc, cat, nil)
	// Column 1 is m.org_id — nullable in the catalog, narrowed only by an
	// UNCONDITIONAL IS NOT NULL. Here the IS NOT NULL is guarded, so the
	// verdict must stay nullable in every shape.
	if len(verdict) != 3 {
		t.Fatalf("verdict has %d columns, want 3", len(verdict))
	}
	if !verdict[1] {
		t.Errorf("SOUNDNESS: m.org_id claimed non-nullable, but its IS NOT NULL is GUARDED "+
			"(guard-off shapes return NULL); rune/byte offset confusion misread the guarded "+
			"conjunct as skeleton. verdict=%v", verdict)
	}
}

func acquireSQLiteWithSchema(t *testing.T, schema string) (*sqlite3.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	conn, cleanup, err := devdb.AcquireSQLite(ctx, devdb.Config{
		ServerVersion: "3",
		SchemaSQL:     []string{schema},
	})
	if err != nil {
		t.Fatalf("acquire SQLite dev database: %v", err)
	}
	t.Cleanup(cleanup)
	return conn, ctx
}

// acquireWithSchema is acquire with a case-specific schema.
func acquireWithSchema(t *testing.T, schema string) (*pgx.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	conn, cleanup, err := devdb.Acquire(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_DSN"),
		AllowDestructive: true,
		ServerVersion:    "16",
		SchemaSQL:        []string{schema},
	})
	if err != nil {
		t.Fatalf("acquire dev database: %v", err)
	}
	t.Cleanup(cleanup)
	return conn, ctx
}
