package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/shape"
	"github.com/moznion/go-sqletch/internal/template"
)

// Policy weaving is checked here against its MEANING, not its text.
//
// Every silent tenant leak the audits found (#118–#124 and friends) was
// a hand-written lexical scanner in internal/policy misreading a query —
// an OR it did not see, a keyword column it took for a clause keyword, a
// join it attached the conjunct to — and Enforce, which shares those
// scanners, agreed with the mistake. A structural oracle built from the
// same reading inherits the same blind spots, so this harness uses an
// independent one: a real engine. Two tenants' rows carry a marker in
// `secret` ('T1' / 'T2'); every generated query projects each designated
// occurrence's `secret`; the woven query runs with tid = 1, and a 'T2'
// in any projected marker is a leak. DML is checked the same way
// against tenant 2's rows, inside a rolled-back transaction.
//
// The query passes through the pipeline's own scan sequence
// (scanChecks: lexical rules, Weave, R1) and the policy enforcement pass,
// exactly as generate would; a query the pipeline refuses (loudly) is not
// a finding. Every runtime shape is executed, not just the maximal one.

// leakEngine is the oracle: a database holding leakSchema/leakData.
type leakEngine interface {
	// query runs a statement and returns its rows, NULL as nullCell.
	query(sql string, args []any) (cols []string, rows [][]string, err error)
	exec(sql string, args []any) error
}

const nullCell = "\x00NULL"

// leakDialect describes what the generator may emit on one dialect.
type leakDialect struct {
	name      string
	paramType string   // the policy parameter's @param type
	quirkCols []string // keyword-named columns legal bare on this dialect
	rightJoin bool
	fullJoin  bool
	// mysqlOps enables MySQL-only operand prefixes (BINARY, INTERVAL) —
	// the audit-18 operand-position class.
	mysqlOps bool
	// updateFrom / deleteUsing / multiTableDML select the dialect's
	// joined-DML spellings.
	updateFrom    bool
	deleteUsing   bool
	multiTableDML bool
	// tableSuffix renames orders/acc (orders_<suffix>, …) where a
	// fuzz worker process cannot get a schema of its own (MySQL test
	// users cannot CREATE DATABASE).
	tableSuffix string
}

var leakTableRe = regexp.MustCompile(`\b(orders|acc)\b`)

// rename applies d.tableSuffix to every table name in s; column names
// like acc_id are untouched (`_` is a word character).
func (d leakDialect) rename(s string) string {
	if d.tableSuffix == "" {
		return s
	}
	return leakTableRe.ReplaceAllString(s, "${1}_"+d.tableSuffix)
}

// leakSchema returns the DDL for d, quoting the quirk columns.
func leakSchema(d leakDialect, quote func(string) string) []string {
	var extra strings.Builder
	for _, c := range d.quirkCols {
		fmt.Fprintf(&extra, ", %s INTEGER", quote(c))
	}
	return []string{
		d.rename("CREATE TABLE acc (id INTEGER PRIMARY KEY, k INTEGER, g INTEGER)"),
		d.rename("CREATE TABLE orders (id INTEGER PRIMARY KEY, tenant_id INTEGER NOT NULL, acc_id INTEGER, k INTEGER, v INTEGER, secret VARCHAR(8) NOT NULL" + extra.String() + ")"),
	}
}

// leakData seeds both tenants symmetrically, so a predicate that
// selects some tenant-1 row selects its tenant-2 twin — a leaked
// disjunct is observable rather than vacuously empty. NULLs are in
// every nullable column.
func leakData(d leakDialect, quote func(string) string) []string {
	out := []string{
		d.rename("INSERT INTO acc (id, k, g) VALUES (1, 0, 0), (2, 1, 1), (3, 1, NULL)"),
	}
	var qcols, qvals strings.Builder
	for _, c := range d.quirkCols {
		fmt.Fprintf(&qcols, ", %s", quote(c))
	}
	id := 1
	for _, tenant := range []int{1, 2} {
		for _, acc := range []string{"1", "2", "NULL"} {
			for _, k := range []string{"0", "1", "NULL"} {
				for _, v := range []string{"0", "1"} {
					qvals.Reset()
					for range d.quirkCols {
						fmt.Fprintf(&qvals, ", %d", id%2)
					}
					out = append(out, d.rename(fmt.Sprintf(
						"INSERT INTO orders (id, tenant_id, acc_id, k, v, secret%s) VALUES (%d, %d, %s, %s, %s, 'T%d'%s)",
						qcols.String(), id, tenant, acc, k, v, tenant, qvals.String())))
					id++
				}
			}
		}
	}
	return out
}

