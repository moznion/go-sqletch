package sqlite

import (
	"context"
	"strings"
	"testing"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/dialect"
)

func testConn(t *testing.T) *sqlite3.Conn {
	t.Helper()
	conn, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Exec(`
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL,
    status     TEXT NOT NULL,
    org_id     INTEGER,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE audit_logs (
    id        INTEGER PRIMARY KEY,
    tenant_id INTEGER NOT NULL,
    action    TEXT NOT NULL
);
`); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestOracle_Describe(t *testing.T) {
	o := NewOracle(testConn(t))
	ctx := context.Background()

	desc, err := o.Describe(ctx, "SELECT u.id, u.email, u.org_id, count(*) AS n FROM users AS u WHERE u.status = ? LIMIT ?")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(desc.Params) != 2 || desc.Params[0].OID != 0 {
		t.Errorf("params must exist untyped: %+v", desc.Params)
	}
	if len(desc.Columns) != 4 {
		t.Fatalf("columns = %+v", desc.Columns)
	}
	wantTypes := []uint32{TypeInteger, TypeText, TypeInteger, 0} // count(*) has no decltype
	for i, want := range wantTypes {
		if desc.Columns[i].Type.OID != want {
			t.Errorf("col %d (%s) type = %d, want %d", i, desc.Columns[i].Name, desc.Columns[i].Type.OID, want)
		}
	}
	// Source identity resolves through the snapshot for direct refs.
	if desc.Columns[0].SrcRel == 0 || desc.Columns[0].SrcAtt != 1 {
		t.Errorf("col id source = (%d,%d)", desc.Columns[0].SrcRel, desc.Columns[0].SrcAtt)
	}
	if desc.Columns[3].SrcRel != 0 {
		t.Errorf("expression column must have no source: %+v", desc.Columns[3])
	}

	// Errors carry a position when SQLite reports an offset.
	_, err = o.Describe(ctx, "SELECT nonexistent FROM users")
	var oe *dialect.OracleError
	if err == nil {
		t.Fatal("bad describe must fail")
	}
	if !asOracleErr(err, &oe) {
		t.Fatalf("error type = %T", err)
	}

	// The oracle survives errors (no reboot needed, unlike WASI PG).
	if _, err := o.Describe(ctx, "SELECT id FROM users"); err != nil {
		t.Fatalf("oracle unusable after error: %v", err)
	}
}

func TestOracle_PlanAndPlanText(t *testing.T) {
	o := NewOracle(testConn(t))
	ctx := context.Background()

	if err := o.Plan(ctx, "SELECT u.id FROM users AS u WHERE u.status = ?"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	text, err := o.PlanText(ctx, "SELECT u.id FROM users AS u WHERE u.status = ?")
	if err != nil {
		t.Fatalf("plan text: %v", err)
	}
	if !strings.Contains(text, "SCAN") {
		t.Errorf("plan text = %q", text)
	}
	if err := o.Plan(ctx, "SELECT nope FROM users"); err == nil {
		t.Error("plan of broken SQL must fail")
	}
}

func TestOracle_Snapshot(t *testing.T) {
	o := NewOracle(testConn(t))
	cat, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Tables) != 2 {
		t.Fatalf("tables = %+v", cat.Tables)
	}
	u := cat.Lookup("users")
	if u == nil {
		t.Fatal("users missing")
	}
	if c := u.Col("email"); c == nil || !c.NotNull || c.TypeOID != TypeText {
		t.Errorf("users.email = %+v", c)
	}
	if c := u.Col("org_id"); c == nil || c.NotNull {
		t.Errorf("users.org_id must be nullable: %+v", c)
	}
	if c := u.Col("created_at"); c == nil || !c.HasDefault || c.TypeOID != TypeTime {
		t.Errorf("users.created_at = %+v", c)
	}
	// INTEGER PRIMARY KEY: rowid alias — implicitly NOT NULL and
	// auto-assigned.
	if c := u.Col("id"); c == nil || !c.NotNull || !c.HasDefault {
		t.Errorf("users.id = %+v", c)
	}
}

// SQLite marks a genuine INTEGER PRIMARY KEY rowid alias as implicitly
// NOT NULL + auto-assigned, but that treatment is UNSOUND for the
// look-alikes `pragma table_info` cannot distinguish from it: `INTEGER
// PRIMARY KEY DESC` (not a rowid alias — accepts NULL), the first
// INTEGER column of a composite `PRIMARY KEY(a,b)` (pk==1 is a position
// within the key, not aliasing), and a WITHOUT ROWID single INTEGER PK
// (no auto rowid, so no implicit default). Narrowing those to NOT NULL
// scans a genuine NULL into a non-Option field; a spurious HasDefault
// lets an INSERT that omits the column verify offline yet fail on the
// engine.
func TestOracle_Snapshot_RowidAliasHeuristic(t *testing.T) {
	cases := []struct {
		name       string
		ddl        string
		col        string
		notNull    bool
		hasDefault bool
	}{
		{
			// Genuine rowid alias — behavior must be UNCHANGED.
			name: "single INTEGER PRIMARY KEY (rowid alias)",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT)",
			col:  "id", notNull: true, hasDefault: true,
		},
		{
			name: "INTEGER PRIMARY KEY ASC (still a rowid alias)",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY ASC, x TEXT)",
			col:  "id", notNull: true, hasDefault: true,
		},
		{
			name: "INTEGER PRIMARY KEY AUTOINCREMENT (rowid alias)",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, x TEXT)",
			col:  "id", notNull: true, hasDefault: true,
		},
		{
			// DESC disables the alias — the column accepts NULL.
			name: "INTEGER PRIMARY KEY DESC (NOT a rowid alias)",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY DESC, x TEXT)",
			col:  "id", notNull: false, hasDefault: false,
		},
		{
			// pk==1 is the position within the composite key, not aliasing.
			name: "composite PRIMARY KEY(a,b), first INTEGER col",
			ddl:  "CREATE TABLE t (a INTEGER, b TEXT, PRIMARY KEY (a, b))",
			col:  "a", notNull: false, hasDefault: false,
		},
		{
			// No auto rowid: NotNull is real (table_info reports it), but
			// there is NO implicit default — the INSERT must supply it.
			name: "WITHOUT ROWID single INTEGER PK",
			ddl:  "CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT) WITHOUT ROWID",
			col:  "id", notNull: true, hasDefault: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := sqlite3.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			if err := conn.Exec(tc.ddl); err != nil {
				t.Fatalf("ddl: %v", err)
			}
			o := NewOracle(conn)
			cat, err := o.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tbl := cat.Lookup("t")
			if tbl == nil {
				t.Fatal("table t missing from snapshot")
			}
			c := tbl.Col(tc.col)
			if c == nil {
				t.Fatalf("column %s missing", tc.col)
			}
			if c.NotNull != tc.notNull {
				t.Errorf("%s.NotNull = %v, want %v", tc.col, c.NotNull, tc.notNull)
			}
			if c.HasDefault != tc.hasDefault {
				t.Errorf("%s.HasDefault = %v, want %v", tc.col, c.HasDefault, tc.hasDefault)
			}
		})
	}
}

