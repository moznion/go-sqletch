package template

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
)

func scan(t *testing.T, src string) (*QueryFile, []diagnostics.Diagnostic) {
	t.Helper()
	return NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(src))
}

func scanClean(t *testing.T, src string) *QueryFile {
	t.Helper()
	f, diags := scan(t, src)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics:\n%s", renderAll(diags, src))
	}
	return f
}

func renderAll(diags []diagnostics.Diagnostic, src string) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString(d.Render([]byte(src)))
		b.WriteString("\n")
	}
	return b.String()
}

func hasCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// useCase1 is the spec's faceted-search example (Use Case 1).
const useCase1 = `-- name: SearchUsers :many
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
`

func TestScan_UseCase1_Structure(t *testing.T) {
	f := scanClean(t, useCase1)
	if len(f.Queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(f.Queries))
	}
	q := f.Queries[0]
	if q.Name != "SearchUsers" || q.Annotation != AnnotationMany {
		t.Fatalf("header = (%s, %s), want (SearchUsers, :many)", q.Name, q.Annotation)
	}

	// Expected item sequence: skel, join, skel, where×3 (with skels
	// between), choose, skel.
	var joins, wheres []*IfPresent
	var chooses []*Choose
	for _, it := range q.Items {
		switch v := it.(type) {
		case *IfPresent:
			if v.Slot == SlotJoinItem {
				joins = append(joins, v)
			} else {
				wheres = append(wheres, v)
			}
		case *Choose:
			chooses = append(chooses, v)
		}
	}
	if len(joins) != 1 || len(wheres) != 3 || len(chooses) != 1 {
		t.Fatalf("joins=%d wheres=%d chooses=%d, want 1/3/1", len(joins), len(wheres), len(chooses))
	}

	// Join fragment: SepNone, body verbatim starting with JOIN.
	if joins[0].Sep != SepNone || !strings.HasPrefix(joins[0].Body, "JOIN organization_users") {
		t.Errorf("join fragment = sep %v body %q", joins[0].Sep, joins[0].Body)
	}
	if len(joins[0].Guards) != 1 || joins[0].Guards[0].Param != "organization_id" {
		t.Errorf("join guards = %+v", joins[0].Guards)
	}

	// WHERE conjuncts: AND lifted, SepAnd.
	if wheres[0].Sep != SepAnd || wheres[0].Body != "u.status = :status" {
		t.Errorf("where[0] = sep %v body %q", wheres[0].Sep, wheres[0].Body)
	}
	if wheres[1].Body != "u.email LIKE :email_prefix || '%'" {
		t.Errorf("where[1] body = %q", wheres[1].Body)
	}

	// Choose block.
	ch := chooses[0]
	if ch.Param != "sort" || ch.Slot != SlotOrderBy {
		t.Errorf("choose = param %q slot %v", ch.Param, ch.Slot)
	}
	if len(ch.Cases) != 3 || ch.Default == nil {
		t.Fatalf("choose cases = %d default? %v, want 3 with default", len(ch.Cases), ch.Default != nil)
	}
	wantCases := []string{"created_at_desc", "created_at_asc", "email_asc"}
	for i, w := range wantCases {
		if ch.Cases[i].Name != w {
			t.Errorf("case[%d] = %q, want %q", i, ch.Cases[i].Name, w)
		}
		if !strings.HasPrefix(ch.Cases[i].Body, "ORDER BY") {
			t.Errorf("case[%d] body = %q, want ORDER BY prefix", i, ch.Cases[i].Body)
		}
	}
	if ch.Default.Body != "ORDER BY u.id ASC" {
		t.Errorf("default body = %q", ch.Default.Body)
	}

	// Guard bits in first-appearance order.
	wantAtoms := []string{"organization_id", "status", "email_prefix", "created_after"}
	if len(q.GuardAtoms) != len(wantAtoms) {
		t.Fatalf("guard atoms = %d, want %d", len(q.GuardAtoms), len(wantAtoms))
	}
	for i, w := range wantAtoms {
		if q.GuardAtoms[i].Param != w {
			t.Errorf("guard bit %d = %q, want %q", i, q.GuardAtoms[i].Param, w)
		}
		if q.Params[w].GuardBit != i {
			t.Errorf("param %q guard bit = %d, want %d", w, q.Params[w].GuardBit, i)
		}
	}

	// Params: limit is unguarded skeleton occurrence.
	limit := q.Params["limit"]
	if limit == nil || len(limit.Occurrences) != 1 || limit.Occurrences[0].Guards != nil {
		t.Fatalf("limit param = %+v", limit)
	}
	// status occurrence carries its guard.
	st := q.Params["status"]
	if st == nil || len(st.Occurrences) != 1 || len(st.Occurrences[0].Guards) != 1 ||
		st.Occurrences[0].Guards[0].Param != "status" {
		t.Fatalf("status param = %+v", st)
	}
}