// leakGen decodes fuzz bytes into a query: every byte is a production
// choice, and an exhausted input reads as zeros, so ANY input yields a
// syntactically plausible template (random byte mutation on raw SQL
// text rarely gets past the scanner with any structure — measured: of a
// 30 s FuzzComposeConformance corpus, 3 % combined two constructs and
// none nested one inside another).
type leakGen struct {
	b []byte
	i int
	d leakDialect
}

func (g *leakGen) pick(n int) int {
	if g.i >= len(g.b) {
		return 0
	}
	v := int(g.b[g.i]) % n
	g.i++
	return v
}

// rel is one FROM item the expression generator may reference.
type rel struct {
	name       string // effective name (alias else table)
	designated bool   // an `orders` occurrence
}

var (
	ordersCols = []string{"id", "tenant_id", "acc_id", "k", "v"}
	accCols    = []string{"id", "k", "g"}
)

func (g *leakGen) col(r rel) string {
	if r.designated {
		cols := append(append([]string(nil), ordersCols...), g.d.quirkCols...)
		return cols[g.pick(len(cols))]
	}
	return accCols[g.pick(len(accCols))]
}

// ref is a column reference: qualified, or bare when the column is
// unique to orders (a bare quirk column is the audit-16 shape).
func (g *leakGen) ref(rels []rel) string {
	r := rels[g.pick(len(rels))]
	if r.designated && len(g.d.quirkCols) > 0 && g.pick(4) == 0 {
		return g.d.quirkCols[g.pick(len(g.d.quirkCols))]
	}
	return r.name + "." + g.col(r)
}

func (g *leakGen) atom(rels []rel) string {
	x := g.ref(rels)
	switch g.pick(16) {
	case 0:
		return x + " = 1"
	case 1:
		return x + " <> 0"
	case 2:
		return x + " IS NULL"
	case 3:
		return x + " IS NOT NULL"
	case 4:
		return x + " IN (0, 1)"
	case 5:
		return x + " BETWEEN 0 AND 1"
	case 6:
		return "CASE WHEN " + x + " = 1 THEN 1 ELSE 0 END = 1"
	case 7:
		return "CASE " + x + " WHEN 0 THEN 1 END = 1"
	case 8:
		return "(" + x + " + 1) = 2"
	case 9:
		return x + " = " + g.ref(rels)
	case 10:
		return "EXISTS (SELECT 1 FROM acc AS x WHERE x.k = " + x + ")"
	case 11:
		return "1 = 1"
	case 12:
		if g.d.mysqlOps {
			return "BINARY " + x + " = 1"
		}
		return "CAST(" + x + " AS INTEGER) = 1"
	case 13:
		if g.d.mysqlOps {
			return "DATE '2020-01-01' + INTERVAL " + x + " DAY > DATE '2019-01-01'"
		}
		return "-" + x + " < 0"
	case 14:
		return "NOT " + x + " = 1"
	default:
		return x + " > 0"
	}
}

func (g *leakGen) expr(rels []rel, depth int) string {
	if depth >= 3 {
		return g.atom(rels)
	}
	switch g.pick(7) {
	case 0, 1:
		return g.expr(rels, depth+1) + " AND " + g.expr(rels, depth+1)
	case 2, 3:
		return g.expr(rels, depth+1) + " OR " + g.expr(rels, depth+1)
	case 4:
		return "(" + g.expr(rels, depth+1) + ")"
	case 5:
		return "NOT (" + g.expr(rels, depth+1) + ")"
	default:
		return g.atom(rels)
	}
}

