package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	code := Explain(context.Background(), configPath, nil, true, false, &out, &errW)
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
}

func TestExplainEnumerate_FilterAndMiss(t *testing.T) {
	_, configPath := writeProject(t)
	var out, errW bytes.Buffer
	if code := Explain(context.Background(), configPath, []string{"FindT"}, true, false, &out, &errW); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, errW.String())
	}
	if code := Explain(context.Background(), configPath, []string{"Nope"}, true, false, &out, &errW); code != ExitDiagnostics {
		t.Fatalf("unknown query must exit %d, got %d", ExitDiagnostics, code)
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
