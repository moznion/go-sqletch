package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// branchWant describes one expected branch of scanSetOp by the source
// text found at each recorded offset ("" = offset absent).
type branchWant struct {
	lead      string
	afterKw   string // prefix of src[whereKwEnd:]
	atTail    string // prefix of src[tailStart:]
	afterEnd  string // prefix of src[end:]
	whereSegs int
	whereOR   bool
}

// scanSetOp is the lexical half of the set-operation cross-check
// (design 14 §13.2): it must find exactly the leaf cores, in document
// order, with each core's own WHERE clause and insertion points —
// never a subquery's or a CTE body's.
func TestScanSetOp(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []branchWant
	}{
		{
			name: "flat UNION ALL, OR-WHERE and a GROUP BY tail",
			body: "SELECT o.id FROM orders o WHERE o.a = 1 OR o.b = 2 UNION ALL SELECT a.id FROM archive a GROUP BY a.id",
			want: []branchWant{
				{lead: "SELECT", afterKw: " o.a = 1", afterEnd: " UNION ALL", whereSegs: 2, whereOR: true},
				{lead: "SELECT", atTail: "GROUP BY", afterEnd: "\n"},
			},
		},
		{
			name: "parenthesized operands with their own LIMIT and a set-level ORDER BY",
			body: "(SELECT id FROM orders o LIMIT 5) UNION (SELECT id FROM users u WHERE u.ok) ORDER BY 1",
			want: []branchWant{
				{lead: "SELECT", atTail: "LIMIT 5", afterEnd: ") UNION"},
				{lead: "SELECT", afterKw: " u.ok", afterEnd: ") ORDER", whereSegs: 1},
			},
		},
		{
			name: "nested operands flatten in document order",
			body: "SELECT 1 FROM t1 UNION (SELECT 1 FROM t2 UNION SELECT 1 FROM t3 WHERE x) EXCEPT SELECT 1 FROM t4",
			want: []branchWant{
				{lead: "SELECT", afterEnd: " UNION ("},
				{lead: "SELECT", afterEnd: " UNION SELECT 1 FROM t3"},
				{lead: "SELECT", afterKw: " x)", afterEnd: ") EXCEPT", whereSegs: 1},
				{lead: "SELECT", afterEnd: "\n"},
			},
		},
		{
			name: "a statement-level WITH is skipped; its body's set operation is not a branch",
			body: "WITH x AS (SELECT id FROM orders WHERE p UNION SELECT 2) SELECT id FROM x UNION SELECT id FROM users WHERE ok",
			want: []branchWant{
				{lead: "SELECT", afterEnd: " UNION SELECT id FROM users"},
				{lead: "SELECT", afterKw: " ok", afterEnd: "\n", whereSegs: 1},
			},
		},
		{
			name: "WITH before a parenthesized first operand",
			body: "WITH x (n) AS MATERIALIZED (SELECT 1) (SELECT n FROM x WHERE n > 0) UNION SELECT 2",
			want: []branchWant{
				{lead: "SELECT", afterKw: " n > 0", afterEnd: ") UNION", whereSegs: 1},
				{lead: "SELECT", afterEnd: "\n"},
			},
		},
		{
			name: "a set operation inside a subquery is not a split",
			body: "SELECT id FROM a WHERE id IN (SELECT 1 UNION SELECT 2) UNION SELECT id FROM b",
			want: []branchWant{
				{lead: "SELECT", afterKw: " id IN", afterEnd: " UNION SELECT id FROM b", whereSegs: 1},
				{lead: "SELECT", afterEnd: "\n"},
			},
		},
		{
			name: "a dotted keyword-column is not a set operator",
			body: "SELECT t.union FROM t WHERE t.except = 1 UNION SELECT 1",
			want: []branchWant{
				{lead: "SELECT", afterKw: " t.except", afterEnd: " UNION SELECT 1", whereSegs: 1},
				{lead: "SELECT", afterEnd: "\n"},
			},
		},
		{
			name: "a VALUES operand is a core with its own lead keyword",
			body: "SELECT id FROM orders UNION VALUES (1)",
			want: []branchWant{
				{lead: "SELECT", afterEnd: " UNION VALUES"},
				{lead: "VALUES", afterEnd: "\n"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "-- name: Q :many\n" + tc.body + "\n"
			q := scanOne(t, src)
			so := scanSetOp(postgres.Profile{}, q)
			if !so.ok {
				t.Fatalf("scanSetOp not ok")
			}
			if len(so.branches) != len(tc.want) {
				t.Fatalf("%d branches, want %d", len(so.branches), len(tc.want))
			}
			at := func(off int, want string) string {
				if off < 0 {
					if want == "" {
						return ""
					}
					return "<absent>"
				}
				if want == "" {
					return "<present: " + src[off:] + ">"
				}
				if strings.HasPrefix(src[off:], want) {
					return want
				}
				return src[off:]
			}
			for i, b := range so.branches {
				w := tc.want[i]
				if b.leadKw != w.lead {
					t.Errorf("branch %d lead = %q, want %q", i, b.leadKw, w.lead)
				}
				if got := at(b.whereKwEnd, w.afterKw); got != w.afterKw {
					t.Errorf("branch %d after WHERE = %q, want prefix %q", i, got, w.afterKw)
				}
				if got := at(b.tailStart, w.atTail); got != w.atTail {
					t.Errorf("branch %d tail = %q, want prefix %q", i, got, w.atTail)
				}
				if got := at(b.end, w.afterEnd); got != w.afterEnd {
					t.Errorf("branch %d end = %q, want prefix %q", i, got, w.afterEnd)
				}
				if len(b.where.segs) != w.whereSegs || b.where.hasOR != w.whereOR {
					t.Errorf("branch %d where segs=%d or=%v, want %d/%v", i, len(b.where.segs), b.where.hasOR, w.whereSegs, w.whereOR)
				}
			}
		})
	}
}

// Relation name tokens are attributed to the core that contains them;
// tokens of a subquery belong to the enclosing core, tokens of a
// set-level clause to none.
func TestScanSetOp_BranchOf(t *testing.T) {
	body := "(SELECT id FROM orders o WHERE id IN (SELECT id FROM refunds)) UNION SELECT id FROM users ORDER BY (SELECT 1 FROM tail_t)"
	src := "-- name: Q :many\n" + body + "\n"
	q := scanOne(t, src)
	so := scanSetOp(postgres.Profile{}, q)
	if !so.ok {
		t.Fatal("not ok")
	}
	for name, want := range map[string]int{"orders": 0, "refunds": 0, "users": 1, "tail_t": 1} {
		off := strings.Index(src, name)
		got, ok := so.branchOf[off]
		if !ok || got != want {
			t.Errorf("branchOf(%s) = %d,%v; want %d", name, got, ok, want)
		}
	}
	// An unparenthesized last core lexically swallows the set-level
	// ORDER BY — attributing tail_t to branch 1 is harmless because the
	// AST reports it as no branch's relation, so it is never looked up.
}

// A WITH clause that does not have the modeled shape fails closed.
func TestScanSetOp_MalformedWithFailsClosed(t *testing.T) {
	q := scanOne(t, "-- name: Q :many\nWITH x SELECT 1 UNION SELECT 2\n")
	if so := scanSetOp(postgres.Profile{}, q); so.ok {
		t.Fatalf("expected ok=false, got %d branches", len(so.branches))
	}
}
