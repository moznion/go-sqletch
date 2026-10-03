//go:build devdb

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// rewriteCase pins one row of the performance-lint chapter
// (docs/manual/14-performance-lints.md) against a REAL planner: the
// form a lint flags must lose the index, and the rewrite the chapter
// recommends must keep it — including the dialect-specific rows (a
// PostgreSQL B-tree never serves a parameterized LIKE; SQLite never
// optimizes `:q || '%'`). The lint verdict is taken from `check --lint`
// on the same predicate, so the advice and the lint cannot drift apart.
type rewriteCase struct {
	name    string
	where   string // template predicate
	hint    string // `-- @param` lines (MySQL/SQLite)
	planSQL string // the predicate as planned
	args    []any  // bind values for the plan (MySQL/SQLite)
	index   string // MySQL: the index FORCEd for the plan
	lint    diagnostics.Code
	usesIdx bool
}

func checkRewrites(t *testing.T, ctx context.Context, dialect, version, dsn, schema, table string, cases []rewriteCase,
	plan func(c rewriteCase) (string, bool)) {
	t.Helper()
	queries := map[string]perfQuery{}
	for _, c := range cases {
		queries[c.name] = perfQuery{where: c.where, hint: c.hint}
	}
	got := perfCheckCodes(t, ctx, dialect, version, dsn, schema, queries, table)
	for _, c := range cases {
		var want []diagnostics.Code
		if c.lint != "" {
			want = []diagnostics.Code{c.lint}
		}
		if !slices.Equal(got[c.name], want) {
			t.Errorf("%s: lint = %v, want %v", c.name, got[c.name], want)
		}
		text, usesIdx := plan(c)
		if usesIdx != c.usesIdx {
			t.Errorf("%s: index used = %v, want %v (the chapter's claim):\n%s", c.name, usesIdx, c.usesIdx, text)
		}
	}
}

func TestPerfRewritesPostgres(t *testing.T) {
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

	const schema = `CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE TABLE u (
    id         bigint PRIMARY KEY,
    email      text NOT NULL,
    alt        text NOT NULL,
    body       text NOT NULL,
    created_at timestamptz NOT NULL,
    note       text
);
CREATE INDEX u_email ON u (email);
CREATE INDEX u_email_pat ON u (email text_pattern_ops);
CREATE INDEX u_alt_lower ON u (lower(alt));
CREATE INDEX u_body_trgm ON u USING gin (body gin_trgm_ops);
CREATE INDEX u_created ON u (created_at);
`
	cases := []rewriteCase{
		// SQLETCHL001
		{name: "l001_lower", where: "lower(email) = :v", planSQL: "lower(email) = $1", lint: diagnostics.CodePerfWrappedColumn},
		{name: "l001_bare", where: "email = :v", planSQL: "email = $1", usesIdx: true},
		{name: "l001_cast_date", where: "created_at::date = :v::date", planSQL: "created_at::date = $1::date", lint: diagnostics.CodePerfWrappedColumn},
		{name: "l001_range", where: "created_at >= :a AND created_at < :b", planSQL: "created_at >= $1 AND created_at < $2", usesIdx: true},
		// An expression index serves exactly that expression; the lint
		// cannot see indexes, so it still fires (the @nolint case).
		{name: "l001_expr_index", where: "lower(alt) = :v", planSQL: "lower(alt) = $1", lint: diagnostics.CodePerfWrappedColumn, usesIdx: true},
		// SQLETCHL002
		{name: "l002_leading", where: "email LIKE '%' || :v", planSQL: "email LIKE '%' || $1", lint: diagnostics.CodePerfLeadingLike},
		// A parameterized prefix is NOT served by a B-tree on PostgreSQL,
		// not even text_pattern_ops (generic plan): not flagged, but not
		// a rewrite the chapter may recommend here.
		{name: "l002_param_prefix", where: "email LIKE :v || '%'", planSQL: "email LIKE $1 || '%'"},
		{name: "l002_bound_pattern", where: "email LIKE :v", planSQL: "email LIKE $1"},
		{name: "l002_range", where: "email >= :lo AND email < :hi", planSQL: "email >= $1 AND email < $2", usesIdx: true},
		// A trigram index serves a leading wildcard (the @nolint case).
		{name: "l002_trigram", where: "body LIKE '%' || :v", planSQL: "body LIKE '%' || $1", lint: diagnostics.CodePerfLeadingLike, usesIdx: true},
	}

	conn, closeConn, err := devdb.Acquire(ctx, devdb.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	checkRewrites(t, ctx, "postgres", "16", dsn, schema, "u", cases, func(c rewriteCase) (string, bool) {
		if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
			t.Fatal(err)
		}
		res, err := conn.PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, COSTS OFF) SELECT note FROM u WHERE "+c.planSQL).ReadAll()
		if err != nil || len(res) != 1 || res[0].Err != nil {
			t.Fatalf("%s: explain: %v %v", c.name, err, res)
		}
		var plan strings.Builder
		for _, row := range res[0].Rows {
			plan.Write(row[0])
			plan.WriteByte('\n')
		}
		return plan.String(), strings.Contains(plan.String(), "Index")
	})
}

