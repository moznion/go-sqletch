package mysql

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	parsertypes "github.com/pingcap/tidb/pkg/parser/types"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// CatalogSchemaName is the schema name stamped on natively-built
// catalogs. It matches the devdb container database (devdb/mysql.go's
// WithDatabase), which is what every server-captured cache and corpus
// case records — byte-identity across oracle backends (design 15 §3)
// depends on the two never diverging.
const CatalogSchemaName = "sqletch"

func init() {
	// Render integer column types the MySQL >= 8.0.19 way (display
	// width dropped, tinyint(1) kept): information_schema COLUMN_TYPE
	// spellings are part of the catalog's byte-identity contract with
	// server snapshots. Consequence: the native backend models
	// MySQL >= 8.0.19 semantics only (the version pin selects within
	// that range).
	parsertypes.TiDBStrictIntegerDisplayWidth = true
}

// UnsupportedDDLError reports a schema statement outside the native
// catalog builder's modeled subset (design 15 §5.1, decision D5). The
// CLI maps it to SQLETCH215 with a span into the schema file. Fail
// closed: anything the builder does not fully model is refused, never
// approximated.
type UnsupportedDDLError struct {
	File string // schema input path as given
	Pos  int    // byte offset of the offending statement in that file
	Msg  string
}

func (e *UnsupportedDDLError) Error() string {
	return fmt.Sprintf("%s: offset %d: %s", e.File, e.Pos, e.Msg)
}

// BuildCatalog parses the ordered schema inputs into a catalog
// byte-identical to Snapshot over a server that ran the same DDL:
// table OIDs 1-based in table-name order, column att numbers are
// definition ordinals, type names are information_schema COLUMN_TYPE
// spellings, HasDefault mirrors the snapshot query's predicate
// (non-NULL default, auto_increment, or generated default).
//
// v1 subset: CREATE TABLE, DROP TABLE, and no-op SET statements.
func BuildCatalog(schema []cache.SchemaFile) (*cache.Catalog, error) {
	tables := map[string]*cache.Table{}
	for _, f := range schema {
		content := string(f.Content)
		stmts, _, err := parser.New().ParseSQL(content)
		if err != nil {
			pos := 0
			var perr *dialect.ParseError
			if errors.As(toParseError(content, err), &perr) {
				pos = perr.Pos
			}
			return nil, &UnsupportedDDLError{File: f.Path, Pos: pos,
				Msg: "unparsable schema statement: " + err.Error()}
		}
		searchFrom := 0
		for _, stmt := range stmts {
			pos := stmtOffset(content, stmt, &searchFrom)
			switch s := stmt.(type) {
			case *ast.CreateTableStmt:
				t, err := buildTable(f.Path, pos, s)
				if err != nil {
					return nil, err
				}
				if _, dup := tables[t.Name]; dup {
					return nil, &UnsupportedDDLError{File: f.Path, Pos: pos,
						Msg: fmt.Sprintf("table %q is created twice (DROP it first, or deduplicate the schema inputs)", t.Name)}
				}
				tables[t.Name] = t
			case *ast.DropTableStmt:
				if s.IsView {
					return nil, &UnsupportedDDLError{File: f.Path, Pos: pos, Msg: unsupportedMsg("DROP VIEW")}
				}
				for _, tn := range s.Tables {
					if _, ok := tables[tn.Name.O]; !ok && !s.IfExists {
						return nil, &UnsupportedDDLError{File: f.Path, Pos: pos,
							Msg: fmt.Sprintf("DROP TABLE %q: no such table in the schema inputs", tn.Name.O)}
					}
					delete(tables, tn.Name.O)
				}
			case *ast.SetStmt:
				// Session/variable statements have no catalog effect.
			default:
				return nil, &UnsupportedDDLError{File: f.Path, Pos: pos,
					Msg: unsupportedMsg(stmtLabel(stmt))}
			}
		}
	}

	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	// Byte-order table names to assign synthetic OIDs. This must match
	// the server snapshot's ordering exactly (design 15 §3, byte-identity
	// across backends); the server query orders by BINARY table_name, so
	// a byte-wise sort here — not a case-insensitive collation — is what
	// keeps mixed-case schemas ("Zebra" vs "apple") agreeing.
	sort.Strings(names)
	cat := &cache.Catalog{}
	for i, name := range names {
		t := tables[name]
		t.OID = uint32(i + 1)
		cat.Tables = append(cat.Tables, *t)
	}
	return cat, nil
}

func unsupportedMsg(what string) string {
	return what + " is outside the native catalog builder's subset (CREATE TABLE, DROP TABLE, SET); consolidate the schema into plain CREATE TABLE files, or switch to database.oracle: \"server\""
}

