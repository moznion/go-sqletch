package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// Woven skeleton text exists in no template file, so the source map
// must treat it like any other synthesized emission: every rendered
// offset inside it maps synth=true, anchored at the insertion offset.
// (Regression: woven items carried a zero-width span but rendered
// through the verbatim path, which emitted NON-synth segments covering
// len(text) bytes — ToTemplate then pointed diagnostics landing inside
// woven text at unrelated template bytes AFTER the insertion point.)
func TestWeave_WovenTextMapsSynthAtInsertionPoint(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n"
	orig := scanOne(t, src)
	insertAt := orig.WhereKwEnd // the WHERE-path insertion offset

	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	r, err := ast.Render(postgres.Profile{}, res.Query, nil)
	if err != nil {
		t.Fatal(err)
	}

	woven := "(o.tenant_id = $1) AND"
	idx := strings.Index(r.SQL, woven)
	if idx < 0 {
		t.Fatalf("woven conjunct not in rendering:\n%s", r.SQL)
	}
	for off := idx; off < idx+len(woven); off++ {
		tOff, synth := r.Map.ToTemplate(off)
		if !synth {
			t.Fatalf("offset %d (%q) inside woven text maps NON-synth to template offset %d (%q)",
				off, r.SQL[off:off+1], tOff, src[tOff:tOff+1])
		}
		if tOff != insertAt {
			t.Fatalf("offset %d inside woven text anchors at %d, want the insertion offset %d",
				off, tOff, insertAt)
		}
	}

	// The author's own text around the insertion still maps verbatim.
	for _, marker := range []string{"FROM orders o", "o.status ="} {
		rIdx := strings.Index(r.SQL, marker)
		if rIdx < 0 {
			t.Fatalf("marker %q not in rendering:\n%s", marker, r.SQL)
		}
		tOff, synth := r.Map.ToTemplate(rIdx)
		if synth {
			t.Errorf("author text %q wrongly maps as synthesized", marker)
			continue
		}
		if got := src[tOff : tOff+len(marker)]; got != marker {
			t.Errorf("author text %q maps to template bytes %q", marker, got)
		}
	}
}

// The same discipline on the ON-weave path, where the woven text sits
// mid-statement with author text on both sides.
func TestWeaveON_WovenTextMapsSynthAtInsertionPoint(t *testing.T) {
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	r, err := ast.Render(postgres.Profile{}, res.Query, nil)
	if err != nil {
		t.Fatal(err)
	}

	woven := "AND (o.tenant_id = $1)"
	idx := strings.Index(r.SQL, woven)
	if idx < 0 {
		t.Fatalf("woven ON conjunct not in rendering:\n%s", r.SQL)
	}
	insertAt := strings.Index(src, " WHERE u.ok") // ON-clause end = insertion offset
	if insertAt < 0 {
		t.Fatal("marker not in src")
	}
	for off := idx; off < idx+len(woven); off++ {
		tOff, synth := r.Map.ToTemplate(off)
		if !synth {
			t.Fatalf("offset %d inside woven ON text maps NON-synth to %d", off, tOff)
		}
		if tOff != insertAt {
			t.Fatalf("offset %d inside woven ON text anchors at %d, want %d", off, tOff, insertAt)
		}
	}
	// Author text after the insertion (the WHERE clause) still maps
	// verbatim — this is exactly the region the bug mis-attributed
	// woven text to.
	rIdx := strings.Index(r.SQL, "u.ok")
	tOff, synth := r.Map.ToTemplate(rIdx)
	if synth {
		t.Fatal("author text after the insertion wrongly maps as synthesized")
	}
	if got := src[tOff : tOff+len("u.ok")]; got != "u.ok" {
		t.Errorf("author text after the insertion maps to %q", got)
	}
}