// from returns a FROM clause and the relations it exposes.
func (g *leakGen) from() (string, []rel) {
	o := rel{"o", true}
	a := rel{"a", false}
	on := func(rels []rel) string {
		base := "o.acc_id = a.id"
		switch g.pick(4) {
		case 1:
			return base + " AND " + g.expr(rels, 1)
		case 2:
			return base + " OR " + g.expr(rels, 1)
		}
		return base
	}
	both := []rel{o, a}
	switch g.pick(14) {
	case 0:
		return "orders AS o", []rel{o}
	case 1:
		return "orders", []rel{{"orders", true}}
	case 2:
		return "acc AS a JOIN orders AS o ON " + on(both), both
	case 3:
		return "acc AS a LEFT JOIN orders AS o ON " + on(both), both
	case 4:
		return "orders AS o LEFT JOIN acc AS a ON " + on(both), both
	case 5:
		return "acc AS a CROSS JOIN orders AS o", both
	case 6:
		return "orders AS o, acc AS a", both
	case 7:
		if g.d.rightJoin {
			return "orders AS o RIGHT JOIN acc AS a ON " + on(both), both
		}
		return "acc AS a, orders AS o", both
	case 8:
		o1, o2 := rel{"o1", true}, rel{"o2", true}
		return "orders AS o1 JOIN orders AS o2 ON o2.k = o1.k", []rel{o1, o2}
	case 9:
		b := rel{"b", false}
		return "acc AS a LEFT JOIN orders AS o ON " + on(both) + " LEFT JOIN acc AS b ON b.id = o.acc_id", []rel{o, a, b}
	case 10:
		if g.d.fullJoin {
			return "acc AS a FULL JOIN orders AS o ON " + on(both), both
		}
		return "orders AS o JOIN acc AS a ON " + on(both), both
	case 11:
		return "orders AS o JOIN acc AS a USING (k)", both
	case 12:
		// o is on the PRESERVED side of its own (inner) join and
		// null-extended by the enclosing LEFT JOIN: the inner ON is the
		// wrong place to scope it (audit-20 wrong-join class).
		b := rel{"b", false}
		return "acc AS a LEFT JOIN (orders AS o JOIN acc AS b ON b.id = o.acc_id) ON o.acc_id = a.id", []rel{o, a, b}
	default:
		return "acc AS a LEFT JOIN orders AS o ON o.acc_id = a.id CROSS JOIN acc AS c", []rel{o, a, {"c", false}}
	}
}

func projection(rels []rel) string {
	var parts []string
	n := 0
	for _, r := range rels {
		if r.designated {
			parts = append(parts, fmt.Sprintf("%s.secret AS s%d", r.name, n))
			n++
		}
	}
	return strings.Join(parts, ", ")
}

// where emits an optional WHERE, an optional guarded conjunct (so the
// weave is checked against every guard shape), and an optional tail —
// clause keywords are where the scanners' terminator logic lives.
func (g *leakGen) where(rels []rel) string {
	var s strings.Builder
	hasWhere := false
	if g.pick(4) != 0 {
		s.WriteString("\nWHERE " + g.expr(rels, 0))
		hasWhere = true
	}
	if g.pick(3) == 0 {
		if !hasWhere {
			s.WriteString("\nWHERE TRUE")
		}
		s.WriteString("\n@if-present(f)\n  AND " + g.ref(rels) + " = :f\n@endif")
	}
	return s.String()
}

func (g *leakGen) tail(rels []rel) string {
	switch g.pick(5) {
	case 1:
		return "\nORDER BY " + g.ref(rels)
	case 2:
		return "\nLIMIT 1000"
	case 3:
		return "\nORDER BY 1 LIMIT 1000 OFFSET 0"
	}
	return ""
}

// leakCase is one generated template plus how to judge it.
type leakCase struct {
	src string
	dml bool
}

