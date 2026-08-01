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
