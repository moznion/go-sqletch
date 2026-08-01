package sqlite

import "testing"

func TestParse_TrueAnchorsAndReturning(t *testing.T) {
	// R6 anchors render as WHERE TRUE / HAVING TRUE.
	for _, q := range []string{
		"SELECT u.id FROM users AS u WHERE TRUE AND (u.status = ?) ORDER BY u.id LIMIT ?",
		"SELECT a, count(*) FROM t GROUP BY a HAVING TRUE AND (count(*) >= ?)",
		"UPDATE users SET tenant_id = tenant_id, email = ? WHERE id = ? RETURNING id, email",
		"INSERT INTO users (email, status) VALUES (?, ?) RETURNING id",
	} {
		if _, err := (Frontend{}).Parse(q); err != nil {
			t.Errorf("parse %q: %v", q, err)
		}
	}
}
