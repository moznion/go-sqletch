package template

import (
	"bytes"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// benchSource is a query file of the shape real ones take: several
// queries, each mixing guarded fragments, a @choose block, an
// @order-by block and parameters. The scanner runs over the whole file
// on every LSP keystroke, so its cost is editor latency.
var benchSource = []byte(`-- name: SearchUsers :many
SELECT
    u.id,
    u.email,
    u.status,
    u.created_at
FROM users AS u

@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif

WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif

@if-present(created_after)
  AND u.created_at >= :created_after
@endif

@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(created_at_asc)
ORDER BY u.created_at ASC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;

-- name: ListUsersSorted :many
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@order-by(sort)
@key(email)
u.email
@key(created_at)
u.created_at
@key(id)
u.id
@default
ORDER BY u.id ASC
@end

LIMIT :limit;

-- name: CountUsers :one
SELECT count(*) AS total
FROM users AS u
WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif
;
`)

func BenchmarkScanFile(b *testing.B) {
	profile := postgres.Profile{}
	b.ReportAllocs()
	b.SetBytes(int64(len(benchSource)))
	for b.Loop() {
		f, diags := NewScanner(profile).ScanFile("bench.sql", benchSource)
		if len(diags) > 0 {
			b.Fatalf("unexpected diagnostics: %v", diags)
		}
		sinkFile = f
	}
}

var sinkFile *QueryFile

// BenchmarkScanFileLarge scales the same content up, so growth that is
// quadratic in file size shows up rather than hiding behind a small
// input.
func BenchmarkScanFileLarge(b *testing.B) {
	profile := postgres.Profile{}
	var buf bytes.Buffer
	for i := range 20 {
		buf.Write(bytes.ReplaceAll(benchSource, []byte("-- name: "),
			[]byte("-- name: Q"+string(rune('a'+i%26)))))
	}
	src := buf.Bytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		f, _ := NewScanner(profile).ScanFile("bench.sql", src)
		sinkFile = f
	}
}
