package mysql

import "testing"

// MySQL admits only SELECT bodies in WITH (TiDB's CommonTableExpression
// holds a SubqueryExpr), so NestedDML is always empty and a DML CTE body
// does not parse at all (security finding F9 confirmation).
func TestNestedDML_Empty(t *testing.T) {
	for _, sql := range []string{
		"SELECT id FROM posts",
		"WITH c AS (SELECT id FROM posts) SELECT id FROM c",
		"WITH c AS (SELECT id FROM posts) DELETE FROM posts WHERE id IN (SELECT id FROM c)",
		"INSERT INTO posts (id) VALUES (1) ON DUPLICATE KEY UPDATE id = 2",
	} {
		if got := parse(t, sql).NestedDML(); len(got) != 0 {
			t.Errorf("NestedDML(%q) = %+v, want none", sql, got)
		}
	}
	if _, err := (Frontend{}).Parse("WITH d AS (DELETE FROM posts WHERE id = 1) SELECT 1"); err == nil {
		t.Error("a DML CTE body parsed on MySQL; NestedDML must then be implemented")
	}
}
