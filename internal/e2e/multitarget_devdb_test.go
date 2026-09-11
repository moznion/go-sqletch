//go:build devdb

package e2e_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
)

// TestMultiTargetGeneratedModules is the design-19 loop end to end: ONE
// config, one capturing target, two generated packages next to the apps
// that own them — and both compile and run against the real database.
//
// The two apps deliberately define a query with the SAME name: the
// scope of a query name is its target, and nothing downstream (file
// stems, explain data, the shape space) may collide because of it.
// SQLite keeps the test in-process, so it needs no container.
func TestMultiTargetGeneratedModules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dev.sqlite3")
	writeFile(t, dir, "db/schema.sql", `CREATE TABLE users (
    id    INTEGER PRIMARY KEY,
    email TEXT NOT NULL
);
CREATE TABLE orders (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL,
    note    TEXT
);
`)
	writeFile(t, dir, "app/signin/queries/q.sql", `-- name: Lookup :many
-- @param email: text
SELECT u.id, u.email
FROM users AS u
WHERE u.email = :email;
`)
	writeFile(t, dir, "app/orders/queries/q.sql", `-- name: Lookup :many
-- @param user_id: integer
SELECT o.id, o.note
FROM orders AS o
WHERE o.user_id = :user_id;
`)
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: sqlite
server_version: "3"
database:
  dsn: `+dbPath+`
schema:
  files: [db/schema.sql]
targets:
  - queries: ["(app/*)/queries/*.sql"]
    output:
      package: gen
      path: $1/gen
cache:
  path: .sqletch/cache
`)
	configPath := filepath.Join(dir, "sqletch.yaml")

	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, configPath, false, cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("cold generate: exit %d\n%s%s", code, out.String(), errW.String())
	}
	// Each app got its own package, and each package's method carries
	// its own app's parameters — the two same-named queries never met.
	signin := readFile(t, dir, "app/signin/gen/lookup.sql.gen.go")
	orders := readFile(t, dir, "app/orders/gen/lookup.sql.gen.go")
	if !strings.Contains(signin, "Email string") {
		t.Errorf("signin package did not get its own params:\n%s", signin)
	}
	if !strings.Contains(orders, "UserID int64") {
		t.Errorf("orders package did not get its own params:\n%s", orders)
	}
	for _, p := range []string{
		".sqletch/explain/app/signin/gen/Lookup.json",
		".sqletch/explain/app/orders/gen/Lookup.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Errorf("explain data %s: %v", p, err)
		}
	}

	// A warm run with an unusable DSN: the multi-target pipeline is as
	// offline as the single-target one — one cache serves both packages.
	writeFile(t, dir, "sqletch.yaml", strings.Replace(readFile(t, dir, "sqletch.yaml"),
		"dsn: "+dbPath, "dsn: /nonexistent-sqletch-dir/nope.sqlite3", 1))
	out.Reset()
	errW.Reset()
	if code := cli.Check(ctx, configPath, false, false, cli.RunOptions{}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("warm offline check: exit %d\n%s%s", code, out.String(), errW.String())
	}
	if !strings.Contains(out.String(), "offline: yes") {
		t.Errorf("warm check must be offline: %s", out.String())
	}

	// ---- build and run both generated packages --------------------------
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	drvVer := regexp.MustCompile(`github\.com/ncruces/go-sqlite3 (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
	if drvVer == nil {
		t.Fatal("ncruces/go-sqlite3 version not found in parent go.mod")
	}
	optVer := regexp.MustCompile(`github\.com/moznion/go-optional (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
	if optVer == nil {
		t.Fatal("go-optional version not found in parent go.mod")
	}
	goMod := "module sqletchgen\n\ngo 1.24\n\nrequire (\n" +
		"\tgithub.com/moznion/go-optional " + optVer[1] + "\n" +
		"\tgithub.com/ncruces/go-sqlite3 " + drvVer[1] + "\n" +
		"\tgithub.com/moznion/go-sqletch v0.0.0\n)\n\n" +
		"replace github.com/moznion/go-sqletch => " + repoRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	parentSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), parentSum, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(multiTargetE2EMain), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "SQLETCH_TEST_SQLITE="+dbPath)
		cmdOut, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, cmdOut)
		}
		return string(cmdOut)
	}
	run("mod", "tidy")
	if runOut := run("run", "."); !strings.Contains(runOut, "E2E-OK") {
		t.Fatalf("generated modules did not run:\n%s", runOut)
	}
}

// multiTargetE2EMain imports BOTH generated packages — same package
// name, different import paths, which is exactly what a consumer of
// per-app output writes.
const multiTargetE2EMain = `package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"

	orders "sqletchgen/app/orders/gen"
	signin "sqletchgen/app/signin/gen"
)

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}

func main() {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+os.Getenv("SQLETCH_TEST_SQLITE"))
	die(err)
	defer db.Close()

	for _, stmt := range []string{
		"DELETE FROM orders",
		"DELETE FROM users",
		"INSERT INTO users (id, email) VALUES (1, 'a@example.com')",
		"INSERT INTO orders (id, user_id, note) VALUES (10, 1, NULL), (11, 1, 'note')",
	} {
		_, err := db.ExecContext(ctx, stmt)
		die(err)
	}

	users, err := signin.New(db).Lookup(ctx, signin.LookupParams{Email: "a@example.com"})
	die(err)
	if len(users) != 1 || users[0].ID != 1 {
		fmt.Fprintf(os.Stderr, "signin.Lookup = %+v\n", users)
		os.Exit(1)
	}

	rows, err := orders.New(db).Lookup(ctx, orders.LookupParams{UserID: 1})
	die(err)
	if len(rows) != 2 || rows[0].Note.IsSome() {
		fmt.Fprintf(os.Stderr, "orders.Lookup = %+v\n", rows)
		os.Exit(1)
	}

	fmt.Println("E2E-OK")
}
`
