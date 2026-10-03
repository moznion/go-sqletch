//go:build devdb

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// perfTypeCase is one comparison of TestPerfTypeMismatch*: the template
// predicate the lint sees, and the same predicate as the engine plans
// it. The property pinned is the whitelist's contract (design 24 §3.5)
// against the REAL planner and the REAL oracle types: every flagged
// pair loses the index, and every index-safe pair in the table is left
// alone.
type perfTypeCase struct {
	name     string
	template string // the WHERE predicate, with :v
	hint     string // MySQL `-- @param v:` type ("" on PostgreSQL)
	planSQL  string // the predicate as planned ($1 / ?)
	planArg  any    // MySQL bind value for the prepared EXPLAIN
	flagged  bool
}

// perfCheckFlagged runs `sqletch check --format json` on a project with
// one query file per case and returns the case names carrying
// SQLETCHL005 — the lint as a user sees it, through the oracle's types.
func perfCheckFlagged(t *testing.T, ctx context.Context, dialect, version, dsn, schema string, cases []perfTypeCase, table string) map[string]bool {
	t.Helper()
	queries := map[string]perfQuery{}
	for _, c := range cases {
		queries[c.name] = perfQuery{where: c.template, hint: c.hint}
	}
	flagged := map[string]bool{}
	for name, codes := range perfCheckCodes(t, ctx, dialect, version, dsn, schema, queries, table) {
		for _, code := range codes {
			if code != diagnostics.CodePerfTypeMismatch {
				t.Errorf("%s: unexpected diagnostic %s", name, code)
				continue
			}
			flagged[name] = true
		}
	}
	return flagged
}

// perfQuery is one `:one` query of a perfCheckCodes project: its WHERE
// predicate and its `-- @param` hint lines (MySQL/SQLite).
type perfQuery struct {
	where string
	hint  string // "v: bigint" style, one per line; "" = none
}

