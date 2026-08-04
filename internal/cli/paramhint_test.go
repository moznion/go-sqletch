package cli

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// hintCatalog is the schema every case in this file resolves against.
func hintCatalog() *cache.Catalog {
	return &cache.Catalog{Tables: []cache.Table{
		{Schema: "public", Name: "users", OID: 101, Cols: []cache.Column{
			{Name: "id", Att: 1, TypeOID: 20, TypeName: "int8", NotNull: true},
			{Name: "status", Att: 2, TypeOID: 1043, TypeName: "varchar", NotNull: true},
		}},
	}}
}

// runResolvedChecks drives the shared catalog-dependent pass with a
// hand-built oracle answer, so `-- @param` handling is testable without
// a database. params is the Desc's parameter list, by position.
func runResolvedChecks(t *testing.T, dialectName, src string, params []dialect.TypeRef) (map[string]dialect.TypeRef, []diagnostics.Diagnostic) {
	t.Helper()
	drv := driverFor(config.Config{Dialect: dialectName})
	file, diags := template.NewScanner(drv.profile).ScanFile("t.sql", []byte(src))
	if diagnostics.HasErrors(diags) || len(file.Queries) != 1 {
		t.Fatalf("test template must scan cleanly: %v", diags)
	}
	rs, err := ast.Renderings(drv.profile, file.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	descs := make([]dialect.Desc, len(rs))
	for i := range rs {
		descs[i] = dialect.Desc{
			Params:  params,
			Columns: []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20, Name: "int8"}, SrcRel: 101, SrcAtt: 1}},
		}
	}
	types, d, err := resolvedChecks(drv, dialectName, nil, file.Queries[0], rs, descs, hintCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return types, d
}

func findCode(diags []diagnostics.Diagnostic, code diagnostics.Code) *diagnostics.Diagnostic {
	for i := range diags {
		if diags[i].Code == code {
			return &diags[i]
		}
	}
	return nil
}

const hintInListTemplate = `-- name: Q :many
-- @param statuses: %s
SELECT u.id FROM users AS u
WHERE u.status @in(:statuses);
`

// A scalar annotation on an @in parameter is the motivating case: on
// PostgreSQL the oracle infers the ARRAY type from `= ANY($1)`, so the
// annotation would bind a plain string where an array is required —
// previously a silent wrong Go type that only failed at execution time.
func TestParamHint_ConflictWithOracleIsRejected(t *testing.T) {
	src := strings.Replace(hintInListTemplate, "%s", "varchar(16)", 1)
	types, diags := runResolvedChecks(t, "postgres", src,
		[]dialect.TypeRef{{OID: 1015, Name: "_varchar"}})

	d := findCode(diags, diagnostics.CodeParamHintConflict)
	if d == nil {
		t.Fatalf("want %s, got %v", diagnostics.CodeParamHintConflict, diags)
	}
	if !strings.Contains(d.Message, "statuses") {
		t.Errorf("message must name the parameter: %q", d.Message)
	}
	if !strings.Contains(d.Hint, "varchar[]") {
		t.Errorf("hint must show the compliant rewrite (the inferred type): %q", d.Hint)
	}
	// The verified type wins: a rejected hint must never reach codegen.
	if got := types["statuses"]; got.OID != 1015 {
		t.Errorf("param type = %+v, want the oracle's _varchar (1015)", got)
	}
}

func TestParamHint_AgreeingAnnotationIsAccepted(t *testing.T) {
	src := strings.Replace(hintInListTemplate, "%s", "varchar[]", 1)
	types, diags := runResolvedChecks(t, "postgres", src,
		[]dialect.TypeRef{{OID: 1015, Name: "_varchar"}})

	if d := findCode(diags, diagnostics.CodeParamHintConflict); d != nil {
		t.Fatalf("agreeing annotation must not be rejected: %+v", d)
	}
	if got := types["statuses"]; got.OID != 1015 {
		t.Errorf("param type = %+v, want 1015", got)
	}
}

// Spelling variants of the same type agree: the check compares OIDs,
// not the names the author happened to write.
func TestParamHint_SpellingVariantAgrees(t *testing.T) {
	src := `-- name: Q :many
-- @param id: bigint
SELECT u.id FROM users AS u WHERE u.id = :id;
`
	_, diags := runResolvedChecks(t, "postgres", src,
		[]dialect.TypeRef{{OID: 20, Name: "int8"}})
	if d := findCode(diags, diagnostics.CodeParamHintConflict); d != nil {
		t.Fatalf("`bigint` and int8 are the same type: %+v", d)
	}
}

// The silent class: Go-compatible but semantically different types
// (timezone handling) never errored at runtime either.
func TestParamHint_SilentMismatchIsRejected(t *testing.T) {
	src := `-- name: Q :many
-- @param since: timestamp
SELECT u.id FROM users AS u WHERE u.id > :since;
`
	_, diags := runResolvedChecks(t, "postgres", src,
		[]dialect.TypeRef{{OID: 1184, Name: "timestamptz"}})
	d := findCode(diags, diagnostics.CodeParamHintConflict)
	if d == nil {
		t.Fatalf("want %s, got %v", diagnostics.CodeParamHintConflict, diags)
	}
	if !strings.Contains(d.Hint, "timestamptz") {
		t.Errorf("hint must name the inferred type: %q", d.Hint)
	}
}

// Tier 2 has no oracle parameter types at all, so annotations remain
// the sole source and can never conflict.
func TestParamHint_Tier2AnnotationStillSupplies(t *testing.T) {
	src := `-- name: Q :many
-- @param status: varchar(32)
SELECT u.id FROM users AS u WHERE u.status = :status;
`
	types, diags := runResolvedChecks(t, "mysql", src, nil)
	if d := findCode(diags, diagnostics.CodeParamHintConflict); d != nil {
		t.Fatalf("Tier 2 annotations must not conflict: %+v", d)
	}
	if got, ok := types["status"]; !ok || got.Name == "" {
		t.Errorf("Tier 2 annotation must supply the type, got %+v", got)
	}
}

// Diagnostics must not depend on Go map iteration order.
func TestParamHint_DiagnosticsAreDeterministic(t *testing.T) {
	src := `-- name: Q :many
-- @param a: text
-- @param b: text
-- @param c: text
SELECT u.id FROM users AS u
WHERE u.id = :a AND u.id = :b AND u.id = :c;
`
	var first []string
	for range 20 {
		_, diags := runResolvedChecks(t, "postgres", src, []dialect.TypeRef{
			{OID: 20, Name: "int8"}, {OID: 20, Name: "int8"}, {OID: 20, Name: "int8"},
		})
		var got []string
		for _, d := range diags {
			if d.Code == diagnostics.CodeParamHintConflict {
				got = append(got, d.Message)
			}
		}
		if len(got) != 3 {
			t.Fatalf("want 3 conflicts, got %d: %v", len(got), diags)
		}
		if first == nil {
			first = got
			continue
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("diagnostic order is not deterministic:\n%v\n%v", first, got)
			}
		}
	}
}
