package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// The `--json` stream is a machine contract consumed by editors, so its
// shape is pinned here field for field: one JSON object per line, these
// seven keys and no others. Changing it is a breaking change and must
// break this test.
var jsonDiagKeys = []string{"code", "col", "file", "hint", "line", "message", "severity"}

// decodeJSONLines parses the emitted stream and fails the test on any
// line that is not a well-formed diagnostic object.
func decodeJSONLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	if strings.TrimSpace(s) == "" {
		t.Fatal("no output to decode")
	}
	var got []map[string]any
	for i, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not valid JSON (%v): %q", i, err, line)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != strings.Join(jsonDiagKeys, ",") {
			t.Fatalf("line %d key set = %v, want %v", i, keys, jsonDiagKeys)
		}
		got = append(got, m)
	}
	return got
}

func TestPrintDiags_JSONLines(t *testing.T) {
	src := []byte("SELECT 1\n  AND t.x = :x\n")
	// Deliberately out of order: PrintDiags must sort by (file, offset).
	second := diagnostics.Errorf(diagnostics.CodeVacuousGuard,
		diagnostics.Span{File: "q.sql", Start: 15, End: 17}, "guard %q is vacuous", "x").
		WithHint("drop the guard")
	first := diagnostics.Warnf(diagnostics.CodeOptionalInsertNotNull,
		diagnostics.Span{File: "q.sql", Start: 0, End: 6}, "column has no default")

	var buf bytes.Buffer
	res := &Result{
		Diags:   []diagnostics.Diagnostic{second, first},
		Sources: map[string][]byte{"q.sql": src},
	}
	PrintDiags(&buf, res, true)

	got := decodeJSONLines(t, buf.String())
	if len(got) != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", len(got), buf.String())
	}

	// Line 0: the warning at offset 0 sorts first and carries no hint.
	want0 := map[string]any{
		"code": "SQLETCH212", "severity": "warning", "file": "q.sql",
		"line": 1.0, "col": 1.0, "message": "column has no default", "hint": "",
	}
	for k, want := range want0 {
		if got[0][k] != want {
			t.Errorf("line 0 %s = %#v, want %#v", k, got[0][k], want)
		}
	}

	// Line 1: the error at offset 15 is on line 2, column 7, with a hint.
	want1 := map[string]any{
		"code": "SQLETCH110", "severity": "error", "file": "q.sql",
		"line": 2.0, "col": 7.0, "message": `guard "x" is vacuous`, "hint": "drop the guard",
	}
	for k, want := range want1 {
		if got[1][k] != want {
			t.Errorf("line 1 %s = %#v, want %#v", k, got[1][k], want)
		}
	}
}

// A diagnostic attached to a file that is not a scanned template (the
// config file, for SQLETCH200) still has to produce a well-formed line
// rather than a panic or a truncated object.
func TestPrintDiags_JSONSourceNotLoaded(t *testing.T) {
	d := diagnostics.Errorf(diagnostics.CodeServerVersionMismatch,
		diagnostics.Span{File: "sqletch.yaml"}, "server version mismatch").
		WithHint(`set server_version: "17"`)

	var buf bytes.Buffer
	// Sources is nil here, exactly as explainAnalyze constructs it.
	PrintDiags(&buf, &Result{Diags: []diagnostics.Diagnostic{d}}, true)

	got := decodeJSONLines(t, buf.String())
	if len(got) != 1 {
		t.Fatalf("lines = %d, want 1", len(got))
	}
	if got[0]["file"] != "sqletch.yaml" || got[0]["line"] != 1.0 || got[0]["col"] != 1.0 {
		t.Errorf("unloaded source must degrade to 1:1, got %#v", got[0])
	}
	if got[0]["code"] != "SQLETCH200" {
		t.Errorf("code = %v", got[0]["code"])
	}
}

// `col` counts BYTES, not runes or UTF-16 units. The excerpt renderer
// aligns its caret in runes and the LSP converts to UTF-16 code units;
// this pins which of the three the JSON stream speaks so a change is
// never silent.
func TestPrintDiags_JSONColumnIsBytes(t *testing.T) {
	src := []byte("-- 日本語\nSELECT 1\n")
	d := diagnostics.Errorf(diagnostics.CodeBadIdentifier,
		diagnostics.Span{File: "q.sql", Start: 12, End: 13}, "bad name")

	var buf bytes.Buffer
	PrintDiags(&buf, &Result{
		Diags:   []diagnostics.Diagnostic{d},
		Sources: map[string][]byte{"q.sql": src},
	}, true)

	got := decodeJSONLines(t, buf.String())
	// "-- 日本語\n" is 13 bytes; offset 12 is the last byte of that line.
	if got[0]["line"] != 1.0 || got[0]["col"] != 13.0 {
		t.Errorf("line/col = %v/%v, want 1/13 (byte columns)", got[0]["line"], got[0]["col"])
	}
}

