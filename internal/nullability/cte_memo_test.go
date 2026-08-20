package nullability

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

// chainedCTE builds a template whose N chained CTEs each reference the
// previous definition TWICE:
//
//	WITH c1 AS (SELECT o.id, o.name FROM orgs AS o),
//	     c2 AS (SELECT a.id, a.name FROM c1 AS a JOIN c1 AS b ON a.id = b.id),
//	     ...
//	     cN AS (SELECT a.id, a.name FROM c(N-1) AS a JOIN c(N-1) AS b ON a.id = b.id)
//	SELECT c.name FROM cN AS c;
//
// Without memoization every reference fully re-descends the referenced
// body, so the base relation `orgs` is re-analyzed 2^(N-1) times — the
// exponential DoS. All joins are plain INNER joins with no null
// extension, so `orgs.name` (catalog NOT NULL) narrows to non-nullable:
// the shape is both the timing repro AND a correctness fixture.
func chainedCTE(n int) string {
	var b strings.Builder
	b.WriteString("-- name: Q :many\nWITH c1 AS (SELECT o.id, o.name FROM orgs AS o)")
	for i := 2; i <= n; i++ {
		fmt.Fprintf(&b, ",\n     c%d AS (SELECT a.id, a.name FROM c%d AS a JOIN c%d AS b ON a.id = b.id)",
			i, i-1, i-1)
	}
	fmt.Fprintf(&b, "\nSELECT c.name FROM c%d AS c;\n", n)
	return b.String()
}

// analyzeChain parses and analyzes a chained-CTE template, returning the
// per-column nullability of its single `name` column (attributed to
// orgs.name, OID 200 att 2).
func analyzeChain(t *testing.T, n int) []bool {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(chainedCTE(n)))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	rs, err := ast.Renderings(postgres.Profile{}, f.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	return Analyze(tree, rs[0], dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}}, cat(), nil)
}

// A deep chain of CTEs, each referencing the previous body twice, must
// be analyzed in bounded time. Without memoization the referenced body
// is re-descended 2^(N-1) times: measured n=24 already takes seconds and
// n>=30 runs for minutes — an unbounded-CPU DoS on a few-KB template
// that runs in the core check/generate pipeline (and warm-cached LSP).
//
// On the pre-fix code this test does not finish within the bound and
// fails; with per-body memoization the descent count is O(N) and it
// completes in well under a millisecond.
func TestAnalyze_ChainedCTETerminatesQuickly(t *testing.T) {
	const depth = 32
	const bound = 2 * time.Second

	done := make(chan []bool, 1)
	go func() { done <- analyzeChain(t, depth) }()

	select {
	case got := <-done:
		// Correctness under the deep chain: all joins are INNER with no
		// null extension, so orgs.name narrows to non-nullable.
		assertNullable(t, got, []bool{false})
	case <-time.After(bound):
		t.Fatalf("Analyze of a depth-%d chained-CTE template did not finish within %s "+
			"(exponential re-analysis DoS — needs per-body memoization)", depth, bound)
	}
}

// Correctness under a deep chain, checked synchronously and
// independently of the timing harness: the null-extension hazard of a
// LEFT JOIN buried at the base of a long reference chain must still
// reach the result column. Here c1's body LEFT JOINs orgs onto users,
// so orgs.name is null-extended; every later definition merely re-reads
// it. Memoization must preserve that verdict (nullable), never cache a
// clean contribution that drops the hazard.
func TestAnalyze_ChainedCTEPreservesNullExtension(t *testing.T) {
	const n = 30
	var b strings.Builder
	b.WriteString("-- name: Q :many\n" +
		"WITH c1 AS (SELECT u.id, o.name FROM users AS u LEFT JOIN orgs AS o ON o.id = u.org_id)")
	for i := 2; i <= n; i++ {
		fmt.Fprintf(&b, ",\n     c%d AS (SELECT a.id, a.name FROM c%d AS a JOIN c%d AS b ON a.id = b.id)",
			i, i-1, i-1)
	}
	fmt.Fprintf(&b, "\nSELECT c.name FROM c%d AS c;\n", n)

	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(b.String()))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	rs, err := ast.Renderings(postgres.Profile{}, f.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	got := Analyze(tree, rs[0], dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}}, cat(), nil)
	// orgs.name is null-extended at the base of the chain — stays nullable.
	assertNullable(t, got, []bool{true})
}
