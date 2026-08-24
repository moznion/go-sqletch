package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

const nativeTestDDL = `
CREATE TABLE users (
    id        BIGINT AUTO_INCREMENT PRIMARY KEY,
    email     VARCHAR(255) NOT NULL,
    org_id    BIGINT,
    mood      ENUM('ok','meh') NOT NULL
);
CREATE TABLE orgs (
    id   BIGINT PRIMARY KEY,
    name VARCHAR(64) NOT NULL
);
CREATE TABLE dupes (
    email VARCHAR(255) NOT NULL
);
CREATE TABLE wirey (
    y  YEAR,
    b  BIT(5),
    tt TINYTEXT,
    mb MEDIUMBLOB
);
`

func nativeOracle(t *testing.T) *NativeOracle {
	t.Helper()
	o, err := NewNativeOracle([]cache.SchemaFile{{Path: "s.sql", Content: []byte(nativeTestDDL)}}, "8.4")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func describe(t *testing.T, sql string) (dialect.Desc, error) {
	t.Helper()
	return nativeOracle(t).Describe(context.Background(), sql)
}

func TestNativeDescribeDirectColumns(t *testing.T) {
	desc, err := describe(t, `SELECT u.id, u.email AS mail, org_id FROM users AS u WHERE u.id = ? LIMIT ?`)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Params) != 2 {
		t.Fatalf("want 2 zero param slots, got %d", len(desc.Params))
	}
	for _, p := range desc.Params {
		if p.OID != 0 || p.Name != "" {
			t.Fatalf("param slots must stay zero (annotation-filled downstream): %+v", p)
		}
	}
	want := []dialect.ColumnDesc{
		{Name: "id", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"}, SrcRel: 3, SrcAtt: 1},
		{Name: "mail", Type: dialect.TypeRef{OID: typeVarString, Name: "varchar"}, SrcRel: 3, SrcAtt: 2},
		{Name: "org_id", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"}, SrcRel: 3, SrcAtt: 3},
	}
	if len(desc.Columns) != len(want) {
		t.Fatalf("want %d columns, got %+v", len(want), desc.Columns)
	}
	for i, w := range want {
		if desc.Columns[i] != w {
			t.Errorf("column %d: got %+v, want %+v", i, desc.Columns[i], w)
		}
	}
}

func TestNativeDescribeStarExpansion(t *testing.T) {
	desc, err := describe(t, `SELECT o.*, u.id FROM users AS u JOIN orgs AS o ON o.id = u.org_id`)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(desc.Columns))
	for _, c := range desc.Columns {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "id,name,id" {
		t.Fatalf("star must expand in catalog att order: %s", got)
	}
	if desc.Columns[0].SrcRel != desc.Columns[1].SrcRel {
		t.Fatal("o.* columns must share the orgs source relation")
	}
}

// TestNativeDescribeWireNormalization pins the protocol quirks the
// differential gate discovered: TEXT/BLOB flavors collapse to the
// BLOB wire code, and YEAR/BIT carry the UNSIGNED flag.
func TestNativeDescribeWireNormalization(t *testing.T) {
	desc, err := describe(t, "SELECT w.y, w.b, w.tt, w.mb FROM wirey AS w")
	if err != nil {
		t.Fatal(err)
	}
	want := []dialect.TypeRef{
		{OID: typeYear | FlagUnsigned, Name: "year unsigned"},
		{OID: typeBit | FlagUnsigned, Name: "bit unsigned"},
		{OID: typeBlob, Name: "text"},
		{OID: typeBlob | FlagBinary, Name: "blob"},
	}
	for i, w := range want {
		if desc.Columns[i].Type != w {
			t.Errorf("column %d: got %+v, want %+v", i, desc.Columns[i].Type, w)
		}
	}
}

func TestNativeDescribeExpressionColumns(t *testing.T) {
	// Hinted and aliased: accepted, type from the annotation.
	sql := "-- @column total: bigint\nSELECT count(*) AS total FROM users"
	desc, err := describe(t, sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Columns) != 1 || desc.Columns[0] != (dialect.ColumnDesc{
		Name: "total", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"},
	}) {
		t.Fatalf("hinted expression column: %+v", desc.Columns)
	}
}

func TestNativeDescribeDML(t *testing.T) {
	for _, sql := range []string{
		"INSERT INTO users (email, org_id) VALUES (?, ?)",
		"INSERT INTO users SET email = ?",
		"INSERT INTO users (email) VALUES (?) ON DUPLICATE KEY UPDATE email = ?",
		"UPDATE users AS u SET u.email = ? WHERE u.id = ?",
		"DELETE FROM users WHERE id = ?",
	} {
		desc, err := describe(t, sql)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
			continue
		}
		if len(desc.Columns) != 0 {
			t.Errorf("%s: DML must describe no columns", sql)
		}
	}
}

func TestNativeDescribeEngineParityRejections(t *testing.T) {
	// These are rejections the real engine would also make: they must
	// surface as OracleError (SQLETCH202-class), not as refusals.
	for _, tt := range []struct{ name, sql, want string }{
		{"unknown table", "SELECT id FROM missing", "doesn't exist"},
		{"unknown column", "SELECT nope FROM users", "unknown column"},
		{"unknown qualified", "SELECT u.nope FROM users AS u", "unknown column"},
		{"unknown qualifier", "SELECT x.id FROM users AS u", "unknown table"},
		{"table name hidden by alias", "SELECT users.id FROM users AS u", "unknown table"},
		{"ambiguous", "SELECT email FROM users, dupes", "ambiguous"},
		{"where unknown", "SELECT u.id FROM users AS u WHERE ghost = ?", "unknown column"},
		{"insert arity", "INSERT INTO users (email, org_id) VALUES (?)", "doesn't match"},
		{"insert unknown col", "INSERT INTO users (ghost) VALUES (?)", "unknown column"},
		{"unparsable", "SELECT FROM WHERE", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var oe *dialect.OracleError
			if !errors.As(err, &oe) {
				t.Fatalf("want *dialect.OracleError, got %T: %v", err, err)
			}
			if !strings.Contains(oe.Msg, tt.want) {
				t.Errorf("message %q should mention %q", oe.Msg, tt.want)
			}
		})
	}
}

