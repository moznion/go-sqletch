package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Security audit F4 (2026-10-21): database.dsn comes from the committed
// sqletch.yaml, i.e. from whoever authored the repository, yet pgx fills
// every key the DSN omits from the RUNNING developer's environment
// (PGPASSWORD, PGPASSFILE / ~/.pgpass, PGSERVICE, …) and by default
// answers a server's cleartext-password request. `dsn:
// postgres://attacker.example/x` therefore shipped the developer's
// PGPASSWORD to a host the repository chose. Owner decision
// 2026-10-21 ("loopback only"): restrict the DSN's keys, refuse
// multi-host, let an ambient password reach only a loopback host, and
// demand SCRAM from any other host. These tests pin each half.

// isolatePGEnv gives the test a hermetic libpq environment: no PG*
// variable leaks in from the developer's shell, and HOME points at an
// empty directory so ~/.pgpass, ~/.pg_service.conf and
// ~/.postgresql/* cannot either.
func isolatePGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE",
		"PGAPPNAME", "PGCONNECT_TIMEOUT", "PGSSLMODE", "PGSSLKEY", "PGSSLCERT",
		"PGSSLSNI", "PGSSLROOTCERT", "PGSSLPASSWORD", "PGSSLNEGOTIATION",
		"PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE", "PGTZ", "PGOPTIONS",
		"PGMINPROTOCOLVERSION", "PGMAXPROTOCOLVERSION", "PGCHANNELBINDING",
		"PGREQUIREAUTH",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PGUSER", "dev") // skip the OS-user lookup; harmless
}

// writePassfile installs a ~/.pgpass-style file whose wildcard entry
// matches every host, as a real developer's often does.
func writePassfile(t *testing.T, password string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(p, []byte("*:*:*:*:"+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGPASSFILE", p)
}

func TestDevConnConfig_ForbiddenKeysRejected(t *testing.T) {
	isolatePGEnv(t)
	// Every key outside the allow-list is refused BEFORE pgx touches the
	// filesystem or the network — including the file-reading keys the
	// finding names and keys pgx would silently turn into runtime
	// parameters. Both DSN spellings.
	for _, tc := range []struct{ dsn, key string }{
		{"postgres://h/db?passfile=/tmp/x", "passfile"},
		{"postgres://h/db?service=dev", "service"},
		{"postgres://h/db?servicefile=/tmp/svc", "servicefile"},
		{"postgres://h/db?sslkey=/tmp/k", "sslkey"},
		{"postgres://h/db?sslcert=/tmp/c", "sslcert"},
		{"postgres://h/db?sslrootcert=/tmp/r", "sslrootcert"},
		{"postgres://h/db?sslpassword=x", "sslpassword"},
		{"postgres://h/db?require_auth=password", "require_auth"},
		{"postgres://h/db?target_session_attrs=any", "target_session_attrs"},
		{"postgres://h/db?search_path=x", "search_path"},
		{"host=h passfile=/tmp/x", "passfile"},
		{"host=h service=dev", "service"},
		{"host=h servicefile=/tmp/svc", "servicefile"},
		{"host=h sslkey=/tmp/k", "sslkey"},
		{"host=h hostaddr=10.0.0.1", "hostaddr"},
	} {
		_, _, err := DevConnConfig(tc.dsn)
		var pe *DSNPolicyError
		if !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want *DSNPolicyError", tc.dsn, err)
			continue
		}
		if pe.Key != tc.key {
			t.Errorf("%s: Key = %q, want %q", tc.dsn, pe.Key, tc.key)
		}
	}
}

func TestDevConnConfig_AllowedKeysAccepted(t *testing.T) {
	isolatePGEnv(t)
	// The accepted neighbor: everything a dev-database DSN ordinarily
	// says, in both spellings.
	for _, dsn := range []string{
		"postgres://u:pw@127.0.0.1:5432/db?sslmode=disable&connect_timeout=5&application_name=sqletch",
		"postgresql://u@localhost/db?ssl=true",
		"host=127.0.0.1 port=5432 dbname=db user=u password=pw sslmode=disable connect_timeout=5 application_name=x",
		"dbname=db",
	} {
		if _, _, err := DevConnConfig(dsn); err != nil {
			t.Errorf("%s: unexpected error %v", dsn, err)
		}
	}
}

func TestDevConnConfig_MultiHostRejected(t *testing.T) {
	isolatePGEnv(t)
	for _, dsn := range []string{
		"postgres://127.0.0.1,attacker.example/db",
		"postgres://localhost:5432,localhost:5433/db",
		"host=127.0.0.1,attacker.example dbname=db",
	} {
		_, _, err := DevConnConfig(dsn)
		var pe *DSNPolicyError
		if !errors.As(err, &pe) || !pe.MultiHost {
			t.Errorf("%s: err = %v, want a multi-host *DSNPolicyError", dsn, err)
		}
	}
	// A single host with sslmode=prefer also yields a pgx fallback (the
	// plaintext retry of the same host); that is NOT multi-host.
	if _, _, err := DevConnConfig("postgres://127.0.0.1/db?sslmode=prefer"); err != nil {
		t.Errorf("single host with TLS fallback: %v", err)
	}
}

func TestDevConnConfig_AmbientPasswordReachesOnlyLoopback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dsn      string
		loopback bool
	}{
		{"ipv4 loopback", "postgres://u@127.0.0.1/db", true},
		{"other 127/8 address", "postgres://u@127.1.2.3/db", true},
		{"ipv6 loopback", "postgres://u@[::1]/db", true},
		{"localhost", "postgres://u@localhost/db", true},
		{"LOCALHOST", "postgres://u@LOCALHOST/db", true},
		{"unix socket", "host=/var/run/postgresql dbname=db", true},
		{"remote name", "postgres://u@attacker.example/db", false},
		{"remote ip", "postgres://u@203.0.113.7/db", false},
		{"private ip is not loopback", "postgres://u@10.0.0.5/db", false},
		{"localhost lookalike", "postgres://u@localhost.attacker.example/db", false},
		{"unspecified address", "postgres://u@0.0.0.0/db", false},
		{"remote kv", "host=attacker.example dbname=db", false},
	} {
		t.Run(tc.name+"/PGPASSWORD", func(t *testing.T) {
			isolatePGEnv(t)
			t.Setenv("PGPASSWORD", "ambient-secret")
			cfg, dropped, err := DevConnConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if tc.loopback {
				if cfg.Password != "ambient-secret" || dropped {
					t.Errorf("loopback: Password = %q dropped = %v, want ambient kept", cfg.Password, dropped)
				}
				if cfg.RequireAuth != "" {
					t.Errorf("loopback: RequireAuth = %q, want unrestricted", cfg.RequireAuth)
				}
				return
			}
			if cfg.Password != "" || !dropped {
				t.Errorf("remote: Password = %q dropped = %v, want ambient discarded", cfg.Password, dropped)
			}
			if cfg.RequireAuth != "scram-sha-256" {
				t.Errorf("remote: RequireAuth = %q, want scram-sha-256", cfg.RequireAuth)
			}
		})
		t.Run(tc.name+"/passfile", func(t *testing.T) {
			isolatePGEnv(t)
			writePassfile(t, "passfile-secret")
			cfg, _, err := DevConnConfig(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.loopback {
				want = "passfile-secret"
			}
			if cfg.Password != want {
				t.Errorf("Password = %q, want %q", cfg.Password, want)
			}
		})
	}
}

