package pathpat

import (
	"strings"
	"testing"
)

// FuzzPattern drives the pattern compiler and the `$n` substitution
// with arbitrary bytes. A pattern comes from sqletch.yaml, which is a
// trusted first-party artifact, so this is a ROBUSTNESS target, not a
// vulnerability one: a malformed pattern must produce a diagnostic,
// never a panic (an index slip in the capture/segment bookkeeping) —
// and the pieces must stay consistent with each other, which is what
// the invariants below pin.
//
// It deliberately does not touch the filesystem: Walk's behavior is
// covered by the brute-force property test, and a fuzz target that
// creates directories would be neither fast nor deterministic.
func FuzzPattern(f *testing.F) {
	seeds := []string{
		"queries/*.sql",
		"(api/**/queries)/*.sql",
		"(a)/(b)/**/*.go",
		"**",
		"(**)/x",
		"a/**x",
		"((a))/b",
		"$1/gen",
		"$${", // `$$` escape immediately followed by a brace
		"a/[a-z]*.sql",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := Compile(s)
		if err != nil {
			if p != nil {
				t.Fatalf("Compile(%q) returned both a pattern and an error", s)
			}
			return
		}
		if p.String() != s {
			t.Fatalf("String() = %q, want the pattern as written %q", p.String(), s)
		}
		// A compiled pattern never claims more captures than it has
		// `(` characters, and every capture range is well formed.
		if n := p.NumCaptures(); n > strings.Count(s, "(") {
			t.Fatalf("NumCaptures() = %d, more than the %d `(` in %q", n, strings.Count(s, "("), s)
		}
		for _, c := range p.caps {
			if c.start < 0 || c.end > len(p.segs) || c.start >= c.end {
				t.Fatalf("capture %+v out of range for %d segments (%q)", c, len(p.segs), s)
			}
		}

		// MaxRef and Substitute must agree on what a reference is: a
		// string MaxRef accepts with n references must substitute with
		// n captures, and one it rejects must never substitute.
		caps := make([]string, p.NumCaptures())
		for i := range caps {
			caps[i] = "c"
		}
		n, refErr := MaxRef(s)
		out, subErr := Substitute(s, caps)
		if (refErr == nil) != (subErr == nil || n > len(caps)) {
			t.Fatalf("MaxRef(%q) err=%v disagrees with Substitute err=%v", s, refErr, subErr)
		}
		if refErr == nil && n <= len(caps) {
			if subErr != nil {
				t.Fatalf("Substitute(%q) failed after MaxRef accepted it: %v", s, subErr)
			}
			// Substitution only ever consumes `$`-references, so it
			// can never introduce one.
			if strings.Count(out, "$") > strings.Count(s, "$") {
				t.Fatalf("Substitute(%q) = %q gained a `$`", s, out)
			}
			if !strings.Contains(s, "$") && out != s {
				t.Fatalf("Substitute(%q) = %q changed a `$`-free string", s, out)
			}
		}
	})
}
