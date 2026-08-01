package ast

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

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

func scanOne(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	if len(f.Queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(f.Queries))
	}
	return f.Queries[0]
}

func TestRenderings_UseCase1(t *testing.T) {
	q := scanOne(t, useCase1)
	rs, err := Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	// maximal + 2 extra cases (email_asc, default).
	if len(rs) != 3 {
		t.Fatalf("renderings = %d, want 3", len(rs))
	}
	max := rs[0]
	if max.Kind != RenderMaximal {
		t.Fatalf("rs[0].Kind = %v, want maximal", max.Kind)
	}

	// Placeholders numbered in first-occurrence order.
	wantSeq := []string{"organization_id", "status", "limit"}
	if len(max.ParamsSeq) != len(wantSeq) {
		t.Fatalf("ParamsSeq = %v, want %v", max.ParamsSeq, wantSeq)
	}
	for i, w := range wantSeq {
		if max.ParamsSeq[i] != w {
			t.Errorf("ParamsSeq[%d] = %q, want %q", i, max.ParamsSeq[i], w)
		}
	}

	// The maximal SQL: no :name placeholders remain, no template
	// constructs remain, conjunct is wrapped.
	for _, frag := range []string{":organization_id", ":status", ":limit", "@if-present", "@endif", "@choose", "@case", "@end"} {
		if strings.Contains(max.SQL, frag) {
			t.Errorf("maximal SQL still contains %q:\n%s", frag, max.SQL)
		}
	}
	for _, want := range []string{
		"FROM users AS u",
		"JOIN organization_users AS ou",
		"AND ou.organization_id = $1",
		"WHERE TRUE",
		"AND (u.status = $2)",
		"ORDER BY u.created_at DESC",
		"LIMIT $3;",
	} {
		if !strings.Contains(max.SQL, want) {
			t.Errorf("maximal SQL missing %q:\n%s", want, max.SQL)
		}
	}

	// Case renderings substitute exactly the ORDER BY.
	caseEmail := rs[1]
	if caseEmail.Kind != RenderCase || caseEmail.CaseIdx != 1 {
		t.Fatalf("rs[1] = kind %v case %d, want case ordinal 1", caseEmail.Kind, caseEmail.CaseIdx)
	}
	if !strings.Contains(caseEmail.SQL, "ORDER BY u.email ASC, u.id ASC") ||
		strings.Contains(caseEmail.SQL, "created_at DESC") {
		t.Errorf("case rendering wrong ORDER BY:\n%s", caseEmail.SQL)
	}
	def := rs[2]
	if def.CaseIdx != 2 || !strings.Contains(def.SQL, "ORDER BY u.id ASC") {
		t.Errorf("default rendering:\n%s", def.SQL)
	}

	// Determinism: rendering twice is byte-identical.
	rs2, err := Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if rs2[0].SQL != max.SQL {
		t.Error("re-render differs (determinism violated)")
	}
}

func TestRender_FragmentRanges(t *testing.T) {
	q := scanOne(t, useCase1)
	rs, err := Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	max := rs[0]
	if len(max.Frags) != 3 { // join, where conjunct, choose
		t.Fatalf("frags = %d, want 3", len(max.Frags))
	}
	// Each range must cover the emitted fragment including synthesized
	// wrapping and be in ascending order.
	prevEnd := 0
	for i, fr := range max.Frags {
		if fr.Start < prevEnd || fr.End <= fr.Start || fr.End > len(max.SQL) {
			t.Fatalf("frag %d range [%d,%d) invalid (prevEnd %d, len %d)", i, fr.Start, fr.End, prevEnd, len(max.SQL))
		}
		prevEnd = fr.End
	}
	// The where conjunct's emitted text is the wrapped form.
	whereTxt := max.SQL[max.Frags[1].Start:max.Frags[1].End]
	if whereTxt != "AND (u.status = $2)" {
		t.Errorf("where fragment emitted text = %q", whereTxt)
	}
}

func TestSourceMap_RoundTrip(t *testing.T) {
	q := scanOne(t, useCase1)
	rs, err := Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	max := rs[0]

	// A rendered offset inside verbatim text maps to the same bytes in
	// the template.
	idx := strings.Index(max.SQL, "organization_users")
	if idx < 0 {
		t.Fatal("marker not found")
	}
	tOff, synth := max.Map.ToTemplate(idx)
	if synth {
		t.Error("verbatim text mapped as synthesized")
	}
	if got := useCase1[tOff : tOff+len("organization_users")]; got != "organization_users" {
		t.Errorf("template at mapped offset = %q", got)
	}

	// A rendered offset inside a $n placeholder maps to the :name span.
	pIdx := strings.Index(max.SQL, "$2")
	tOff, synth = max.Map.ToTemplate(pIdx)
	if !synth {
		t.Error("placeholder must be synthesized")
	}
	if got := useCase1[tOff : tOff+len(":status")]; got != ":status" {
		t.Errorf("placeholder maps to %q, want :status", got)
	}

	// Offsets inside the synthesized "AND (" map to the fragment.
	fr := max.Frags[1]
	tOff, synth = max.Map.ToTemplate(fr.Start)
	if !synth {
		t.Error("synthesized AND ( must be marked synth")
	}
	if tOff < 0 || tOff > len(useCase1) {
		t.Errorf("anchor offset out of range: %d", tOff)
	}
}

func TestRender_MultibyteTemplate(t *testing.T) {
	src := "-- name: J :many\n" +
		"SELECT t.id -- 日本語コメント\n" +
		"FROM t\nWHERE TRUE\n" +
		"@if-present(x)\n  AND t.x = :x\n@endif\n;\n"
	q := scanOne(t, src)
	rs, err := Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	max := rs[0]
	idx := strings.Index(max.SQL, "$1")
	tOff, _ := max.Map.ToTemplate(idx)
	if got := src[tOff : tOff+len(":x")]; got != ":x" {
		t.Errorf("mapped %q, want :x", got)
	}
}

func TestRender_EmptyDefaultOmitsClause(t *testing.T) {
	src := `-- name: S :many
SELECT 1 FROM t WHERE TRUE
@choose(sort)
@case(a)
ORDER BY t.a
@default
@end
;
`
	q := scanOne(t, src)
	rs, err := Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	// rs[1] selects the empty default: no ORDER BY in output.
	if strings.Contains(rs[1].SQL, "ORDER BY") {
		t.Errorf("empty default must omit the clause:\n%s", rs[1].SQL)
	}
}

func TestRender_SharedParamAcrossFragments(t *testing.T) {
	src := `-- name: S :many
SELECT 1 FROM t WHERE t.a = :v
@if-present(x)
  AND t.x = :x AND t.b = :v
@endif
;
`
	q := scanOne(t, src)
	rs, err := Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	max := rs[0]
	// :v appears twice but gets ONE placeholder number.
	if len(max.ParamsSeq) != 2 || max.ParamsSeq[0] != "v" || max.ParamsSeq[1] != "x" {
		t.Fatalf("ParamsSeq = %v, want [v x]", max.ParamsSeq)
	}
	if !strings.Contains(max.SQL, "t.b = $1") {
		t.Errorf("second :v occurrence must reuse $1:\n%s", max.SQL)
	}
}