func TestDevConnConfig_AmbientHostIsStillJudged(t *testing.T) {
	// The rule is about where the password GOES, not who wrote the host:
	// a DSN that leaves the host to PGHOST is judged on the effective
	// host like any other (owner decision: loopback only).
	isolatePGEnv(t)
	t.Setenv("PGHOST", "db.example")
	t.Setenv("PGPASSWORD", "ambient-secret")
	cfg, _, err := DevConnConfig("dbname=db")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "" || cfg.RequireAuth != "scram-sha-256" {
		t.Errorf("Password = %q RequireAuth = %q, want discarded + scram", cfg.Password, cfg.RequireAuth)
	}
}

func TestDevConnConfig_StatedPasswordKeptForRemote(t *testing.T) {
	isolatePGEnv(t)
	t.Setenv("PGPASSWORD", "ambient-secret")
	writePassfile(t, "passfile-secret")
	for _, tc := range []struct{ dsn, want string }{
		{"postgres://u:dsn-secret@db.example/db", "dsn-secret"},
		{"postgres://u@db.example/db?password=dsn-secret", "dsn-secret"},
		{"postgres://u:userinfo@db.example/db?password=query-wins", "query-wins"},
		{"host=db.example dbname=db password=dsn-secret", "dsn-secret"},
		{"host=db.example dbname=db password='quoted secret'", "quoted secret"},
	} {
		cfg, dropped, err := DevConnConfig(tc.dsn)
		if err != nil {
			t.Fatalf("%s: %v", tc.dsn, err)
		}
		if cfg.Password != tc.want || dropped {
			t.Errorf("%s: Password = %q dropped = %v, want %q kept", tc.dsn, cfg.Password, dropped, tc.want)
		}
		if cfg.RequireAuth != "scram-sha-256" {
			t.Errorf("%s: RequireAuth = %q, want scram-sha-256", tc.dsn, cfg.RequireAuth)
		}
	}
}