func TestScan_UseCase3_CursorPagination(t *testing.T) {
	src := `-- name: ListAuditLogs :many
SELECT a.id, a.actor_id, a.action, a.created_at
FROM audit_logs AS a
WHERE a.tenant_id = :tenant_id

@if-present(after_id)
  AND a.id < :after_id
@endif

ORDER BY a.id DESC
LIMIT :limit;
`
	f := scanClean(t, src)
	q := f.Queries[0]
	if q.Name != "ListAuditLogs" {
		t.Fatalf("name = %q", q.Name)
	}
	var frags []*IfPresent
	for _, it := range q.Items {
		if ip, ok := it.(*IfPresent); ok {
			frags = append(frags, ip)
		}
	}
	if len(frags) != 1 || frags[0].Slot != SlotWhereConjunct || frags[0].Body != "a.id < :after_id" {
		t.Fatalf("fragment = %+v", frags)
	}
	if q.Params["tenant_id"].Occurrences[0].Guards != nil {
		t.Error("tenant_id must be an unguarded occurrence")
	}
}

func TestScan_MultipleQueriesPerFile(t *testing.T) {
	src := `-- name: A :one
SELECT 1;

-- name: B :exec
DELETE FROM t WHERE TRUE
@if-present(x)
  AND t.x = :x
@endif
;
`
	f := scanClean(t, src)
	if len(f.Queries) != 2 || f.Queries[0].Name != "A" || f.Queries[1].Name != "B" {
		t.Fatalf("queries = %+v", f.Queries)
	}
	if f.Queries[0].Annotation != AnnotationOne || f.Queries[1].Annotation != AnnotationExec {
		t.Fatalf("annotations = %v, %v", f.Queries[0].Annotation, f.Queries[1].Annotation)
	}
}

// TestScan_Reconstruction: item raw spans are contiguous and reproduce
// the source (design 01 §9 invariant).
func TestScan_Reconstruction(t *testing.T) {
	f := scanClean(t, useCase1)
	q := f.Queries[0]
	pos := q.HeaderSpan.End
	var b strings.Builder
	b.WriteString(useCase1[q.HeaderSpan.Start:q.HeaderSpan.End])
	for i, it := range q.Items {
		r := it.Raw()
		if r.Start != pos {
			t.Fatalf("item %d starts at %d, want %d (gap/overlap)", i, r.Start, pos)
		}
		b.WriteString(useCase1[r.Start:r.End])
		pos = r.End
	}
	if pos != len(useCase1) {
		t.Fatalf("items end at %d, want %d", pos, len(useCase1))
	}
	if b.String() != useCase1 {
		t.Fatal("reconstructed source differs from input")
	}
}

func TestScan_Rejected(t *testing.T) {
	tests := []struct {
		name string
		src  string
		code diagnostics.Code
	}{
		{
			name: "construct in projection (R1 fast path)",
			code: diagnostics.CodeConstructBadSlot,
			src: `-- name: Bad :many
SELECT
    u.id
@if-present(with_email)
  , u.email
@endif
FROM users AS u;
`,
		},
		{
			name: "nested guards (R5)",
			code: diagnostics.CodeConstructNesting,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(a)
  @if-present(b)
    AND t.x = :b
  @endif
@endif
;
`,
		},
		{
			name: "positional parameter",
			code: diagnostics.CodePositionalParam,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE t.x = $1;
`,
		},
		{
			name: "statement without header",
			code: diagnostics.CodeMissingHeader,
			src:  "SELECT 1;\n",
		},
		{
			name: "two statements under one header",
			code: diagnostics.CodeMultipleStatements,
			src: `-- name: Bad :many
SELECT 1;
SELECT 2;
`,
		},
		{
			name: "construct inside parens",
			code: diagnostics.CodeConstructNested,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE EXISTS (
  SELECT 1 FROM u WHERE TRUE
  @if-present(x)
    AND u.x = :x
  @endif
);
`,
		},
		{
			name: "conjunct without AND",
			code: diagnostics.CodeConjunctNeedsAnd,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(x)
  t.x = :x
@endif
;
`,
		},
		{
			name: "choose without cases",
			code: diagnostics.CodeChooseStructure,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@choose(sort)
@end
;
`,
		},
		{
			name: "duplicate case",
			code: diagnostics.CodeChooseStructure,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@choose(sort)
@case(a)
ORDER BY t.a
@case(a)
ORDER BY t.b
@end
;
`,
		},
		{
			name: "default not last",
			code: diagnostics.CodeChooseStructure,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@choose(sort)
@default
ORDER BY t.a
@case(b)
ORDER BY t.b
@end
;
`,
		},
		{
			// GROUP BY became a legal @choose target in v0.2; a body
			// that is neither clause is still rejected.
			name: "case body must start with ORDER BY or GROUP BY",
			code: diagnostics.CodeChooseStructure,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@choose(sort)
@case(a)
LIMIT 5
@end
;
`,
		},
		{
			name: "duplicate query name",
			code: diagnostics.CodeDuplicateQueryName,
			src: `-- name: Dup :one
SELECT 1;
-- name: Dup :one
SELECT 2;
`,
		},
		{
			name: "guard name must be snake_case",
			code: diagnostics.CodeBadIdentifier,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(CamelCase)
  AND t.x = :x
@endif
;
`,
		},
		{
			name: "unmatched endif",
			code: diagnostics.CodeConstructGrammar,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@endif
;
`,
		},
		{
			name: "unterminated if-present",
			code: diagnostics.CodeConstructGrammar,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(x)
  AND t.x = :x
;
`,
		},
		{
			name: "if-present in ORDER BY position",
			code: diagnostics.CodeConstructBadSlot,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
ORDER BY
@if-present(x)
  t.x
@endif
;
`,
		},
		{
			name: "duplicate guard parameter",
			code: diagnostics.CodeConstructGrammar,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(x, x)
  AND t.x = :x
@endif
;
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := scan(t, tt.src)
			if !hasCode(diags, tt.code) {
				t.Errorf("want %s, got:\n%s", tt.code, renderAll(diags, tt.src))
			}
		})
	}
}

