//go:build devdb

package e2e_test

import (
	"testing"
	"time"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/nullability"
	"github.com/moznion/go-sqletch/internal/shape"
)

// The verdict-soundness property (design 05 §5): for EVERY corpus
// template and EVERY enumerable shape, execute against NULL-heavy seed
// data and require that no column the analyzer claims non-nullable
// ever returns NULL. Unlike the hand-written adversarial suite
// (nullability_soundness_devdb_test.go) this lane needs no
// per-construct foresight — any future analyzer or frontend change
// that reintroduces a NULL-into-value hole on a corpus construct
// fails here mechanically.
//
// The check is one-directional: nullable verdicts are never asserted
// against (false positives cost an Option, not a panic), and a shape
// returning zero rows simply proves nothing for that shape.

// propertyParamValues binds every corpus parameter by name. Values are
// chosen to MATCH the seed data (a filter that excludes all rows
// proves nothing).
var propertyParamValues = map[string]any{
	"organization_id": int64(1),
	"status":          "active",
	"email_prefix":    "a",
	"created_after":   time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	"since":           time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	"limit":           int64(100),
	"tenant_id":       int64(1),
	"after_id":        int64(1_000_000),
	"min_actions":     int64(0),
	"action":          "login",
	"id":              int64(1),
	"new_email":       "new@example.com",
	"nickname":        "nick",
	"bio":             "bio",
	"email":           "fresh@example.com",
	"min_id":          int64(0),
	"statuses":        []string{"active", "banned"},
	"scope_tenant_id": int64(1),
	"scope_status":    "active",
	"scope_prefix":    "a",
	"vol_min_users":   int64(0),
	"vol_max_id":      int64(0),
}

// paramArgs resolves a shape rendering's positional binds by name.
// elemIn flattens slice values to a single element for dialects whose
// @in expansion binds one placeholder per element.
func paramArgs(t *testing.T, seq []string, elemIn bool) []any {
	t.Helper()
	args := make([]any, len(seq))
	for i, name := range seq {
		v, ok := propertyParamValues[name]
		if !ok {
			t.Fatalf("no property value for param %q — extend propertyParamValues", name)
		}
		if s, isSlice := v.([]string); isSlice && elemIn {
			v = s[0]
		}
		args[i] = v
	}
	return args
}

// checkVerdicts fails for every column that saw a NULL despite a
// non-nullable verdict.
func checkVerdicts(t *testing.T, label string, verdict, sawNull []bool, desc dialect.Desc) {
	t.Helper()
	for i := range verdict {
		if i < len(sawNull) && !verdict[i] && sawNull[i] {
			name := ""
			if i < len(desc.Columns) {
				name = desc.Columns[i].Name
			}
			t.Errorf("%s: column %d %q claimed non-nullable but execution returned NULL", label, i, name)
		}
	}
}

const propertySeedSQL = `
INSERT INTO users (email, status, tenant_id, org_id, nickname, bio) VALUES
  ('a1@example.com', 'active', 1, 10,   'nick', 'bio'),
  ('a2@example.com', 'active', 1, NULL, NULL,   NULL),
  ('b1@example.com', 'banned', 1, NULL, NULL,   NULL),
  ('c1@example.com', 'active', 2, 20,   NULL,   'x');
INSERT INTO organization_users (user_id, organization_id) VALUES (1, 1), (2, 1);
INSERT INTO audit_logs (tenant_id, actor_id, action) VALUES
  (1, 1, 'login'), (1, NULL, 'cron'), (1, 2, 'login'), (2, NULL, 'x');
`

