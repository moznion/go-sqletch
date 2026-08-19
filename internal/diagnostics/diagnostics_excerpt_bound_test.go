package diagnostics

import (
	"strings"
	"testing"
)

// A diagnostic on a pathologically long single line must render a
// bounded excerpt: the old code copied the whole line and rune-counted
// the whole prefix per diagnostic, so a 1 MB one-liner rendered across
// many diagnostics produced gigabytes. The windowed excerpt keeps the
// output small while still pointing the caret at the span.
func TestRenderExcerpt_LongLineIsWindowed(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 500000))
	b.WriteString("BADTOKEN")
	b.WriteString(strings.Repeat("y", 500000))
	src := []byte(b.String())
	start := strings.Index(string(src), "BADTOKEN")

	d := Errorf(CodeBadIdentifier, Span{File: "q.sql", Start: start, End: start + len("BADTOKEN")}, "boom").
		WithHint("fix it")
	got := d.RenderExcerpt(src)

	// Excerpt must be tiny relative to the 1 MB line.
	if len(got) > 4000 {
		t.Fatalf("excerpt is %d bytes; expected a bounded window, not the whole line", len(got))
	}
	// Ellipses mark the truncation on both sides.
	if !strings.Contains(got, "…") {
		t.Errorf("windowed excerpt should carry an ellipsis:\n%s", got)
	}
	// The span text and caret must survive.
	if !strings.Contains(got, "BADTOKEN") {
		t.Errorf("windowed excerpt dropped the span text:\n%s", got)
	}
	if !strings.Contains(got, "^^^^^^^^") {
		t.Errorf("windowed excerpt dropped the caret:\n%s", got)
	}
}

// A span covering (most of) a huge line must not produce a caret as long
// as the line — the caret is truncated to a bounded width.
func TestRenderExcerpt_LongSpanCaretBounded(t *testing.T) {
	line := strings.Repeat("a", 100000)
	src := []byte("SELECT 1\n" + line + "\n")
	start := strings.Index(string(src), line)
	d := Errorf(CodeBadIdentifier, Span{File: "q.sql", Start: start, End: start + len(line)}, "m")
	got := d.RenderExcerpt(src)
	if strings.Count(got, "^") > excerptMaxCaretRunes {
		t.Fatalf("caret has %d '^', want <= %d", strings.Count(got, "^"), excerptMaxCaretRunes)
	}
	if len(got) > 4000 {
		t.Fatalf("excerpt is %d bytes; expected bounded output", len(got))
	}
}

// Lines within the full-render threshold stay byte-identical to the old
// behaviour (the common case must not change).
func TestRenderExcerpt_ShortLineUnchanged(t *testing.T) {
	src := []byte("SELECT 1\nFROM users AS u\nWHERE bad_col = 1;\n")
	start := strings.Index(string(src), "bad_col")
	d := Errorf(CodeScopeViolation, Span{File: "q.sql", Start: start, End: start + len("bad_col")}, "boom").
		WithHint("fix it")
	want := "q.sql:3:7: error[SQLETCH115]: boom\n" +
		"  |\n" +
		"3 | WHERE bad_col = 1;\n" +
		"  |       ^^^^^^^\n" +
		"help: fix it"
	if got := d.RenderExcerpt(src); got != want {
		t.Errorf("short-line excerpt changed:\n%q\nwant:\n%q", got, want)
	}
}
