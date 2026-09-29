package sqlite

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect/dialecttest"
)

// SetOpBranches splits a compound SELECT into its cores (design 14
// §13). rqlite chains compounds through SelectStatement.Compound and
// hangs the compound's own WITH/ORDER BY/LIMIT on the FIRST core, so
// the branch facades must strip those — and, above all, must not
// report every branch's relations as the first core's (Relations() on
// the whole statement does: that is what let a compound weave land in
// the wrong branch).
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
			// The compound's ORDER BY/LIMIT (stored on the first core)
			// belong to no branch: a subquery there is the statement's
			// deep read only.
			SQL:      "SELECT id FROM orders UNION SELECT id FROM users ORDER BY (SELECT max(id) FROM archive) LIMIT 3",
			Branches: [][]string{{"orders"}, {"users"}},
			Deep:     [][]string{{"orders"}, {"users"}},
		},
		{
			SQL:      "WITH x AS (SELECT id FROM orders) SELECT id FROM users UNION SELECT id FROM x",
			Branches: [][]string{{"users"}, {"x"}},
			Deep:     [][]string{{"users"}, {"x"}},
		},
		{
			SQL:      "SELECT id FROM users WHERE id IN (SELECT user_id FROM orders) UNION SELECT id FROM archive",
			Branches: [][]string{{"users"}, {"archive"}},
			Deep:     [][]string{{"orders", "users"}, {"archive"}},
		},
		{
			SQL:      "SELECT id FROM orders UNION VALUES (1)",
			Branches: [][]string{{"orders"}, {}},
			Deep:     [][]string{{"orders"}, nil},
		},
	}
	dialecttest.CheckSetOpBranches(t, Frontend{}, cases)
}
