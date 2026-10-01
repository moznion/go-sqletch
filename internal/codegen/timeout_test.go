package codegen

import (
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

// genTimeout generates one single-column query under the given style
// and default timeout and returns its query file.
func genTimeout(t *testing.T, style runtime.Style, def time.Duration, src string) string {
	t.Helper()
	var (
		q     *template.QueryTemplate
		frags []runtime.Frag
		tm    dialect.TypeMap
		typ   dialect.TypeRef
		ptyp  dialect.TypeRef
	)
	if style == runtime.StyleQuestion {
		f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(src))
		if len(diags) != 0 {
			t.Fatalf("scan: %+v", diags)
		}
		q, frags, tm = f.Queries[0], BuildFrags(mysql.Profile{}, f.Queries[0]), mysql.TypeMap{}
		typ, ptyp = dialect.TypeRef{OID: 8}, dialect.TypeRef{OID: 8} // BIGINT
	} else {
		q = scanOne(t, src)
		frags, tm = BuildFrags(postgres.Profile{}, q), postgres.TypeMap{}
		typ, ptyp = dialect.TypeRef{OID: 20}, dialect.TypeRef{OID: 20}
	}
	in := QueryInput{Q: q, Frags: frags, ParamTypes: map[string]dialect.TypeRef{"id": ptyp}}
	switch q.Annotation {
	case template.AnnotationExec, template.AnnotationExecRows:
	default:
		in.Columns = []dialect.ColumnDesc{{Name: "id", Type: typ}}
		in.Nullable = []bool{false}
	}
	files, diags := Generate(Options{Package: "gen", Style: style, DefaultTimeout: def}, tm, []QueryInput{in})
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}
	return string(files["q.sql.gen.go"])
}

const timeoutWrap = "\tctx, cancel := context.WithTimeout(ctx, "

// Every annotation on both driver flavors wraps the context exactly
// once, AFTER composition (rejects are not deadline-bound and observe
// the caller's ctx) and BEFORE the exec clock and the database call,
// with the cancel deferred so the deadline covers row iteration and
// scanning too.
func TestGenerate_TimeoutPlacement(t *testing.T) {
	bodies := map[string]string{
		"many":     "-- name: Q :many\n-- @timeout 1500ms\nSELECT id FROM t WHERE id = :id;\n",
		"one":      "-- name: Q :one\n-- @timeout 1500ms\nSELECT id FROM t WHERE id = :id;\n",
		"maybeone": "-- name: Q :maybe-one\n-- @timeout 1500ms\nSELECT id FROM t WHERE id = :id;\n",
		"exec":     "-- name: Q :exec\n-- @timeout 1500ms\nDELETE FROM t WHERE id = :id;\n",
		"execrows": "-- name: Q :execrows\n-- @timeout 1500ms\nDELETE FROM t WHERE id = :id;\n",
	}
	for _, style := range []runtime.Style{runtime.StyleDollar, runtime.StyleQuestion} {
		for name, src := range bodies {
			got := genTimeout(t, style, 0, src)
			want := timeoutWrap + "1500*time.Millisecond)\n\tdefer cancel()\n"
			if strings.Count(got, timeoutWrap) != 1 || !strings.Contains(got, want) {
				t.Errorf("%v/%s: want one %q\n----\n%s", style, name, want, got)
				continue
			}
			wrap := strings.Index(got, timeoutWrap)
			for _, before := range []string{"q.hook(key, sqlText)"} {
				if i := strings.Index(got, before); i < 0 || i > wrap {
					t.Errorf("%v/%s: %q must precede the deadline\n----\n%s", style, name, before, got)
				}
			}
			for _, after := range []string{"var execStart time.Time", "q.db."} {
				if i := strings.Index(got, after); i < wrap {
					t.Errorf("%v/%s: %q must follow the deadline\n----\n%s", style, name, after, got)
				}
			}
			if !strings.Contains(got, "// Deadline from `-- @timeout 1.5s` (design 23).") {
				t.Errorf("%v/%s: provenance comment missing\n----\n%s", style, name, got)
			}
		}
	}
}

