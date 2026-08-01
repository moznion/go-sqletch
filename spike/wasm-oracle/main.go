// Command wasm-oracle is the v0.4 spike mandated by
// docs/design/08-later-phases.md: boot libpglite's WASI build of
// PostgreSQL 16 in-process under wazero, expose its file-transport
// wire pump as a net.Conn, and run sqletch's UNMODIFIED postgres
// oracle (pgx-based) against it — Describe, Plan (GENERIC_PLAN),
// PlanText, Snapshot, ServerVersion — measuring cold-start against
// the < 2s budget and probing error recovery.
//
// Usage:
//
//	go run ./spike/wasm-oracle [-dir <workdir>]
//
// The libpglite WASI bundle (~14 MB, PostgreSQL 16 + a pre-initdb'd
// data directory) is downloaded into the workdir on first run from
// the electric-sql/pglite-bindings repository.
//
// Transport: the engine's "socketfile" mode — wire bytes are written
// to <PGDATA>/.s.PGSQL.5432.lock.in and atomically renamed to .in;
// interactive_one() consumes them; replies appear in .s.PGSQL.5432.out
// (the same recipe the community Rust bindings use, which drive real
// PostgreSQL clients through this pump).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

const bundleURL = "https://raw.githubusercontent.com/electric-sql/pglite-bindings/main/16.x/pglite-wasi.tar.gz"

// engine is one booted WASI PostgreSQL instance.
type engine struct {
	ctx     context.Context
	mod     api.Module
	workdir string
	dead    bool
}

func (e *engine) ioPath(suffix string) string {
	return filepath.Join(e.workdir, "tmp/pglite/base/.s.PGSQL.5432"+suffix)
}

