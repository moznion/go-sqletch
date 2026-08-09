package devdb

import (
	"context"
	"fmt"
	"strings"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	sqldriver "github.com/go-sql-driver/mysql"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"

	"github.com/moznion/go-sqletch/internal/dialect"
	mysqldialect "github.com/moznion/go-sqletch/internal/dialect/mysql"
)

// AcquireMySQLDSN is AcquireMySQL for callers that need to hand the
// database to a subprocess; it returns the go-sql-driver DSN.
func AcquireMySQLDSN(ctx context.Context, cfg Config) (string, func(), error) {
	conn, dsn, cleanup, err := acquireMySQL(ctx, cfg)
	if err != nil {
		return "", cleanup, err
	}
	_ = conn.Close()
	return dsn, cleanup, nil
}

// AcquireMySQL connects to (or starts) the MySQL dev database, verifies
// the version pin, and applies the schema. DSNs use the go-sql-driver
// format (user:pass@tcp(host:port)/dbname). The database is disposable
// by contract: when SchemaSQL is non-empty, every table in the target
// database is dropped before the schema is applied.
func AcquireMySQL(ctx context.Context, cfg Config) (*gomysqlclient.Conn, func(), error) {
	conn, _, cleanup, err := acquireMySQL(ctx, cfg)
	return conn, cleanup, err
}

func acquireMySQL(ctx context.Context, cfg Config) (*gomysqlclient.Conn, string, func(), error) {
	stopContainer := func() {}
	dsn := cfg.DSN
	if dsn == "" {
		image := "mysql:8.4"
		if cfg.ServerVersion != "" {
			image = "mysql:" + cfg.ServerVersion
		}
		ctr, err := tcmysql.Run(ctx, image,
			tcmysql.WithDatabase("sqletch"),
			tcmysql.WithUsername("sqletch"),
			tcmysql.WithPassword("sqletch"),
		)
		if err != nil {
			return nil, "", func() {}, fmt.Errorf("start MySQL dev database container: %w", err)
		}
		stopContainer = func() { _ = testcontainers.TerminateContainer(ctr) }
		dsn, err = ctr.ConnectionString(ctx)
		if err != nil {
			stopContainer()
			return nil, "", func() {}, err
		}
	}

	parsed, err := sqldriver.ParseDSN(dsn)
	if err != nil {
		stopContainer()
		return nil, "", func() {}, fmt.Errorf("parse MySQL DSN: %w", err)
	}
	conn, err := gomysqlclient.ConnectWithContext(ctx, parsed.Addr, parsed.User, parsed.Passwd, parsed.DBName, 0)
	if err != nil {
		stopContainer()
		return nil, "", func() {}, fmt.Errorf("connect to MySQL dev database: %w", err)
	}
	closeAll := func() {
		_ = conn.Close()
		stopContainer()
	}

	if cfg.wantVersion() {
		r, err := conn.Execute("SELECT VERSION()")
		if err != nil {
			closeAll()
			return nil, "", func() {}, err
		}
		actual, _ := r.GetString(0, 0)
		r.Close()
		if err := cfg.recordVersion(actual, "MySQL"); err != nil {
			closeAll()
			return nil, "", func() {}, err
		}
	}

	if hasSchema(cfg.SchemaSQL) {
		if err := resetMySQL(conn); err != nil {
			closeAll()
			return nil, "", func() {}, fmt.Errorf("reset dev database schema: %w", err)
		}
		for i, input := range cfg.SchemaSQL {
			for _, stmt := range splitStatements(input) {
				if _, err := conn.Execute(stmt); err != nil {
					closeAll()
					return nil, "", func() {}, fmt.Errorf("apply schema input %d: %w", i, err)
				}
			}
		}
	}
	return conn, dsn, closeAll, nil
}

// splitStatements splits schema SQL on top-level semicolons using the
// MySQL lexer profile, so strings and comments never split (the wire
// protocol executes one statement per command).
func splitStatements(input string) []string {
	src := []byte(input)
	profile := mysqldialect.Profile{}
	var out []string
	pos, start := 0, 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			break
		}
		if tok.Kind == dialect.KindSemicolon {
			if s := strings.TrimSpace(string(src[start:tok.Start])); s != "" {
				out = append(out, s)
			}
			start = tok.End
		}
		pos = tok.End
	}
	if s := strings.TrimSpace(string(src[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

// resetMySQL drops every table in the current database (the MySQL
// analog of the PostgreSQL schema reset).
func resetMySQL(conn *gomysqlclient.Conn) error {
	res, err := conn.Execute(
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'")
	if err != nil {
		return err
	}
	var tables []string
	for i := range res.Values {
		name, err := res.GetString(i, 0)
		if err != nil {
			return err
		}
		tables = append(tables, "`"+strings.ReplaceAll(name, "`", "``")+"`")
	}
	res.Close()
	if len(tables) == 0 {
		return nil
	}
	if _, err := conn.Execute("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		return err
	}
	if _, err := conn.Execute("DROP TABLE IF EXISTS " + strings.Join(tables, ", ")); err != nil {
		return err
	}
	_, err = conn.Execute("SET FOREIGN_KEY_CHECKS = 1")
	return err
}
