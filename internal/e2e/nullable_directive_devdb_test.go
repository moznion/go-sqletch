//go:build devdb

package e2e_test

// `-- @nullable` directives (design 25) against real engines: the
// directive forces the generated row field's type in both directions,
// the generated module compiles and scans adversarial rows (NULLs,
// multibyte, empty strings) into it, and a WRONG non-null assertion
// fails loudly at scan time — never a silent zero value.
//
// Each suite runs the real CLI pipeline (cold generate against the dev
// database), compiles the generated package into a throwaway module,
// and runs it against the same database.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
)

type nullableDialect struct {
	name          string
	serverVersion string
	acquire       func(t *testing.T, ctx context.Context, dir string) (dsn string, cleanup func())
	runDSN        func(dsn string) string
	schema        string
	requires      []string
	imports       []string
	open          string
}

// Every suite: books.author_id and books.subtitle are nullable,
// authors.name and books.title are NOT NULL.
const nullableQueries = `-- name: BooksWithAuthor :many
-- Every row has an author (the WHERE filters the outer join's
-- null-extension away), which the analyzer cannot see through a
-- LEFT JOIN: forced non-null.
-- @nullable author_name: false
SELECT b.id, a.name AS author_name
FROM books AS b LEFT JOIN authors AS a ON a.id = b.author_id
WHERE a.id IS NOT NULL
ORDER BY b.id;

-- name: BookTitles :many
-- A NOT NULL column forced nullable (e.g. ahead of a migration).
-- @nullable title: true
SELECT b.id, b.title
FROM books AS b
ORDER BY b.id;

-- name: BadSubtitles :many
-- A WRONG assertion: subtitle really is NULL for some rows.
-- @nullable subtitle: false
SELECT b.id, b.subtitle
FROM books AS b
ORDER BY b.id;
`

func TestNullableDirectivePostgres(t *testing.T) {
	runNullableSuite(t, nullableDialect{
		name:          "postgres",
		serverVersion: "16",
		acquire: func(t *testing.T, ctx context.Context, _ string) (string, func()) {
			dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
				DSN:              os.Getenv("SQLETCH_TEST_DSN"),
				AllowDestructive: true,
				ServerVersion:    "16",
			})
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			return dsn, cleanup
		},
		runDSN: func(dsn string) string { return dsn },
		schema: "CREATE TABLE authors (id bigint PRIMARY KEY, name text NOT NULL);\n" +
			"CREATE TABLE books (id bigint PRIMARY KEY, author_id bigint REFERENCES authors(id), title text NOT NULL, subtitle text);\n",
		requires: []string{"github.com/jackc/pgx/v5"},
		imports:  []string{`"github.com/jackc/pgx/v5/pgxpool"`},
		open: `
func open(ctx context.Context, dsn string) (*gen.Queries, func(string) error) {
	pool, err := pgxpool.New(ctx, dsn)
	die(err)
	return gen.New(pool), func(s string) error { _, err := pool.Exec(ctx, s); return err }
}
`,
	})
}

func TestNullableDirectiveMySQL(t *testing.T) {
	runNullableSuite(t, nullableDialect{
		name:          "mysql",
		serverVersion: "8.4",
		acquire: func(t *testing.T, ctx context.Context, _ string) (string, func()) {
			dsn, cleanup, err := devdb.AcquireMySQLDSN(ctx, devdb.Config{
				DSN:              os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
				AllowDestructive: true,
				ServerVersion:    "8.4",
			})
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			return dsn, cleanup
		},
		runDSN: func(dsn string) string { return dsn },
		schema: "CREATE TABLE authors (id BIGINT PRIMARY KEY, name VARCHAR(64) NOT NULL);\n" +
			"CREATE TABLE books (id BIGINT PRIMARY KEY, author_id BIGINT, title VARCHAR(64) NOT NULL, subtitle VARCHAR(64));\n",
		requires: []string{"github.com/go-sql-driver/mysql"},
		imports:  []string{`"database/sql"`, `_ "github.com/go-sql-driver/mysql"`},
		open:     sqlOpen("mysql", "dsn"),
	})
}

func TestNullableDirectiveSQLite(t *testing.T) {
	runNullableSuite(t, nullableDialect{
		name:          "sqlite",
		serverVersion: "3",
		acquire: func(_ *testing.T, _ context.Context, dir string) (string, func()) {
			return filepath.Join(dir, "dev.sqlite3"), func() {}
		},
		runDSN: func(dsn string) string { return "file:" + dsn },
		schema: "CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT NOT NULL);\n" +
			"CREATE TABLE books (id INTEGER PRIMARY KEY, author_id INTEGER REFERENCES authors(id), title TEXT NOT NULL, subtitle TEXT);\n",
		requires: []string{"github.com/ncruces/go-sqlite3"},
		imports:  []string{`"database/sql"`, `_ "github.com/ncruces/go-sqlite3/driver"`},
		open:     sqlOpen("sqlite3", "dsn"),
	})
}