func buildTable(file string, pos int, s *ast.CreateTableStmt) (*cache.Table, error) {
	reject := func(what string) (*cache.Table, error) {
		return nil, &UnsupportedDDLError{File: file, Pos: pos, Msg: unsupportedMsg(what)}
	}
	if s.ReferTable != nil {
		return reject("CREATE TABLE ... LIKE")
	}
	if s.Select != nil {
		return reject("CREATE TABLE ... AS SELECT")
	}
	if s.TemporaryKeyword != ast.TemporaryNone {
		return reject("CREATE TEMPORARY TABLE")
	}

	// PRIMARY KEY columns are implicitly NOT NULL in MySQL.
	pkCols := map[string]bool{}
	for _, con := range s.Constraints {
		if con.Tp != ast.ConstraintPrimaryKey {
			continue // secondary indexes, FKs, CHECKs: no column effect
		}
		for _, k := range con.Keys {
			if k.Column == nil {
				return reject("an expression PRIMARY KEY")
			}
			pkCols[k.Column.Name.O] = true
		}
	}

	t := &cache.Table{Schema: CatalogSchemaName, Name: s.Table.Name.O}
	for i, def := range s.Cols {
		col := cache.Column{
			Name:     def.Name.Name.O,
			Att:      int16(i + 1), // information_schema ordinal_position
			TypeName: columnTypeStr(def.Tp),
			NotNull:  pkCols[def.Name.Name.O],
		}
		if tr, ok := (TypeMap{}).TypeByName(col.TypeName); ok {
			col.TypeOID = tr.OID // zero when unmapped, matching Snapshot
		}
		for _, opt := range def.Options {
			switch opt.Tp {
			case ast.ColumnOptionNotNull:
				col.NotNull = true
			case ast.ColumnOptionNull:
				col.NotNull = pkCols[col.Name]
			case ast.ColumnOptionPrimaryKey:
				col.NotNull = true
			case ast.ColumnOptionAutoIncrement:
				col.HasDefault = true // snapshot: extra LIKE '%auto_increment%'
			case ast.ColumnOptionDefaultValue:
				// DEFAULT NULL leaves information_schema.column_default
				// NULL, so the snapshot predicate reports no default.
				if v, ok := opt.Expr.(ast.ValueExpr); !ok || v.GetValue() != nil {
					col.HasDefault = true
				}
			case ast.ColumnOptionGenerated:
				return reject("a generated column")
			case ast.ColumnOptionComment, ast.ColumnOptionCollate,
				ast.ColumnOptionUniqKey, ast.ColumnOptionCheck,
				ast.ColumnOptionReference:
				// No effect on the snapshot's column facts.
			case ast.ColumnOptionOnUpdate:
				// ON UPDATE CURRENT_TIMESTAMP does not create a default.
			default:
				return reject(fmt.Sprintf("column option %d on %q", opt.Tp, col.Name))
			}
		}
		t.Cols = append(t.Cols, col)
	}
	return t, nil
}

// columnTypeStr renders a parsed column type the way
// information_schema COLUMN_TYPE spells it. One divergence from the
// parser's InfoSchemaStr, found by the differential gate: TiDB prints
// YEAR with a width (`year(-1)`/`year(4)`) where MySQL >= 8.0.19
// prints bare `year`.
func columnTypeStr(ft *parsertypes.FieldType) string {
	s := ft.InfoSchemaStr()
	if strings.HasPrefix(s, "year") {
		return "year"
	}
	return s
}

// stmtOffset locates stmt's byte offset by searching for its original
// text from the previous statement's end — the parser does not carry
// statement offsets.
func stmtOffset(content string, stmt ast.StmtNode, searchFrom *int) int {
	text := stmt.Text()
	if text == "" {
		return *searchFrom
	}
	idx := strings.Index(content[*searchFrom:], text)
	if idx < 0 {
		return *searchFrom
	}
	pos := *searchFrom + idx
	*searchFrom = pos + len(text)
	return pos
}

// stmtLabel names a statement kind for diagnostics.
func stmtLabel(stmt ast.StmtNode) string {
	switch stmt.(type) {
	case *ast.AlterTableStmt:
		return "ALTER TABLE"
	case *ast.CreateViewStmt:
		return "CREATE VIEW"
	case *ast.CreateIndexStmt:
		return "CREATE INDEX"
	case *ast.CreateDatabaseStmt:
		return "CREATE DATABASE"
	case *ast.TruncateTableStmt:
		return "TRUNCATE TABLE"
	default:
		return fmt.Sprintf("%T", stmt)
	}
}