func TestNativeDescribeSubsetRefusals(t *testing.T) {
	// These are deliberate subset exclusions: they must surface as
	// NativeUnsupportedError (SQLETCH214), never as engine errors and
	// never as guesses.
	for _, tt := range []struct{ name, sql, want string }{
		{"derived table", "SELECT x.id FROM (SELECT id FROM users) AS x", "derived table"},
		{"scalar subquery", "SELECT (SELECT max(id) FROM users) AS m FROM users", "subquery"},
		{"exists subquery", "SELECT u.id FROM users AS u WHERE EXISTS (SELECT 1 FROM orgs)", "EXISTS"},
		{"in subquery", "SELECT u.id FROM users AS u WHERE u.org_id IN (SELECT id FROM orgs)", "subquery"},
		{"unaliased expression", "SELECT count(*) FROM users", "AS alias"},
		{"unhinted expression", "SELECT count(*) AS total FROM users", "@column"},
		{"enum projection", "SELECT mood FROM users", "ENUM/SET"},
		{"enum via star", "SELECT * FROM users", "ENUM/SET"},
		{"other statement", "SHOW TABLES", "SELECT/INSERT/UPDATE/DELETE"},
		{"insert select", "INSERT INTO users (email) SELECT name FROM orgs", "INSERT ... SELECT"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError, got %T: %v", err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, tt.want) {
				t.Errorf("refusal %q/%q should mention %q", ne.Construct, ne.Hint, tt.want)
			}
		})
	}
}

func TestNativeDescribeRefusesWithClause(t *testing.T) {
	// A WITH (CTE) clause is outside the native subset. It must be
	// REFUSED as a NativeUnsupportedError (SQLETCH214), never silently
	// ignored: the describers used to skip s.With entirely, so a query
	// whose CTE the main statement does not reference (or whose CTE body
	// is broken) verified offline and then failed at execution on a
	// real server — exactly what fail-closed refusal exists to prevent.
	for _, tt := range []struct{ name, sql string }{
		// Unused CTE: the pre-fix sail-through — the main query is
		// wholly valid, so describe returned a clean Desc (err=nil).
		{"select unused cte", "WITH x AS (SELECT id FROM users) SELECT o.id FROM orgs o"},
		// CTE body references a nonexistent table/column: pre-fix this
		// was neither validated nor refused when unreferenced.
		{"select unused broken cte", "WITH x AS (SELECT bogus FROM nonexistent) SELECT o.id FROM orgs o"},
		// Referenced CTE: pre-fix failed with the wrong message
		// ("table x doesn't exist"), an OracleError not a 214.
		{"select referenced cte", "WITH x AS (SELECT id FROM users) SELECT x.id FROM x"},
		{"recursive cte", "WITH RECURSIVE x AS (SELECT 1 AS n) SELECT o.id FROM orgs o"},
		{"update with cte", "WITH x AS (SELECT id FROM users) UPDATE orgs SET name = 'a'"},
		{"delete with cte", "WITH x AS (SELECT id FROM users) DELETE FROM orgs"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError for %q, got %T: %v", tt.sql, err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, "WITH") {
				t.Errorf("refusal %q/%q should mention WITH", ne.Construct, ne.Hint)
			}
		})
	}
}

