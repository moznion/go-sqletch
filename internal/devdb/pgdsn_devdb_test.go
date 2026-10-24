//go:build devdb

package devdb

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Security audit F4 (owner decision 2026-10-21, "loopback only"): the
// DSN policy must not break the ordinary local setup it exists to keep
// — a loopback database.dsn WITHOUT a password, authenticated by the
// developer's PGPASSWORD. Pinned against a real PostgreSQL server
// (SQLETCH_TEST_DSN, or a disposable container).
func TestAcquire_LoopbackDSNUsesAmbientPassword(t *testing.T) {
	ctx := context.Background()
	for _, k := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGSERVICE", "PGSERVICEFILE", "PGREQUIREAUTH", "PGSSLMODE"} {
		t.Setenv(k, "")
	}

	// Any reachable server: the one the suite was given, else a fresh
	// container (its DSN states its own password).
	base, cleanup, err := AcquireDSN(ctx, Config{DSN: os.Getenv("SQLETCH_TEST_DSN"), AllowDestructive: true})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	cc, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if cc.Password == "" {
		t.Fatal("the base DSN must carry a password for this test to move it into PGPASSWORD")
	}

	// The same server, password moved out of the DSN into the
	// environment.
	host := cc.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	dsn := fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable", cc.User, host, cc.Port, cc.Database)
	t.Setenv("PGPASSWORD", cc.Password)

	conn, cleanup2, err := Acquire(ctx, Config{DSN: dsn, AllowDestructive: true, SchemaSQL: []string{"CREATE TABLE f4_probe (id int)"}})
	defer cleanup2()

	ip, perr := netip.ParseAddr(cc.Host)
	loopback := strings.EqualFold(cc.Host, "localhost") || strings.HasPrefix(cc.Host, "/") || (perr == nil && ip.IsLoopback())
	if !loopback {
		// A remote Docker host / test server: the must-stay half applies
		// instead — the ambient password is withheld, and the failure
		// says why.
		if err == nil || !strings.Contains(err.Error(), "not loopback") {
			t.Fatalf("non-loopback %q: want the ambient password withheld, got err=%v", cc.Host, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("loopback DSN with PGPASSWORD must still connect: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM f4_probe").Scan(&n); err != nil {
		t.Fatalf("schema was not applied through the policy-built connection: %v", err)
	}
}
