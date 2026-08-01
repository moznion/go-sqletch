package template

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/dialect/postgres"
)

func format(t *testing.T, src string) string {
	t.Helper()
	out, diags := Format(postgres.Profile{}, "t.sql", []byte(src))
	for _, d := range diags {
		t.Fatalf("format diagnostics: %+v", d)
	}
	return string(out)
}

func TestFormat_CanonicalizesConstructLayout(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT 1 FROM t\nWHERE TRUE\n" +
		"@if-present( a ,b )\n  AND t.x BETWEEN :a AND :b\n@endif\n;\n"
	got := format(t, src)
	if !strings.Contains(got, "@if-present(a, b)\n  AND t.x BETWEEN :a AND :b\n@endif") {
		t.Errorf("canonical construct layout missing:\n%s", got)
	}
}

func TestFormat_InsertsWhereTrueAnchor(t *testing.T) {
	src := `-- name: Q :many
SELECT 1 FROM t
WHERE
@if-present(x)
  AND t.x = :x
@endif
;
`
	got := format(t, src)
	if !strings.Contains(got, "WHERE TRUE\n@if-present(x)") {
		t.Errorf("anchor not inserted:\n%s", got)
	}
	// The fixed output passes the scan cleanly and keeps the fragment.
	f, diags := NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(got))
	if len(diags) != 0 {
		t.Fatalf("formatted output has diagnostics: %+v", diags)
	}
	if len(f.Queries[0].GuardAtoms) != 1 {
		t.Fatalf("structure lost: %+v", f.Queries[0].GuardAtoms)
	}
}

func TestFormat_WhenCanonical(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT 1 FROM t\nWHERE TRUE\n" +
		"@when( include_deleted   =false )\n  AND t.deleted_at IS NULL\n@end\n;\n"
	got := format(t, src)
	if !strings.Contains(got, "@when(include_deleted = false)\n  AND t.deleted_at IS NULL\n@end") {
		t.Errorf("canonical @when layout missing:\n%s", got)
	}
}

func TestFormat_HavingAnchorInsertion(t *testing.T) {
	src := `-- name: Q :many
SELECT t.a, count(*) AS n FROM t
GROUP BY t.a
HAVING
@if-present(min_n)
  AND count(*) >= :min_n
@endif
;
`
	got := format(t, src)
	if !strings.Contains(got, "HAVING TRUE\n@if-present(min_n)") {
		t.Errorf("HAVING anchor not inserted:\n%s", got)
	}
}

func TestFormat_Fixpoint(t *testing.T) {
	sources := []string{
		useCase1,
		useCase2,
		whenTemplate,
		havingTemplate,
		"-- name: Q :many\nSELECT 1 FROM t\nWHERE\n@if-present(x)\n  AND t.x = :x\n@endif\n;\n",
		`-- name: G :many
SELECT count(*) AS n FROM t
WHERE TRUE
@choose(g)
@case(a)
GROUP BY t.a
@default
@end
;
`,
		`-- name: O :many
SELECT t.id FROM t
WHERE TRUE
@order-by(sort)
@key(id)
t.id
@key(name)
t.name
@default
ORDER BY t.id
@end
;
`,
	}
	for i, src := range sources {
		once := format(t, src)
		twice := format(t, once)
		if once != twice {
			t.Errorf("source %d not a fixpoint:\n--- once ---\n%s\n--- twice ---\n%s", i, once, twice)
		}
	}
}

func TestFormat_PreservesSkeletonBytes(t *testing.T) {
	src := "-- name: Q :many\nSELECT   1,\n\t2 -- comment stays\nFROM t;\n"
	if got := format(t, src); got != src {
		t.Errorf("construct-free file must be unchanged:\n%q\nvs\n%q", got, src)
	}
}

func TestFormat_BrokenFileUnchanged(t *testing.T) {
	src := "-- name: Q :many\nSELECT 1\n@if-present(x\n;\n"
	out, diags := Format(postgres.Profile{}, "t.sql", []byte(src))
	if len(diags) == 0 {
		t.Fatal("expected diagnostics")
	}
	if string(out) != src {
		t.Error("broken files must be returned unchanged")
	}
}