func TestNativeDescribeRefusesSchemaQualifiedFrom(t *testing.T) {
	// The native backend models a SINGLE DDL-built database with no DSN;
	// a cross-database FROM reference (`otherdb.users`) is unmodelable.
	// Pre-fix scopeFrom looked up `r.Table` ignoring `r.Schema`, so
	// `SELECT id FROM otherdb.users` silently resolved to the LOCAL
	// `users` and typed `id` from the wrong table (err=nil). It must be
	// REFUSED as a NativeUnsupportedError (SQLETCH214), matching the
	// existing schema-qualified column/star refusals — never a guess.
	for _, tt := range []struct{ name, sql string }{
		{"schema-qualified from table", "SELECT id FROM otherdb.users"},
		{"schema-qualified from with alias", "SELECT u.id FROM otherdb.users u"},
		{"schema-qualified comma join", "SELECT o.id FROM orgs o, otherdb.users u"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError for %q, got %T: %v", tt.sql, err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, "schema-qualified") {
				t.Errorf("refusal %q/%q should mention schema-qualified", ne.Construct, ne.Hint)
			}
		})
	}
}

func TestNativeDescribeRefusesNamedWindow(t *testing.T) {
	// A statement-level named WINDOW clause is the same sail-through
	// class as WITH: its body lives only in SelectStmt.WindowSpecs, so a
	// broken or unused named window used to pass describe with err=nil.
	// It must be REFUSED (SQLETCH214). An inline OVER(...) in the select
	// list is walked normally and stays out of this refusal.
	for _, tt := range []struct{ name, sql string }{
		// Unused named window whose body references a ghost column:
		// pre-fix this returned a clean Desc.
		{"unused named window", "SELECT o.id FROM orgs o WINDOW w AS (ORDER BY ghost)"},
		{"named window referenced by name", "SELECT COUNT(*) OVER w AS c FROM orgs o WINDOW w AS (ORDER BY o.id)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError for %q, got %T: %v", tt.sql, err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, "WINDOW") {
				t.Errorf("refusal %q/%q should mention WINDOW", ne.Construct, ne.Hint)
			}
		})
	}
}

func TestNativeDescribeInertInSubquery(t *testing.T) {
	// The dialect's own arity-0 @in emission must pass: it is pinned
	// skeleton text (dialect.InEmptySQL), not an authored subquery.
	sql := "SELECT u.id FROM users AS u WHERE u.email IN (SELECT NULL FROM DUAL WHERE FALSE)"
	if _, err := describe(t, sql); err != nil {
		t.Fatalf("arity-0 @in emission must be accepted: %v", err)
	}
	// It must be recognized verbatim from dialect.InEmptySQL, so a drift
	// between the matcher and the emission cannot pass silently.
	sql = "SELECT u.id FROM users AS u WHERE u.email " + (Profile{}).InEmptySQL()
	if _, err := describe(t, sql); err != nil {
		t.Fatalf("dialect.InEmptySQL emission must be accepted: %v", err)
	}
}

// TestNativeDescribeMultiColumnSubqueryRefused is the fail-closed
// regression: a multi-column constant subquery is ER_OPERAND_COLUMNS
// (1241) at PREPARE on a real server, so the native oracle must refuse
// it (SQLETCH214) rather than statically bless a shape the engine
// rejects. Before the fix inertSubquery accepted these (err=nil).
func TestNativeDescribeMultiColumnSubqueryRefused(t *testing.T) {
	for _, tt := range []struct{ name, sql string }{
		{"in list", "SELECT id FROM users WHERE id IN (SELECT 1, 2)"},
		{"equality", "SELECT id FROM users WHERE id = (SELECT 1, 2)"},
		{"projected", "-- @column x: bigint\nSELECT (SELECT 1, 2) AS x FROM users"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError, got %T: %v", err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, "subquery") {
				t.Errorf("refusal %q/%q should mention subquery", ne.Construct, ne.Hint)
			}
		})
	}
}

