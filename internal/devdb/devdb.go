// Package devdb manages the development database the type oracle runs
// against: a user-supplied DSN, or an auto-managed disposable
// container. See docs/design/04-type-oracle.md §2.
package devdb

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type Config struct {
	// DSN, when set, is used as-is instead of starting a container.
	// The referenced database MUST be disposable: whenever SchemaSQL
	// is non-empty, sqletch resets the public schema (DROP SCHEMA
	// public CASCADE) before applying it, so repeated runs are
	// idempotent. Never point this at a database you care about.
	//
	// Because the DSN comes from sqletch.yaml — repo-controlled, so a
	// cloned project could aim it at a database the developer cares
	// about — a user-supplied DSN does NOT reset by default: Acquire
	// returns *DestructiveResetError unless AllowDestructive is set. A
	// database sqletch provisioned itself (empty DSN → a fresh
	// container or temp file) is disposable by construction and always
	// resets.
	DSN string
	// AllowDestructive clears the user-supplied-DSN reset guard above:
	// the caller (a person passing --allow-destructive on the command
	// line) has confirmed the database at DSN is disposable, so sqletch
	// may drop and recreate its schema. It is ignored when DSN is empty.
	AllowDestructive bool
	// ServerVersion is the pinned major version (e.g. "16" or
	// "16.4"); it selects the container image and is validated against
	// whatever we connect to.
	ServerVersion string
	// SchemaSQL is executed in order after connecting (plain SQL —
	// schema files are read by the caller).
	SchemaSQL []string
	// Detected, when non-nil, receives what Acquire learned by
	// connecting — facts no caller can compute offline. It is filled
	// in before the schema is applied; on an error return its contents
	// are undefined. Callers that do not care leave it nil, and then
	// the extra round trip is skipped unless the version pin needs it.
	Detected *Detected
}

// Detected is the connected server's own account of itself, reported
// back to callers that asked for it via Config.Detected.
type Detected struct {
	// ServerVersion is the raw string the engine reported, e.g.
	// "16.4 (Debian 16.4-1.pgdg120+1)", "8.0.36-log", "3.50.4".
	ServerVersion string
}

// wantVersion reports whether Acquire must ask the server for its
// version: to validate the pin, to answer Detected, or both.
func (c Config) wantVersion() bool { return c.ServerVersion != "" || c.Detected != nil }

// recordVersion hands the reported version to the caller's sink and
// validates the pin. A nil Detected means "not interested".
func (c Config) recordVersion(actual, server string, prefixMatch bool) error {
	if c.Detected != nil {
		c.Detected.ServerVersion = actual
	}
	if c.ServerVersion == "" {
		return nil
	}
	ok := sameMajor(c.ServerVersion, actual)
	if prefixMatch {
		ok = versionPrefixMatch(c.ServerVersion, actual)
	}
	if !ok {
		return &VersionMismatchError{Pinned: c.ServerVersion, Actual: actual, Server: server}
	}
	return nil
}

// VersionMismatchError signals that the connected server does not
// match the pinned server_version (SQLETCH200 at the CLI layer). Every
// dialect's Acquire returns it, so Server names the engine actually
// connected to — the message is not PostgreSQL-specific.
type VersionMismatchError struct {
	Pinned, Actual string
	Server         string // display name, e.g. "PostgreSQL", "MySQL", "SQLite"
}

func (e *VersionMismatchError) Error() string {
	server := e.Server
	if server != "" {
		server += " "
	}
	return fmt.Sprintf("connected server is %s%s but sqletch.yaml pins server_version %s", server, e.Actual, e.Pinned)
}

// DestructiveResetError signals that Acquire declined to reset a
// user-supplied database's schema because AllowDestructive
// (--allow-destructive) was not set — the clone-and-run guard
// (SQLETCH204 at the CLI layer). Like VersionMismatchError it is shared
// by all three dialects, so Server names the engine actually targeted;
// the disposable-reset contract is not PostgreSQL-specific. The DSN is
// deliberately NOT carried here: it may embed credentials, and the
// diagnostic points at database.dsn in the config rather than echoing
// the string back.
type DestructiveResetError struct {
	Server string // display name, e.g. "PostgreSQL", "MySQL", "SQLite"
}

