package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

func writeProject(t *testing.T) (dir, configPath string) {
	t.Helper()
	dir = t.TempDir()
	write := func(name, content string) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("db/schema.sql", "CREATE TABLE t (id bigint NOT NULL, x text);")
	write("queries/q.sql", `-- name: FindT :many
SELECT t.id FROM t
WHERE TRUE
@if-present(x)
  AND t.x = :x
@endif
@choose(sort)
@case(id_desc)
ORDER BY t.id DESC
@default
ORDER BY t.id ASC
@end
;
`)
	write("sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
`)
	return dir, filepath.Join(dir, "sqletch.yaml")
}

// explain --enumerate is fully offline: scan + render, no database.
func TestExplainEnumerate_Offline(t *testing.T) {
	_, configPath := writeProject(t)
	var out, errW bytes.Buffer
	code := Explain(context.Background(), configPath, nil, ExplainOptions{Enumerate: true}, &out, &errW)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, errW.String())
	}
	got := out.String()
	// 2 guard states x 2 sort cases = 4 shapes.
	if n := strings.Count(got, "-- FindT shape "); n != 4 {
		t.Fatalf("shapes printed = %d, want 4:\n%s", n, got)
	}
	for _, want := range []string{"g=0;c=0", "g=1;c=1", "AND (t.x = $1)", "ORDER BY t.id DESC"} {
		if !strings.Contains(got, want) {
			t.Errorf("enumerate output missing %q:\n%s", want, got)
		}
	}
	if errW.Len() != 0 {
		t.Errorf("an untruncated enumeration must say nothing on stderr:\n%s", errW.String())
	}
}

func TestExplainEnumerate_FilterAndMiss(t *testing.T) {
	_, configPath := writeProject(t)
	var out, errW bytes.Buffer
	if code := Explain(context.Background(), configPath, []string{"FindT"}, ExplainOptions{Enumerate: true}, &out, &errW); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, errW.String())
	}
	if code := Explain(context.Background(), configPath, []string{"Nope"}, ExplainOptions{Enumerate: true}, &out, &errW); code != ExitDiagnostics {
		t.Fatalf("unknown query must exit %d, got %d", ExitDiagnostics, code)
	}
}

// Truncating `explain --enumerate` is a WARNING on stderr, not an SQL
// comment mixed into the shape stream on stdout: the exit code stays 0
// because plain enumeration is an inspection command that never claimed
// completeness, but the notice must not pollute `explain > shapes.sql`.
func TestExplainEnumerate_CapWarnsOnStderr(t *testing.T) {
	_, configPath := writeProject(t)
	var out, errW bytes.Buffer
	code := Explain(context.Background(), configPath, nil,
		ExplainOptions{Enumerate: true, MaxShapes: 2}, &out, &errW)
	if code != ExitOK {
		t.Fatalf("enumerate truncation must stay exit %d, got %d\n%s", ExitOK, code, errW.String())
	}
	if n := strings.Count(out.String(), "-- FindT shape "); n != 2 {
		t.Errorf("shapes printed = %d, want 2 (the cap):\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "truncated") || strings.Contains(out.String(), "SQLETCH") {
		t.Errorf("stdout must carry only shape SQL, no cap notice:\n%s", out.String())
	}
	for _, want := range []string{"warning", string(diagnostics.CodeShapeCapReached), "FindT", "--max-shapes"} {
		if !strings.Contains(errW.String(), want) {
			t.Errorf("cap warning missing %q:\n%s", want, errW.String())
		}
	}
}

// Raising the cap past the reachable shape count clears the warning.
func TestExplainEnumerate_MaxShapesRaisesCap(t *testing.T) {
	_, configPath := writeProject(t)
	var out, errW bytes.Buffer
	code := Explain(context.Background(), configPath, nil,
		ExplainOptions{Enumerate: true, MaxShapes: 100}, &out, &errW)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, errW.String())
	}
	if n := strings.Count(out.String(), "-- FindT shape "); n != 4 {
		t.Errorf("shapes printed = %d, want all 4:\n%s", n, out.String())
	}
	if errW.Len() != 0 {
		t.Errorf("a cap that is never reached must not warn:\n%s", errW.String())
	}
}

func TestExplain_NegativeMaxShapesRejected(t *testing.T) {
	_, configPath := writeProject(t)
	var out, errW bytes.Buffer
	code := Explain(context.Background(), configPath, nil,
		ExplainOptions{Enumerate: true, MaxShapes: -1}, &out, &errW)
	if code != ExitEnvironment {
		t.Fatalf("negative --max-shapes must exit %d, got %d\n%s", ExitEnvironment, code, errW.String())
	}
	if !strings.Contains(errW.String(), "--max-shapes") {
		t.Errorf("rejection must name the flag:\n%s", errW.String())
	}
}

func TestFmt_CheckAndWrite(t *testing.T) {
	dir, configPath := writeProject(t)
	queryPath := filepath.Join(dir, "queries", "q.sql")
	// Introduce a fixable anchor omission and sloppy guard spacing.
	sloppy := `-- name: FindT :many
SELECT t.id FROM t
WHERE
@if-present( x )
  AND t.x = :x
@endif
;
`
	if err := os.WriteFile(queryPath, []byte(sloppy), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	if code := Fmt(configPath, true, &out, &errW); code != ExitDiagnostics {
		t.Fatalf("--check on unformatted file must exit 1, got %d\n%s", code, errW.String())
	}
	if !strings.Contains(out.String(), "q.sql") {
		t.Errorf("--check must list the file: %s", out.String())
	}

	out.Reset()
	if code := Fmt(configPath, false, &out, &errW); code != ExitOK {
		t.Fatalf("fmt exit %d\n%s", code, errW.String())
	}
	formatted, err := os.ReadFile(queryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), "WHERE TRUE\n@if-present(x)") {
		t.Errorf("fmt did not fix the file:\n%s", formatted)
	}

	// Second run is a no-op (fixpoint at the CLI level).
	out.Reset()
	if code := Fmt(configPath, true, &out, &errW); code != ExitOK {
		t.Fatalf("formatted project must pass --check, got %d\n%s", code, out.String())
	}
}
