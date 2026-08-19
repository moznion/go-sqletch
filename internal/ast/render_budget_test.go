package ast

import (
	"fmt"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"

	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

func mustRenderings(t *testing.T, prof dialect.LexerProfile, q *template.QueryTemplate) []Rendering {
	t.Helper()
	rs, err := Renderings(prof, q)
	if err != nil {
		t.Fatalf("Renderings: %v", err)
	}
	return rs
}

// RenderingCount must equal len(Renderings) for every template and
// dialect, or the pre-materialisation budget check in scanChecks would
// refuse (or admit) the wrong inputs. This pins that lock-step invariant
// across @choose, @order-by (@default), and @in (question-style only).
func TestRenderingCount_MatchesRenderings(t *testing.T) {
	// A @choose with two named cases + @default → maximal + 2.
	if got, rs := count(t, postgres.Profile{}, useCase1); got != len(rs) || got != 3 {
		t.Fatalf("useCase1: RenderingCount=%d len=%d, want 3", got, len(rs))
	}

	// An @order-by with a @default body → maximal + 1.
	const orderTmpl = `-- name: L :many
SELECT u.id FROM users AS u
WHERE TRUE
@order-by(sort)
@key(id)
u.id
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`
	if got, rs := count(t, postgres.Profile{}, orderTmpl); got != len(rs) || got != 2 {
		t.Fatalf("orderTmpl: RenderingCount=%d len=%d, want 2", got, len(rs))
	}

	// @in adds its arity-0 rendering on question-style dialects (mysql)
	// but is a single static shape on $n dialects (postgres).
	const inTmpl = `-- name: F :many
SELECT u.id FROM users AS u
WHERE u.status @in(:statuses);
`
	qIn := scanOne(t, inTmpl)
	if got, rs := RenderingCount(mysql.Profile{}, qIn), mustRenderings(t, mysql.Profile{}, qIn); got != len(rs) || got != 2 {
		t.Fatalf("inTmpl (mysql): RenderingCount=%d len=%d, want 2", got, len(rs))
	}
	if got, rs := RenderingCount(postgres.Profile{}, qIn), mustRenderings(t, postgres.Profile{}, qIn); got != len(rs) || got != 1 {
		t.Fatalf("inTmpl (postgres): RenderingCount=%d len=%d, want 1", got, len(rs))
	}
}

// RenderingCount scales with a @choose block's case count and computes
// that without allocating any rendering — the property the budget check
// relies on to refuse a pathological template before it exhausts memory.
func TestRenderingCount_ManyCases(t *testing.T) {
	const cases = 200
	var b strings.Builder
	b.WriteString("-- name: Big :many\nSELECT u.id FROM users AS u\nWHERE TRUE\n@choose(sort)\n")
	for j := 0; j < cases; j++ {
		fmt.Fprintf(&b, "@case(c%d)\nORDER BY u.id, %d\n", j, j)
	}
	b.WriteString("@default\nORDER BY u.id ASC\n@end\nLIMIT :limit;\n")
	q := scanOne(t, b.String())

	// maximal(1) + ((cases named + default) - 1) = 1 + cases.
	want := 1 + cases
	if got := RenderingCount(postgres.Profile{}, q); got != want {
		t.Fatalf("RenderingCount = %d, want %d", got, want)
	}
	if rs := mustRenderings(t, postgres.Profile{}, q); len(rs) != want {
		t.Fatalf("len(Renderings) = %d, want %d", len(rs), want)
	}
}

func count(t *testing.T, prof dialect.LexerProfile, src string) (int, []Rendering) {
	t.Helper()
	q := scanOne(t, src)
	return RenderingCount(prof, q), mustRenderings(t, prof, q)
}