func TestScan_AtOperatorsAreNotConstructs(t *testing.T) {
	src := `-- name: Jsonb :many
SELECT 1 FROM t WHERE t.tags @> :tags AND t.doc @@ :q;
`
	f := scanClean(t, src)
	q := f.Queries[0]
	if len(q.Items) != 1 {
		t.Fatalf("items = %d, want 1 skeleton", len(q.Items))
	}
	if q.Params["tags"] == nil || q.Params["q"] == nil {
		t.Fatal("params inside @> / @@ expressions must be collected")
	}
}

func TestScan_ParamsInStringsAreIgnored(t *testing.T) {
	src := `-- name: S :many
SELECT ':not_a_param' AS c, $$ :also_not $$ AS d FROM t WHERE t.x = :real;
`
	f := scanClean(t, src)
	q := f.Queries[0]
	if len(q.Params) != 1 || q.Params["real"] == nil {
		t.Fatalf("params = %v, want only 'real'", q.ParamOrder)
	}
}

func TestScan_ChooseCaseParamsAreMarked(t *testing.T) {
	src := `-- name: S :many
SELECT 1 FROM t WHERE TRUE
@choose(sort)
@case(a)
ORDER BY t.score - :min_score
@end
;
`
	f := scanClean(t, src)
	q := f.Queries[0]
	p := q.Params["min_score"]
	if p == nil || len(p.Occurrences) != 1 || !p.Occurrences[0].InChooseCase {
		t.Fatalf("min_score = %+v, want one InChooseCase occurrence", p)
	}
}

func TestScan_GuardLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("-- name: Big :many\nSELECT 1 FROM t WHERE TRUE\n")
	for i := range 65 {
		b.WriteString("@if-present(g")
		b.WriteString(strings.Repeat("x", i%3+1)) // vary names: gx, gxx, gxxx …
		b.WriteString(itoa(i))
		b.WriteString(")\n  AND t.c = :v")
		b.WriteString(itoa(i))
		b.WriteString("\n@endif\n")
	}
	b.WriteString(";\n")
	_, diags := scan(t, b.String())
	if !hasCode(diags, diagnostics.CodeTooManyGuards) {
		t.Errorf("want %s for 65 guards, got:\n%s", diagnostics.CodeTooManyGuards, renderAll(diags, b.String()))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// Diagnostic spans are indexed into the source by the excerpt renderer
// and by the LSP's UTF-16 position conversion, so they must stay within
// bounds even when the construct that triggered them ends at EOF. The
// "empty body" diagnostics point at the character *after* the marker,
// which does not exist there. Found by FuzzScan; the corpus entry under
// testdata/fuzz/FuzzScan pins the exact input.
func TestScan_DiagnosticSpansStayInBounds(t *testing.T) {
	// Each input ends exactly where a construct wants to point one past.
	inputs := []string{
		"--name:A :many\n@order-by(A)@key(A)",
		"-- name: A :many\nSELECT 1 FROM t WHERE TRUE\n  AND @filter-tree(s)\n@predicate(a)",
		"-- name: A :many\n@choose(s)@case(a)",
		"-- name: A :many\n@order-by(s)\n@key(a)",
	}
	for _, src := range inputs {
		_, diags := scan(t, src)
		for _, d := range diags {
			if d.Span.Start < 0 || d.Span.End < d.Span.Start || d.Span.End > len(src) {
				t.Errorf("%q: %s span %+v out of bounds (len %d)", src, d.Code, d.Span, len(src))
			}
		}
	}
}