func (g *leakGen) generate() leakCase {
	hdr := "-- name: Q :many\n-- @param f: " + g.d.paramType + "\n"
	switch g.pick(10) {
	case 7: // UPDATE
		var s string
		switch {
		case g.d.updateFrom && g.pick(2) == 0:
			rels := []rel{{"o", true}, {"a", false}}
			s = "UPDATE orders AS o SET v = 9 FROM acc AS a WHERE o.acc_id = a.id AND (" + g.expr(rels, 1) + ")"
		case g.d.multiTableDML && g.pick(2) == 0:
			rels := []rel{{"o", true}, {"a", false}}
			s = "UPDATE acc AS a JOIN orders AS o ON o.acc_id = a.id SET o.v = 9" + g.where(rels)
		default:
			rels := []rel{{"orders", true}}
			s = "UPDATE orders SET v = 9" + g.where(rels)
		}
		return leakCase{src: strings.Replace(hdr, ":many", ":exec", 1) + s + "\n", dml: true}
	case 8: // DELETE
		var s string
		switch {
		case g.d.deleteUsing && g.pick(2) == 0:
			rels := []rel{{"o", true}, {"a", false}}
			s = "DELETE FROM orders AS o USING acc AS a WHERE o.acc_id = a.id AND (" + g.expr(rels, 1) + ")"
		default:
			rels := []rel{{"orders", true}}
			s = "DELETE FROM orders" + g.where(rels)
		}
		return leakCase{src: strings.Replace(hdr, ":many", ":exec", 1) + s + "\n", dml: true}
	case 9: // set operation: every branch is a scoped read of its own
		var branches []string
		for range 1 + g.pick(2) + 1 {
			o := []rel{{"o", true}}
			b := "SELECT o.secret AS s0 FROM orders AS o"
			if g.pick(2) == 0 {
				b += " WHERE " + g.expr(o, 1)
			}
			branches = append(branches, b)
		}
		op := []string{"\nUNION ALL\n", "\nUNION\n"}[g.pick(2)]
		return leakCase{src: hdr + strings.Join(branches, op) + "\n"}
	default:
		fromSQL, rels := g.from()
		return leakCase{src: hdr + "SELECT " + projection(rels) + "\nFROM " + fromSQL + g.where(rels) + g.tail(rels) + "\n"}
	}
}

// leakPolicies returns the tenant policy in one of two spellings: the
// second carries a top-level OR, so an unparenthesized splice of the
// PREDICATE (audit-11) is as observable as an unwrapped query clause.
func leakPolicies(d leakDialect, variant int) []policy.Policy {
	pred := "{}.tenant_id = :tid"
	if variant%2 == 1 {
		pred = "{}.tenant_id = :tid OR {}.tenant_id = -1"
	}
	return []policy.Policy{{Name: "tenant", Tables: []string{d.rename("orders")}, Predicate: pred,
		ParamName: "tid", ParamType: d.paramType}}
}

// leakArgs binds every placeholder: tid = 1 (tenant 1 is the caller),
// f = 1.
func leakArgs(seq []string) []any {
	out := make([]any, len(seq))
	for i := range seq {
		out[i] = int64(1)
	}
	return out
}

const tenant2Rows = "SELECT id, v FROM orders WHERE tenant_id = 2 ORDER BY id"