func TestPropertyVerdictSoundness(t *testing.T) {
	conn, ctx := acquire(t)
	oracle := postgres.NewOracle(conn)
	if _, err := conn.Exec(ctx, propertySeedSQL); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for name, src := range corpus {
		t.Run(name, func(t *testing.T) {
			q := compile(t, src)
			rs, err := ast.Renderings(postgres.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			descs := make([]dialect.Desc, len(rs))
			for i := range rs {
				if descs[i], err = oracle.Describe(ctx, rs[i].SQL); err != nil {
					t.Fatalf("describe rendering %d: %v", i, err)
				}
			}
			verdict, err := nullability.AnalyzeAll(postgres.Frontend{}, rs, descs, cat, nil)
			if err != nil {
				t.Fatal(err)
			}

			keys, truncated := shape.Enumerate(q, 4096)
			if truncated {
				t.Fatal("corpus template exceeds the test cap")
			}
			for _, k := range keys {
				r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
				if err != nil {
					t.Fatalf("shape %s: render: %v", k, err)
				}
				args := paramArgs(t, r.ParamsSeq, false)

				// Mutating templates (UPDATE/INSERT … RETURNING) run in
				// a rolled-back transaction so every shape sees the
				// same seed.
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := tx.Query(ctx, r.SQL, args...)
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Fatalf("shape %s: execute: %v\nSQL:\n%s", k, err, r.SQL)
				}
				sawNull := make([]bool, len(verdict))
				for rows.Next() {
					vals, err := rows.Values()
					if err != nil {
						t.Fatal(err)
					}
					for i, v := range vals {
						if v == nil && i < len(sawNull) {
							sawNull[i] = true
						}
					}
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					_ = tx.Rollback(ctx)
					t.Fatalf("shape %s: %v", k, err)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				checkVerdicts(t, "shape "+k.String(), verdict, sawNull, descs[0])
			}
		})
	}
}

const mysqlPropertySeedSQL1 = `
INSERT INTO users (email, status, tenant_id, org_id, nickname, bio) VALUES
  ('a1@example.com', 'active', 1, 10,   'nick', 'bio'),
  ('a2@example.com', 'active', 1, NULL, NULL,   NULL),
  ('b1@example.com', 'banned', 1, NULL, NULL,   NULL),
  ('c1@example.com', 'active', 2, 20,   NULL,   'x')`

const mysqlPropertySeedSQL2 = `
INSERT INTO organization_users (user_id, organization_id) VALUES (1, 1), (2, 1)`

func TestMySQLPropertyVerdictSoundness(t *testing.T) {
	conn, ctx := acquireMySQL(t)
	oracle := mysql.NewOracle(conn)
	for _, stmt := range []string{mysqlPropertySeedSQL1, mysqlPropertySeedSQL2} {
		if _, err := conn.Execute(stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for name, src := range mysqlCorpus {
		t.Run(name, func(t *testing.T) {
			q := compileMySQL(t, src)
			rs, err := ast.Renderings(mysql.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			descs := make([]dialect.Desc, len(rs))
			for i := range rs {
				if descs[i], err = oracle.Describe(ctx, rs[i].SQL); err != nil {
					t.Fatalf("describe rendering %d: %v", i, err)
				}
			}
			verdict, err := nullability.AnalyzeAll(mysql.Frontend{}, rs, descs, cat, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(descs[0].Columns) == 0 {
				return // :execrows — nothing to cross-check, and no mutation wanted
			}

			keys, truncated := shape.EnumerateExpand(q, 4096, true)
			if truncated {
				t.Fatal("corpus template exceeds the test cap")
			}
			for _, k := range keys {
				r, err := ast.RenderShape(mysql.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
				if err != nil {
					t.Fatalf("shape %s: render: %v", k, err)
				}
				args := paramArgs(t, r.ParamsSeq, true)
				stmt, err := conn.Prepare(r.SQL)
				if err != nil {
					t.Fatalf("shape %s: prepare: %v\nSQL:\n%s", k, err, r.SQL)
				}
				res, err := stmt.Execute(args...)
				if err != nil {
					_ = stmt.Close()
					t.Fatalf("shape %s: execute: %v\nSQL:\n%s", k, err, r.SQL)
				}
				sawNull := make([]bool, len(verdict))
				for _, row := range res.Values {
					for i := range row {
						if i < len(sawNull) && row[i].Value() == nil {
							sawNull[i] = true
						}
					}
				}
				res.Close()
				_ = stmt.Close()
				checkVerdicts(t, "shape "+k.String(), verdict, sawNull, descs[0])
			}
		})
	}
}

const sqlitePropertySeedSQL = `
INSERT INTO users (email, status, tenant_id, org_id, nickname, bio) VALUES
  ('a1@example.com', 'active', 1, 10,   'nick', 'bio'),
  ('a2@example.com', 'active', 1, NULL, NULL,   NULL),
  ('b1@example.com', 'banned', 1, NULL, NULL,   NULL),
  ('c1@example.com', 'active', 2, 20,   NULL,   'x');
INSERT INTO organization_users (user_id, organization_id) VALUES (1, 1), (2, 1);
`

func TestSQLitePropertyVerdictSoundness(t *testing.T) {
	conn, ctx := acquireSQLite(t)
	oracle := sqlite.NewOracle(conn)
	if err := conn.Exec(sqlitePropertySeedSQL); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for name, src := range sqliteCorpus {
		t.Run(name, func(t *testing.T) {
			q := compileSQLite(t, src)
			rs, err := ast.Renderings(sqlite.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			descs := make([]dialect.Desc, len(rs))
			for i := range rs {
				if descs[i], err = oracle.Describe(ctx, rs[i].SQL); err != nil {
					t.Fatalf("describe rendering %d: %v", i, err)
				}
			}
			verdict, err := nullability.AnalyzeAll(sqlite.Frontend{}, rs, descs, cat, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(descs[0].Columns) == 0 {
				return // :execrows
			}

			keys, truncated := shape.EnumerateExpand(q, 4096, true)
			if truncated {
				t.Fatal("corpus template exceeds the test cap")
			}
			for _, k := range keys {
				r, err := ast.RenderShape(sqlite.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
				if err != nil {
					t.Fatalf("shape %s: render: %v", k, err)
				}
				args := paramArgs(t, r.ParamsSeq, true)
				stmt, _, err := conn.Prepare(r.SQL)
				if err != nil {
					t.Fatalf("shape %s: prepare: %v\nSQL:\n%s", k, err, r.SQL)
				}
				for i, v := range args {
					var bindErr error
					switch tv := v.(type) {
					case int64:
						bindErr = stmt.BindInt64(i+1, tv)
					case string:
						bindErr = stmt.BindText(i+1, tv)
					default:
						t.Fatalf("shape %s: unsupported bind type %T", k, v)
					}
					if bindErr != nil {
						t.Fatalf("shape %s: bind %d: %v", k, i+1, bindErr)
					}
				}
				sawNull := make([]bool, len(verdict))
				for stmt.Step() {
					for i := range sawNull {
						if stmt.ColumnType(i) == sqlite3.NULL {
							sawNull[i] = true
						}
					}
				}
				if err := stmt.Err(); err != nil {
					t.Fatalf("shape %s: %v\nSQL:\n%s", k, err, r.SQL)
				}
				_ = stmt.Close()
				checkVerdicts(t, "shape "+k.String(), verdict, sawNull, descs[0])
			}
		})
	}
}
