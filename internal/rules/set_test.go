package rules

import (
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/shape"
)

const updateTemplate = `-- name: UpdateUserProfile :one
UPDATE users
SET
    updated_at = now()
@if-present(email)
  , email = :email
@endif
@if-present(nickname)
  , nickname = :nickname
@endif
WHERE id = :id
RETURNING id, email, nickname, updated_at;
`

func TestCheckR1_SetItems_Clean(t *testing.T) {
	if diags := checkR1(t, updateTemplate); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
}

func TestCheckR1_SetItem_Smuggling(t *testing.T) {
	src := `-- name: Bad :exec
UPDATE users
SET
    updated_at = now()
@if-present(email)
  , email = :email WHERE users.admin
@endif
WHERE id = :id;
`
	diags := checkR1(t, src)
	if !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for SET-item smuggling, got %+v", diags)
	}
}

func TestCheckLexical_SetAnchor(t *testing.T) {
	src := `-- name: Bad :exec
UPDATE users
SET
@if-present(email)
  , email = :email
@endif
WHERE id = :id;
`
	q := scanOne(t, src)
	diags := CheckLexical(postgres.Profile{}, q)
	if !hasCode(diags, diagnostics.CodeUnanchoredSet) {
		t.Fatalf("want SQLETCH118, got %+v", diags)
	}

	// With the anchor, the same template is clean.
	q2 := scanOne(t, updateTemplate)
	if diags := CheckLexical(postgres.Profile{}, q2); len(diags) != 0 {
		t.Fatalf("anchored template must be clean: %+v", diags)
	}
}

// Parse-level shape soundness for the PATCH pattern: all four shapes
// (email/nickname on/off) must render to parseable SQL, including the
// minimal shape kept valid by the anchor.
func TestUpdateShapesParse(t *testing.T) {
	q := scanOne(t, updateTemplate)
	keys, _ := shape.Enumerate(q, 0)
	if len(keys) != 4 {
		t.Fatalf("shapes = %d, want 4", len(keys))
	}
	fe := postgres.Frontend{}
	for _, k := range keys {
		r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fe.Parse(r.SQL); err != nil {
			t.Fatalf("shape %s does not parse: %v\n%s", k, err, r.SQL)
		}
	}
}
