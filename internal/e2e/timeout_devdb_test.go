//go:build devdb

package e2e_test

// Query timeouts (design 23) against real engines: the generated
// method's context deadline must actually stop a slow statement on
// every dialect's driver and surface as context.DeadlineExceeded; the
// config-level default must apply to queries without a directive;
// `@timeout none` must opt out of it; a caller's shorter deadline must
// still win; and a generous deadline must not cut off row iteration.
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

// timeoutDialect is everything dialect-specific about a timeout suite.
type timeoutDialect struct {
	name          string
	serverVersion string
	acquire       func(t *testing.T, ctx context.Context, dir string) (dsn string, cleanup func())
	// runDSN maps the config DSN to what the harness main opens.
	runDSN   func(dsn string) string
	schema   string
	queries  map[string]string // file stem -> template
	requires []string          // module paths copied from the parent go.mod
	// open is the main's import block additions plus an `open` func
	// returning the generated Queries and a raw exec function.
	imports []string
	open    string
	// driverCtxErr, when set, is a Go boolean expression over `err` that
	// must ALSO hold for every context failure — the driver's own error
	// that the generated code must preserve while normalizing (SQLite's
	// sqlite3.INTERRUPT, design 23 §5). Every dialect must satisfy
	// errors.Is(err, context.DeadlineExceeded / context.Canceled).
	driverCtxErr string
	// unit is the "amount" that makes the slow statement take a short
	// while (it is doubled until a run exceeds the default timeout);
	// huge makes it outlast every deadline in the suite.
	amountType, unit, huge string
}

// timeoutDefault is query_timeout.default in every suite. SlowNone
// must outlast it to prove the opt-out.
const timeoutDefault = 400 * time.Millisecond

func TestQueryTimeoutPostgres(t *testing.T) {
	sleep := "SELECT 1 AS one FROM (SELECT pg_sleep(:amount)) AS s;\n"
	runTimeoutSuite(t, timeoutDialect{
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
		runDSN:   func(dsn string) string { return dsn },
		schema:   "CREATE TABLE items (id bigint PRIMARY KEY, label text NOT NULL);\n",
		queries:  timeoutQueries("", sleep, "UPDATE items SET label = label WHERE id > 0;\n"),
		requires: []string{"github.com/jackc/pgx/v5"},
		imports:  []string{`"github.com/jackc/pgx/v5/pgxpool"`},
		// A pool, as in production: pgx closes a single connection whose
		// query was interrupted by its context, and the pool replaces it.
		open: `
func open(ctx context.Context, dsn string) (*gen.Queries, func(string) error) {
	pool, err := pgxpool.New(ctx, dsn)
	die(err)
	return gen.New(pool), func(s string) error { _, err := pool.Exec(ctx, s); return err }
}
`,
		amountType: "float64", unit: "0.1", huge: "30",
	})
}

func TestQueryTimeoutMySQL(t *testing.T) {
	sleep := "SELECT SLEEP(:amount) AS slept;\n"
	runTimeoutSuite(t, timeoutDialect{
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
		runDSN:     func(dsn string) string { return dsn },
		schema:     "CREATE TABLE items (id BIGINT PRIMARY KEY, label VARCHAR(64) NOT NULL);\n",
		queries:    timeoutQueries("-- @param amount: double\n", sleep, "UPDATE items SET label = label WHERE id > 0;\n"),
		requires:   []string{"github.com/go-sql-driver/mysql"},
		imports:    []string{`"database/sql"`, `_ "github.com/go-sql-driver/mysql"`},
		open:       sqlOpen("mysql", "dsn"),
		amountType: "float64", unit: "0.1", huge: "30",
	})
}

// SQLite has no sleep function: the slow statement is a recursive CTE
// counting to :amount, interrupted by the driver on context expiry.
func TestQueryTimeoutSQLite(t *testing.T) {
	count := "-- @column total: integer\n" +
		"WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < :amount)\n" +
		"SELECT count(*) AS total FROM c;\n"
	runTimeoutSuite(t, timeoutDialect{
		name:          "sqlite",
		serverVersion: "3",
		acquire: func(_ *testing.T, _ context.Context, dir string) (string, func()) {
			return filepath.Join(dir, "dev.sqlite3"), func() {}
		},
		runDSN:   func(dsn string) string { return "file:" + dsn },
		schema:   "CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL);\n",
		queries:  timeoutQueries("-- @param amount: integer\n", count, "UPDATE items SET label = label WHERE id > 0;\n"),
		requires: []string{"github.com/ncruces/go-sqlite3"},
		imports: []string{`"database/sql"`, `"github.com/ncruces/go-sqlite3"`,
			`_ "github.com/ncruces/go-sqlite3/driver"`},
		open: sqlOpen("sqlite3", "dsn"),
		// ncruces/go-sqlite3 stops the statement on context expiry via
		// sqlite3_interrupt and reports SQLITE_INTERRUPT; the generated
		// code adds the context error (runtime.CtxErr) and must keep
		// the driver's (design 23 §5).
		driverCtxErr: "errors.Is(err, sqlite3.INTERRUPT)",
		amountType:   "int64", unit: "100000", huge: "1000000000000",
	})
}

