package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

var perfProfiles = map[string]dialect.LexerProfile{
	"postgres": postgres.Profile{},
	"mysql":    mysql.Profile{},
	"sqlite":   sqlite.Profile{},
}

// perfLint scans src under the dialect and runs the catalog-free
// performance lints over every verification rendering.
func perfLint(t *testing.T, dialectName, src string) []diagnostics.Diagnostic {
	t.Helper()
	profile := perfProfiles[dialectName]
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	rs, err := ast.Renderings(profile, q)
	if err != nil {
		t.Fatal(err)
	}
	return CheckPerf(profile, q, rs)
}

// spanTexts returns the template text under every diagnostic of code.
func spanTexts(src string, diags []diagnostics.Diagnostic, code diagnostics.Code) []string {
	var out []string
	for _, d := range diags {
		if d.Code == code {
			out = append(out, src[d.Span.Start:d.Span.End])
		}
	}
	return out
}

func assertWarnings(t *testing.T, diags []diagnostics.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		if d.Severity != diagnostics.Warning {
			t.Errorf("performance lints are warnings only: %+v", d)
		}
		if d.Message == "" || d.Hint == "" {
			t.Errorf("message states the rule and a hint shows the rewrite: %+v", d)
		}
	}
}

// ---- SQLETCH128: function/cast on the column side --------------------

