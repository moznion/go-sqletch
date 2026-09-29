package mysql

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/dialecttest"
)

// A statement-level set operation is StmtOther on MySQL, so R1
// (SQLETCH103) rejects it before any later phase runs — the nullability
// soundness suite relies on that ("the vector cannot occur"), and so
// does policy weaving: SetOpBranches is implemented and tested below,
// but MySQL set operations never reach the weaver (design 14 §13.6).
// Admitting them is a separate decision (nullability attribution,
// native oracle) — change this test only together with that.
func TestFrontend_SetOperationIsNotADMLKind(t *testing.T) {
	tree, err := Frontend{}.Parse("SELECT id FROM orders UNION ALL SELECT id FROM archive")
	if err != nil {
		t.Fatal(err)
	}
	if k := tree.Kind(); k != dialect.StmtOther {
		t.Fatalf("Kind = %v, want StmtOther (set operations are rejected on MySQL)", k)
	}
}

// SetOpBranches flattens a statement-level set operation into its leaf
// query cores in document order, each with its own relations LOCATED in
// the original SQL (design 14 §13). TiDB relation nodes carry no
// offsets, so the location comes from the lexical relation locator,
// which must see through parenthesized operands.
func TestFrontend_SetOpBranches(t *testing.T) {
	cases := []dialecttest.SetOpCase{
		{SQL: "SELECT id FROM orders", Branches: nil},
		{SQL: "(SELECT id FROM orders)", Branches: nil},
		{SQL: "SELECT id FROM orders WHERE id IN (SELECT id FROM a UNION SELECT id FROM b)", Branches: nil},
		{
			SQL:      "SELECT o.id FROM orders o UNION ALL SELECT a.id FROM archive a",
			Branches: [][]string{{"orders o"}, {"archive a"}},
			Deep:     [][]string{{"orders"}, {"archive"}},
		},
		{
			SQL:      "SELECT id FROM orders UNION SELECT id FROM orders INTERSECT SELECT id FROM users EXCEPT SELECT id FROM archive",
			Branches: [][]string{{"orders"}, {"orders"}, {"users"}, {"archive"}},
			Deep:     [][]string{{"orders"}, {"orders"}, {"users"}, {"archive"}},
		},
		{
			SQL:      "(SELECT id FROM orders o LIMIT 5) UNION ALL (SELECT id FROM users u JOIN orders o2 ON o2.user_id = u.id) ORDER BY 1",
			Branches: [][]string{{"orders o"}, {"users u", "orders o2"}},
			Deep:     [][]string{{"orders"}, {"orders", "users"}},
		},
		{
			SQL:      "SELECT id FROM t1 UNION (SELECT id FROM t2 UNION SELECT id FROM t3) EXCEPT SELECT id FROM t4",
			Branches: [][]string{{"t1"}, {"t2"}, {"t3"}, {"t4"}},
			Deep:     [][]string{{"t1"}, {"t2"}, {"t3"}, {"t4"}},
		},
		{
			SQL:      "SELECT id FROM t1 UNION ((SELECT id FROM t2) UNION DISTINCT (SELECT id FROM t3))",
			Branches: [][]string{{"t1"}, {"t2"}, {"t3"}},
			Deep:     [][]string{{"t1"}, {"t2"}, {"t3"}},
		},
		{
			SQL:      "WITH x AS (SELECT id FROM orders) SELECT id FROM users UNION SELECT id FROM x",
			Branches: [][]string{{"users"}, {"x"}},
			Deep:     [][]string{{"users"}, {"x"}},
		},
		{
			// A statement-level WITH followed by a parenthesized first
			// operand: the CTE body's closing paren restores operand
			// position.
			SQL:      "WITH x AS (SELECT 1) (SELECT id FROM orders) UNION (SELECT id FROM users)",
			Branches: [][]string{{"orders"}, {"users"}},
			Deep:     [][]string{{"orders"}, {"users"}},
		},
		{
			SQL:      "(WITH y AS (SELECT id FROM orders) SELECT id FROM y) UNION SELECT id FROM users",
			Branches: [][]string{{"y"}, {"users"}},
			Deep:     [][]string{{"orders", "y"}, {"users"}},
		},
		{
			// A subquery inside a branch — including a quantified
			// `ALL (SELECT …)` that must not be mistaken for a
			// parenthesized UNION ALL operand — is that branch's deep read.
			SQL:      "SELECT id FROM users WHERE id > ALL (SELECT user_id FROM orders) UNION ALL SELECT id FROM archive",
			Branches: [][]string{{"users"}, {"archive"}},
			Deep:     [][]string{{"orders", "users"}, {"archive"}},
		},
		{
			SQL:      "SELECT id FROM orders UNION VALUES ROW(1)",
			Branches: [][]string{{"orders"}, {}},
			Deep:     [][]string{{"orders"}, nil},
		},
	}
	dialecttest.CheckSetOpBranches(t, Frontend{}, cases)
}
