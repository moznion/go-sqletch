//go:build devdb

package e2e_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
)

// TestGoSourceInputEquivalence is the contract of docs/design/13:
// authoring a template in a `//sqletch:query` const inside a .go file
// is the same compilation as authoring it in a .sql file. Run through
// the real oracle (SQLite in-process, no Docker), the two forms must
// produce byte-identical generated code AND byte-identical cache
// entries — the latter meaning a committed cache stays valid when a
// query moves between forms.
func TestGoSourceInputEquivalence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// A real template with a guard, a param annotation and an @in list.
	tmpl := sqliteCorpus["search_users"] + "\n" + sqliteCorpus["in_list"] + "\n"
	if strings.Contains(tmpl, "`") {
		t.Fatal("corpus template contains a backquote and cannot be embedded in a Go raw literal")
	}

	setup := func(queriesGlob string) string {
		dir := t.TempDir()
		writeFile(t, dir, "db/schema.sql", sqliteSchemaSQL)
		writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: sqlite
server_version: "3"
database:
  dsn: `+filepath.Join(dir, "dev.sqlite3")+`
schema:
  files: [db/schema.sql]
queries: [`+queriesGlob+`]
output:
  package: gen
  path: gen
cache:
  path: .sqletch/cache
`)
		return dir
	}

	sqlDir := setup("queries/*.sql")
	writeFile(t, sqlDir, "queries/users.sql", tmpl)

	goDir := setup("repo/*.go")
	writeFile(t, goDir, "repo/users.go", "package repo\n\n//sqletch:query\nconst usersSQL = `\n"+tmpl+"`\n\n"+
		// Go code around the literal must be invisible to the scanner,
		// including a header-shaped string that would otherwise scan.
		"func Unrelated() string { return \"-- name: NotAQuery :one\" }\n")

	for _, dir := range []string{sqlDir, goDir} {
		var out, errW bytes.Buffer
		if code := cli.Generate(ctx, filepath.Join(dir, "sqletch.yaml"), false, &out, &errW); code != cli.ExitOK {
			t.Fatalf("generate in %s: exit %d\n%s%s", dir, code, out.String(), errW.String())
		}
		if !strings.Contains(out.String(), "offline: no") {
			t.Errorf("cold generate must not be offline: %s", out.String())
		}
	}

	assertTreesEqual(t, filepath.Join(sqlDir, "gen"), filepath.Join(goDir, "gen"))
	assertTreesEqual(t, filepath.Join(sqlDir, ".sqletch", "cache"), filepath.Join(goDir, ".sqletch", "cache"))

	// The Go-authored project must go offline on the warm cache just
	// like the .sql one: point the DSN at nothing and re-check.
	writeFile(t, goDir, "sqletch.yaml",
		strings.Replace(readFile(t, goDir, "sqletch.yaml"),
			filepath.Join(goDir, "dev.sqlite3"), "/nonexistent-sqletch-dir/nope.sqlite3", 1))
	var out, errW bytes.Buffer
	if code := cli.Check(ctx, filepath.Join(goDir, "sqletch.yaml"), false, false, &out, &errW); code != cli.ExitOK {
		t.Fatalf("warm offline check: exit %d\n%s%s", code, out.String(), errW.String())
	}
	if !strings.Contains(out.String(), "offline: yes") {
		t.Errorf("warm check on a Go-authored project must report offline: %s", out.String())
	}
}

// assertTreesEqual compares two directory trees byte for byte.
func assertTreesEqual(t *testing.T, a, b string) {
	t.Helper()
	listing := func(root string) map[string][]byte {
		files := map[string][]byte{}
		if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[rel] = content
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
		return files
	}
	fa, fb := listing(a), listing(b)
	if len(fa) == 0 {
		t.Fatalf("%s is empty; nothing was compared", a)
	}
	for name, want := range fa {
		got, ok := fb[name]
		if !ok {
			t.Errorf("%s missing from %s", name, b)
			continue
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s differs between input forms:\n--- %s\n%s\n--- %s\n%s", name, a, want, b, got)
		}
	}
	for name := range fb {
		if _, ok := fa[name]; !ok {
			t.Errorf("%s present only in %s", name, b)
		}
	}
}
