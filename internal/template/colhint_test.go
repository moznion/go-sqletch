package template

import "testing"

func TestScan_ColumnHint(t *testing.T) {
	src := `-- name: CountByStatus :many
-- @param min: integer
-- @column n: integer
SELECT status, count(*) AS n FROM t WHERE t.v >= :min GROUP BY status;
`
	f := scanClean(t, src)
	q := f.Queries[0]
	hint, ok := q.ColumnHints["n"]
	if !ok || hint.SQLType != "integer" {
		t.Fatalf("column hints = %+v", q.ColumnHints)
	}
	if _, ok := q.TypeHints["min"]; !ok {
		t.Fatalf("param hints = %+v", q.TypeHints)
	}
	if _, ok := q.TypeHints["n"]; ok {
		t.Error("@column must not register as a param hint")
	}
}