// End to end through the command: a scan-phase mistake never reaches the
// database, so this exercises the real --json wiring with no DB.
func TestCheck_JSONDiagnostics(t *testing.T) {
	dir, configPath := writeProject(t)
	if err := os.WriteFile(filepath.Join(dir, "queries", "q.sql"), []byte(
		"-- name: Unterminated :many\nSELECT t.id FROM t\nWHERE TRUE\n@if-present(x)\n  AND t.x = :x\n;\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	code := Check(context.Background(), configPath, false, true, RunOptions{}, &out, &errW)
	if code != ExitDiagnostics {
		t.Fatalf("exit %d, want %d\nstderr: %s", code, ExitDiagnostics, errW.String())
	}
	got := decodeJSONLines(t, errW.String())
	var codes []string
	for _, m := range got {
		codes = append(codes, m["code"].(string))
		if f, _ := m["file"].(string); !strings.HasSuffix(f, "q.sql") {
			t.Errorf("file = %q, want the template path", f)
		}
	}
	if !strings.Contains(strings.Join(codes, ","), "SQLETCH001") {
		t.Errorf("codes = %v, want an unterminated-construct diagnostic", codes)
	}

	// The same run without the flag must NOT be JSON — proves the flag is
	// actually threaded through rather than the format being unconditional.
	errW.Reset()
	if code := Check(context.Background(), configPath, false, false, RunOptions{}, &out, &errW); code != ExitDiagnostics {
		t.Fatalf("text mode exit %d", code)
	}
	if json.Valid([]byte(strings.SplitN(errW.String(), "\n", 2)[0])) {
		t.Errorf("text mode emitted JSON: %s", errW.String())
	}
}

// Config-load failures print through printBare, a separate call site
// that has to honour the flag too.
func TestCheck_JSONConfigDiagnostics(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sqletch.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\ndialect: nosuchdb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	if code := Check(context.Background(), configPath, false, true, RunOptions{}, &out, &errW); code != ExitDiagnostics {
		t.Fatalf("exit %d, want %d\n%s", code, ExitDiagnostics, errW.String())
	}
	got := decodeJSONLines(t, errW.String())
	for _, m := range got {
		if c, _ := m["code"].(string); !strings.HasPrefix(c, "SQLETCH3") {
			t.Errorf("config diagnostic code = %q, want a 3xx code", c)
		}
	}
}

// Generate shares runPipeline with Check; the flag has to survive that
// path as well.
func TestGenerate_JSONDiagnostics(t *testing.T) {
	dir, configPath := writeProject(t)
	if err := os.WriteFile(filepath.Join(dir, "queries", "q.sql"), []byte(
		"-- name: Dup :one\nSELECT t.id FROM t WHERE t.id = :id;\n"+
			"-- name: Dup :one\nSELECT t.id FROM t WHERE t.id = :id;\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	if code := Generate(context.Background(), configPath, true, RunOptions{}, &out, &errW); code != ExitDiagnostics {
		t.Fatalf("exit %d, want %d\n%s", code, ExitDiagnostics, errW.String())
	}
	got := decodeJSONLines(t, errW.String())
	if got[0]["code"] != string(diagnostics.CodeDuplicateQueryName) {
		t.Errorf("code = %v, want %s", got[0]["code"], diagnostics.CodeDuplicateQueryName)
	}
}

// A version-pin mismatch is a diagnostic, not an environment failure.
// Run() returning it as a diagnostic is covered in devdbconfig_test.go;
// what matters here is the command layer on top: exit 1 (not 2), and a
// well-formed --json line so editors see it. SQLite runs in process, so
// this needs no container.
func TestCheck_VersionPinMismatchExitCodeAndJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "1.0", "dev.sqlite3")

	var out, errW bytes.Buffer
	code := Check(context.Background(), cfgPath, false, false, RunOptions{}, &out, &errW)
	if code != ExitDiagnostics {
		t.Fatalf("exit %d, want %d (a bad pin is a user mistake, not an environment failure)\n%s",
			code, ExitDiagnostics, errW.String())
	}
	if !strings.Contains(errW.String(), string(diagnostics.CodeServerVersionMismatch)) {
		t.Errorf("text output missing the code: %s", errW.String())
	}

	errW.Reset()
	if code := Check(context.Background(), cfgPath, false, true, RunOptions{}, &out, &errW); code != ExitDiagnostics {
		t.Fatalf("json mode exit %d", code)
	}
	got := decodeJSONLines(t, errW.String())
	if len(got) != 1 {
		t.Fatalf("lines = %d, want 1:\n%s", len(got), errW.String())
	}
	if got[0]["code"] != string(diagnostics.CodeServerVersionMismatch) {
		t.Errorf("code = %v, want %s", got[0]["code"], diagnostics.CodeServerVersionMismatch)
	}
	if got[0]["file"] != cfgPath {
		t.Errorf("file = %v, want the config path %q", got[0]["file"], cfgPath)
	}
	if h, _ := got[0]["hint"].(string); !strings.Contains(h, "server_version") {
		t.Errorf("hint must spell the fix, got %q", h)
	}
}