func sqlOpen(driver, dsnExpr string) string {
	return `
func open(ctx context.Context, dsn string) (*gen.Queries, func(string) error) {
	db, err := sql.Open("` + driver + `", ` + dsnExpr + `)
	die(err)
	return gen.New(db), func(s string) error { _, err := db.ExecContext(ctx, s); return err }
}
`
}

// timeoutQueries builds the shared query set over one dialect's slow
// statement (which binds :amount) and a no-op UPDATE.
func timeoutQueries(paramHint, slow, update string) map[string]string {
	return map[string]string{
		"slow":         "-- name: Slow :one\n-- @timeout 200ms\n" + paramHint + slow,
		"slow_default": "-- name: SlowDefault :one\n" + paramHint + slow,
		"slow_none":    "-- name: SlowNone :one\n-- @timeout none\n" + paramHint + slow,
		"slow_long":    "-- name: SlowLong :one\n-- @timeout 1m\n" + paramHint + slow,
		"fast":         "-- name: Fast :many\n-- @timeout 5s\nSELECT id, label FROM items ORDER BY id;\n",
		"touch":        "-- name: Touch :execrows\n-- @timeout 5s\n" + update,
	}
}

func runTimeoutSuite(t *testing.T, d timeoutDialect) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dir := t.TempDir()
	dsn, cleanup := d.acquire(t, ctx, dir)
	defer cleanup()

	writeFile(t, dir, "db/schema.sql", d.schema)
	for stem, q := range d.queries {
		writeFile(t, dir, "queries/"+stem+".sql", q)
	}
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
query_timeout:
  default: `+timeoutDefault.String()+`
`)
	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, filepath.Join(dir, "sqletch.yaml"), false,
		cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("generate: exit %d\n%s%s", code, out.String(), errW.String())
	}
	// The emitted deadlines are part of the contract: pin them.
	for file, want := range map[string]string{
		"gen/slow.sql.gen.go":         "context.WithTimeout(ctx, 200*time.Millisecond)",
		"gen/slow_default.sql.gen.go": "context.WithTimeout(ctx, 400*time.Millisecond)",
		"gen/slow_long.sql.gen.go":    "context.WithTimeout(ctx, 1*time.Minute)",
		"gen/fast.sql.gen.go":         "context.WithTimeout(ctx, 5*time.Second)",
	} {
		if src := readFile(t, dir, file); !strings.Contains(src, want) {
			t.Errorf("%s lacks %q:\n%s", file, want, src)
		}
	}
	if src := readFile(t, dir, "gen/slow_none.sql.gen.go"); strings.Contains(src, "WithTimeout") {
		t.Errorf("@timeout none still emitted a deadline:\n%s", src)
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

	main := timeoutMain
	main = strings.Replace(main, "//IMPORTS", "\t"+strings.Join(d.imports, "\n\t"), 1)
	driverErr := d.driverCtxErr
	if driverErr == "" {
		driverErr = "true"
	}
	main = strings.Replace(main, "//OPEN", d.open+`
func deadline(err error) bool { return errors.Is(err, context.DeadlineExceeded) && (`+driverErr+`) }

func canceled(err error) bool { return errors.Is(err, context.Canceled) && (`+driverErr+`) }
`, 1)
	main = strings.NewReplacer("AMOUNT_T", d.amountType, "UNIT", d.unit, "HUGE", d.huge,
		"DEFAULT_MS", timeoutDefault.String()).Replace(main)
	writeFile(t, dir, "main.go", main)

	run := func(args ...string) string {
		c := exec.CommandContext(ctx, "go", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "SQLETCH_TIMEOUT_DSN="+d.runDSN(dsn))
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	run("mod", "tidy")
	if got := run("run", "."); !strings.Contains(got, "TIMEOUT-OK") {
		t.Fatalf("harness did not report success:\n%s", got)
	}
}

const timeoutMain = `package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	sqletchruntime "github.com/moznion/go-sqletch/runtime"

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
// obs records exec events that carried an error: a deadline is an
// execution failure and must be observed like any other (design 18),
// and the observer must see the same normalized error the caller does.
type obs struct{ execErrs []error }

