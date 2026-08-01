package rules

import (
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/sqlite"
	"github.com/moznion/sqletch/internal/template"
)

// checkSQLite runs the offline rule chain under the SQLite dialect.
func checkSQLite(t *testing.T, src string) []diagnostics.Diagnostic {
	t.Helper()
	f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	out := CheckLexical(sqlite.Profile{}, q)
	rs, err := ast.Renderings(sqlite.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	return append(out, CheckR1(sqlite.Profile{}, sqlite.Frontend{}, q, rs)...)
}

func TestR1_SQLiteDialect(t *testing.T) {
	good := `-- name: SearchUsers :many
-- @param organization_id: integer
-- @param email_prefix: text
-- @param limit: integer
SELECT u.id, u.email FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif
@choose(sort)
@case(email_asc)
ORDER BY u.email ASC
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`
	if diags := checkSQLite(t, good); len(diags) != 0 {
		t.Fatalf("valid SQLite template rejected: %+v", diags)
	}

	bad := `-- name: Bad :many
-- @param x: integer
SELECT u.id FROM users AS u
WHERE TRUE
@if-present(x)
  AND u.a = :x ORDER BY u.b
@endif
;
`
	diags := checkSQLite(t, bad)
	if !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("multi-clause fragment accepted: %+v", diags)
	}
}
