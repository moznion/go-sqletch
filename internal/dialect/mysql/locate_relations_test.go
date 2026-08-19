package mysql

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// locateRelations must pin a relation to its FROM-position token, not to
// a same-named column reference, a backquoted identifier whose content
// looks like a keyword, or a name inside a (VALUES …) constructor.
func TestLocateRelations_Precision(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		tab  string
		// want is the byte offset locateRelations should assign; it is
		// computed per-case below (LastIndex picks the real FROM entry,
		// which in each adversarial case is the LAST occurrence).
		want func(sql string) int
	}{
		{
			// '.' predecessor: the SELECT-list `u.orders` must not be
			// mistaken for the FROM relation `orders`.
			name: "dot-qualified column ref",
			sql:  "SELECT u.orders FROM orders u",
			tab:  "orders",
			want: func(s string) int { return strings.LastIndex(s, "orders") },
		},
		{
			// A backquoted identifier named FROM must not act as the
			// FROM keyword for the following token.
			name: "backquoted keyword identifier",
			sql:  "SELECT `FROM` users FROM users",
			tab:  "users",
			want: func(s string) int { return strings.LastIndex(s, "users") },
		},
		{
			// A relation-looking name inside a (VALUES …) constructor is
			// opaque; the real `orders` is the one after the comma.
			name: "values constructor is opaque",
			sql:  "SELECT * FROM (VALUES ROW(orders)) o, orders t",
			tab:  "orders",
			want: func(s string) int { return strings.LastIndex(s, "orders") },
		},
		{
			// Regression: a genuinely schema-qualified name (`db.orders`)
			// whose qualifier IS in FROM position still locates.
			name: "schema-qualified relation still located",
			sql:  "SELECT id FROM db.orders",
			tab:  "orders",
			want: func(s string) int { return strings.Index(s, "orders") },
		},
		{
			// Regression: an ordinary FROM relation still locates.
			name: "plain relation",
			sql:  "SELECT id FROM orders",
			tab:  "orders",
			want: func(s string) int { return strings.Index(s, "orders") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rels := []dialect.RelRef{{Table: c.tab}}
			locateRelations(c.sql, rels)
			if got, want := rels[0].Loc, c.want(c.sql); got != want {
				t.Errorf("Loc = %d (%q), want %d (%q)",
					got, sliceAt(c.sql, got), want, sliceAt(c.sql, want))
			}
		})
	}
}

func sliceAt(s string, off int) string {
	if off < 0 || off >= len(s) {
		return "<none>"
	}
	end := off + 10
	if end > len(s) {
		end = len(s)
	}
	return s[off:end]
}
