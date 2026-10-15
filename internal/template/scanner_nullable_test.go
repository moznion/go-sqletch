package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

func TestScan_Nullable(t *testing.T) {
	src := "-- name: Q :many\n" +
		"-- @nullable author_name: false\n" +
		"--   @nullable   deleted_at :true  \n" +
		"SELECT o.id, a.name AS author_name, o.deleted_at FROM orders o LEFT JOIN authors a ON a.id = o.author_id\n"
	q := scanClean(t, src).Queries[0]
	if len(q.NullOverrides) != 2 {
		t.Fatalf("got %+v", q.NullOverrides)
	}
	an, ok := q.NullOverrides["author_name"]
	if !ok || an.Nullable {
		t.Errorf("author_name: got %+v (present=%v), want nullable=false", an, ok)
	}
	if got := src[an.Span.Start:an.Span.End]; got != "-- @nullable author_name: false" {
		t.Errorf("author_name span covers %q", got)
	}
	if d, ok := q.NullOverrides["deleted_at"]; !ok || !d.Nullable {
		t.Errorf("deleted_at: got %+v (present=%v), want nullable=true", d, ok)
	}
	q = scanClean(t, "-- name: Q :many\nSELECT id FROM orders\n").Queries[0]
	if q.NullOverrides != nil {
		t.Errorf("absent directive recorded: %+v", q.NullOverrides)
	}
}

// Every malformed form is SQLETCH018 and records nothing: a typo that
// silently left the analyzer's verdict in place would generate a type
// its author believes was overridden.
func TestScan_NullableMalformed(t *testing.T) {
	cases := []string{
		"-- @nullable",
		"-- @nullable ",
		"-- @nullable c",                 // no value
		"-- @nullable c:",                // empty value
		"-- @nullable c false",           // missing colon
		"-- @nullable: c false",          // colon in the wrong place
		"-- @nullable c: no",             // not a boolean
		"-- @nullable c: FALSE",          // literals are lowercase only
		"-- @nullable c: 0",              // not a boolean
		"-- @nullable c: false (reason)", // trailing words
		"-- @nullable c, d: false",       // one column per directive
		"-- @nullable C: false",          // column names are lowercase identifiers, like @column
		"-- @nullable 1c: false",         // not an identifier
		"-- @nullable c: false -- x",     // trailing comment text
	}
	for _, line := range cases {
		src := "-- name: Q :many\n" + line + "\nSELECT id AS c FROM orders\n"
		f, diags := scan(t, src)
		if !hasCode(diags, diagnostics.CodeBadNullable) {
			t.Errorf("no SQLETCH018 for %q: %+v", line, diags)
		}
		if len(f.Queries) == 1 && len(f.Queries[0].NullOverrides) != 0 {
			t.Errorf("malformed directive recorded for %q: %+v", line, f.Queries[0].NullOverrides)
		}
	}
}

// A second @nullable for one column is SQLETCH018 at the second
// directive, even when it agrees: which one wins would otherwise depend
// on source order. The first stands.
func TestScan_NullableDuplicate(t *testing.T) {
	for _, second := range []string{"-- @nullable c: true", "-- @nullable c: false"} {
		src := "-- name: Q :many\n-- @nullable c: false\n" + second + "\nSELECT id AS c FROM orders\n"
		f, diags := scan(t, src)
		var found bool
		for _, d := range diags {
			if d.Code == diagnostics.CodeBadNullable {
				found = true
				if got := src[d.Span.Start:d.Span.End]; got != second {
					t.Errorf("duplicate diagnostic points at %q", got)
				}
			}
		}
		if !found {
			t.Fatalf("no SQLETCH018 for %q: %+v", second, diags)
		}
		if o, ok := f.Queries[0].NullOverrides["c"]; !ok || o.Nullable {
			t.Errorf("first directive should stand: %+v", f.Queries[0].NullOverrides)
		}
	}
	// Distinct columns are not duplicates.
	q := scanClean(t, "-- name: Q :many\n-- @nullable a: false\n-- @nullable b: true\nSELECT 1 AS a, 2 AS b\n").Queries[0]
	if len(q.NullOverrides) != 2 {
		t.Errorf("got %+v", q.NullOverrides)
	}
}

// Like every directive, @nullable in the gap before a header annotates
// the FOLLOWING query.
func TestScan_NullableGapAttachesToFollowingQuery(t *testing.T) {
	src := "-- name: A :many\n" +
		"SELECT id FROM orders\n" +
		"-- @nullable id: true\n" +
		"-- name: B :many\n" +
		"SELECT id FROM orders\n"
	f := scanClean(t, src)
	if f.Queries[0].NullOverrides != nil {
		t.Errorf("gap directive leaked onto A: %+v", f.Queries[0].NullOverrides)
	}
	if o, ok := f.Queries[1].NullOverrides["id"]; !ok || !o.Nullable {
		t.Errorf("gap directive did not attach to B: %+v", f.Queries[1].NullOverrides)
	}
}

// The comment stays in the skeleton verbatim, like every directive —
// so editing it re-keys the query's oracle entries (design 25).
func TestScan_NullableStaysInSkeleton(t *testing.T) {
	f := scanClean(t, "-- name: Q :many\n-- @nullable id: true\nSELECT id FROM orders\n")
	var text string
	for _, it := range f.Queries[0].Items {
		if s, ok := it.(*Skeleton); ok {
			text += s.Text
		}
	}
	if !strings.Contains(text, "-- @nullable id: true") {
		t.Errorf("directive excised from skeleton:\n%s", text)
	}
}

// `-- @nullables` and similar are not the directive (word boundary):
// they stay ordinary comments rather than tripping SQLETCH018.
func TestScan_NullableWordBoundary(t *testing.T) {
	q := scanClean(t, "-- name: Q :many\n-- @nullables are discussed elsewhere\nSELECT 1\n").Queries[0]
	if q.NullOverrides != nil {
		t.Errorf("non-directive comment recorded: %+v", q.NullOverrides)
	}
}