func (e *engine) call(name string, params ...uint64) (uint64, error) {
	f := e.mod.ExportedFunction(name)
	if f == nil {
		return 0, fmt.Errorf("export %s not found", name)
	}
	res, err := f.Call(e.ctx, params...)
	if err != nil {
		// A trap (PG error without sjlj recovery) kills the instance.
		e.dead = true
		return 0, fmt.Errorf("%s trapped: %w", name, err)
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

func (e *engine) hasExport(name string) bool {
	return e.mod.ExportedFunction(name) != nil
}

// sendWire forwards wire-protocol bytes via the file transport and
// returns whatever replies the tick produced.
func (e *engine) sendWire(payload []byte) ([]byte, error) {
	if err := os.WriteFile(e.ioPath(".lock.in"), payload, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(e.ioPath(".lock.in"), e.ioPath(".in")); err != nil {
		return nil, err
	}
	return e.tickAndDrain()
}

// tickAndDrain runs one interactive_one and collects reply frames.
func (e *engine) tickAndDrain() ([]byte, error) {
	if _, err := e.call("interactive_one"); err != nil {
		if e.hasExport("clear_error") {
			e.dead = false
			if _, cerr := e.call("clear_error"); cerr == nil {
				e.dead = false
			}
		}
		_ = os.Remove(e.ioPath(".in"))
		return nil, err
	}
	var out []byte
	for {
		data, err := os.ReadFile(e.ioPath(".out"))
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return nil, err
		}
		out = append(out, data...)
		_ = os.Remove(e.ioPath(".out"))
		_ = os.Remove(e.ioPath(".lock.out"))
	}
	return out, nil
}

func ensureBundle(workdir string) error {
	if _, err := os.Stat(filepath.Join(workdir, "tmp/pglite/bin/postgres.wasi")); err == nil {
		return nil
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	tarball := filepath.Join(workdir, "pglite-wasi.tar.gz")
	if _, err := os.Stat(tarball); err != nil {
		fmt.Printf("downloading %s ...\n", bundleURL)
		resp, err := http.Get(bundleURL)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download: %s", resp.Status)
		}
		f, err := os.Create(tarball)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	cmd := exec.Command("tar", "xzf", "pglite-wasi.tar.gz")
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("extract: %v\n%s", err, out)
	}
	return nil
}

// boot instantiates a fresh engine from the compiled module.
func boot(ctx context.Context, r wazero.Runtime, compiled wazero.CompiledModule, workdir string, logOut io.Writer) (*engine, error) {
	devDir := filepath.Join(workdir, "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		return nil, err
	}
	seed := make([]byte, 128)
	for i := range seed {
		seed[i] = byte(i * 37)
	}
	if err := os.WriteFile(filepath.Join(devDir, "urandom"), seed, 0o644); err != nil {
		return nil, err
	}
	cfg := wazero.NewModuleConfig().
		WithArgs("/tmp/pglite/bin/postgres", "--single", "postgres").
		WithEnv("ENVIRONMENT", "wasi-embed").
		WithEnv("PREFIX", "/tmp/pglite").
		WithEnv("PGDATA", "/tmp/pglite/base").
		WithEnv("PGSYSCONFDIR", "/tmp/pglite").
		WithEnv("PGUSER", "postgres").
		WithEnv("PGDATABASE", "template1").
		WithEnv("MODE", "REACT").
		WithEnv("REPL", "N").
		WithEnv("TZ", "UTC").
		WithEnv("PGTZ", "UTC").
		WithEnv("PATH", "/tmp/pglite/bin").
		WithStdout(logOut).
		WithStderr(logOut).
		WithFSConfig(wazero.NewFSConfig().
			WithDirMount(filepath.Join(workdir, "tmp"), "/tmp").
			WithDirMount(devDir, "/dev")).
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(nil).
		WithName("") // anonymous: allows repeated instantiation
	mod, err := r.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return nil, err
	}
	e := &engine{ctx: ctx, mod: mod, workdir: workdir}
	// Stale transport files from a previous run confuse the pump.
	for _, s := range []string{".in", ".lock.in", ".out", ".lock.out"} {
		_ = os.Remove(e.ioPath(s))
	}
	if _, err := e.call("pg_initdb"); err != nil {
		return nil, err
	}
	if e.hasExport("pgl_backend") {
		if _, err := e.call("pgl_backend"); err != nil {
			return nil, err
		}
	}
	if e.hasExport("use_socketfile") {
		if _, err := e.call("use_socketfile"); err != nil {
			return nil, err
		}
	} else {
		fmt.Println("NOTE: no use_socketfile export; file transport may not work")
	}
	if _, err := e.call("use_wire", 1); err != nil {
		return nil, err
	}
	return e, nil
}

func connect(ctx context.Context, e *engine) (*pgx.Conn, error) {
	cc, err := pgx.ParseConfig("postgres://postgres:password@pglite:5432/template1?sslmode=disable")
	if err != nil {
		return nil, err
	}
	cc.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return newEngineConn(e), nil
	}
	cc.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		return []string{"127.0.0.1"}, nil
	}
	return pgx.ConnectConfig(ctx, cc)
}