// checkNoLeak is the property. It returns true when the input was
// usable (the pipeline accepted it and the unwoven query runs).
func checkNoLeak(t *testing.T, d leakDialect, eng leakEngine, raw []byte) bool {
	t.Helper()
	if len(raw) == 0 {
		return false
	}
	g := &leakGen{b: raw[1:], d: d}
	lc := g.generate()
	lc.src = d.rename(lc.src)
	pols := leakPolicies(d, int(raw[0]))
	drv := driverFor(config.Config{Dialect: d.name})

	f, diags := template.NewScanner(drv.profile).ScanFile("q.sql", []byte(lc.src))
	if diagnostics.HasErrors(diags) || len(f.Queries) != 1 {
		return false
	}
	q := f.Queries[0]
	wres, rs, diags, err := scanChecks(drv, pols, q, 0, false)
	if err != nil || diagnostics.HasErrors(diags) || len(rs) == 0 {
		return false // refused loudly (incl. SQLETCH125): not a leak
	}
	tree, err := drv.frontend.Parse(rs[0].SQL)
	if err != nil {
		t.Fatalf("woven maximal rendering does not parse: %v\n%s", err, rs[0].SQL)
	}
	if ed := policy.Enforce(drv.profile, drv.frontend, pols, wres.Query, tree, rs[0]); diagnostics.HasErrors(ed) {
		// Weave accepted and woven it, enforcement disagrees: one of
		// the two misreads the query.
		t.Fatalf("Weave/Enforce disagree on\n%s\nwoven:\n%s\nenforce: %+v", lc.src, rs[0].SQL, ed)
	}

	keys, _ := shape.Enumerate(wres.Query, 64)
	usable := false
	for _, k := range keys {
		plain, err1 := ast.RenderShape(drv.profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
		woven, err2 := ast.RenderShape(drv.profile, wres.Query, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
		if err1 != nil || err2 != nil {
			continue
		}
		if lc.dml {
			if checkDMLShape(t, eng, d.rename(tenant2Rows), lc.src, plain, woven) {
				usable = true
			}
			continue
		}
		if _, _, err := eng.query(plain.SQL, leakArgs(plain.ParamsSeq)); err != nil {
			continue // the generator wrote something this engine rejects
		}
		usable = true
		cols, rows, err := eng.query(woven.SQL, leakArgs(woven.ParamsSeq))
		if err != nil {
			t.Fatalf("unwoven query runs but the woven one fails: %v\ntemplate:\n%s\nwoven:\n%s", err, lc.src, woven.SQL)
		}
		for _, row := range rows {
			for i, c := range cols {
				if strings.HasPrefix(c, "s") && row[i] == "T2" {
					t.Fatalf("TENANT LEAK: tid=1 read a tenant-2 row (column %s)\ntemplate:\n%s\nwoven shape %s:\n%s",
						c, lc.src, k, woven.SQL)
				}
			}
		}
	}
	return usable
}

// checkDMLShape runs the unwoven and the woven statement each in a
// rolled-back transaction and requires the woven one to leave tenant
// 2's rows untouched.
func checkDMLShape(t *testing.T, eng leakEngine, tenant2Rows, src string, plain, woven ast.Rendering) bool {
	t.Helper()
	_, before, err := eng.query(tenant2Rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func(r ast.Rendering) error {
		if err := eng.exec("BEGIN", nil); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := eng.exec("ROLLBACK", nil); err != nil {
				t.Fatal(err)
			}
		}()
		if err := eng.exec(r.SQL, leakArgs(r.ParamsSeq)); err != nil {
			return err
		}
		_, after, err := eng.query(tenant2Rows, nil)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(after) != fmt.Sprint(before) {
			return errLeak
		}
		return nil
	}
	if err := run(plain); err != nil && err != errLeak {
		return false
	}
	switch err := run(woven); {
	case err == errLeak:
		t.Fatalf("TENANT LEAK: tid=1 modified tenant-2 rows\ntemplate:\n%s\nwoven:\n%s", src, woven.SQL)
	case err != nil:
		t.Fatalf("unwoven statement runs but the woven one fails: %v\ntemplate:\n%s\nwoven:\n%s", err, src, woven.SQL)
	}
	return true
}

var errLeak = fmt.Errorf("tenant-2 rows changed")

// ---- SQLite: in process, so this target runs in the plain suite and CI.

var sqliteLeakDialect = leakDialect{
	name:       "sqlite",
	paramType:  "integer",
	quirkCols:  []string{"offset", "window", "fetch"},
	updateFrom: true,
}

type sqliteLeakEngine struct{ conn *sqlite3.Conn }

func (e sqliteLeakEngine) query(sql string, args []any) ([]string, [][]string, error) {
	stmt, tail, err := e.conn.Prepare(sql)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = stmt.Close() }()
	if strings.TrimSpace(tail) != "" {
		return nil, nil, fmt.Errorf("trailing SQL %q", tail)
	}
	for i, a := range args {
		if err := stmt.BindInt64(i+1, a.(int64)); err != nil {
			return nil, nil, err
		}
	}
	cols := make([]string, stmt.ColumnCount())
	for i := range cols {
		cols[i] = stmt.ColumnName(i)
	}
	var rows [][]string
	for stmt.Step() {
		row := make([]string, len(cols))
		for i := range cols {
			if stmt.ColumnType(i) == sqlite3.NULL {
				row[i] = nullCell
			} else {
				row[i] = stmt.ColumnText(i)
			}
		}
		rows = append(rows, row)
	}
	return cols, rows, stmt.Err()
}

func (e sqliteLeakEngine) exec(sql string, args []any) error {
	_, _, err := e.query(sql, args)
	return err
}

func newSQLiteLeakEngine(tb testing.TB) leakEngine {
	tb.Helper()
	q := func(s string) string { return `"` + s + `"` }
	d := sqliteLeakDialect
	conn, cleanup, err := devdb.AcquireSQLite(context.Background(), devdb.Config{
		SchemaSQL: []string{strings.Join(leakSchema(d, q), ";\n") + ";"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(cleanup)
	for _, s := range leakData(d, q) {
		if err := conn.Exec(s); err != nil {
			tb.Fatal(err)
		}
	}
	return sqliteLeakEngine{conn}
}

// leakSeeds are the audit regressions in generator form plus one seed
// per statement family; the fuzzer mutates from here.
func leakSeeds() [][]byte {
	return [][]byte{
		{0},
		{1, 0, 3, 0, 1, 2},          // OR predicate variant, alias o, WHERE with OR
		{0, 0, 3, 2, 3, 0, 0, 0, 0}, // LEFT JOIN nullable side, ON OR
		{0, 0, 2, 2},                // inner join, ON OR
		{0, 0, 5, 1, 2},             // CROSS JOIN
		{0, 0, 8, 1, 2},             // self join, two occurrences
		{0, 0, 9, 1},                // join chain through the nullable side
		{0, 7, 0, 1},                // UPDATE
		{0, 8, 0, 1},                // DELETE
		{0, 9, 1, 0, 0},             // set operation
		{0, 0, 0, 1, 2, 0, 0, 3},    // bare quirk column before OR
		{0, 0, 12, 1},               // nested join: null-extended from outside
	}
}

// FuzzPolicyWeaveNoLeak_SQLite: see the file comment.
func FuzzPolicyWeaveNoLeak_SQLite(f *testing.F) {
	for _, s := range leakSeeds() {
		f.Add(s)
	}
	eng := newSQLiteLeakEngine(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		checkNoLeak(t, sqliteLeakDialect, eng, raw)
	})
}

// TestPolicyWeaveNoLeak_GeneratorIsLive guards the harness against
// going vacuous: a generator whose output the pipeline mostly refuses,
// or whose unwoven queries never READ tenant 2, would pass forever.
func TestPolicyWeaveNoLeak_GeneratorIsLive(t *testing.T) {
	eng := newSQLiteLeakEngine(t)
	d := sqliteLeakDialect
	usable, unscopedT2 := 0, 0
	const n = 600
	for i := range n {
		raw := []byte{byte(i), byte(i * 7), byte(i * 13), byte(i * 31), byte(i * 61), byte(i * 3), byte(i * 11), byte(i * 17)}
		if !checkNoLeak(t, d, eng, raw) {
			continue
		}
		usable++
		g := &leakGen{b: raw[1:], d: d}
		lc := g.generate()
		if lc.dml {
			continue
		}
		drv := driverFor(config.Config{Dialect: d.name})
		fl, _ := template.NewScanner(drv.profile).ScanFile("q.sql", []byte(lc.src))
		r, err := ast.Render(drv.profile, fl.Queries[0], nil)
		if err != nil {
			continue
		}
		cols, rows, err := eng.query(r.SQL, leakArgs(r.ParamsSeq))
		if err != nil {
			continue
		}
	scan:
		for _, row := range rows {
			for j, c := range cols {
				if strings.HasPrefix(c, "s") && row[j] == "T2" {
					unscopedT2++
					break scan
				}
			}
		}
	}
	if usable < n/2 {
		t.Errorf("only %d/%d generated cases were usable; the generator drifted from what the pipeline accepts", usable, n)
	}
	if unscopedT2 < n/4 {
		t.Errorf("only %d/%d unwoven queries read tenant 2 at all; the leak check is close to vacuous", unscopedT2, n)
	}
	t.Logf("usable %d/%d, unwoven reads tenant 2 in %d", usable, n, unscopedT2)
}
