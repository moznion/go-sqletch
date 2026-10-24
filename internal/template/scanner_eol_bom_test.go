package template

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// A leading UTF-8 BOM must be treated as editor byte-order noise, not
// stray content: previously it lexed as an identifier and tripped
// SQLETCH003 even though the query is valid.
func TestScan_LeadingUTF8BOM(t *testing.T) {
	src := "\xef\xbb\xbf-- name: A :one\nSELECT 1;"
	f, diags := scan(t, src)
	if len(f.Queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(f.Queries))
	}
	if hasCode(diags, diagnostics.CodeMissingHeader) {
		t.Fatalf("spurious SQLETCH003 for a BOM-prefixed valid file:\n%s", renderAll(diags, src))
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics:\n%s", renderAll(diags, src))
	}
	// Spans stay indexed to the ORIGINAL bytes (BOM not stripped), so the
	// header name still resolves after the 3 BOM bytes.
	if f.Queries[0].Name != "A" {
		t.Fatalf("name = %q, want A", f.Queries[0].Name)
	}
}

// Only a SINGLE leading BOM is skipped: a second BOM is ordinary
// (non-trivia) content before the header and still trips SQLETCH003.
func TestScan_BOMOnlyLeadingSkipped(t *testing.T) {
	src := "\xef\xbb\xbf\xef\xbb\xbf-- name: A :one\nSELECT 1;"
	_, diags := scan(t, src)
	if !hasCode(diags, diagnostics.CodeMissingHeader) {
		t.Fatalf("expected SQLETCH003 for a second (non-leading) BOM; got:\n%s", renderAll(diags, src))
	}
}
