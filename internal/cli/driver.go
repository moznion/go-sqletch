package cli

import (
	"context"

	"github.com/moznion/sqletch/internal/config"
	"github.com/moznion/sqletch/internal/devdb"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/mysql"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/dialect/sqlite"
	"github.com/moznion/sqletch/runtime"
)

// driver bundles the per-dialect components the pipeline dispatches
// over. Everything else in the pipeline is dialect-agnostic.
type driver struct {
	profile  dialect.LexerProfile
	frontend dialect.Frontend
	typemap  dialect.TypeMap
	// typeByName resolves `-- @param` annotations.
	typeByName func(string) (dialect.TypeRef, bool)
	// writableName is typeByName's inverse, used to spell the compliant
	// rewrite when an annotation disagrees with the oracle
	// (SQLETCH213). Only Tier 1 can disagree, so Tier 2 leaves it nil.
	writableName func(uint32) (string, bool)
	style        runtime.Style
	// expandIn: @in is arity-expanded (a shape dimension) rather than a
	// single array bind.
	expandIn bool
	// annotationsRequired: the oracle cannot type parameters; every
	// parameter needs a `-- @param` annotation (Tier 2).
	annotationsRequired bool
	// columnHintsRequired: the oracle cannot type expression result
	// columns (SQLite decltype); they need `-- @column` annotations.
	columnHintsRequired bool
	acquire             func(ctx context.Context, cfg config.Config, schemaSQL []string) (dialect.Oracle, func(), error)
}

// planTexter is the optional oracle capability behind explain
// --analyze; both shipped oracles implement it.
type planTexter interface {
	PlanText(ctx context.Context, sql string) (string, error)
}

func driverFor(cfg config.Config) driver {
	if cfg.Dialect == "sqlite" {
		return driver{
			profile:             sqlite.Profile{},
			frontend:            sqlite.Frontend{},
			typemap:             sqlite.TypeMap{},
			typeByName:          sqlite.TypeMap{}.TypeByName,
			style:               runtime.StyleQuestion,
			expandIn:            true,
			annotationsRequired: true,
			columnHintsRequired: true,
			acquire: func(ctx context.Context, cfg config.Config, schemaSQL []string) (dialect.Oracle, func(), error) {
				conn, cleanup, err := devdb.AcquireSQLite(ctx, devdb.Config{
					DSN:           cfg.Database.DSN,
					ServerVersion: cfg.ServerVersion,
					SchemaSQL:     schemaSQL,
				})
				if err != nil {
					return nil, cleanup, err
				}
				return sqlite.NewOracle(conn), cleanup, nil
			},
		}
	}
	if cfg.Dialect == "mysql" {
		return driver{
			profile:             mysql.Profile{},
			frontend:            mysql.Frontend{},
			typemap:             mysql.TypeMap{},
			typeByName:          mysql.TypeMap{}.TypeByName,
			style:               runtime.StyleQuestion,
			expandIn:            true,
			annotationsRequired: true,
			acquire: func(ctx context.Context, cfg config.Config, schemaSQL []string) (dialect.Oracle, func(), error) {
				conn, cleanup, err := devdb.AcquireMySQL(ctx, devdb.Config{
					DSN:           cfg.Database.DSN,
					ServerVersion: cfg.ServerVersion,
					SchemaSQL:     schemaSQL,
				})
				if err != nil {
					return nil, cleanup, err
				}
				return mysql.NewOracle(conn), cleanup, nil
			},
		}
	}
	return driver{
		profile:      postgres.Profile{},
		frontend:     postgres.Frontend{},
		typemap:      postgres.TypeMap{},
		typeByName:   postgres.TypeMap{}.TypeByName,
		writableName: postgres.TypeMap{}.WritableName,
		style:        runtime.StyleDollar,
		acquire: func(ctx context.Context, cfg config.Config, schemaSQL []string) (dialect.Oracle, func(), error) {
			conn, cleanup, err := devdb.Acquire(ctx, devdb.Config{
				DSN:           cfg.Database.DSN,
				ServerVersion: cfg.ServerVersion,
				SchemaSQL:     schemaSQL,
			})
			if err != nil {
				return nil, cleanup, err
			}
			return postgres.NewOracle(conn), cleanup, nil
		},
	}
}