// The metadata the heuristic produces must agree with the live engine:
// a column marked nullable must actually accept NULL, and a column
// without a default must actually be required by an INSERT that omits
// it. This closes the loop the pure-metadata assertions above leave open.
func TestOracle_Snapshot_RowidAliasMatchesEngine(t *testing.T) {
	t.Run("DESC PK accepts NULL yet metadata said NOT NULL before the fix", func(t *testing.T) {
		conn, err := sqlite3.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY DESC, x TEXT)"); err != nil {
			t.Fatal(err)
		}
		// The engine really does store a NULL in this "PRIMARY KEY" column.
		if err := conn.Exec("INSERT INTO t (id, x) VALUES (NULL, 'a')"); err != nil {
			t.Fatalf("engine rejected NULL PK insert: %v", err)
		}
		cat, err := NewOracle(conn).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if c := cat.Lookup("t").Col("id"); c == nil || c.NotNull {
			t.Errorf("id must be nullable to match the engine: %+v", c)
		}
	})

	t.Run("WITHOUT ROWID PK is required at INSERT so HasDefault must be false", func(t *testing.T) {
		conn, err := sqlite3.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT) WITHOUT ROWID"); err != nil {
			t.Fatal(err)
		}
		// Omitting the PK fails on the engine — there is no auto rowid.
		if err := conn.Exec("INSERT INTO t (x) VALUES ('a')"); err == nil {
			t.Fatal("engine unexpectedly allowed a WITHOUT ROWID insert omitting the PK")
		}
		cat, err := NewOracle(conn).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if c := cat.Lookup("t").Col("id"); c == nil || c.HasDefault {
			t.Errorf("id must not be defaulted to match the engine: %+v", c)
		}
	})
}

func TestOracle_ServerVersion(t *testing.T) {
	o := NewOracle(testConn(t))
	v, err := o.ServerVersion(context.Background())
	if err != nil || !strings.HasPrefix(v, "3.") {
		t.Fatalf("version = %q, %v", v, err)
	}
}

func asOracleErr(err error, out **dialect.OracleError) bool {
	oe, ok := err.(*dialect.OracleError)
	if ok {
		*out = oe
	}
	return ok
}