// TestNativeDescribeUserConstSubqueryRefused pins the doc-15-faithful
// choice: only the backend's own arity-0 emission is inert. Even a
// single-column user constant subquery — server-acceptable, but not
// something the v1 subset types — is refused, never silently skipped.
func TestNativeDescribeUserConstSubqueryRefused(t *testing.T) {
	for _, tt := range []struct{ name, sql string }{
		{"single-column const in", "SELECT id FROM users WHERE id IN (SELECT 1)"},
		{"single-column const eq", "SELECT id FROM users WHERE id = (SELECT 1)"},
		{"null but where true", "SELECT id FROM users WHERE id IN (SELECT NULL FROM DUAL WHERE TRUE)"},
		{"null but limited", "SELECT id FROM users WHERE id IN (SELECT NULL FROM DUAL WHERE FALSE LIMIT 1)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError, got %T: %v", err, err)
			}
		})
	}
}

// TestNativeDescribeNonInertDualSubqueryRefused pins the fail-closed
// boundary of the inert @in matcher. The pinned TiDB parser parses
// every bare/unquoted `FROM DUAL` spelling to a nil From, so a non-nil
// From carrying a "dual" relation arises only from a REAL table
// reference — backquoted (`dual`) or schema-qualified (somedb.dual) —
// which a real server rejects at PREPARE with ER_NO_SUCH_TABLE.
// Accepting those was fail-open (err=nil for a shape the engine
// refuses); they must be SQLETCH214 refusals. A `?` projection is a
// user bind parameter smuggled into a dead subquery — not the
// emission either, and likewise refused.
func TestNativeDescribeNonInertDualSubqueryRefused(t *testing.T) {
	for _, tt := range []struct{ name, sql string }{
		{"schema-qualified dual", "SELECT id FROM users WHERE id IN (SELECT NULL FROM somedb.dual WHERE FALSE)"},
		{"backquoted dual", "SELECT id FROM users WHERE id IN (SELECT NULL FROM `dual` WHERE FALSE)"},
		{"param marker projection", "SELECT id FROM users WHERE id IN (SELECT ? FROM DUAL WHERE FALSE)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError (SQLETCH214), got %T: %v", err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, "subquery") {
				t.Errorf("refusal %q/%q should mention subquery", ne.Construct, ne.Hint)
			}
		})
	}
}

// TestNativeDescribeFromlessInSubqueryRefused is the M7(a) regression:
// only the dialect's own arity-0 @in emission (dialect.InEmptySQL, which
// carries FROM DUAL) is inert. The pinned TiDB parser drops FROM DUAL to
// a nil From — so a genuinely FROM-less `SELECT NULL WHERE FALSE` parses
// to the SAME nil-From shape yet is REJECTED by a real MySQL 8.0 server
// (ER_NO_TABLES_USED: a SELECT with a WHERE and no FROM). Accepting it
// was fail-open — native passed what the server refuses. It must be a
// SQLETCH214 refusal; the genuine emission must still be accepted.
func TestNativeDescribeFromlessInSubqueryRefused(t *testing.T) {
	fromless := []struct{ name, sql string }{
		{"fromless null where false", "SELECT id FROM users WHERE id IN (SELECT NULL WHERE FALSE)"},
		{"fromless null where true", "SELECT id FROM users WHERE id IN (SELECT NULL WHERE TRUE)"},
	}
	for _, tt := range fromless {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError (SQLETCH214), got %T: %v", err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, "subquery") {
				t.Errorf("refusal %q/%q should mention subquery", ne.Construct, ne.Hint)
			}
		})
	}
	// The genuine arity-0 @in emission (with FROM DUAL) must still pass.
	sql := "SELECT u.id FROM users AS u WHERE u.email " + (Profile{}).InEmptySQL()
	if _, err := describe(t, sql); err != nil {
		t.Fatalf("dialect.InEmptySQL emission must still be accepted: %v", err)
	}
}

// TestNativeDescribeIntoOutfileRefused is the M7(b) regression:
// describeSelect never checked SelectIntoOpt, so `SELECT id FROM t1 INTO
// OUTFILE '…'` described with one result column while a real server's
// COM_STMT_PREPARE reports ZERO result columns (rows go to the file) — a
// divergence. It must be refused (SQLETCH214). A normal SELECT with no
// INTO clause still describes.
func TestNativeDescribeIntoOutfileRefused(t *testing.T) {
	for _, tt := range []struct{ name, sql string }{
		{"into outfile", "SELECT id FROM users INTO OUTFILE '/tmp/x'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError (SQLETCH214), got %T: %v", err, err)
			}
		})
	}
	// Control: a normal SELECT still describes.
	if _, err := describe(t, "SELECT id FROM users"); err != nil {
		t.Fatalf("plain SELECT must still describe: %v", err)
	}
}

