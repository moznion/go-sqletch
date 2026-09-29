package rules

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// checkLexR1 runs the scanner, the lexical pass (R6), and R1 under one
// dialect; the scanner must accept the template.
func checkLexR1(t *testing.T, profile dialect.LexerProfile, fe dialect.Frontend, src string) []diagnostics.Diagnostic {
	t.Helper()
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scanner diagnostics (test precondition): %+v", diags)
	}
	q := f.Queries[0]
	out := CheckLexical(profile, q)
	rs, err := ast.Renderings(profile, q)
	if err != nil {
		t.Fatal(err)
	}
	return append(out, CheckR1(profile, fe, q, rs)...)
}

// Inside a statement-level set operation, a WHERE/HAVING conjunct slot
// belongs to the unparenthesized operand core it sits in: R1's
// membership test counts the top-level conjuncts of THAT core's clause
// (spec R1; design 03 §"Set-operation operands"). The statement-level
// clause of a set operation is empty on PostgreSQL and is the FIRST
// core's on SQLite, so without this every branch conjunct was either
// unplaceable (0 conjuncts) or checked against the wrong core.
func TestR1_SetOpBranchConjuncts(t *testing.T) {
	pg := struct {
		profile dialect.LexerProfile
		fe      dialect.Frontend
	}{postgres.Profile{}, postgres.Frontend{}}
	lite := struct {
		profile dialect.LexerProfile
		fe      dialect.Frontend
	}{sqlite.Profile{}, sqlite.Frontend{}}

	cases := []struct {
		name    string
		dialect string
		src     string
		want    diagnostics.Code // "" = clean
	}{
		{
			name:    "optional conjunct in every branch",
			dialect: "postgres",
			src: `-- name: Q :many
SELECT c.id FROM convs AS c WHERE c.scope = 'room'
@if-present(after)
  AND c.id > :after
@endif
UNION ALL
SELECT c.id FROM convs AS c WHERE c.scope = 'group'
@if-present(after)
  AND c.id > :after
@endif
UNION ALL
SELECT s.conv_id FROM subs AS s JOIN convs AS c ON c.id = s.conv_id WHERE s.user_id = :user_id
@if-present(after)
  AND s.conv_id > :after
@endif
ORDER BY 1
LIMIT :limit;
`,
		},
		{
			name:    "optional conjunct only in a later branch",
			dialect: "postgres",
			src: `-- name: Q :many
SELECT c.id FROM convs AS c
EXCEPT
SELECT c.id FROM convs AS c WHERE c.archived
@if-present(kind)
  AND c.kind = :kind
@endif
;
`,
		},
		{
			name:    "optional HAVING conjunct in a branch",
			dialect: "postgres",
			src: `-- name: Q :many
SELECT c.kind FROM convs AS c GROUP BY c.kind HAVING count(*) > 0
@if-present(min)
  AND count(*) >= :min
@endif
UNION
SELECT 'none';
`,
		},
		{
			// `a OR b AND frag` — the fragment regroups under the OR's
			// right arm; it is no top-level conjunct of its branch.
			name:    "regrouping under OR in a later branch",
			dialect: "postgres",
			want:    diagnostics.CodeNodeIncomplete,
			src: `-- name: Q :many
SELECT c.id FROM convs AS c WHERE TRUE
UNION ALL
SELECT c.id FROM convs AS c WHERE c.a = 1 OR c.b = 2
@if-present(x)
  AND c.c = :x
@endif
;
`,
		},
		{
			name:    "regrouping under OR in the first branch",
			dialect: "postgres",
			want:    diagnostics.CodeNodeIncomplete,
			src: `-- name: Q :many
SELECT c.id FROM convs AS c WHERE c.a = 1 OR c.b = 2
@if-present(x)
  AND c.c = :x
@endif
UNION ALL
SELECT c.id FROM convs AS c WHERE TRUE;
`,
		},
		{
			name:    "unanchored branch WHERE is still R6",
			dialect: "postgres",
			want:    diagnostics.CodeUnanchoredClause,
			src: `-- name: Q :many
SELECT c.id FROM convs AS c WHERE TRUE
UNION ALL
SELECT c.id FROM convs AS c WHERE
@if-present(x)
  AND c.c = :x
@endif
;
`,
		},
		{
			name:    "sqlite: optional conjunct in every branch",
			dialect: "sqlite",
			src: `-- name: Q :many
-- @param after: integer
-- @param user_id: integer
SELECT c.id FROM convs AS c WHERE c.scope = 'room'
@if-present(after)
  AND c.id > :after
@endif
UNION ALL
SELECT s.conv_id FROM subs AS s WHERE s.user_id = :user_id
@if-present(after)
  AND s.conv_id > :after
@endif
ORDER BY 1;
`,
		},
		{
			// SQLite's statement-level WHERE of a compound is the FIRST
			// core's: counting against it would miss this regrouping in
			// the SECOND core entirely (0 != 1 was reported for the
			// clean case, and the first core's conjuncts would be the
			// yardstick for everyone).
			name:    "sqlite: regrouping under OR in a later branch",
			dialect: "sqlite",
			want:    diagnostics.CodeNodeIncomplete,
			src: `-- name: Q :many
-- @param x: integer
SELECT c.id FROM convs AS c WHERE c.a = 1
UNION ALL
SELECT c.id FROM convs AS c WHERE c.a = 1 OR c.b = 2
@if-present(x)
  AND c.c = :x
@endif
;
`,
		},
		{
			// @choose and @in inside an operand were already legal and
			// are verified per case / per arity like anywhere else.
			name:    "choose projection and @in inside an operand stay legal",
			dialect: "postgres",
			src: `-- name: Q :many
SELECT a.id FROM archive AS a
UNION ALL
SELECT
@choose(m)
@case(a)
u.id
@case(b)
u.org_id
@end
 FROM users AS u WHERE u.id @in(:ids);
`,
		},
		{
			name:    "optional join inside an operand is rejected",
			dialect: "postgres",
			want:    diagnostics.CodeNodeIncomplete,
			src: `-- name: Q :many
SELECT a.id FROM archive AS a
UNION ALL
SELECT u.id FROM users AS u
@if-present(x)
JOIN orgs AS o ON o.id = u.org_id AND o.k = :x
@endif
WHERE TRUE;
`,
		},
		{
			// SQLite's statement-level Relations()/WHERE of a compound
			// is the FIRST core's, which used to let an optional join or
			// a @filter-tree through in the first operand only.
			name:    "sqlite: optional join in the FIRST operand is rejected too",
			dialect: "sqlite",
			want:    diagnostics.CodeNodeIncomplete,
			src: `-- name: Q :many
-- @param x: integer
SELECT u.id FROM users AS u
@if-present(x)
JOIN orgs AS o ON o.id = u.org_id AND o.k = :x
@endif
WHERE TRUE
UNION ALL
SELECT a.id FROM archive AS a;
`,
		},
		{
			name:    "sqlite: @filter-tree in the FIRST operand is rejected too",
			dialect: "sqlite",
			want:    diagnostics.CodeNodeIncomplete,
			src: `-- name: Q :many
-- @param st: text
SELECT u.id FROM users AS u WHERE TRUE AND @filter-tree(f)
@predicate(st)
u.status = :st
@end
UNION ALL
SELECT a.id FROM archive AS a;
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := pg
			if tc.dialect == "sqlite" {
				k = lite
			}
			diags := checkLexR1(t, k.profile, k.fe, tc.src)
			if tc.want == "" {
				if len(diags) != 0 {
					t.Fatalf("unexpected diagnostics: %+v", diags)
				}
				return
			}
			if !hasCode(diags, tc.want) {
				t.Fatalf("want %s, got %+v", tc.want, diags)
			}
		})
	}
}
