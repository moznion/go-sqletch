package template

import (
	"strings"
	"testing"
)

// The scanner records the policy weaver's insertion points (design 14
// §4.2): WhereKwEnd just past the top-level WHERE keyword, TailStart
// at the first post-WHERE clause when the statement has no WHERE, and
// StmtEnd at the end of the last statement token.

func TestScan_WeavePoints(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// markers resolved against src: whereAfter is the substring
		// whose first occurrence's end is WhereKwEnd ("" = -1);
		// tailAt is the substring whose start is TailStart ("" = -1);
		// stmtEndAfter is the substring whose end is StmtEnd.
		whereAfter   string
		tailAt       string
		stmtEndAfter string
	}{
		{
			name:         "select with where",
			src:          "-- name: Q :many\nSELECT id FROM orders WHERE tenant_id = :t AND ok\n",
			whereAfter:   "WHERE",
			stmtEndAfter: "ok",
		},
		{
			name:         "select without where, with order by",
			src:          "-- name: Q :many\nSELECT id FROM orders ORDER BY id LIMIT 5\n",
			tailAt:       "ORDER",
			stmtEndAfter: "5",
		},
		{
			name:         "select without where, group by",
			src:          "-- name: Q :many\nSELECT count(*) FROM orders GROUP BY status\n",
			tailAt:       "GROUP",
			stmtEndAfter: "status",
		},
		{
			name:         "bare delete",
			src:          "-- name: Q :exec\nDELETE FROM orders\n",
			stmtEndAfter: "orders",
		},
		{
			name:         "delete with semicolon",
			src:          "-- name: Q :exec\nDELETE FROM orders;\n",
			stmtEndAfter: "orders",
		},
		{
			name:         "update without where",
			src:          "-- name: Q :exec\nUPDATE orders SET status = :s RETURNING id\n",
			tailAt:       "RETURNING",
			stmtEndAfter: "id",
		},
		{
			name:         "update with where",
			src:          "-- name: Q :exec\nUPDATE orders SET status = :s WHERE id = :id\n",
			whereAfter:   "WHERE",
			stmtEndAfter: ":id",
		},
		{
			name:         "subquery where does not count",
			src:          "-- name: Q :many\nSELECT id FROM orders o JOIN (SELECT x FROM t WHERE y) s ON o.id = s.x ORDER BY o.id\n",
			tailAt:       "ORDER",
			stmtEndAfter: "o.id\n", // trailing "o.id" of ORDER BY
		},
		{
			name:         "order-by construct bounds the where slot",
			src:          "-- name: Q :many\nSELECT id FROM orders\n@order-by(sort)\n@key(id) id\n@end\n",
			tailAt:       "@order-by",
			stmtEndAfter: "@end",
		},
		{
			name:         "where before order-by construct",
			src:          "-- name: Q :many\nSELECT id FROM orders WHERE ok\n@order-by(sort)\n@key(id) id\n@end\n",
			whereAfter:   "WHERE",
			stmtEndAfter: "@end",
		},
		{
			name:         "for update marks tail",
			src:          "-- name: Q :many\nSELECT id FROM orders FOR UPDATE\n",
			tailAt:       "FOR",
			stmtEndAfter: "UPDATE\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, diags := scan(t, tc.src)
			if len(diags) != 0 {
				t.Fatalf("unexpected diagnostics:\n%s", renderAll(diags, tc.src))
			}
			if len(f.Queries) != 1 {
				t.Fatalf("got %d queries", len(f.Queries))
			}
			q := f.Queries[0]

			wantWhere := -1
			if tc.whereAfter != "" {
				i := strings.Index(tc.src, tc.whereAfter)
				if i < 0 {
					t.Fatalf("marker %q not in src", tc.whereAfter)
				}
				wantWhere = i + len(tc.whereAfter)
			}
			if q.WhereKwEnd != wantWhere {
				t.Errorf("WhereKwEnd = %d, want %d", q.WhereKwEnd, wantWhere)
			}

			wantTail := -1
			if tc.tailAt != "" {
				wantTail = strings.Index(tc.src, tc.tailAt)
				if wantTail < 0 {
					t.Fatalf("marker %q not in src", tc.tailAt)
				}
			}
			if q.TailStart != wantTail {
				t.Errorf("TailStart = %d, want %d", q.TailStart, wantTail)
			}

			i := strings.LastIndex(tc.src, tc.stmtEndAfter)
			if i < 0 {
				t.Fatalf("marker %q not in src", tc.stmtEndAfter)
			}
			wantEnd := i + len(strings.TrimRight(tc.stmtEndAfter, "\n"))
			if q.StmtEnd != wantEnd {
				t.Errorf("StmtEnd = %d, want %d", q.StmtEnd, wantEnd)
			}
		})
	}
}
