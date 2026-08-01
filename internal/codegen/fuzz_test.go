package codegen

import (
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/mysql"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/dialect/sqlite"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
	"github.com/moznion/sqletch/runtime"
)

// fuzzConformanceProfiles pairs each lexer profile with the placeholder
// style its generated code composes under.
var fuzzConformanceProfiles = []struct {
	name    string
	profile dialect.LexerProfile
	style   runtime.Style
}{
	{"postgres", postgres.Profile{}, runtime.StyleDollar},
	{"mysql", mysql.Profile{}, runtime.StyleQuestion},
	{"sqlite", sqlite.Profile{}, runtime.StyleQuestion},
}

const (
	fuzzMaxInput  = 8192 // keep one exec cheap
	fuzzMaxShapes = 128  // guards alone span 2^64 shapes
)

// FuzzComposeConformance is the differential form of
// TestComposeConformance, the load-bearing invariant of the whole
// design: for every reachable shape, the runtime composer over the
// generated fragment table must produce byte-identical SQL to the
// verification renderer, with identical bind order. The table test pins
// it over a handful of authored templates; this explores the template
// space those templates cannot reach.
//
// @filter-tree and @in are deliberately out of scope: they compose
// through ComposeTree and through an arity shape dimension
// respectively, and have their own conformance tests
// (rules.TestFilterTree_ComposeConformance, TestComposeStyle_InList).
// Including them here would compare against the wrong composer.
func FuzzComposeConformance(f *testing.F) {
	for _, src := range conformanceCorpus {
		f.Add(src)
	}
	f.Add("-- name: A :one\nSELECT 1;")
	f.Add("-- name: B :many\nSELECT 1 FROM t WHERE TRUE\n@if-present(x)\n  AND t.x = :x\n@endif\n;")
	// Repeated binds: the question style must repeat the argument, the
	// dollar style must reuse the number.
	f.Add("-- name: C :many\nSELECT 1 FROM t WHERE t.a = :x AND t.b = :x\n@if-present(y)\n  AND t.c = :x\n@endif\n;")
	// Nested-ish construct sequences and empty bodies.
	f.Add("-- name: D :many\nSELECT 1 FROM t WHERE TRUE\n@if-present(a)\n  AND t.a\n@endif\n@if-present(b)\n  AND t.b\n@endif\n@choose(s)\n@case(x)\nORDER BY 1\n@default\nORDER BY 2\n@end;")
	// Quoting that must survive fragment splitting verbatim.
	f.Add("-- name: E :many\nSELECT '@endif', $q$ @if-present(z) $q$ FROM t WHERE t.x = :x;")
	f.Add("-- name: F :many\nSELECT `@endif` FROM `t` WHERE `x` = :x;")
	f.Add("-- name: G :many\nSELECT [@endif] FROM [t] WHERE [x] = :x;")

	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > fuzzMaxInput {
			return
		}
		for _, p := range fuzzConformanceProfiles {
			file, diags := template.NewScanner(p.profile).ScanFile("fuzz.sql", []byte(src))
			if len(diags) != 0 {
				continue // not a well-formed template for this dialect
			}
			for _, q := range file.Queries {
				if hasUnsupportedConstruct(q) {
					continue
				}
				frags := BuildFrags(p.profile, q)
				keys, _ := shape.Enumerate(q, fuzzMaxShapes)
				for _, k := range keys {
					want, err := ast.RenderShape(p.profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
					if err != nil {
						// The renderer is the reference; a shape it
						// refuses to render has nothing to conform to.
						continue
					}
					got, argIdx := runtime.ComposeStyle(p.style, frags,
						runtime.ShapeKey{Guards: k.Guards, Choices: k.Choices, Orders: k.Orders})
					if got != want.SQL {
						t.Fatalf("%s: %s shape %s:\nruntime:\n%q\nrenderer:\n%q",
							p.name, q.Name, k, got, want.SQL)
					}
					if len(argIdx) != len(want.ParamsSeq) {
						t.Fatalf("%s: %s shape %s: argIdx len %d, ParamsSeq len %d",
							p.name, q.Name, k, len(argIdx), len(want.ParamsSeq))
					}
					for i, idx := range argIdx {
						if int(idx) < 0 || int(idx) >= len(q.ParamOrder) {
							t.Fatalf("%s: %s shape %s: arg %d indexes %d, ParamOrder has %d",
								p.name, q.Name, k, i, idx, len(q.ParamOrder))
						}
						if q.ParamOrder[idx] != want.ParamsSeq[i] {
							t.Fatalf("%s: %s shape %s: arg %d is %q, renderer expects %q",
								p.name, q.Name, k, i, q.ParamOrder[idx], want.ParamsSeq[i])
						}
					}
				}
			}
		}
	})
}

// hasUnsupportedConstruct reports whether the query uses a construct
// that plain Compose does not model (see the target's doc comment).
func hasUnsupportedConstruct(q *template.QueryTemplate) bool {
	for _, it := range q.Items {
		switch it.(type) {
		case *template.FilterTree, *template.InExpr:
			return true
		}
	}
	return false
}