// perfCheckCodes runs `sqletch check --lint --json` against the real
// dev database and returns, per query file, the codes of its
// diagnostics. Every diagnostic must be a lint warning: the run must
// pass (lints never fail it).
func perfCheckCodes(t *testing.T, ctx context.Context, dialect, version, dsn, schema string, queries map[string]perfQuery, table string) map[string][]diagnostics.Code {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", schema)
	for name, pq := range queries {
		q := "-- name: " + strings.ReplaceAll(name, "_", "") + " :one\n"
		for _, h := range strings.Split(pq.hint, "\n") {
			if h == "" {
				continue
			}
			if !strings.Contains(h, ":") {
				h = "v: " + h // the single-parameter shorthand of perfTypeCase
			}
			q += "-- @param " + h + "\n"
		}
		q += "SELECT note FROM " + table + " WHERE " + pq.where + ";\n"
		writeFile(t, dir, "queries/"+name+".sql", q)
	}
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: `+dialect+`
server_version: "`+version+`"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
cache:
  path: .sqletch/cache
`)
	// Lints are opt-in; enable them the way `check --lint` does.
	lint := true
	var out, errW bytes.Buffer
	code := cli.Check(ctx, filepath.Join(dir, "sqletch.yaml"), false, true, cli.RunOptions{AllowDestructive: true, Lint: &lint}, &out, &errW)
	if code != cli.ExitOK {
		t.Fatalf("check must pass (performance lints are warnings): exit %d\n%s%s", code, out.String(), errW.String())
	}
	got := map[string][]diagnostics.Code{}
	sc := bufio.NewScanner(&errW)
	for sc.Scan() {
		var d map[string]any
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("non-JSON diagnostic line %q", sc.Text())
		}
		if d["severity"] != "warning" {
			t.Errorf("only lint warnings are expected: %v", d)
		}
		file, _ := d["file"].(string)
		name := strings.TrimSuffix(filepath.Base(file), ".sql")
		code, _ := d["code"].(string)
		got[name] = append(got[name], diagnostics.Code(code))
	}
	return got
}

func TestPerfTypeMismatchPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_DSN"),
		AllowDestructive: true,
		ServerVersion:    "16",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	const schema = `CREATE TABLE p (
    id   bigint PRIMARY KEY,
    n    integer NOT NULL,
    born date NOT NULL,
    s    text NOT NULL,
    f    double precision NOT NULL,
    note text
);
CREATE INDEX p_n ON p (n);
CREATE INDEX p_born ON p (born);
CREATE INDEX p_s ON p (s);
CREATE INDEX p_f ON p (f);
`
	cases := []perfTypeCase{
		{name: "int8_numeric", template: "id = :v::numeric", planSQL: "id = $1::numeric", flagged: true},
		{name: "int8_float8", template: "id = :v::float8", planSQL: "id = $1::float8", flagged: true},
		{name: "int4_numeric", template: "n < :v::numeric", planSQL: "n < $1::numeric", flagged: true},
		{name: "int8_inferred", template: "id = :v", planSQL: "id = $1::int8"},
		{name: "int4_int8", template: "n = :v::int8", planSQL: "n = $1::int8"},
		{name: "date_timestamp", template: "born < :v::timestamp", planSQL: "born < $1::timestamp"},
		{name: "float8_numeric", template: "f = :v::numeric", planSQL: "f = $1::numeric"},
		{name: "text_varchar", template: "s = :v::varchar", planSQL: "s = $1::varchar"},
	}
	flagged := perfCheckFlagged(t, ctx, "postgres", "16", dsn, schema, cases, "p")

	conn, closeConn, err := devdb.Acquire(ctx, devdb.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		res, err := conn.PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN) SELECT note FROM p WHERE "+c.planSQL).ReadAll()
		if err != nil || len(res) != 1 || res[0].Err != nil {
			t.Fatalf("%s: explain: %v %v", c.name, err, res)
		}
		var plan strings.Builder
		for _, row := range res[0].Rows {
			plan.Write(row[0])
			plan.WriteByte('\n')
		}
		usesIndex := strings.Contains(plan.String(), "Index")
		if usesIndex == c.flagged {
			t.Errorf("%s: whitelist entry contradicts the planner (flagged=%v, index used=%v):\n%s", c.name, c.flagged, usesIndex, plan.String())
		}
		if flagged[c.name] != c.flagged {
			t.Errorf("%s: SQLETCHL005 reported=%v, want %v", c.name, flagged[c.name], c.flagged)
		}
	}
}

func TestPerfTypeMismatchMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, cleanup, err := devdb.AcquireMySQLDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
		AllowDestructive: true,
		ServerVersion:    "8.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	const schema = `CREATE TABLE m (
    id   BIGINT PRIMARY KEY,
    code VARCHAR(16) NOT NULL,
    num  BIGINT NOT NULL,
    note VARCHAR(64),
    KEY m_code (code),
    KEY m_num (num)
);
`
	cases := []perfTypeCase{
		{name: "varchar_bigint", template: "code = :v", hint: "bigint", planSQL: "code = ?", planArg: int64(5), flagged: true},
		{name: "varchar_double", template: "code = :v", hint: "double", planSQL: "code = ?", planArg: float64(5), flagged: true},
		{name: "varchar_varchar", template: "code = :v", hint: "varchar(16)", planSQL: "code = ?", planArg: "5"},
		{name: "bigint_varchar", template: "num = :v", hint: "varchar(16)", planSQL: "num = ?", planArg: "5"},
		{name: "bigint_bigint", template: "num = :v", hint: "bigint", planSQL: "num = ?", planArg: int64(5)},
	}
	flagged := perfCheckFlagged(t, ctx, "mysql", "8.4", dsn, schema, cases, "m")

	conn, closeConn, err := devdb.AcquireMySQL(ctx, devdb.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	// Enough rows that the optimizer's choice is a real one.
	for i := range 64 {
		if _, err := conn.Execute(fmt.Sprintf("INSERT INTO m VALUES (%d, '%d', %d, 'x')", i, i, i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Execute("ANALYZE TABLE m"); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		col := strings.Fields(c.planSQL)[0]
		r, err := conn.Execute("EXPLAIN SELECT note FROM m FORCE INDEX (m_"+col+") WHERE "+c.planSQL, c.planArg)
		if err != nil {
			t.Fatalf("%s: explain: %v", c.name, err)
		}
		accessType, _ := r.GetStringByName(0, "type")
		accessType = strings.Clone(accessType) // GetString aliases pooled buffers
		r.Close()
		// ref/range/const = an index seek; ALL/index = a scan.
		seek := accessType == "ref" || accessType == "range" || accessType == "const" || accessType == "eq_ref"
		if seek == c.flagged {
			t.Errorf("%s: whitelist entry contradicts the optimizer (flagged=%v, access type=%q)", c.name, c.flagged, accessType)
		}
		if flagged[c.name] != c.flagged {
			t.Errorf("%s: SQLETCHL005 reported=%v, want %v", c.name, flagged[c.name], c.flagged)
		}
	}
}

// SQLite: no pair is whitelisted. The plan is fixed at prepare time,
// before any value is bound, and a comparison with a parameter takes
// the column's affinity — so a numeric bind against an indexed TEXT
// column still searches the index. Pinned here so the empty whitelist
// stays a measured fact, not an assumption.
func TestPerfTypeMismatchSQLite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "dev.sqlite3")
	const schema = "CREATE TABLE m (id INTEGER PRIMARY KEY, code TEXT NOT NULL, note TEXT);\nCREATE INDEX m_code ON m (code);\n"
	cases := []perfTypeCase{
		{name: "text_integer", template: "code = :v", hint: "integer", planSQL: "code = ?"},
		{name: "text_real", template: "code = :v", hint: "real", planSQL: "code = ?"},
	}
	flagged := perfCheckFlagged(t, ctx, "sqlite", "3", dbPath, schema, cases, "m")

	conn, closeConn, err := devdb.AcquireSQLite(ctx, devdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	for _, c := range cases {
		stmt, _, err := conn.Prepare("EXPLAIN QUERY PLAN SELECT note FROM m WHERE " + c.planSQL)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for stmt.Step() {
			plan.WriteString(stmt.ColumnText(3))
			plan.WriteByte('\n')
		}
		if err := stmt.Close(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "USING INDEX m_code") {
			t.Errorf("%s: expected an index search, got:\n%s", c.name, plan.String())
		}
		if flagged[c.name] {
			t.Errorf("%s: SQLETCHL005 must never fire on SQLite", c.name)
		}
	}
}
