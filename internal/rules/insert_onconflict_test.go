package rules

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// A legitimate upsert whose guarded VALUES item pairs with a guarded
// column must not be false-rejected under R7. The scanner used to read
// the ON CONFLICT `(a)` conflict-target parens as a second, empty VALUES
// row, which then tripped SQLETCH119 ("VALUES row 2 has 0 optional
// items but the column list has 1").
func TestInsertPairing_UpsertNotFalseRejected(t *testing.T) {
	src := `-- name: Up :exec
INSERT INTO t (
    a
@if-present(b)
  , b
@endif
) VALUES (
    :a
@if-present(b)
  , :b
@endif
) ON CONFLICT (a) DO UPDATE SET b = excluded.b WHERE t.tenant = 1;
`
	q := scanOne(t, src)
	if diags := CheckLexical(postgres.Profile{}, q); hasCode(diags, diagnostics.CodePairedGuards) {
		t.Fatalf("upsert false-rejected with SQLETCH119: %+v", diags)
	}
}
