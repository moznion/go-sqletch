//go:build devdb

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	"github.com/jackc/pgx/v5"

	"github.com/moznion/go-sqletch/internal/devdb"
)

// The server dialects of FuzzPolicyWeaveNoLeak_SQLite (see
// policy_leak_fuzz_test.go): the same generator and property, against a
// real PostgreSQL / MySQL. They exist because the server dialects reach
// what SQLite cannot — RIGHT/FULL joins (the D2a wrong-join class),
// MySQL multi-table UPDATE and its operand prefixes — and because each
// dialect's lexer profile drives the weaver's scanners differently.
//
// Fuzzing runs several worker processes, each executing this setup, so
// every process works in its own schema (PostgreSQL) / database (MySQL)
// on the shared server: concurrent workers never see — or lock — each
// other's rows. SQLETCH_TEST_DSN / SQLETCH_TEST_MYSQL_DSN select an
// existing server (as in the rest of the devdb suite); without one each
// process starts a container, so run locally with a small -parallel.

var postgresLeakDialect = leakDialect{
	name:        "postgres",
	paramType:   "bigint",
	rightJoin:   true,
	fullJoin:    true,
	updateFrom:  true,
	deleteUsing: true,
}

var mysqlLeakDialect = leakDialect{
	name:          "mysql",
	paramType:     "bigint",
	quirkCols:     []string{"offset", "returning"},
	rightJoin:     true,
	mysqlOps:      true,
	multiTableDML: true,
}

type pgLeakEngine struct {
	ctx  context.Context
	conn *pgx.Conn
}

func (e pgLeakEngine) query(sql string, args []any) ([]string, [][]string, error) {
	rows, err := e.conn.Query(e.ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var cols []string
	for _, fd := range rows.FieldDescriptions() {
		cols = append(cols, fd.Name)
	}
	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, nil, err
		}
		row := make([]string, len(vals))
		for i, v := range vals {
			if v == nil {
				row[i] = nullCell
			} else {
				row[i] = fmt.Sprint(v)
			}
		}
		out = append(out, row)
	}
	return cols, out, rows.Err()
}

func (e pgLeakEngine) exec(sql string, args []any) error {
	_, err := e.conn.Exec(e.ctx, sql, args...)
	return err
}

func newPostgresLeakEngine(tb testing.TB) leakEngine {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	tb.Cleanup(cancel)
	// No SchemaSQL: nothing is reset; this process confines itself to
	// its own schema below.
	conn, cleanup, err := devdb.Acquire(ctx, devdb.Config{DSN: os.Getenv("SQLETCH_TEST_DSN")})
	if err != nil {
		tb.Fatalf("acquire PostgreSQL: %v", err)
	}
	tb.Cleanup(cleanup)
	schema := fmt.Sprintf("leak_%d", os.Getpid())
	eng := pgLeakEngine{ctx: ctx, conn: conn}
	q := func(s string) string { return `"` + s + `"` }
	stmts := []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE",
		"CREATE SCHEMA " + schema,
		"SET search_path TO " + schema,
	}
	stmts = append(stmts, leakSchema(postgresLeakDialect, q)...)
	stmts = append(stmts, leakData(postgresLeakDialect, q)...)
	for _, s := range stmts {
		if err := eng.exec(s, nil); err != nil {
			tb.Fatalf("%s: %v", s, err)
		}
	}
	tb.Cleanup(func() { _ = eng.exec("DROP SCHEMA IF EXISTS "+schema+" CASCADE", nil) })
	return eng
}

type mysqlLeakEngine struct{ conn *gomysqlclient.Conn }

func (e mysqlLeakEngine) query(sql string, args []any) ([]string, [][]string, error) {
	res, err := e.conn.Execute(sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer res.Close()
	if res.Resultset == nil {
		return nil, nil, nil
	}
	var cols []string
	for _, f := range res.Fields {
		// go-mysql result bytes alias pooled buffers: clone anything
		// kept past res.Close().
		cols = append(cols, strings.Clone(string(f.Name)))
	}
	var out [][]string
	for r := range res.RowNumber() {
		row := make([]string, len(cols))
		for c := range cols {
			if null, _ := res.IsNull(r, c); null {
				row[c] = nullCell
				continue
			}
			s, err := res.GetString(r, c)
			if err != nil {
				return nil, nil, err
			}
			row[c] = strings.Clone(s)
		}
		out = append(out, row)
	}
	return cols, out, nil
}

func (e mysqlLeakEngine) exec(sql string, args []any) error {
	res, err := e.conn.Execute(sql, args...)
	if err == nil {
		res.Close()
	}
	return err
}

// mysqlLeakDialectFor is mysqlLeakDialect confined to this process's
// tables (the test user cannot create a database per worker).
func mysqlLeakDialectFor() leakDialect {
	d := mysqlLeakDialect
	d.tableSuffix = fmt.Sprintf("p%d", os.Getpid())
	return d
}

func newMySQLLeakEngine(tb testing.TB) leakEngine {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	tb.Cleanup(cancel)
	conn, cleanup, err := devdb.AcquireMySQL(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
		ServerVersion: "8.4",
	})
	if err != nil {
		tb.Fatalf("acquire MySQL: %v", err)
	}
	tb.Cleanup(cleanup)
	eng := mysqlLeakEngine{conn: conn}
	d := mysqlLeakDialectFor()
	q := func(s string) string { return "`" + s + "`" }
	drop := d.rename("DROP TABLE IF EXISTS orders, acc")
	for _, s := range append(append([]string{drop}, leakSchema(d, q)...), leakData(d, q)...) {
		if err := eng.exec(s, nil); err != nil {
			tb.Fatalf("%s: %v", s, err)
		}
	}
	tb.Cleanup(func() { _ = eng.exec(drop, nil) })
	return eng
}

func FuzzPolicyWeaveNoLeak_Postgres(f *testing.F) {
	for _, s := range leakSeeds() {
		f.Add(s)
	}
	eng := newPostgresLeakEngine(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		checkNoLeak(t, postgresLeakDialect, eng, raw)
	})
}

func FuzzPolicyWeaveNoLeak_MySQL(f *testing.F) {
	for _, s := range leakSeeds() {
		f.Add(s)
	}
	eng := newMySQLLeakEngine(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		checkNoLeak(t, mysqlLeakDialectFor(), eng, raw)
	})
}

// The liveness guard per server dialect: the generator must stay inside
// what each pipeline accepts and each engine reads, or the fuzz target
// passes vacuously.
func TestPolicyWeaveNoLeak_ServerGeneratorsAreLive(t *testing.T) {
	for _, tc := range []struct {
		d   leakDialect
		eng func(testing.TB) leakEngine
	}{
		{postgresLeakDialect, newPostgresLeakEngine},
		{mysqlLeakDialectFor(), newMySQLLeakEngine},
	} {
		t.Run(tc.d.name, func(t *testing.T) {
			eng := tc.eng(t)
			usable := 0
			const n = 300
			for i := range n {
				raw := []byte{byte(i), byte(i * 7), byte(i * 13), byte(i * 31), byte(i * 61), byte(i * 3), byte(i * 11), byte(i * 17)}
				if checkNoLeak(t, tc.d, eng, raw) {
					usable++
				}
			}
			if usable < n/3 {
				t.Errorf("only %d/%d generated cases were usable on %s", usable, n, tc.d.name)
			}
			t.Logf("%s: usable %d/%d", tc.d.name, usable, n)
		})
	}
}