func (o *obs) ObserveCompose(string, sqletchruntime.ShapeKey, bool) {}
func (o *obs) ObserveExec(_ context.Context, _, _ string, _ time.Duration, _ int64, err error) {
	if err != nil {
		o.execErrs = append(o.execErrs, err)
	}
}
func (o *obs) ObserveReject(context.Context, string, error) {}

type amountT = AMOUNT_T

func timed(f func() error) (time.Duration, error) {
	start := time.Now()
	err := f()
	return time.Since(start), err
}

func main() {
	ctx := context.Background()
	q, execRaw := open(ctx, os.Getenv("SQLETCH_TIMEOUT_DSN"))
	die(execRaw("DELETE FROM items"))
	die(execRaw("INSERT INTO items (id, label) VALUES (1, 'a'), (2, 'β'), (3, '')"))
	ob := &obs{}
	q.SetObserver(ob)
	defaultTimeout, err := time.ParseDuration("DEFAULT_MS")
	die(err)
	const huge amountT = HUGE

	// 1. The directive's deadline stops a statement that would outlast it.
	took, err := timed(func() error { _, err := q.Slow(ctx, gen.SlowParams{Amount: huge}); return err })
	expect(deadline(err), "Slow: want DeadlineExceeded, got %v", err)
	expect(took < 5*time.Second, "Slow: took %v under a 200ms deadline", took)

	// 2. No directive: the config default applies.
	took, err = timed(func() error { _, err := q.SlowDefault(ctx, gen.SlowDefaultParams{Amount: huge}); return err })
	expect(deadline(err), "SlowDefault: want DeadlineExceeded, got %v", err)
	expect(took < 5*time.Second, "SlowDefault: took %v under the default deadline", took)

	// 3. A caller's shorter deadline wins over a generous directive.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	took, err = timed(func() error { _, err := q.SlowLong(short, gen.SlowLongParams{Amount: huge}); return err })
	cancel()
	expect(deadline(err), "SlowLong: want DeadlineExceeded, got %v", err)
	expect(took < 5*time.Second, "SlowLong: took %v under the caller's 200ms deadline", took)

	// 3b. A caller cancelling mid-statement reads as context.Canceled.
	cctx, ccancel := context.WithCancel(ctx)
	timer := time.AfterFunc(200*time.Millisecond, ccancel)
	took, err = timed(func() error { _, err := q.SlowNone(cctx, gen.SlowNoneParams{Amount: huge}); return err })
	timer.Stop()
	ccancel()
	expect(canceled(err), "SlowNone cancelled: want Canceled, got %v", err)
	expect(took < 5*time.Second, "SlowNone cancelled: took %v after a 200ms cancel", took)

	// 4. @timeout none opts out of the default: grow the work until one
	// successful run outlasts the default by a margin.
	amount := amountT(UNIT)
	var longest time.Duration
	for i := 0; i < 24 && longest < defaultTimeout+defaultTimeout/2; i++ {
		took, err := timed(func() error { _, err := q.SlowNone(ctx, gen.SlowNoneParams{Amount: amount}); return err })
		die(err)
		longest = took
		amount *= 2
	}
	expect(longest >= defaultTimeout+defaultTimeout/2,
		"SlowNone never outlasted the %v default (longest %v)", defaultTimeout, longest)

	// 5. A generous deadline must not cut off row iteration or scanning.
	rows, err := q.Fast(ctx, gen.FastParams{})
	die(err)
	expect(len(rows) == 3, "Fast: got %d rows", len(rows))
	n, err := q.Touch(ctx, gen.TouchParams{})
	die(err)
	expect(n >= 0, "Touch: rows affected %d", n)

	// Three deadline failures and one cancel, each observed as an exec
	// error — normalized exactly as the caller saw it.
	expect(len(ob.execErrs) == 4, "observer saw %d exec errors, want 4: %v", len(ob.execErrs), ob.execErrs)
	for i, e := range ob.execErrs[:3] {
		expect(deadline(e), "observed error %d not normalized: %v", i, e)
	}
	expect(canceled(ob.execErrs[3]), "observed cancel not normalized: %v", ob.execErrs[3])
	fmt.Println("TIMEOUT-OK")
}
`