func TestDevConnConfig_StatedEmptyPasswordNeverFilledForRemote(t *testing.T) {
	// A DSN that states an EMPTY password suppresses PGPASSWORD, but pgx
	// then consults the passfile (it does so whenever the password is
	// empty). "Stated" must therefore mean the stated VALUE, never "a
	// password key was present": otherwise `?password=` re-opens the
	// leak through a wildcard ~/.pgpass entry.
	isolatePGEnv(t)
	writePassfile(t, "passfile-secret")
	for _, dsn := range []string{
		"postgres://u@db.example/db?password=",
		"postgres://u:@db.example/db",
		"postgres://u@db.example/db?password=guess&password=",
		"host=db.example dbname=db password=''",
		"host=db.example dbname=db password=guess password=",
	} {
		cfg, _, err := DevConnConfig(dsn)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if cfg.Password != "" {
			t.Errorf("%s: Password = %q, want empty (passfile must not reach a remote host)", dsn, cfg.Password)
		}
	}
}

// fakeServer accepts one connection on loopback, demands the given
// authentication, and reports the password message the client sent
// (or "" when the client hung up without sending one).
func fakeServer(t *testing.T, auth pgproto3.BackendMessage) (port int, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- ""
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		be := pgproto3.NewBackend(conn, conn)
		if _, err := be.ReceiveStartupMessage(); err != nil {
			ch <- ""
			return
		}
		be.Send(auth)
		if err := be.Flush(); err != nil {
			ch <- ""
			return
		}
		if err := be.SetAuthType(authType(auth)); err != nil {
			ch <- ""
			return
		}
		msg, err := be.Receive()
		if err != nil {
			ch <- ""
			return
		}
		if pm, ok := msg.(*pgproto3.PasswordMessage); ok {
			ch <- "password:" + pm.Password
			return
		}
		ch <- fmt.Sprintf("other:%T", msg)
	}()
	return ln.Addr().(*net.TCPAddr).Port, ch
}

func authType(m pgproto3.BackendMessage) uint32 {
	switch m.(type) {
	case *pgproto3.AuthenticationCleartextPassword:
		return pgproto3.AuthTypeCleartextPassword
	case *pgproto3.AuthenticationMD5Password:
		return pgproto3.AuthTypeMD5Password
	}
	return pgproto3.AuthTypeOk
}

// connectVia dials cfg but resolves every host name to the loopback
// fake server: the remote host stays remote for the policy (the config
// was built from its name) while the bytes stay on this machine.
func connectVia(t *testing.T, cfg *pgx.ConnConfig) {
	t.Helper()
	cfg.LookupFunc = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := pgx.ConnectConfig(ctx, cfg); err == nil {
		_ = conn.Close(ctx)
	}
}

func TestDevConnConfig_WireRemoteNeverReceivesPassword(t *testing.T) {
	// Independent oracle: the real pgx wire client against a server that
	// asks for the password in the clear (and, separately, via MD5). A
	// remote host must get no PasswordMessage at all — neither the
	// ambient password (discarded) nor a DSN-stated one (require_auth).
	for _, tc := range []struct {
		name string
		auth pgproto3.BackendMessage
		dsn  string
	}{
		{"cleartext/ambient", &pgproto3.AuthenticationCleartextPassword{}, "postgres://u@attacker.example:%d/db?sslmode=disable"},
		{"cleartext/stated", &pgproto3.AuthenticationCleartextPassword{}, "postgres://u:dsn-secret@attacker.example:%d/db?sslmode=disable"},
		{"md5/ambient", &pgproto3.AuthenticationMD5Password{Salt: [4]byte{1, 2, 3, 4}}, "postgres://u@attacker.example:%d/db?sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatePGEnv(t)
			t.Setenv("PGPASSWORD", "ambient-secret")
			port, got := fakeServer(t, tc.auth)
			cfg, _, err := DevConnConfig(fmt.Sprintf(tc.dsn, port))
			if err != nil {
				t.Fatal(err)
			}
			connectVia(t, cfg)
			if g := <-got; g != "" {
				t.Errorf("remote host received %q, want no password message", g)
			}
		})
	}
}

func TestDevConnConfig_WireLoopbackReceivesAmbientPassword(t *testing.T) {
	// The accepted neighbor of the test above, through the same fake
	// server: a loopback DSN without a password still authenticates
	// with PGPASSWORD (libpq's ordinary behavior, kept for local dev).
	isolatePGEnv(t)
	t.Setenv("PGPASSWORD", "ambient-secret")
	port, got := fakeServer(t, &pgproto3.AuthenticationCleartextPassword{})
	cfg, _, err := DevConnConfig(fmt.Sprintf("postgres://u@127.0.0.1:%d/db?sslmode=disable", port))
	if err != nil {
		t.Fatal(err)
	}
	connectVia(t, cfg)
	if g := <-got; g != "password:ambient-secret" {
		t.Errorf("loopback host received %q, want the ambient password", g)
	}
}
