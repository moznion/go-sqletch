package rules

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/template"
)

// checkMySQL runs the offline rule chain under the MySQL dialect.
func checkMySQL(t *testing.T, src string) []diagnostics.Diagnostic {
	t.Helper()
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	out := CheckLexical(mysql.Profile{}, q)
	rs, err := ast.Renderings(mysql.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	return append(out, CheckR1(mysql.Profile{}, mysql.Frontend{}, q, rs)...)
}

func TestR1_MySQLDialect(t *testing.T) {
	good := `-- name: SearchUsers :many
-- @param organization_id: bigint
-- @param email_prefix: varchar(255)
-- @param limit: bigint
SELECT u.id, u.email FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@if-present(email_prefix)
  AND u.email LIKE concat(:email_prefix, '%')
@endif
@choose(sort)
@case(email_asc)
ORDER BY u.email ASC
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`
	if diags := checkMySQL(t, good); len(diags) != 0 {
		t.Fatalf("valid MySQL template rejected: %+v", diags)
	}

	// A guarded body that is not one complete conjunct still fails
	// under the MySQL frontend probes.
	bad := `-- name: Bad :many
-- @param x: bigint
SELECT u.id FROM users AS u
WHERE TRUE
@if-present(x)
  AND u.a = :x ORDER BY u.b
@endif
;
`
	diags := checkMySQL(t, bad)
	if !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("multi-clause fragment accepted: %+v", diags)
	}
}
