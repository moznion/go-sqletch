package rules

import (
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/shape"
)

// PROJECT_INSTRUCTION Use Case 2's INSERT counterpart.
const insertTemplate = `-- name: CreateUser :one
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
@if-present(nickname)
  , :nickname
@endif
)
RETURNING id;
`

func TestInsertPairing_Clean(t *testing.T) {
	q := scanOne(t, insertTemplate)
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if diags := checkR1(t, insertTemplate); len(diags) != 0 {
		t.Fatalf("R1 diagnostics: %+v", diags)
	}
}

func TestInsertPairing_Violations(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "guarded column without guarded value",
			src: `-- name: Bad :one
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
)
RETURNING id;
`,
		},
		{
			name: "guard mismatch between pair",
			src: `-- name: Bad :one
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
@if-present(other)
  , :other
@endif
)
RETURNING id;
`,
		},
		{
			name: "second row missing the pair",
			src: `-- name: Bad :exec
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
@if-present(nickname)
  , :nickname
@endif
), (
    :email2
);
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := scanOne(t, tt.src)
			diags := CheckLexical(postgres.Profile{}, q)
			if !hasCode(diags, diagnostics.CodePairedGuards) {
				t.Errorf("want SQLETCH119, got %+v", diags)
			}
		})
	}
}

func TestInsertAnchors(t *testing.T) {
	src := `-- name: Bad :exec
INSERT INTO users (
@if-present(nickname)
  , nickname
@endif
) VALUES (
@if-present(nickname)
  , :nickname
@endif
);
`
	q := scanOne(t, src)
	diags := CheckLexical(postgres.Profile{}, q)
	if !hasCode(diags, diagnostics.CodeUnanchoredSet) {
		t.Fatalf("want SQLETCH118 for all-optional INSERT lists, got %+v", diags)
	}
}

func TestInsertValueSmuggling(t *testing.T) {
	src := `-- name: Bad :one
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
@if-present(nickname)
  , :nickname), (:email
@endif
)
RETURNING id;
`
	diags := checkR1(t, src)
	if !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for VALUES smuggling, got %+v", diags)
	}
}

// All shapes of the INSERT template must render to parseable SQL —
// the guarded pair vanishes from both lists together.
func TestInsertShapesParse(t *testing.T) {
	q := scanOne(t, insertTemplate)
	keys, _ := shape.Enumerate(q, 0)
	if len(keys) != 2 {
		t.Fatalf("shapes = %d, want 2", len(keys))
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

// SQLETCH212: optional NOT NULL column without default warns (and only
// warns — generation proceeds).
func TestInsertNotNullWithoutDefaultWarns(t *testing.T) {
	src := `-- name: Bad :one
INSERT INTO users (
    email
@if-present(status)
  , status
@endif
) VALUES (
    :email
@if-present(status)
  , :status
@endif
)
RETURNING id;
`
	diags := checkResolved(t, src) // fixture catalog: users.status NOT NULL, no default
	found := false
	for _, d := range diags {
		if d.Code == diagnostics.CodeOptionalInsertNotNull && d.Severity == diagnostics.Warning {
			found = true
		}
	}
	if !found {
		t.Fatalf("want SQLETCH212 warning, got %+v", diags)
	}
	if diagnostics.HasErrors(diags) {
		t.Fatalf("warning must not be an error: %+v", diags)
	}
}
