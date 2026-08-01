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
	// DSN, when set, is used as-is (the user owns the database's
	// lifecycle). Otherwise a disposable container is started.
	DSN string
	// ServerVersion is the pinned major version (e.g. "16" or
	// "16.4"); it selects the container image and is validated against
	// whatever we connect to.
	ServerVersion string
	// SchemaSQL is executed in order after connecting (plain SQL —
	// schema files are read by the caller).
	SchemaSQL []string
}

// VersionMismatchError signals that the connected server does not
// match the pinned server_version (SQLETCH200 at the CLI layer).
type VersionMismatchError struct {
	Pinned, Actual string
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("connected server is PostgreSQL %s but sqletch.yaml pins server_version %s", e.Actual, e.Pinned)
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

	if cfg.ServerVersion != "" {
		var actual string
		if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&actual); err != nil {
			closeAll()
			return nil, func() {}, err
		}
		if !sameMajor(cfg.ServerVersion, actual) {
			closeAll()
			return nil, func() {}, &VersionMismatchError{Pinned: cfg.ServerVersion, Actual: actual}
		}
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
	return conn, closeAll, nil
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
