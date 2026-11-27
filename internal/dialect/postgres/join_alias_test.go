package postgres

import (
	"testing"
)

// JoinAliasHidden marks exactly the relations inside a parenthesized
// join group that carries its own alias (`(a JOIN b) AS j`): outside the
// group their names are not referenceable (design 14 §11.11). A USING
// alias (`JOIN … USING (k) AS u`) and an unaliased group hide nothing.
func TestFrontend_JoinAliasHidden(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want map[string]bool // relation alias-else-table -> hidden
	}{
		{name: "plain join", sql: "SELECT 1 FROM orders o JOIN users u ON u.id = o.uid", want: map[string]bool{"o": false, "u": false}},
		{name: "unaliased group", sql: "SELECT 1 FROM acc a JOIN (orders o JOIN users u ON u.id = o.uid) ON a.id = o.aid", want: map[string]bool{"a": false, "o": false, "u": false}},
		{name: "aliased group", sql: "SELECT 1 FROM acc a JOIN (orders o JOIN users u ON u.id = o.uid) AS j ON a.id = j.aid", want: map[string]bool{"a": false, "o": true, "u": true}},
		{name: "aliased group without AS, unaliased member", sql: "SELECT 1 FROM (orders JOIN users u ON u.id = orders.uid) j", want: map[string]bool{"orders": true, "u": true}},
		{name: "group nested in an aliased group", sql: "SELECT 1 FROM ((orders o JOIN users u ON true) JOIN acc a ON true) AS j", want: map[string]bool{"o": true, "u": true, "a": true}},
		{name: "aliased group nested in an unaliased one", sql: "SELECT 1 FROM ((orders o JOIN users u ON true) AS j JOIN acc a ON true)", want: map[string]bool{"o": true, "u": true, "a": false}},
		{name: "USING alias hides nothing", sql: "SELECT 1 FROM orders o JOIN users u USING (id) AS x", want: map[string]bool{"o": false, "u": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := Frontend{}.Parse(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, r := range tree.Relations() {
				n := r.Alias
				if n == "" {
					n = r.Table
				}
				got[n] = r.JoinAliasHidden
			}
			if len(got) != len(tc.want) {
				t.Fatalf("relations = %v, want %v", got, tc.want)
			}
			for n, h := range tc.want {
				if g, ok := got[n]; !ok || g != h {
					t.Errorf("%s: JoinAliasHidden = %v (present %v), want %v", n, g, ok, h)
				}
			}
		})
	}
}
