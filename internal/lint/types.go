package lint

import (
	"slices"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/rules"
	"github.com/moznion/go-sqletch/internal/template"
)

// CheckTypes runs SQLETCHL005 (design 24 §3.5): a column compared
// with a parameter whose type makes the engine convert the COLUMN side,
// which takes an index on the column out of play. It needs the catalog
// (column types) and the resolved parameter types, so it runs in the
// catalog-dependent pass (cli.resolvedChecks) and owns the
// unused-@nolint verdict for SQLETCHL005.
//
// It is a per-dialect WHITELIST of pairs known to defeat the index;
// every other pair — including every pair on SQLite, where a comparison
// with a bound parameter applies the column's affinity to the
// parameter — is silent. Only the top-level statement's own WHERE/ON
// predicates are considered (a subquery's columns would need scope
// resolution this facade does not model), with one side a bare column
// and the other a bare parameter (PostgreSQL also accepts one cast of
// it; the comparison is then judged at the CAST's type, resolved via
// typeByName, because the oracle infers one type per parameter from its
// first use and a cast elsewhere does not change it — an unresolvable
// cast type is silent).
func CheckTypes(profile dialect.LexerProfile, dialectName string, q *template.QueryTemplate, rs []ast.Rendering,
	tree dialect.Tree, cat *cache.Catalog, paramTypes map[string]dialect.TypeRef,
	typeByName func(string) (dialect.TypeRef, bool)) []diagnostics.Diagnostic {

	if cat == nil || len(rs) == 0 || tree.HasSetOperation() {
		return nil // no verdict at all, so no unused-@nolint verdict either
	}
	c := newPerfCollector(q)
	mismatch := typeMismatchRule(dialectName)
	if mismatch != nil {
		res := rules.NewColumnResolver(profile, q, rs[0], tree, cat)
		for _, r := range rs {
			toks, ok := lexRendering(profile, r)
			if !ok {
				continue
			}
			ctes := cteNames(toks)
			pc := predicateContexts(toks)
			for i := range toks {
				if !pc.top[i] {
					continue
				}
				kind := comparisonAt(toks, i)
				if kind == "" || kind == "LIKE" || kind == "ILIKE" {
					continue
				}
				left, right := operands(toks, i)
				col, val := left, right
				if !isColRef(col) {
					if kind != "op" {
						continue
					}
					col, val = right, left
				}
				if !isColRef(col) {
					continue
				}
				params := paramOperands(r.SQL, val, kind == "IN", dialectName == "postgres")
				if len(params) == 0 {
					continue
				}
				column := resolveColumn(res, ctes, col)
				if column == nil {
					continue
				}
				for _, op := range params {
					p := op.param
					pt, ok := paramTypes[p]
					if op.cast != "" {
						pt, ok = dialect.TypeRef{}, false
						if typeByName != nil {
							pt, ok = typeByName(op.cast)
						}
					}
					if !ok {
						continue
					}
					why := mismatch(column.TypeOID, pt.OID)
					if why == "" {
						continue
					}
					span, ok := templateSpan(q, r, toks[i-len(left):i+1+len(right)])
					if !ok {
						if span, ok = templateSpan(q, r, col); !ok {
							continue
						}
					}
					c.add(diagnostics.Warnf(diagnostics.CodePerfTypeMismatch, span,
						"column %q (%s) is compared with parameter %q (%s): %s, so an index on the column cannot be used",
						column.Name, column.TypeName, p, pt.Name, why).
						WithHint("bind the parameter at the column's type (drop the cast, or fix its `-- @param %s:` annotation); `-- @nolint %s` if the conversion is intended", p, diagnostics.CodePerfTypeMismatch))
					break
				}
			}
		}
	}
	return c.finish([]diagnostics.Code{diagnostics.CodePerfTypeMismatch})
}

// paramOperand is one placeholder of a value operand; cast is the
// source text of the type it is cast to ("" when bare).
type paramOperand struct {
	param, cast string
}

// paramOperands returns the parameters of a value operand that is a
// bare placeholder (or, for IN, a parenthesized list of them). With
// allowCast, one `::type` / CAST(… AS type) around the placeholder is
// accepted. Any other shape returns nil. sql is the rendering the
// tokens index into.
func paramOperands(sql string, ops []ptok, inList, allowCast bool) []paramOperand {
	if inList {
		if len(ops) < 3 || ops[0].Kind != dialect.KindLParen || matchingParen(ops, 0) != len(ops)-1 {
			return nil
		}
		var out []paramOperand
		for _, arg := range splitArgs(ops[1 : len(ops)-1]) {
			p := paramOperands(sql, arg, false, allowCast)
			if p == nil {
				return nil
			}
			out = append(out, p...)
		}
		return out
	}
	if len(ops) == 1 && ops[0].param != "" {
		return []paramOperand{{param: ops[0].param}}
	}
	if !allowCast || len(ops) < 3 {
		return nil
	}
	if ops[0].param != "" && ops[1].Kind == dialect.KindCast && isTypeSpec(ops[2:]) {
		for _, t := range ops[2:] {
			if t.Kind == dialect.KindCast {
				return nil
			}
		}
		return []paramOperand{{param: ops[0].param, cast: tokenText(sql, ops[2:])}}
	}
	if ops[0].isIdent("CAST") && len(ops) >= 6 && ops[1].Kind == dialect.KindLParen &&
		matchingParen(ops, 1) == len(ops)-1 && ops[2].param != "" && ops[3].isIdent("AS") &&
		isTypeSpec(ops[4:len(ops)-1]) {
		return []paramOperand{{param: ops[2].param, cast: tokenText(sql, ops[4:len(ops)-1])}}
	}
	return nil
}

