package rules

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/template"
)

func checkR1(t *testing.T, src string) []diagnostics.Diagnostic {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scanner diagnostics (test precondition): %+v", diags)
	}
	if len(f.Queries) != 1 {
		t.Fatalf("queries = %d", len(f.Queries))
	}
	q := f.Queries[0]
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	return CheckR1(postgres.Profile{}, postgres.Frontend{}, q, rs)
}

func hasCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

const cleanTemplate = `-- name: SearchUsers :many
SELECT u.id, u.email, u.status
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

@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;
`

func TestCheckR1_Clean(t *testing.T) {
	diags := checkR1(t, cleanTemplate)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
}

func TestCheckR1_Violations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		code diagnostics.Code
	}{
		{
			name: "RIGHT JOIN in optional join (R2)",
			code: diagnostics.CodeJoinTypeForbidden,
			src: `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(x)
RIGHT JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE;
`,
		},
		{
			name: "FULL JOIN in optional join (R2)",
			code: diagnostics.CodeJoinTypeForbidden,
			src: `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(x)
FULL JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE;
`,
		},
		{
			name: "unbalanced parens in conjunct body",
			code: diagnostics.CodeNodeIncomplete,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(x)
  AND t.a = :x) OR (t.b = 1
@endif
;
`,
		},
		{
			name: "statement smuggling in conjunct body",
			code: diagnostics.CodeNodeIncomplete,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(x)
  AND t.a = :x; DELETE FROM t
@endif
;
`,
		},
		{
			name: "case body with trailing LIMIT",
			code: diagnostics.CodeNodeIncomplete,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@choose(sort)
@case(a)
ORDER BY t.a LIMIT 3
@end
;
`,
		},
		{
			name: "unparseable skeleton",
			code: diagnostics.CodeRenderingParse,
			src: `-- name: Bad :many
SELEC 1 FROM t;
`,
		},
		{
			name: "utility statement",
			code: diagnostics.CodeNotSingleDML,
			src: `-- name: Bad :exec
VACUUM;
`,
		},
		{
			name: "join item smuggling a WHERE",
			code: diagnostics.CodeNodeIncomplete,
			src: `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(x)
JOIN orgs AS o ON o.id = u.org_id AND o.k = :x WHERE u.deleted
@endif
;
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := checkR1(t, tt.src)
			if !hasCode(diags, tt.code) {
				t.Errorf("want %s, got: %+v", tt.code, diags)
			}
		})
	}
}

// Parse errors must point into the template file, not the rendering.
// The broken clause sits AFTER a fragment whose :x → $1 rewrite shifts
// rendered offsets, so this exercises real source-map translation.
// (Note: an unknown operator like `===` is NOT a parse error — operator
// existence is checked by the oracle in P4.)
func TestCheckR1_ErrorPositionMapsToTemplate(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
WHERE TRUE
@if-present(x)
  AND u.status = :x
@endif
GROUP BY;
`
	diags := checkR1(t, src)
	if !hasCode(diags, diagnostics.CodeRenderingParse) {
		t.Fatalf("want parse diagnostic, got %+v", diags)
	}
	for _, d := range diags {
		if d.Code == diagnostics.CodeRenderingParse {
			if d.Span.File != "test.sql" {
				t.Errorf("span file = %q", d.Span.File)
			}
			if d.Span.Start < 0 || d.Span.Start >= len(src) {
				t.Fatalf("span start %d out of template range", d.Span.Start)
			}
			line := lineAt(src, d.Span.Start)
			if !strings.Contains(line, "GROUP BY") {
				t.Errorf("span points at %q, want the GROUP BY line", line)
			}
		}
	}
}

func lineAt(src string, off int) string {
	start := strings.LastIndexByte(src[:off], '\n') + 1
	end := strings.IndexByte(src[off:], '\n')
	if end < 0 {
		return src[start:]
	}
	return src[start : off+end]
}
