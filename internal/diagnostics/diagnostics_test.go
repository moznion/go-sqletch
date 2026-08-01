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