func TestPerfRewritesMySQL(t *testing.T) {
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
    id    BIGINT PRIMARY KEY,
    email VARCHAR(64) NOT NULL,
    alt   VARCHAR(64) NOT NULL,
    note  VARCHAR(64),
    KEY m_email (email),
    KEY m_alt_lower ((lower(alt)))
);
`
	str := "v: varchar(64)"
	cases := []rewriteCase{
		{name: "l001_lower", where: "lower(email) = :v", hint: str, planSQL: "lower(email) = ?", args: []any{"a"}, index: "m_email", lint: diagnostics.CodePerfWrappedColumn},
		{name: "l001_bare", where: "email = :v", hint: str, planSQL: "email = ?", args: []any{"a"}, index: "m_email", usesIdx: true},
		{name: "l001_expr_index", where: "lower(alt) = :v", hint: str, planSQL: "lower(alt) = ?", args: []any{"a"}, index: "m_alt_lower", lint: diagnostics.CodePerfWrappedColumn, usesIdx: true},
		{name: "l002_leading", where: "email LIKE CONCAT('%', :v)", hint: str, planSQL: "email LIKE CONCAT('%', ?)", args: []any{"ab"}, index: "m_email", lint: diagnostics.CodePerfLeadingLike},
		{name: "l002_prefix", where: "email LIKE CONCAT(:v, '%')", hint: str, planSQL: "email LIKE CONCAT(?, '%')", args: []any{"ab"}, index: "m_email", usesIdx: true},
		{name: "l002_bound_pattern", where: "email LIKE :v", hint: str, planSQL: "email LIKE ?", args: []any{"ab%"}, index: "m_email", usesIdx: true},
		{name: "l002_range", where: "email >= :lo AND email < :hi", hint: "lo: varchar(64)\nhi: varchar(64)", planSQL: "email >= ? AND email < ?", args: []any{"ab", "ac"}, index: "m_email", usesIdx: true},
	}

	conn, closeConn, err := devdb.AcquireMySQL(ctx, devdb.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	seeded := false
	checkRewrites(t, ctx, "mysql", "8.4", dsn, schema, "m", cases, func(c rewriteCase) (string, bool) {
		if !seeded {
			// Enough rows that the optimizer's choice is a real one.
			for i := range 64 {
				if _, err := conn.Execute(fmt.Sprintf("INSERT INTO m VALUES (%d, 'ab%d@x', 'Ab%d', 'x')", i, i, i)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := conn.Execute("ANALYZE TABLE m"); err != nil {
				t.Fatal(err)
			}
			seeded = true
		}
		r, err := conn.Execute("EXPLAIN SELECT note FROM m FORCE INDEX ("+c.index+") WHERE "+c.planSQL, c.args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", c.name, err)
		}
		defer r.Close()
		accessType, _ := r.GetStringByName(0, "type")
		accessType = strings.Clone(accessType) // GetString aliases pooled buffers
		// ref/range/const = an index seek; ALL/index = a scan.
		seek := accessType == "ref" || accessType == "range" || accessType == "const" || accessType == "eq_ref"
		return "access type " + accessType, seek
	})
}

func TestPerfRewritesSQLite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "dev.sqlite3")
	const schema = `CREATE TABLE s (id INTEGER PRIMARY KEY, email TEXT NOT NULL, alt TEXT NOT NULL, note TEXT);
CREATE INDEX s_email ON s (email);
CREATE INDEX s_email_nocase ON s (email COLLATE NOCASE);
CREATE INDEX s_alt_lower ON s (lower(alt));
`
	str := "v: text"
	cases := []rewriteCase{
		{name: "l001_lower", where: "lower(email) = :v", hint: str, planSQL: "lower(email) = ?", args: []any{"a"}, lint: diagnostics.CodePerfWrappedColumn},
		{name: "l001_bare", where: "email = :v", hint: str, planSQL: "email = ?", args: []any{"a"}, usesIdx: true},
		{name: "l001_expr_index", where: "lower(alt) = :v", hint: str, planSQL: "lower(alt) = ?", args: []any{"a"}, lint: diagnostics.CodePerfWrappedColumn, usesIdx: true},
		{name: "l002_leading", where: "email LIKE '%' || :v", hint: str, planSQL: "email LIKE '%' || ?", args: []any{"ab"}, lint: diagnostics.CodePerfLeadingLike},
		// SQLite optimizes LIKE only for a literal or a bound pattern
		// that starts with a non-wildcard (and, LIKE being
		// case-insensitive, against a NOCASE index): an expression like
		// `:q || '%'` is never optimized.
		{name: "l002_concat_prefix", where: "email LIKE :v || '%'", hint: str, planSQL: "email LIKE ? || '%'", args: []any{"ab"}},
		{name: "l002_bound_pattern", where: "email LIKE :v", hint: str, planSQL: "email LIKE ?", args: []any{"ab%"}, usesIdx: true},
		{name: "l002_range", where: "email >= :lo AND email < :hi", hint: "lo: text\nhi: text", planSQL: "email >= ? AND email < ?", args: []any{"ab", "ac"}, usesIdx: true},
	}

	checkRewrites(t, ctx, "sqlite", "3", dbPath, schema, "s", cases, func(c rewriteCase) (string, bool) {
		conn, closeConn, err := devdb.AcquireSQLite(ctx, devdb.Config{DSN: dbPath})
		if err != nil {
			t.Fatal(err)
		}
		defer closeConn()
		stmt, _, err := conn.Prepare("EXPLAIN QUERY PLAN SELECT note FROM s WHERE " + c.planSQL)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stmt.Close() }()
		for i, a := range c.args {
			if err := stmt.BindText(i+1, a.(string)); err != nil {
				t.Fatal(err)
			}
		}
		var plan strings.Builder
		for stmt.Step() {
			plan.WriteString(stmt.ColumnText(3))
			plan.WriteByte('\n')
		}
		return plan.String(), strings.Contains(plan.String(), "USING") && strings.Contains(plan.String(), "INDEX")
	})
}