func TestPerf_WrappedColumn(t *testing.T) {
	cases := []struct {
		name, dialect, where string
		want                 []string
	}{
		{"function", "postgres", "lower(u.email) = :email", []string{"lower(u.email)"}},
		{"reversed operands", "postgres", ":email = lower(u.email)", []string{"lower(u.email)"}},
		{"pg cast", "postgres", "u.created_at::date = :day", []string{"u.created_at::date"}},
		{"pg multiword cast", "postgres", "u.created_at::timestamp with time zone >= :t", []string{"u.created_at::timestamp with time zone"}},
		{"CAST", "postgres", "CAST(u.id AS text) = :id", []string{"CAST(u.id AS text)"}},
		{"constant arg first", "postgres", "date_trunc('day', u.created_at) = :day", []string{"date_trunc('day', u.created_at)"}},
		{"coalesce", "postgres", "coalesce(u.status, 'x') <> :s", []string{"coalesce(u.status, 'x')"}},
		{"like", "postgres", "lower(u.email) LIKE :p", []string{"lower(u.email)"}},
		{"in list", "mysql", "lower(u.email) IN (:a, :b)", []string{"lower(u.email)"}},
		{"between", "sqlite", "date(u.created_at) BETWEEN :a AND :b", []string{"date(u.created_at)"}},
		{"boolean group", "postgres", "u.id > 0 AND (lower(u.email) = :e OR u.id = :id)", []string{"lower(u.email)"}},
		{"value side is a cast param", "postgres", "lower(u.email) = :e::text", []string{"lower(u.email)"}},
		{"value side calls a function", "postgres", "upper(u.code) = upper(:c)", []string{"upper(u.code)"}},
		{"mysql function", "mysql", "DATE(u.created_at) = :d", []string{"DATE(u.created_at)"}},
		{"sqlite function", "sqlite", "lower(email) = :e", []string{"lower(email)"}},

		// Under-reporting is the contract: only the unambiguous
		// column-vs-value form is flagged.
		{"both sides columns", "postgres", "lower(u.email) = lower(u.alt_email)", nil},
		{"value side wrapped only", "postgres", "u.email = lower(:e)", nil},
		{"two column args", "postgres", "concat(u.a, u.b) = :x", nil},
		{"bare column", "postgres", "u.email = :e", nil},
		{"param cast", "postgres", "u.id = :id::bigint", nil},
		{"arithmetic", "postgres", "u.id + 1 = :id", nil},
		{"is null", "postgres", "lower(u.email) IS NULL", nil},
		{"not like", "postgres", "lower(u.email) NOT LIKE :p", nil},
		{"not in", "postgres", "lower(u.email) NOT IN (:a)", nil},
		{"case expression", "postgres", "CASE WHEN lower(u.email) = :e THEN true ELSE false END", nil},
		{"function argument", "postgres", "coalesce(lower(u.email) = :e, false)", nil},
		{"subquery value side", "postgres", "lower(u.email) = (SELECT e FROM t)", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "-- name: Q :one\nSELECT u.id FROM users AS u WHERE " + c.where + "\n"
			if c.dialect == "sqlite" && c.name == "sqlite function" {
				src = "-- name: Q :one\nSELECT id FROM users WHERE " + c.where + "\n"
			}
			got := spanTexts(src, perfLint(t, c.dialect, src), diagnostics.CodePerfWrappedColumn)
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Join conditions are predicates too; HAVING is not (it filters
// groups after aggregation, where no index applies), and neither is the
// projection.
func TestPerf_WrappedColumnClauses(t *testing.T) {
	src := `-- name: Q :many
SELECT lower(u.email), count(*)
FROM users AS u
JOIN orgs AS o ON lower(o.slug) = 'acme'
WHERE TRUE
GROUP BY lower(u.email)
HAVING count(*) > :n AND lower(u.email) = :e
ORDER BY lower(u.email)
LIMIT 10
`
	got := spanTexts(src, perfLint(t, "postgres", src), diagnostics.CodePerfWrappedColumn)
	if !slices.Equal(got, []string{"lower(o.slug)"}) {
		t.Errorf("got %q", got)
	}
}

// A subquery's own WHERE is a predicate position; its operand position
// (`IN (SELECT …)`) is not a boolean group.
func TestPerf_WrappedColumnInSubquery(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u
WHERE u.org_id IN (SELECT o.id FROM orgs AS o WHERE lower(o.slug) = :slug)
LIMIT 10
`
	got := spanTexts(src, perfLint(t, "postgres", src), diagnostics.CodePerfWrappedColumn)
	if !slices.Equal(got, []string{"lower(o.slug)"}) {
		t.Errorf("got %q", got)
	}
}

// Guard awareness: a guarded fragment is reachable, so its predicate
// warns, with the span inside the fragment; a predicate that appears in
// several renderings warns once.
func TestPerf_WrappedColumnGuardedAndDeduplicated(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u
@if-present(org)
JOIN orgs AS o ON o.id = u.org_id AND lower(o.slug) = :org
@endif
WHERE TRUE
@if-present(email)
  AND lower(u.email) = :email
@endif
@choose(sort)
@case(newest)
ORDER BY u.id DESC
@case(oldest)
ORDER BY u.id
@end
LIMIT 10
`
	diags := perfLint(t, "postgres", src)
	got := spanTexts(src, diags, diagnostics.CodePerfWrappedColumn)
	if !slices.Equal(got, []string{"lower(o.slug)", "lower(u.email)"}) {
		t.Errorf("got %q", got)
	}
	assertWarnings(t, diags)
}

// @choose case bodies are only in their own renderings; the lint must
// see every case, not just the maximal rendering.
func TestPerf_WrappedColumnInNonDefaultChooseCase(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id,
@choose(extra)
@case(none) 0 AS v
@case(acme) (SELECT count(*) FROM orgs AS o WHERE lower(o.slug) = 'acme') AS v
@end
FROM users AS u
LIMIT 10
`
	got := spanTexts(src, perfLint(t, "postgres", src), diagnostics.CodePerfWrappedColumn)
	if !slices.Equal(got, []string{"lower(o.slug)"}) {
		t.Errorf("got %q", got)
	}
}

// The PostgreSQL @in rendering synthesizes `= ANY($n)`; the wrapped
// operand is still template text, so it is reported.
func TestPerf_WrappedColumnWithIn(t *testing.T) {
	for _, d := range []string{"postgres", "mysql", "sqlite"} {
		src := "-- name: Q :many\nSELECT id FROM users WHERE lower(email) @in(:emails) LIMIT 10\n"
		got := spanTexts(src, perfLint(t, d, src), diagnostics.CodePerfWrappedColumn)
		if !slices.Equal(got, []string{"lower(email)"}) {
			t.Errorf("%s: got %q", d, got)
		}
	}
}

// ---- SQLETCH129: leading-wildcard LIKE --------------------------------

func TestPerf_LeadingWildcardLike(t *testing.T) {
	cases := []struct {
		name, dialect, where string
		want                 []string
	}{
		{"concat operator", "postgres", "u.email LIKE '%' || :q", []string{"'%' || :q"}},
		{"both sides", "postgres", "u.email LIKE '%' || :q || '%'", []string{"'%' || :q || '%'"}},
		{"literal", "postgres", "u.email LIKE '%foo'", []string{"'%foo'"}},
		{"underscore", "postgres", "u.email ILIKE '_oo%'", []string{"'_oo%'"}},
		{"escape string", "postgres", "u.email LIKE E'%foo'", []string{"E'%foo'"}},
		{"escape clause", "postgres", "u.email LIKE '%x!%' ESCAPE '!'", []string{"'%x!%'"}},
		{"mysql CONCAT", "mysql", "u.email LIKE CONCAT('%', :q)", []string{"CONCAT('%', :q)"}},
		{"sqlite concat", "sqlite", "u.email LIKE '%' || :q", []string{"'%' || :q"}},
		{"guarded", "postgres", "TRUE @if-present(q) AND u.email LIKE '%' || :q @endif", []string{"'%' || :q"}},

		{"prefix", "postgres", "u.email LIKE :q || '%'", nil},
		{"bare param", "postgres", "u.email LIKE :q", nil},
		{"no wildcard", "postgres", "u.email LIKE 'foo%'", nil},
		{"not like", "postgres", "u.email NOT LIKE '%foo'", nil},
		{"mysql CONCAT prefix", "mysql", "u.email LIKE CONCAT(:q, '%')", nil},
		{"non-column left", "postgres", ":q LIKE '%foo'", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "-- name: Q :one\nSELECT u.id FROM users AS u WHERE " + c.where + "\n"
			got := spanTexts(src, perfLint(t, c.dialect, src), diagnostics.CodePerfLeadingLike)
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---- SQLETCH130: :many without LIMIT -----------------------------------

func TestPerf_ManyWithoutLimit(t *testing.T) {
	cases := []struct {
		name, dialect, src string
		want               bool
	}{
		{"no limit", "postgres", "-- name: Q :many\nSELECT id FROM users\n", true},
		{"limit", "postgres", "-- name: Q :many\nSELECT id FROM users LIMIT :limit\n", false},
		{"limit all", "postgres", "-- name: Q :many\nSELECT id FROM users LIMIT ALL\n", true},
		{"fetch first", "postgres", "-- name: Q :many\nSELECT id FROM users FETCH FIRST 10 ROWS ONLY\n", false},
		{"subquery limit only", "postgres", "-- name: Q :many\nSELECT s.id FROM (SELECT id FROM users LIMIT 10) AS s\n", true},
		{"cte then limited select", "postgres", "-- name: Q :many\nWITH x AS (SELECT id FROM users) SELECT id FROM x LIMIT 5\n", false},
		{"set operation limit", "postgres", "-- name: Q :many\nSELECT id FROM a UNION ALL SELECT id FROM b LIMIT 5\n", false},
		{"one", "postgres", "-- name: Q :one\nSELECT id FROM users\n", false},
		{"maybe-one", "postgres", "-- name: Q :maybe-one\nSELECT id FROM users\n", false},
		{"update returning", "postgres", "-- name: Q :many\nUPDATE users SET a = 1 RETURNING id\n", false},
		{"insert returning", "postgres", "-- name: Q :many\nINSERT INTO users (a) VALUES (1) RETURNING id\n", false},
		{"delete returning", "sqlite", "-- name: Q :many\nDELETE FROM users RETURNING id\n", false},
		{"mysql limit", "mysql", "-- name: Q :many\nSELECT id FROM users LIMIT :off, :lim\n", false},
		{"sqlite no limit", "sqlite", "-- name: Q :many\nSELECT id FROM users\n", true},
		// The statement's own verb decides DML, never a depth-0 keyword
		// elsewhere: a locking clause or a replace() call is still a
		// SELECT (and an unbounded row-locking SELECT is the costliest).
		{"pg for update", "postgres", "-- name: Q :many\nSELECT id FROM jobs WHERE state = 'queued' FOR UPDATE SKIP LOCKED\n", true},
		{"pg for no key update", "postgres", "-- name: Q :many\nSELECT id FROM jobs FOR NO KEY UPDATE\n", true},
		{"pg for update limited", "postgres", "-- name: Q :many\nSELECT id FROM jobs LIMIT 10 FOR UPDATE SKIP LOCKED\n", false},
		{"mysql for update", "mysql", "-- name: Q :many\nSELECT id FROM jobs FOR UPDATE\n", true},
		{"mysql replace call", "mysql", "-- name: Q :many\nSELECT replace(name, 'a', 'b') AS n FROM users\n", true},
		{"sqlite replace call", "sqlite", "-- name: Q :many\nSELECT replace(name, 'a', 'b') AS n FROM users\n", true},
		{"cte then update returning", "postgres", "-- name: Q :many\nWITH x AS (SELECT id FROM users) UPDATE users SET a = 1 FROM x WHERE users.id = x.id RETURNING users.id\n", false},
		{"cte columns then select", "postgres", "-- name: Q :many\nWITH x (i) AS (SELECT id FROM users) SELECT i FROM x\n", true},
		{"modifying cte then select", "postgres", "-- name: Q :many\nWITH d AS (DELETE FROM users RETURNING id) SELECT id FROM d\n", true},
		{"insert select", "postgres", "-- name: Q :many\nINSERT INTO a (id) SELECT id FROM b RETURNING id\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diags := perfLint(t, c.dialect, c.src)
			got := spanTexts(c.src, diags, diagnostics.CodePerfManyNoLimit)
			if c.want != (len(got) == 1) || len(got) > 1 {
				t.Fatalf("got %q, want warning=%v", got, c.want)
			}
			if c.want && got[0] != "-- name: Q :many" {
				t.Errorf("span = %q, want the header", got[0])
			}
			assertWarnings(t, diags)
		})
	}
}

// ---- SQLETCH131: OFFSET pagination -----------------------------------

func TestPerf_OffsetPagination(t *testing.T) {
	cases := []struct {
		name, dialect, tail string
		want                []string
	}{
		{"pg offset param", "postgres", "LIMIT :limit OFFSET :offset", []string{"OFFSET :offset"}},
		{"pg offset first", "postgres", "OFFSET :offset LIMIT :limit", []string{"OFFSET :offset"}},
		{"pg offset rows fetch", "postgres", "OFFSET :o ROWS FETCH FIRST 5 ROWS ONLY", []string{"OFFSET :o"}},
		{"pg offset expression", "postgres", "LIMIT 10 OFFSET (:page - 1) * 10", []string{"OFFSET (:page - 1) * 10"}},
		{"mysql comma form", "mysql", "LIMIT :off, :lim", []string{":off"}},
		{"mysql offset", "mysql", "LIMIT :lim OFFSET :off", []string{"OFFSET :off"}},
		{"sqlite offset", "sqlite", "LIMIT :lim OFFSET :off", []string{"OFFSET :off"}},
		{"sqlite comma form", "sqlite", "LIMIT :off, :lim", []string{":off"}},
		// ALL/DESC precede a real clause: still flagged.
		{"pg limit all offset", "postgres", "LIMIT ALL OFFSET :o", []string{"OFFSET :o"}},
		{"pg desc offset first", "postgres", "DESC OFFSET :o LIMIT 5", []string{"OFFSET :o"}},

		{"constant offset", "postgres", "LIMIT 10 OFFSET 1", nil},
		{"mysql constant comma", "mysql", "LIMIT 1, :lim", nil},
		{"no offset", "postgres", "LIMIT :limit", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "-- name: Q :many\nSELECT id FROM users ORDER BY id " + c.tail + "\n"
			got := spanTexts(src, perfLint(t, c.dialect, src), diagnostics.CodePerfOffsetPaging)
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// `offset` is a non-reserved word on MySQL/SQLite: a column of that
// name is not the OFFSET clause.
func TestPerf_OffsetColumnIsNotTheClause(t *testing.T) {
	for _, d := range []string{"mysql", "sqlite"} {
		src := "-- name: Q :many\nSELECT offset FROM pages WHERE offset > :o ORDER BY offset LIMIT 10\n"
		if got := spanTexts(src, perfLint(t, d, src), diagnostics.CodePerfOffsetPaging); len(got) != 0 {
			t.Errorf("%s: got %q", d, got)
		}
	}
}

// Every keyword position that takes an operand (or a relation name)
// keeps a bare `offset` a column/table: a false SQLETCH131 there could
// only be silenced by an @nolint that would also hide real OFFSET
// paging, and design 24 §6 promises keyword columns only ever cost a
// MISSED warning. Before any LIMIT, so the predecessor rule decides.
func TestPerf_OffsetKeywordColumnPositions(t *testing.T) {
	cases := []struct{ name, dialect, src string }{
		{"between", "mysql", "SELECT id FROM t WHERE x BETWEEN offset AND 10"},
		{"between sqlite", "sqlite", "SELECT id FROM t WHERE x BETWEEN offset AND 10"},
		{"like", "mysql", "SELECT id FROM t WHERE x LIKE offset"},
		{"like escape", "mysql", "SELECT id FROM t WHERE x LIKE :p ESCAPE offset"},
		{"is", "sqlite", "SELECT id FROM t WHERE x IS offset"},
		{"case", "mysql", "SELECT CASE offset WHEN 1 THEN 'a' END AS c FROM t"},
		{"glob", "sqlite", "SELECT id FROM t WHERE x GLOB offset"},
		{"regexp", "mysql", "SELECT id FROM t WHERE x REGEXP offset"},
		{"interval", "mysql", "SELECT id FROM t WHERE d < now() - INTERVAL offset DAY"},
		{"div", "mysql", "SELECT x DIV offset AS q FROM t"},
		{"mod", "mysql", "SELECT x MOD offset AS q FROM t"},
		{"from table", "mysql", "SELECT id FROM offset WHERE id = :id"},
		{"join table", "sqlite", "SELECT t.id FROM t JOIN offset ON offset.id = t.id"},
		{"returning", "sqlite", "DELETE FROM t WHERE id = :id RETURNING offset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "-- name: Q :many\n" + c.src + "\n"
			if got := spanTexts(src, perfLint(t, c.dialect, src), diagnostics.CodePerfOffsetPaging); len(got) != 0 {
				t.Errorf("got %q", got)
			}
		})
	}
}

// ---- @nolint and SQLETCH133 -------------------------------------------

func TestPerf_NoLintSuppresses(t *testing.T) {
	src := `-- name: Q :many
-- @nolint SQLETCH128, SQLETCH130 (expression index users_lower_email_idx)
SELECT id FROM users WHERE lower(email) = :email
`
	if diags := perfLint(t, "postgres", src); len(diags) != 0 {
		t.Errorf("suppressed lints still reported: %+v", diags)
	}
}

func TestPerf_NoLintIsPerQuery(t *testing.T) {
	src := `-- name: A :many
-- @nolint SQLETCH130
SELECT id FROM users;
-- name: B :many
SELECT id FROM users;
`
	profile := postgres.Profile{}
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	for i, want := range []int{0, 1} {
		q := f.Queries[i]
		rs, err := ast.Renderings(profile, q)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(CheckPerf(profile, q, rs)); got != want {
			t.Errorf("%s: %d diagnostics, want %d", q.Name, got, want)
		}
	}
}

// An @nolint that suppresses nothing is a warning at the directive: a
// stale suppression would otherwise hide the next regression.
func TestPerf_NoLintUnused(t *testing.T) {
	src := `-- name: Q :many
-- @nolint SQLETCH129
SELECT id FROM users LIMIT 10
`
	diags := perfLint(t, "postgres", src)
	got := spanTexts(src, diags, diagnostics.CodePerfNoLintUnused)
	if !slices.Equal(got, []string{"-- @nolint SQLETCH129"}) {
		t.Fatalf("got %q (%+v)", got, diags)
	}
	assertWarnings(t, diags)
}

// SQLETCH132 needs the catalog: the catalog-free pass never calls an
// @nolint of it unused (that verdict belongs to CheckPerfTypes).
func TestPerf_NoLintOfTypeLintNotJudgedOffline(t *testing.T) {
	src := "-- name: Q :many\n-- @nolint SQLETCH132\nSELECT id FROM users LIMIT 1\n"
	if diags := perfLint(t, "postgres", src); len(diags) != 0 {
		t.Errorf("got %+v", diags)
	}
}

// Determinism: identical input, byte-identical diagnostics, in span
// order.
func TestPerf_Deterministic(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u
JOIN orgs AS o ON lower(o.slug) = 'x'
WHERE lower(u.email) = :e AND u.name LIKE '%' || :n
ORDER BY u.id LIMIT :l OFFSET :o
`
	first := fmt.Sprint(perfLint(t, "postgres", src))
	for range 20 {
		if got := fmt.Sprint(perfLint(t, "postgres", src)); got != first {
			t.Fatalf("non-deterministic:\n%s\n%s", first, got)
		}
	}
	diags := perfLint(t, "postgres", src)
	if !slices.IsSortedFunc(diags, func(a, b diagnostics.Diagnostic) int { return a.Span.Start - b.Span.Start }) {
		t.Errorf("not in span order: %+v", diags)
	}
}

// The examples are kept warning-free (design 24 §5): a new example
// that trips a performance lint must fix it or carry a justified
// @nolint.
func TestPerf_ExamplesAreClean(t *testing.T) {
	for name, profile := range perfProfiles {
		paths, err := filepath.Glob(filepath.Join("..", "..", "examples", name, "queries", "*.sql"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("%s: no example queries (%v)", name, err)
		}
		for _, p := range paths {
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			f, diags := template.NewScanner(profile).ScanFile(p, src)
			if len(diags) != 0 {
				t.Fatalf("%s: %+v", p, diags)
			}
			for _, q := range f.Queries {
				rs, err := ast.Renderings(profile, q)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range CheckPerf(profile, q, rs) {
					t.Errorf("%s %s: %s %s", p, q.Name, d.Code, d.Message)
				}
			}
		}
	}
}

// Malformed-but-scannable SQL never panics the lexical walk: operand
// extraction and paren matching stay in bounds on unbalanced or
// degenerate token runs.
func TestPerf_DegenerateInputs(t *testing.T) {
	bodies := []string{
		"SELECT ) WHERE = LIKE",
		"SELECT x FROM t WHERE = :a",
		"SELECT x FROM t WHERE lower( = :a",
		"SELECT x FROM t WHERE :a =",
		"SELECT x FROM t WHERE x LIKE",
		"SELECT x FROM t WHERE x IN",
		"SELECT x FROM t WHERE CAST( = :a",
		"SELECT x FROM t WHERE ((((lower(x) = :a",
		"SELECT x FROM t WHERE x BETWEEN AND",
		"SELECT x FROM t LIMIT , OFFSET",
		"SELECT x FROM t OFFSET",
		"SELECT x FROM t WHERE x = CONCAT(",
		"SELECT x FROM t WHERE x :: = :a",
		"SELECT x FROM t WHERE END CASE END = :a",
	}
	ran := 0
	for _, profile := range perfProfiles {
		for _, b := range bodies {
			f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte("-- name: Q :many\n"+b+"\n"))
			if len(diags) != 0 || len(f.Queries) != 1 {
				continue
			}
			rs, err := ast.Renderings(profile, f.Queries[0])
			if err != nil {
				continue
			}
			_ = CheckPerf(profile, f.Queries[0], rs)
			ran++
		}
	}
	if ran == 0 {
		t.Fatal("no degenerate input reached the lints")
	}
}