// tokenText is the source text spanning a non-empty token run.
func tokenText(sql string, ops []ptok) string {
	return sql[ops[0].Start:ops[len(ops)-1].End]
}

// resolveColumn binds a bare column reference (`col` or `q.col`)
// against the top-level relations through the R3 resolver. nil when
// unresolved, and nil when it resolves through a relation named like a
// statement-level CTE: the CTE shadows the base table, so the catalog's
// types are not the compared column's (under-report, never guess).
func resolveColumn(res *rules.ColumnResolver, ctes []string, col []ptok) *cache.Column {
	name := identText(col[len(col)-1])
	var c *cache.Column
	var rel string
	switch len(col) {
	case 1:
		c, rel = res.Column("", name)
	case 3:
		c, rel = res.Column(identText(col[0]), name)
	default:
		return nil
	}
	for _, cte := range ctes {
		if strings.EqualFold(cte, rel) {
			return nil
		}
	}
	return c
}

// cteNames returns the names a leading statement-level WITH defines:
// the identifier after WITH / RECURSIVE and after each depth-0 comma,
// up to the main statement's verb. Only these can shadow a top-level
// relation (a CTE inside a subquery scopes that subquery alone).
func cteNames(toks []ptok) []string {
	if len(toks) == 0 || !toks[0].isIdent("WITH") {
		return nil
	}
	var out []string
	expect := true
	for _, t := range toks[1:] {
		if t.depth != 0 {
			continue
		}
		switch {
		case t.isIdent("SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "VALUES", "TABLE"):
			return out
		case t.isIdent("RECURSIVE") && len(out) == 0:
		case t.Kind == dialect.KindComma:
			expect = true
		case expect && (t.Kind == dialect.KindIdent || t.Kind == dialect.KindQuotedIdent):
			out = append(out, identText(t))
			expect = false
		}
	}
	return out
}

// identText returns an identifier's name, unquoting a quoted one.
func identText(t ptok) string {
	if t.Kind == dialect.KindQuotedIdent && len(t.Text) >= 2 {
		return t.Text[1 : len(t.Text)-1]
	}
	return t.Text
}

// typeMismatchRule returns the dialect's whitelist: for a (column type,
// parameter type) pair it returns why the comparison converts the
// column, or "" when the pair is index-safe or not known to be unsafe.
func typeMismatchRule(dialectName string) func(colOID, paramOID uint32) string {
	switch dialectName {
	case "postgres":
		return pgTypeMismatch
	case "mysql":
		return mysqlTypeMismatch
	}
	// SQLite: a comparison with a bound parameter applies the column's
	// affinity to the parameter (the parameter has none), never the
	// other way round.
	return nil
}

// PostgreSQL: integer columns compared with numeric/float values. The
// integer btree operator family has no integer-vs-numeric/float
// operator, so the planner resolves `int_col = numeric` as
// `int_col::numeric = numeric` — the cast lands on the column.
// (int2/int4/int8 among themselves, float4/float8, date/timestamp and
// text/varchar compare inside one family: never flagged.)
func pgTypeMismatch(colOID, paramOID uint32) string {
	const (
		int2, int4, int8          = 21, 23, 20
		float4, float8, numericID = 700, 701, 1700
	)
	if !slices.Contains([]uint32{int2, int4, int8}, colOID) {
		return ""
	}
	switch paramOID {
	case numericID:
		return "PostgreSQL compares an integer with a numeric by casting the integer column to numeric on every row"
	case float4, float8:
		return "PostgreSQL compares an integer with a float by casting the integer column to double precision on every row"
	}
	return ""
}

// MySQL: a string column compared with a number is compared as
// floating point (both sides converted), which the MySQL manual calls
// out as unable to use an index on the string column. The reverse (a
// numeric column vs a string value) converts the value and stays
// index-safe.
func mysqlTypeMismatch(colOID, paramOID uint32) string {
	const flags = mysqlFlagUnsigned | mysqlFlagBinary
	stringCodes := []uint32{0xf9, 0xfa, 0xfb, 0xfc, 0xfd, 0xfe} // blob family (TEXT collapses here), VAR_STRING, STRING
	numberCodes := []uint32{0x01, 0x02, 0x03, 0x04, 0x05, 0x08, 0x09, 0xf6}
	if !slices.Contains(stringCodes, colOID&^flags) || !slices.Contains(numberCodes, paramOID&^flags) {
		return ""
	}
	return "MySQL compares a string column with a number as floating point, converting every row's column value"
}

// The MySQL TypeRef flag bits (internal/dialect/mysql/typemap.go:
// FlagUnsigned, FlagBinary), repeated here so the lint package stays
// free of dialect-implementation imports; TestPerfTypes_MySQLFlagsAgree pins them.
const (
	mysqlFlagUnsigned uint32 = 1 << 8
	mysqlFlagBinary   uint32 = 1 << 9
)