// Without a directive and without a configured default, the generated
// method is byte-identical to what it was before design 23.
func TestGenerate_NoTimeoutUnchanged(t *testing.T) {
	src := "-- name: Q :many\nSELECT id FROM t WHERE id = :id;\n"
	for _, style := range []runtime.Style{runtime.StyleDollar, runtime.StyleQuestion} {
		if got := genTimeout(t, style, 0, src); strings.Contains(got, "WithTimeout") || strings.Contains(got, "cancel") {
			t.Errorf("%v: unexpected deadline\n----\n%s", style, got)
		}
	}
}

// query_timeout.default applies to queries without a directive; a
// directive overrides it, and `@timeout none` opts out of it.
func TestGenerate_TimeoutDefault(t *testing.T) {
	plain := "-- name: Q :many\nSELECT id FROM t WHERE id = :id;\n"
	got := genTimeout(t, runtime.StyleDollar, 2*time.Second, plain)
	if !strings.Contains(got, timeoutWrap+"2*time.Second)\n") ||
		!strings.Contains(got, "// Deadline from query_timeout.default (2s) (design 23).") {
		t.Errorf("default not applied\n----\n%s", got)
	}

	override := "-- name: Q :many\n-- @timeout 250ms\nSELECT id FROM t WHERE id = :id;\n"
	got = genTimeout(t, runtime.StyleDollar, 2*time.Second, override)
	if strings.Count(got, timeoutWrap) != 1 || !strings.Contains(got, timeoutWrap+"250*time.Millisecond)\n") {
		t.Errorf("directive did not override the default\n----\n%s", got)
	}

	none := "-- name: Q :many\n-- @timeout none\nSELECT id FROM t WHERE id = :id;\n"
	got = genTimeout(t, runtime.StyleDollar, 2*time.Second, none)
	if strings.Contains(got, "WithTimeout") {
		t.Errorf("@timeout none did not opt out\n----\n%s", got)
	}
	// `none` without a default is simply no deadline.
	if got := genTimeout(t, runtime.StyleDollar, 0, none); strings.Contains(got, "WithTimeout") {
		t.Errorf("@timeout none without a default emitted a deadline\n----\n%s", got)
	}
}

// The filter-tree and strict-static-expansion paths share the same
// placement: after their reject branches, before the call.
func TestGenerate_TimeoutFilterTree(t *testing.T) {
	q := scanOne(t, `-- name: Pick :many
-- @timeout 3s
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
t.tenant_id = :scope_tenant_id
@end;
`)
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
		Nullable:   []bool{false},
		ParamTypes: map[string]dialect.TypeRef{"scope_tenant_id": {OID: 20}},
	}})
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}
	src := string(files["pick.sql.gen.go"])
	wrap := strings.Index(src, timeoutWrap+"3*time.Second)\n")
	if wrap < 0 {
		t.Fatalf("no deadline\n----\n%s", src)
	}
	if i := strings.Index(src, "q.observeReject(ctx, \"Pick\", err)"); i < 0 || i > wrap {
		t.Errorf("reject branch must precede the deadline\n----\n%s", src)
	}
	if i := strings.Index(src, "q.db.Query("); i < wrap {
		t.Errorf("call must follow the deadline\n----\n%s", src)
	}
}

// The literal is the largest unit that divides the duration exactly,
// so it reads like the directive and is a pure function of the value.
func TestDurationLiteral(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour:                          "1*time.Hour",
		90 * time.Minute:                   "90*time.Minute",
		2 * time.Second:                    "2*time.Second",
		90 * time.Second:                   "90*time.Second",
		1500 * time.Millisecond:            "1500*time.Millisecond",
		250 * time.Microsecond:             "250*time.Microsecond",
		10 * time.Nanosecond:               "10*time.Nanosecond",
		time.Second + time.Nanosecond:      "1000000001*time.Nanosecond",
		time.Duration(1<<63 - 1):           "9223372036854775807*time.Nanosecond",
		3*time.Hour + 30*time.Minute:       "210*time.Minute",
		24 * time.Hour:                     "24*time.Hour",
		time.Minute + 500*time.Millisecond: "60500*time.Millisecond",
	} {
		if got := durationLiteral(d); got != want {
			t.Errorf("durationLiteral(%v) = %q, want %q", d, got, want)
		}
	}
}
