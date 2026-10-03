package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ExplainFormat selects the plan representation an Explain<Query>
// method returns (design doc 22).
type ExplainFormat uint8

const (
	// ExplainText is the dialect's human-readable plan (the zero value).
	ExplainText ExplainFormat = iota
	// ExplainJSON is a JSON document: the engine's own JSON plan on
	// PostgreSQL and MySQL, the EXPLAIN QUERY PLAN rows on SQLite.
	ExplainJSON
)

// ExplainOptions configures an Explain<Query> call. The vocabulary is
// closed on purpose: the EXPLAIN prefix is a constant selected from it,
// never caller-supplied text.
type ExplainOptions struct {
	// Analyze executes the statement to report actual row counts and
	// timings. The generated code always runs it inside a transaction
	// (or savepoint) that is rolled back, whatever the statement kind.
	Analyze bool
	Format  ExplainFormat
}

// Plan is the result of an Explain<Query> call.
type Plan struct {
	Query    string // query name
	ShapeKey string // canonical shape key, as OnQuery reports it
	// SQL is the composed statement — byte-identical to the SQL the
	// query method itself would send for the same arguments.
	SQL string
	// Statement is what was sent: the EXPLAIN prefix followed by SQL.
	Statement string
	Format    ExplainFormat
	Analyzed  bool
	Output    string // the plan, text or a JSON document per Format
}

var (
	// ErrExplainUnsupported reports an option combination the dialect
	// cannot express (SQLite has no ANALYZE) or an unknown enum value.
	ErrExplainUnsupported = errors.New("sqletch: explain option not supported by this dialect")
	// ErrExplainNoTx reports an Analyze request on a DBTX that can open
	// neither a transaction nor a savepoint: ANALYZE is never run
	// outside a rolled-back transaction.
	ErrExplainNoTx = errors.New("sqletch: explain analyze needs a DBTX that can begin a transaction or savepoint")
)

// ExplainDialect identifies the EXPLAIN vocabulary generated code
// speaks. The zero value is deliberately not a dialect.
type ExplainDialect uint8

const (
	ExplainPostgres ExplainDialect = iota + 1
	ExplainMySQL
	ExplainSQLite
)

func (d ExplainDialect) String() string {
	switch d {
	case ExplainPostgres:
		return "postgres"
	case ExplainMySQL:
		return "mysql"
	case ExplainSQLite:
		return "sqlite"
	}
	return fmt.Sprintf("ExplainDialect(%d)", uint8(d))
}

// explainPrefixes is the complete set of prefixes Explain<Query> can
// send, indexed by [Analyze][Format]. An empty entry is unsupported.
var explainPrefixes = map[ExplainDialect][2][2]string{
	ExplainPostgres: {
		{"EXPLAIN ", "EXPLAIN (FORMAT JSON) "},
		{"EXPLAIN (ANALYZE) ", "EXPLAIN (ANALYZE, FORMAT JSON) "},
	},
	ExplainMySQL: {
		{"EXPLAIN FORMAT=TREE ", "EXPLAIN FORMAT=JSON "},
		{"EXPLAIN ANALYZE ", "EXPLAIN ANALYZE FORMAT=JSON "},
	},
	ExplainSQLite: {
		{"EXPLAIN QUERY PLAN ", "EXPLAIN QUERY PLAN "},
		{"", ""},
	},
}

// ExplainStatement wraps a composed statement in the dialect's EXPLAIN
// prefix for opts. Part of the generated-code contract.
func ExplainStatement(d ExplainDialect, opts ExplainOptions, sql string) (string, error) {
	table, ok := explainPrefixes[d]
	if !ok || opts.Format > ExplainJSON {
		return "", fmt.Errorf("%w: dialect %v, format %d", ErrExplainUnsupported, d, opts.Format)
	}
	a := 0
	if opts.Analyze {
		a = 1
	}
	prefix := table[a][opts.Format]
	if prefix == "" {
		return "", fmt.Errorf("%w: %v has no EXPLAIN ANALYZE", ErrExplainUnsupported, d)
	}
	return prefix + sql, nil
}

// SQLitePlanRow is one row of SQLite's EXPLAIN QUERY PLAN output (the
// unused third column dropped).
type SQLitePlanRow struct {
	ID     int64  `json:"id"`
	Parent int64  `json:"parent"`
	Detail string `json:"detail"`
}

// SQLitePlan renders EXPLAIN QUERY PLAN rows. Text indents each detail
// two spaces per tree depth, in engine order; a parent id that never
// appeared counts as the root. JSON is the rows as an array.
func SQLitePlan(rows []SQLitePlanRow, format ExplainFormat) (string, error) {
	switch format {
	case ExplainText:
		depth := map[int64]int{}
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			d := 0
			if pd, ok := depth[r.Parent]; ok {
				d = pd + 1
			}
			depth[r.ID] = d
			lines = append(lines, strings.Repeat("  ", d)+r.Detail)
		}
		return strings.Join(lines, "\n"), nil
	case ExplainJSON:
		if rows == nil {
			rows = []SQLitePlanRow{}
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return "", fmt.Errorf("%w: format %d", ErrExplainUnsupported, format)
}