func runNullableSuite(t *testing.T, d nullableDialect) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dir := t.TempDir()
	dsn, cleanup := d.acquire(t, ctx, dir)
	defer cleanup()

	writeFile(t, dir, "db/schema.sql", d.schema)
	writeFile(t, dir, "queries/books.sql", nullableQueries)
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: `+d.name+`
server_version: "`+d.serverVersion+`"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
`)
	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, filepath.Join(dir, "sqletch.yaml"), false,
		cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("generate: exit %d\n%s%s", code, out.String(), errW.String())
	}
	// The forced types are the contract: pin them in the emitted rows.
	genFiles, err := filepath.Glob(filepath.Join(dir, "gen", "*.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	var src string
	for _, p := range genFiles {
		src += readFile(t, dir, filepath.Join("gen", filepath.Base(p)))
	}
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\tAuthorName\s+string$`),
		regexp.MustCompile(`(?m)^\tTitle\s+optional\.Option\[string\]$`),
		regexp.MustCompile(`(?m)^\tSubtitle\s+string$`),
	} {
		if !want.MatchString(src) {
			t.Errorf("generated rows lack %s:\n%s", want, src)
		}
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var req strings.Builder
	for _, mod := range append([]string{"github.com/moznion/go-optional"}, d.requires...) {
		m := regexp.MustCompile(regexp.QuoteMeta(mod) + ` (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
		if m == nil {
			t.Fatalf("%s version not found in parent go.mod", mod)
		}
		req.WriteString("\t" + mod + " " + m[1] + "\n")
	}
	writeFile(t, dir, "go.mod", "module sqletchgen\n\ngo 1.24\n\nrequire (\n"+req.String()+
		"\tgithub.com/moznion/go-sqletch v0.0.0\n)\n\n"+
		"replace github.com/moznion/go-sqletch => "+repoRoot+"\n")
	parentSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "go.sum", string(parentSum))

	main := strings.Replace(nullableMain, "//IMPORTS", "\t"+strings.Join(d.imports, "\n\t"), 1)
	main = strings.Replace(main, "//OPEN", d.open, 1)
	writeFile(t, dir, "main.go", main)

	run := func(args ...string) string {
		c := exec.CommandContext(ctx, "go", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "SQLETCH_NULLABLE_DSN="+d.runDSN(dsn))
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	run("mod", "tidy")
	if got := run("run", "."); !strings.Contains(got, "NULLABLE-OK") {
		t.Fatalf("harness did not report success:\n%s", got)
	}
}

const nullableMain = `package main

import (
	"context"
	"fmt"
	"os"

	gen "sqletchgen/gen"
//IMPORTS
)

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}

func expect(cond bool, format string, args ...any) {
	if !cond {
		fmt.Fprintf(os.Stderr, "EXPECT FAILED: "+format+"\n", args...)
		os.Exit(1)
	}
}
//OPEN
func main() {
	ctx := context.Background()
	q, execRaw := open(ctx, os.Getenv("SQLETCH_NULLABLE_DSN"))
	die(execRaw("DELETE FROM books"))
	die(execRaw("DELETE FROM authors"))
	die(execRaw("INSERT INTO authors (id, name) VALUES (1, 'Ōe'), (2, '')"))
	// Book 3 has no author (null-extended by the LEFT JOIN, filtered by
	// the WHERE); book 4 has a NULL subtitle.
	die(execRaw("INSERT INTO books (id, author_id, title, subtitle) VALUES " +
		"(1, 1, '万延元年', 'フットボール'), (2, 2, '', ''), (3, NULL, 'orphan', 'x'), (4, 1, 't', NULL)"))

	// 1. Forced non-null on the outer join's side: plain strings,
	// multibyte and empty included.
	rows, err := q.BooksWithAuthor(ctx, gen.BooksWithAuthorParams{})
	die(err)
	expect(len(rows) == 3, "BooksWithAuthor: got %d rows", len(rows))
	expect(rows[0].AuthorName == "Ōe" && rows[1].AuthorName == "" && rows[2].AuthorName == "Ōe",
		"BooksWithAuthor: got %+v", rows)

	// 2. Forced nullable on a NOT NULL column: every value is Some,
	// including the empty string (which must not collapse to None).
	titles, err := q.BookTitles(ctx, gen.BookTitlesParams{})
	die(err)
	expect(len(titles) == 4, "BookTitles: got %d rows", len(titles))
	for i, want := range []string{"万延元年", "", "orphan", "t"} {
		got, err := titles[i].Title.Take()
		expect(err == nil && got == want, "BookTitles[%d]: got %v (%v), want Some(%q)", i, titles[i].Title, err, want)
	}

	// 3. A wrong non-null assertion is LOUD: the driver refuses to scan
	// NULL into a plain string — never a silent "".
	_, err = q.BadSubtitles(ctx, gen.BadSubtitlesParams{})
	expect(err != nil, "BadSubtitles: scanning a NULL into a forced non-null field must fail")

	fmt.Println("NULLABLE-OK")
}
`
