package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// This test pins the load-bearing SHIELD that keeps a latent scanner
// defect from becoming a tenant-scoping leak.
//
// The scanner computes WhereKwEnd for INSERT statements too, via the
// isInsert/joinPendingOn/onWasJoin/crossNaturalPending machinery. That
// value is currently DEAD for soundness: the policy weaver REFUSES every
// INSERT whose SELECT body reads a designated table (SQLETCH125), so a
// wrong INSERT WhereKwEnd never drives a spliced scoping conjunct into the
// wrong place. If INSERT...SELECT weaving is ever enabled (the
// coversSelect/hidden scaffolding already exists), the WhereKwEnd
// correctness for INSERTs — the very thing the crossNaturalPending /
// STRAIGHT_JOIN scanner fix addresses — becomes soundness-load-bearing.
//
// Keeping this refusal pinned means that flipping INSERT weaving on will
// FAIL this test, forcing the WhereKwEnd correctness to be revisited
// deliberately rather than silently leaking.
//
// The srcs stay within what the postgres frontend parses: the exact
// `cross`/`natural`/STRAIGHT_JOIN lexical shapes the scanner fix guards do
// not parse on PostgreSQL (a further, independent shield) and so are
// pinned at the scanner layer instead (scanner_onconflict_test.go). Here
// the INSERT...SELECT reads a designated table — with and without a
// JOIN...ON `conflict` column — and every shape must be refused.
func TestWeave_InsertSelectReadingDesignatedTableRefused(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			// The plainest form: INSERT...SELECT straight off a designated
			// table with no join at all.
			name: "plain insert-select of designated table",
			src: "-- name: Q :exec\n" +
				"INSERT INTO dst (v) SELECT id FROM orders WHERE tenant = 5;\n",
		},
		{
			// A JOIN whose ON carries a bare column named `conflict` — the
			// #90 shape — feeding the INSERT off the designated table.
			name: "join with conflict column, designated read",
			src: "-- name: Q :exec\n" +
				"INSERT INTO dst (v) SELECT o.id FROM orders o JOIN b ON conflict = b.id WHERE o.tenant = 5;\n",
		},
		{
			// A real ON CONFLICT clause on the same INSERT: still a
			// designated read in the feed, still refused.
			name: "insert-select with real on conflict",
			src: "-- name: Q :exec\n" +
				"INSERT INTO orders (id) SELECT id FROM orders ON CONFLICT (id) DO NOTHING;\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, tc.src)
			res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{tenantPolicy()}, q)
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("want exactly one SQLETCH125 (INSERT-read refusal), got %+v", res.Diags)
			}
			if !strings.Contains(res.Diags[0].Message, "INSERT's SELECT body") {
				t.Errorf("message %q does not name the INSERT SELECT body", res.Diags[0].Message)
			}
			// Nothing may be woven: the returned template must be identity and
			// carry no scoping conjunct.
			if res.Query != q {
				t.Fatalf("refused INSERT read must not be rewritten")
			}
			if got := renderSQL(t, res.Query); strings.Contains(got, "tenant_id") {
				t.Errorf("leak: a scoping conjunct was woven into a refused INSERT:\n%s", got)
			}
		})
	}
}