func TestNativeDescribeAliasVisibility(t *testing.T) {
	// Output aliases are visible to GROUP BY / HAVING / ORDER BY,
	// like MySQL's own resolution.
	sql := "-- @column total: bigint\n" +
		"SELECT org_id AS grp, count(*) AS total FROM users GROUP BY grp HAVING total > ? ORDER BY total DESC"
	if _, err := describe(t, sql); err != nil {
		t.Fatalf("alias resolution in GROUP BY/HAVING/ORDER BY: %v", err)
	}
	// But not in WHERE (the server rejects it there too).
	sql = "-- @column total: bigint\nSELECT count(*) AS total FROM users WHERE total > ?"
	var oe *dialect.OracleError
	if _, err := describe(t, sql); !errors.As(err, &oe) {
		t.Fatalf("aliases must not resolve in WHERE, got %v", err)
	}
}

func TestNativeDescribeQuestionCountsIgnoreStringsAndComments(t *testing.T) {
	desc, err := describe(t, "SELECT u.id FROM users AS u WHERE u.email = '?' -- ? in comment\n  AND u.id = ?")
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Params) != 1 {
		t.Fatalf("placeholders in strings/comments must not count: got %d", len(desc.Params))
	}
}

func TestCountPlaceholdersFailsClosedOnExecutableComment(t *testing.T) {
	// PR #84 Fix C makes the lexer refuse `/*! … */` (the server strips the
	// markers and executes the content, so a placeholder inside it is
	// invisible to sqletch). countPlaceholders must PROPAGATE that lex
	// error, never return the partial count accumulated before it — a
	// short count would size fewer param slots than the server binds.
	sql := "SELECT id FROM users WHERE a = ? AND b = /*! ? */ 1"
	n, err := countPlaceholders(sql)
	if err == nil {
		t.Fatalf("countPlaceholders swallowed the executable-comment lex error and returned a partial count %d; want an error", n)
	}
	var le *dialect.LexError
	if !errors.As(err, &le) {
		t.Fatalf("countPlaceholders error = %T (%v), want *dialect.LexError", err, err)
	}
	if !strings.Contains(le.Msg, "executable comment") {
		t.Errorf("lex error should mention the executable comment, got %q", le.Msg)
	}
}

func TestNativeDescribeFailsClosedOnExecutableComment(t *testing.T) {
	// Defense-in-depth sibling of the countPlaceholders backstop: a `/*! …
	// */` that somehow reaches the oracle (parseSQL strips it and describes
	// fine) must make Describe REFUSE, not silently emit too few param
	// slots. Here the server binds two '?', but only one is lexer-visible.
	sql := "SELECT u.id FROM users AS u WHERE u.id = ? /*! AND u.org_id = ? */"
	_, err := describe(t, sql)
	if err == nil {
		t.Fatal("Describe must fail closed on an executable comment, got nil error")
	}
	var ue *dialect.NativeUnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("Describe error = %T (%v), want *dialect.NativeUnsupportedError", err, err)
	}
	if !strings.Contains(ue.Construct, "executable comment") {
		t.Errorf("refusal should name the executable comment, got %q", ue.Construct)
	}
}

func TestNativeDescribeJoinOnConditionsResolve(t *testing.T) {
	// H1: scopeFrom flattens the FROM tree but drops the join
	// conditions, so a JOIN ON referencing a nonexistent column used to
	// describe with err=nil while the server rejects it
	// (ER_BAD_FIELD_ERROR). The ON expression must resolve against the
	// scope. This covers SELECT, UPDATE, and (multi-table) DELETE FROM
	// trees.
	for _, tt := range []struct{ name, sql, want string }{
		{"select on lhs ghost",
			"SELECT o.id FROM orgs o JOIN users u ON o.ghost = u.org_id", "unknown column"},
		{"select on rhs ghost",
			"SELECT o.id FROM orgs o JOIN users u ON o.id = u.ghost", "unknown column"},
		{"select on unknown qualifier",
			"SELECT o.id FROM orgs o JOIN users u ON x.id = u.org_id", "unknown table"},
		{"update multi-table on ghost",
			"UPDATE orgs o JOIN users u ON o.ghost = u.org_id SET o.name = ?", "unknown column"},
		{"delete multi-table on ghost",
			"DELETE o FROM orgs o JOIN users u ON o.id = u.ghost", "unknown column"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var oe *dialect.OracleError
			if !errors.As(err, &oe) {
				t.Fatalf("want *dialect.OracleError, got %T: %v", err, err)
			}
			if !strings.Contains(oe.Msg, tt.want) {
				t.Errorf("message %q should mention %q", oe.Msg, tt.want)
			}
		})
	}
}

