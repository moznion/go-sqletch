package diagnostics

import (
	"strings"
	"testing"
)

func TestLineCol(t *testing.T) {
	src := []byte("abc\ndef\nghi")
	tests := []struct {
		off, line, col int
	}{
		{0, 1, 1},
		{2, 1, 3},
		{3, 1, 4}, // at the newline itself
		{4, 2, 1}, // first byte of line 2
		{8, 3, 1},
		{10, 3, 3},
		{99, 3, 4}, // clamped past EOF
	}
	for _, tt := range tests {
		line, col := LineCol(src, tt.off)
		if line != tt.line || col != tt.col {
			t.Errorf("LineCol(%d) = (%d,%d), want (%d,%d)", tt.off, line, col, tt.line, tt.col)
		}
	}
}

func TestRender(t *testing.T) {
	src := []byte("SELECT 1\nFROM t;")
	d := Errorf(CodeMissingHeader, Span{File: "q.sql", Start: 9, End: 13}, "boom").
		WithHint("add a header")
	got := d.Render(src)
	if !strings.Contains(got, "q.sql:2:1: error[SQLETCH003]: boom") {
		t.Errorf("Render = %q", got)
	}
	if !strings.Contains(got, "help: add a header") {
		t.Errorf("Render missing hint: %q", got)
	}
}

func TestRenderExcerpt(t *testing.T) {
	src := []byte("SELECT 1\nFROM users AS u\nWHERE bad_col = 1;\n")
	start := strings.Index(string(src), "bad_col")
	d := Errorf(CodeScopeViolation, Span{File: "q.sql", Start: start, End: start + len("bad_col")}, "boom").
		WithHint("fix it")
	got := d.RenderExcerpt(src)
	want := "q.sql:3:7: error[SQLETCH115]: boom\n" +
		"  |\n" +
		"3 | WHERE bad_col = 1;\n" +
		"  |       ^^^^^^^\n" +
		"help: fix it"
	if got != want {
		t.Errorf("RenderExcerpt:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderExcerpt_MultibyteAlignment(t *testing.T) {
	src := []byte("SELECT 1 -- 日本語\nWHERE x = 1;\n")
	start := strings.Index(string(src), "x = 1")
	d := Errorf(CodeBadIdentifier, Span{File: "q.sql", Start: start, End: start + 1}, "m")
	got := d.RenderExcerpt(src)
	// The caret line must align in runes: "WHERE " = 6 runes before x.
	if !strings.Contains(got, "2 | WHERE x = 1;\n  |       ^") {
		t.Errorf("multibyte-safe caret misaligned:\n%s", got)
	}
}

func TestRenderExcerpt_FallbackWithoutSource(t *testing.T) {
	d := Errorf(CodeMissingHeader, Span{File: "q.sql", Start: 5, End: 6}, "m")
	if got := d.RenderExcerpt(nil); !strings.Contains(got, "SQLETCH003") {
		t.Errorf("fallback rendering: %q", got)
	}
}

func TestSortAndHasErrors(t *testing.T) {
	diags := []Diagnostic{
		Errorf(CodePositionalParam, Span{File: "b.sql", Start: 5}, "x"),
		Errorf(CodeMissingHeader, Span{File: "a.sql", Start: 9}, "y"),
		Errorf(CodeBadIdentifier, Span{File: "a.sql", Start: 3}, "z"),
	}
	Sort(diags)
	wantOrder := []Code{CodeBadIdentifier, CodeMissingHeader, CodePositionalParam}
	for i, w := range wantOrder {
		if diags[i].Code != w {
			t.Errorf("sorted[%d] = %s, want %s", i, diags[i].Code, w)
		}
	}
	if !HasErrors(diags) {
		t.Error("HasErrors = false, want true")
	}
	if HasErrors(nil) {
		t.Error("HasErrors(nil) = true, want false")
	}
	warn := []Diagnostic{{Code: CodeChooseStructure, Severity: Warning}}
	if HasErrors(warn) {
		t.Error("HasErrors(warning-only) = true, want false")
	}
}