func (e *DestructiveResetError) Error() string {
	server := e.Server
	if server != "" {
		server += " "
	}
	return fmt.Sprintf("refusing to reset the %sdatabase at the configured dsn: sqletch drops the schema before applying it, which is safe only for a disposable database", server)
}

// guardReset reports whether Acquire must refuse the disposable schema
// reset for this config: the DSN is user-supplied (sqletch did not
// provision the database) and AllowDestructive was not passed. An empty
// DSN — a container or temp file sqletch created itself — always resets.
func (c Config) guardReset() bool {
	return c.DSN != "" && !c.AllowDestructive
}

// AcquireDSN starts (or reuses) the dev database, verifies the version
// pin, applies the schema, and returns the DSN — for callers that need
// to hand the database to a subprocess. cleanup is never nil.
func AcquireDSN(ctx context.Context, cfg Config) (string, func(), error) {
	conn, cleanup, err := Acquire(ctx, cfg)
	if err != nil {
		return "", cleanup, err
	}
	dsn := conn.Config().ConnString()
	_ = conn.Close(ctx)
	return dsn, cleanup, nil
}

// Acquire connects to (or starts) the dev database, verifies the
// version pin, and applies the schema. The returned cleanup is never
// nil and terminates the container (when one was started) after
// closing the connection.
func Acquire(ctx context.Context, cfg Config) (*pgx.Conn, func(), error) {
	stopContainer := func() {}
	dsn := cfg.DSN
	if dsn == "" {
		image := "postgres:16-alpine"
		if cfg.ServerVersion != "" {
			image = "postgres:" + cfg.ServerVersion + "-alpine"
		}
		ctr, err := tcpostgres.Run(ctx, image,
			tcpostgres.WithDatabase("sqletch"),
			tcpostgres.WithUsername("sqletch"),
			tcpostgres.WithPassword("sqletch"),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			return nil, func() {}, fmt.Errorf("start dev database container: %w", err)
		}
		stopContainer = func() { _ = testcontainers.TerminateContainer(ctr) }
		dsn, err = ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			stopContainer()
			return nil, func() {}, err
		}
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		stopContainer()
		return nil, func() {}, fmt.Errorf("connect to dev database: %w", err)
	}
	closeAll := func() {
		_ = conn.Close(context.Background())
		stopContainer()
	}

	if cfg.wantVersion() {
		var actual string
		if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&actual); err != nil {
			closeAll()
			return nil, func() {}, err
		}
		if err := cfg.recordVersion(actual, "PostgreSQL", false); err != nil {
			closeAll()
			return nil, func() {}, err
		}
	}

	if hasSchema(cfg.SchemaSQL) {
		if cfg.guardReset() {
			closeAll()
			return nil, func() {}, &DestructiveResetError{Server: "PostgreSQL"}
		}
		// Dev databases are disposable by contract (see Config.DSN):
		// reset so schema application is idempotent across runs.
		if _, err := conn.Exec(ctx,
			"DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public"); err != nil {
			closeAll()
			return nil, func() {}, fmt.Errorf("reset dev database schema: %w", err)
		}
		for i, stmt := range cfg.SchemaSQL {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := conn.Exec(ctx, stmt); err != nil {
				closeAll()
				return nil, func() {}, fmt.Errorf("apply schema input %d: %w", i, err)
			}
		}
	}
	return conn, closeAll, nil
}

func hasSchema(stmts []string) bool {
	for _, s := range stmts {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

func sameMajor(pinned, actual string) bool {
	return major(pinned) == major(actual)
}

func major(v string) string {
	if i := strings.IndexAny(v, ". "); i >= 0 {
		return v[:i]
	}
	return v
}