func TestNativeDescribeJoinOnConditionsValid(t *testing.T) {
	// The valid counterpart must still pass unchanged (byte-identity):
	// a well-formed ON condition resolves.
	for _, sql := range []string{
		"SELECT o.id FROM orgs o JOIN users u ON o.id = u.org_id",
		"UPDATE orgs o JOIN users u ON o.id = u.org_id SET o.name = ?",
		"DELETE o FROM orgs o JOIN users u ON o.id = u.org_id",
	} {
		if _, err := describe(t, sql); err != nil {
			t.Errorf("valid ON condition must pass: %s: %v", sql, err)
		}
	}
}

func TestNativeDescribeRefusesNaturalAndUsingJoins(t *testing.T) {
	// H2: MySQL COALESCEs the common columns of a NATURAL/USING join
	// into a single output column (appearing once, from the first
	// table). The native star expander concatenates all columns of all
	// tables, so `SELECT * FROM t1 JOIN t2 USING (id)` produced a wrong
	// struct shape (a duplicated `id`) and broke byte-identity with the
	// server backend. Refuse these joins (SQLETCH214) rather than guess.
	for _, tt := range []struct{ name, sql string }{
		{"star using", "SELECT * FROM orgs o JOIN users u USING (id)"},
		{"star natural", "SELECT * FROM orgs o NATURAL JOIN users u"},
		{"projected using", "SELECT o.id FROM orgs o JOIN users u USING (id)"},
		{"projected natural", "SELECT o.name FROM orgs o NATURAL JOIN users u"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError, got %T: %v", err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, "USING") {
				t.Errorf("refusal %q/%q should mention NATURAL/USING", ne.Construct, ne.Hint)
			}
		})
	}
}

func TestNativeDescribeRejectsDuplicateEffectiveNames(t *testing.T) {
	// H3: two FROM items sharing an effective qualifier (alias-else-
	// table) leave a qualified reference without a single referent.
	// MySQL rejects the statement outright (ER_NONUNIQ_TABLE); scopeFrom
	// used to accept it. Comparison is case-insensitive.
	for _, tt := range []struct{ name, sql string }{
		{"same table twice", "SELECT id FROM orgs, orgs"},
		{"duplicate alias", "SELECT a.name FROM orgs AS a, users AS a"},
		{"duplicate alias case-insensitive", "SELECT id FROM orgs AS a, users AS A"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var oe *dialect.OracleError
			if !errors.As(err, &oe) {
				t.Fatalf("want *dialect.OracleError, got %T: %v", err, err)
			}
			if !strings.Contains(oe.Msg, "not unique") {
				t.Errorf("message %q should mention %q", oe.Msg, "not unique")
			}
		})
	}
}

func TestNativeDescribeMultiTableDeleteTargets(t *testing.T) {
	// H4: a multi-table DELETE names its delete targets in s.Tables,
	// which describeDelete used to ignore — `DELETE ghost FROM ...`
	// described with err=nil while the server rejects the unknown
	// target. Each target must reference a FROM table or alias.
	t.Run("unknown target", func(t *testing.T) {
		_, err := describe(t, "DELETE ghost FROM orgs o JOIN users u ON o.id = u.org_id")
		var oe *dialect.OracleError
		if !errors.As(err, &oe) {
			t.Fatalf("want *dialect.OracleError, got %T: %v", err, err)
		}
		if !strings.Contains(oe.Msg, "MULTI DELETE") {
			t.Errorf("message %q should mention MULTI DELETE", oe.Msg)
		}
	})
	t.Run("valid alias target", func(t *testing.T) {
		if _, err := describe(t, "DELETE o FROM orgs o JOIN users u ON o.id = u.org_id"); err != nil {
			t.Fatalf("valid delete target must pass: %v", err)
		}
	})
}

func TestNativeServerVersionIsThePin(t *testing.T) {
	v, err := nativeOracle(t).ServerVersion(context.Background())
	if err != nil || v != "8.4" {
		t.Fatalf("ServerVersion must return the pin verbatim, got %q, %v", v, err)
	}
}