func main() {
	dir := flag.String("dir", "spike-workdir", "working directory (bundle + data dir)")
	flag.Parse()
	ctx := context.Background()

	if err := ensureBundle(*dir); err != nil {
		log.Fatal(err)
	}
	logOut, err := os.Create(filepath.Join(*dir, "pg.log"))
	if err != nil {
		log.Fatal(err)
	}

	t0 := time.Now()
	wasmBytes, err := os.ReadFile(filepath.Join(*dir, "tmp/pglite/bin/postgres.wasi"))
	if err != nil {
		log.Fatal(err)
	}
	cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(*dir, "wazero-cache"))
	if err != nil {
		log.Fatal(err)
	}
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
	defer func() { _ = r.Close(ctx) }()
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		log.Fatalf("compile: %v", err)
	}
	tCompile := time.Since(t0)

	t1 := time.Now()
	e, err := boot(ctx, r, compiled, *dir, logOut)
	if err != nil {
		log.Fatalf("boot: %v", err)
	}
	tBoot := time.Since(t1)

	t2 := time.Now()
	conn, err := connect(ctx, e)
	if err != nil {
		log.Fatalf("pgx connect: %v", err)
	}
	tConnect := time.Since(t2)

	var version string
	if err := conn.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		log.Fatalf("version: %v", err)
	}
	fmt.Printf("version: %s\n", version)

	for _, ddl := range []string{
		"CREATE SCHEMA IF NOT EXISTS public",
		"SET search_path = public",
		"DROP TABLE IF EXISTS users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL, status text NOT NULL, org_id bigint, created_at timestamptz NOT NULL DEFAULT now())",
	} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			log.Fatalf("ddl %q: %v", ddl, err)
		}
	}

	// ---- sqletch's actual oracle, unmodified ----------------------------
	oracle := postgres.NewOracle(conn)

	t3 := time.Now()
	desc, err := oracle.Describe(ctx, "SELECT u.id, u.email, u.org_id FROM users AS u WHERE u.status = $1 LIMIT $2")
	if err != nil {
		log.Fatalf("oracle.Describe: %v", err)
	}
	fmt.Printf("Describe in %v:\n  params: %+v\n", time.Since(t3), desc.Params)
	for _, c := range desc.Columns {
		fmt.Printf("  col %s oid=%d srcrel=%d srcatt=%d\n", c.Name, c.Type.OID, c.SrcRel, c.SrcAtt)
	}

	t4 := time.Now()
	if err := oracle.Plan(ctx, "SELECT u.id FROM users AS u WHERE u.status = $1"); err != nil {
		log.Fatalf("oracle.Plan: %v", err)
	}
	fmt.Printf("Plan (GENERIC_PLAN) ok in %v\n", time.Since(t4))

	t5 := time.Now()
	plan, err := oracle.PlanText(ctx, "SELECT u.id FROM users AS u WHERE u.status = $1")
	if err != nil {
		log.Fatalf("oracle.PlanText: %v", err)
	}
	fmt.Printf("PlanText in %v:\n%s\n", time.Since(t5), plan)

	t6 := time.Now()
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		log.Fatalf("oracle.Snapshot: %v", err)
	}
	fmt.Printf("Snapshot in %v: %d tables\n", time.Since(t6), len(cat.Tables))
	if u := cat.Lookup("users"); u != nil {
		for _, c := range u.Cols {
			fmt.Printf("  users.%s type=%s notnull=%v hasdefault=%v\n", c.Name, c.TypeName, c.NotNull, c.HasDefault)
		}
	}

	sv, err := oracle.ServerVersion(ctx)
	if err != nil {
		log.Fatalf("ServerVersion: %v", err)
	}
	fmt.Printf("ServerVersion: %s\n", sv)

	// Identity columns exercise sequence machinery (ALTER SEQUENCE OWNED
	// BY) — a previously suspect path; verify on clean state.
	if _, err := conn.Exec(ctx, "CREATE TABLE ident_t (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v text)"); err != nil {
		fmt.Printf("IDENTITY DDL FAILED: %v\n", err)
	} else {
		fmt.Printf("IDENTITY DDL OK\n")
	}

	// ---- error recovery: the oracle MUST survive a bad query ------------
	t7 := time.Now()
	_, err = oracle.Describe(ctx, "SELECT nonexistent_column FROM users")
	fmt.Printf("bad describe -> err=%v (in %v, engine dead=%v)\n", err, time.Since(t7), e.dead)
	if _, err := oracle.Describe(ctx, "SELECT id FROM users"); err != nil {
		fmt.Printf("RECOVERY FAILED: engine unusable after error: %v\n", err)
		// Reboot cost — the mitigation if in-instance recovery is absent.
		t8 := time.Now()
		e2, err := boot(ctx, r, compiled, *dir, logOut)
		if err != nil {
			log.Fatalf("reboot: %v", err)
		}
		conn2, err := connect(ctx, e2)
		if err != nil {
			log.Fatalf("reconnect: %v", err)
		}
		fmt.Printf("REBOOT in %v\n", time.Since(t8))
		if _, err := conn2.Exec(ctx, "SET search_path = public"); err != nil {
			log.Fatalf("post-reboot bootstrap: %v", err)
		}
		if _, err := postgres.NewOracle(conn2).Describe(ctx, "SELECT id FROM users"); err != nil {
			log.Fatalf("post-reboot describe: %v", err)
		}
		fmt.Printf("post-reboot describe OK\n")
	} else {
		fmt.Printf("RECOVERY OK: engine survives query errors\n")
	}

	fmt.Printf("\ntimings: compile=%v boot=%v connect=%v total=%v\n",
		tCompile, tBoot, tConnect, time.Since(t0))
}
