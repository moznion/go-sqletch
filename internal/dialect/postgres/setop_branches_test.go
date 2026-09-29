package postgres

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect/dialecttest"
)

// SetOpBranches flattens a statement-level set operation into its leaf
// SELECT cores in document order, each with its own relations located
// in the original SQL (design 14 §13) — the per-branch weaving input.
func TestFrontend_SetOpBranches(t *testing.T) {
	cases := []dialecttest.SetOpCase{
		{SQL: "SELECT id FROM orders", Branches: nil},
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
			// Parenthesized operands keep their own LIMIT; the trailing
			// ORDER BY belongs to the whole set operation.
			SQL:      "(SELECT id FROM orders o LIMIT 5) UNION ALL (SELECT id FROM users u JOIN orders o2 ON o2.user_id = u.id) ORDER BY 1",
			Branches: [][]string{{"orders o"}, {"users u", "orders o2"}},
			Deep:     [][]string{{"orders"}, {"orders", "users"}},
		},
		{
			// Nested operands flatten in document order.
			SQL:      "SELECT id FROM t1 UNION (SELECT id FROM t2 UNION SELECT id FROM t3) EXCEPT SELECT id FROM t4",
			Branches: [][]string{{"t1"}, {"t2"}, {"t3"}, {"t4"}},
			Deep:     [][]string{{"t1"}, {"t2"}, {"t3"}, {"t4"}},
		},
		{
			// The statement-level WITH belongs to no branch: its body is
			// visible only through the statement's DeepTables.
			SQL:      "WITH x AS (SELECT id FROM orders) SELECT id FROM users UNION SELECT id FROM x",
			Branches: [][]string{{"users"}, {"x"}},
			Deep:     [][]string{{"users"}, {"x"}},
		},
		{
			// A parenthesized operand's own WITH belongs to its branch.
			SQL:      "(WITH y AS (SELECT id FROM orders) SELECT id FROM y) UNION SELECT id FROM users",
			Branches: [][]string{{"y"}, {"users"}},
			Deep:     [][]string{{"orders", "y"}, {"users"}},
		},
		{
			// A subquery inside a branch is that branch's deep read only.
			SQL:      "SELECT id FROM users WHERE id IN (SELECT user_id FROM orders) UNION SELECT id FROM archive",
			Branches: [][]string{{"users"}, {"archive"}},
			Deep:     [][]string{{"orders", "users"}, {"archive"}},
		},
		{
			// A VALUES operand has no relations.
			SQL:      "SELECT id FROM orders UNION VALUES (1)",
			Branches: [][]string{{"orders"}, {}},
			Deep:     [][]string{{"orders"}, nil},
		},
	}
	dialecttest.CheckSetOpBranches(t, Frontend{}, cases)
}
