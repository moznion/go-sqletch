//go:build devdb

package e2e_test

// PostgreSQL enum types (design 26). The driver-boundary tests pin the
// pgx behavior the generated code relies on BEFORE codegen relies on it
// (design 26 §3/§6): pgx has no codec for an unregistered enum OID and
// must fall back to the Go value's own codec — through the underlying
// type for a named `type T string` — on both bind and scan, with NULLs
// and with a label added after the statement was prepared. (Codec
// planning is per connection, so a pool changes nothing here; the
// generated-module run goes through pgxpool anyway.)

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

const enumSchemaSQL = `
CREATE TYPE event_kind AS ENUM ('STARTED', 'ANSWERED', 'SKIPPED');
-- sort order differs from creation order: 'QUEUED' is created last but
-- sorts first, so a label list read in creation (OID) order is wrong.
ALTER TYPE event_kind ADD VALUE 'QUEUED' BEFORE 'STARTED';
CREATE TABLE ev (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind  event_kind NOT NULL,
    maybe event_kind
);
`

// eventKind is what generated code declares for an enum: a named string.
type eventKind string

func TestPgEnumDriverBoundary(t *testing.T) {
	conn, ctx := acquireWithSchema(t, enumSchemaSQL)
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeDescribeExec} {
		t.Run(mode.String(), func(t *testing.T) {
			// pgx takes the exec mode as a leading argument.
			qr := func(sql string, args ...any) pgx.Row { return conn.QueryRow(ctx, sql, append([]any{mode}, args...)...) }
			insert := `INSERT INTO ev (kind, maybe) VALUES ($1, $2) RETURNING id`
			sel := `SELECT kind, maybe FROM ev WHERE id = $1`

			// Bind: plain string, named string, *named, nil *named
			// (the generated bind shape for Option/Omittable is *T).
			answered := eventKind("ANSWERED")
			var id int64
			if err := qr(insert, "STARTED", &answered).Scan(&id); err != nil {
				t.Fatalf("bind string + *T: %v", err)
			}
			var kindS string
			var maybeT *eventKind // generated scan: *T temporary
			if err := qr(sel, id).Scan(&kindS, &maybeT); err != nil {
				t.Fatalf("scan *string + **T: %v", err)
			}
			if kindS != "STARTED" || maybeT == nil || *maybeT != "ANSWERED" {
				t.Fatalf("round trip = %q, %v", kindS, maybeT)
			}

			var nilKind *eventKind
			if err := qr(insert, eventKind("SKIPPED"), nilKind).Scan(&id); err != nil {
				t.Fatalf("bind T + nil *T: %v", err)
			}
			var kindT eventKind
			maybeT = &answered // must be overwritten with nil
			if err := qr(sel, id).Scan(&kindT, &maybeT); err != nil {
				t.Fatalf("scan *T + **T(NULL): %v", err)
			}
			if kindT != "SKIPPED" || maybeT != nil {
				t.Fatalf("NULL round trip = %q, %v", kindT, maybeT)
			}

			// An unknown label is the SERVER's refusal (22P02); the Go
			// type does not close the value set (design 26 §3).
			_, err := conn.Exec(ctx, insert, mode, eventKind("typo"), nilKind)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
				t.Fatalf("unknown label: err = %v, want SQLSTATE 22P02", err)
			}
		})
	}

	// A label added AFTER the statement was prepared (and cached by
	// pgx) binds and scans: why D3 generates no Valid().
	insert := `INSERT INTO ev (kind) VALUES ($1) RETURNING id`
	var id int64
	if err := conn.QueryRow(ctx, insert, eventKind("STARTED")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `ALTER TYPE event_kind ADD VALUE 'ABANDONED'`); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, insert, eventKind("ABANDONED")).Scan(&id); err != nil {
		t.Fatalf("label added after prepare: %v", err)
	}
	var got eventKind
	if err := conn.QueryRow(ctx, `SELECT kind FROM ev WHERE id = $1`, id).Scan(&got); err != nil || got != "ABANDONED" {
		t.Fatalf("scan label added after prepare = %q, %v", got, err)
	}
}

// TestPgEnumDescribe pins design 26 §4.1: Describe attaches Ext to every
// user-defined type (enum → labels in enumsortorder order; an array of
// an enum is user-defined but kind "b") and to no built-in.
func TestPgEnumDescribe(t *testing.T) {
	conn, ctx := acquireWithSchema(t, enumSchemaSQL)
	oracle := postgres.NewOracle(conn)

	desc, err := oracle.Describe(ctx,
		`SELECT id, kind, maybe, ARRAY[kind] AS kinds FROM ev WHERE kind = $1 AND id > $2`)
	if err != nil {
		t.Fatal(err)
	}
	wantLabels := []string{"QUEUED", "STARTED", "ANSWERED", "SKIPPED"}
	if len(desc.Params) != 2 || len(desc.Columns) != 4 {
		t.Fatalf("desc = %+v", desc)
	}

	p := desc.Params[0]
	if p.Name != "event_kind" || p.Ext == nil || p.Ext.Kind != "e" || p.Ext.Schema != "public" ||
		!slices.Equal(p.Ext.Labels, wantLabels) {
		t.Errorf("enum param = %+v (ext %+v)", p, p.Ext)
	}
	for _, i := range []int{1, 2} {
		c := desc.Columns[i].Type
		if c.Ext == nil || c.Ext.Kind != "e" || !slices.Equal(c.Ext.Labels, wantLabels) {
			t.Errorf("enum column %s = %+v (ext %+v)", desc.Columns[i].Name, c, c.Ext)
		}
	}
	arr := desc.Columns[3].Type
	if arr.Ext == nil || arr.Ext.Kind != "b" || len(arr.Ext.Labels) != 0 {
		t.Errorf("enum array column = %+v (ext %+v); want user-defined kind b, no labels", arr, arr.Ext)
	}
	if desc.Params[1].Ext != nil || desc.Columns[0].Type.Ext != nil {
		t.Errorf("built-ins must carry no Ext: %+v / %+v", desc.Params[1], desc.Columns[0].Type)
	}
}

const enumProjectSchema = `CREATE TYPE event_kind AS ENUM ('STARTED', 'ANSWERED', 'SKIPPED');
ALTER TYPE event_kind ADD VALUE 'QUEUED' BEFORE 'STARTED';
CREATE TYPE mood AS ENUM ('ok', 'in-progress', '済み');
CREATE TABLE ev (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind  event_kind NOT NULL,
    maybe event_kind,
    mood  mood,
    note  text
);
`

const enumProjectQueries = `-- name: InsertEvent :one
INSERT INTO ev (kind, maybe, mood, note)
VALUES (:kind, :maybe, :mood, :note)
RETURNING id;

-- name: ListEvents :many
SELECT id, kind, maybe, mood
FROM ev
WHERE TRUE
@if-present(kind)
  AND kind = :kind
@endif
ORDER BY id
LIMIT :limit;

-- name: LegacyInsert :exec
INSERT INTO ev (kind) VALUES (CAST(:kind AS text)::event_kind);
`

// TestPgEnumGeneratedModule is design 26 end to end through the real
// CLI: a cold generate types enum params/columns as generated named
// types, the committed cache carries the enum metadata (a warm run is
// offline), a pre-design entry is re-described rather than misread
// (D4), and the generated package binds and scans enums — NULLs,
// multibyte labels, a label added after generation, the server's 22P02
// — through pgxpool.
func TestPgEnumGeneratedModule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_DSN"),
		ServerVersion: "16",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", enumProjectSchema)
	writeFile(t, dir, "queries/events.sql", enumProjectQueries)
	config := func(dsn string) string {
		return `version: 1
dialect: postgres
server_version: "16"
database:
  dsn: ` + dsn + `
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
cache:
  path: .sqletch/cache
`
	}
	configPath := filepath.Join(dir, "sqletch.yaml")
	generate := func() string {
		t.Helper()
		var out, errW bytes.Buffer
		if code := cli.Generate(ctx, configPath, false, cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
			t.Fatalf("generate: exit %d\n%s%s", code, out.String(), errW.String())
		}
		return out.String() + errW.String()
	}
	offlineCheck := func() (int, string) {
		t.Helper()
		writeFile(t, dir, "sqletch.yaml", config("postgres://nobody@127.0.0.1:1/none?connect_timeout=1"))
		defer writeFile(t, dir, "sqletch.yaml", config(dsn))
		var out, errW bytes.Buffer
		code := cli.Check(ctx, configPath, false, false, cli.RunOptions{}, &out, &errW)
		return code, out.String() + errW.String()
	}

	// 1. Cold generate: no cast anywhere, and no SQLETCH311.
	writeFile(t, dir, "sqletch.yaml", config(dsn))
	if out := generate(); strings.Contains(out, "SQLETCH") {
		t.Fatalf("cold generate reported diagnostics:\n%s", out)
	}
	assertPatternsIn(t, readFile(t, dir, "gen/insert_event.sql.gen.go"),
		`Kind\s+EventKind\b`, `Maybe\s+optional\.Option\[EventKind\]`, `Mood\s+optional\.Option\[Mood\]`)
	assertPatternsIn(t, readFile(t, dir, "gen/list_events.sql.gen.go"),
		`Kind\s+sqletch\.Omittable\[EventKind\]`, `Kind\s+EventKind\b`, `Mood\s+optional\.Option\[Mood\]`)
	assertPatternsIn(t, readFile(t, dir, "gen/legacy_insert.sql.gen.go"), `Kind\s+string\b`) // the cast keeps working
	assertPatternsIn(t, readFile(t, dir, "gen/enums.gen.go"),
		`EventKindQueued\s+EventKind = "QUEUED"\n\tEventKindStarted`, // enumsortorder
		`MoodInProgress\s+Mood = "in-progress"`, `Mood済み\s+Mood = "済み"`)

	// 2. The committed cache carries the enum metadata: warm is offline.
	if code, out := offlineCheck(); code != cli.ExitOK || !strings.Contains(out, "offline: yes") {
		t.Fatalf("warm check must pass offline: exit %d\n%s", code, out)
	}

	// 3. Build and run the generated package against the database.
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var req strings.Builder
	for _, mod := range []string{"github.com/moznion/go-optional", "github.com/jackc/pgx/v5"} {
		m := regexp.MustCompile(regexp.QuoteMeta(mod) + ` (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
		if m == nil {
			t.Fatalf("%s version not found in parent go.mod", mod)
		}
		req.WriteString("\t" + mod + " " + m[1] + "\n")
	}
	writeFile(t, dir, "go.mod", "module sqletchgen\n\ngo 1.24\n\nrequire (\n"+req.String()+
		"\tgithub.com/moznion/go-sqletch v0.0.0\n)\n\n"+
		"replace github.com/moznion/go-sqletch => "+repoRoot+"\n")
	parentSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "go.sum", string(parentSum))
	writeFile(t, dir, "main.go", enumE2EMain)
	run := func(args ...string) string {
		c := exec.CommandContext(ctx, "go", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "SQLETCH_ENUM_DSN="+dsn)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	run("mod", "tidy")
	if got := run("run", "."); !strings.Contains(got, "ENUM-OK") {
		t.Fatalf("generated module did not run:\n%s", got)
	}

	// 4. D4: strip the metadata (= an entry written before design 26).
	// It must be a MISS — offline the run needs the database it cannot
	// reach — never a hit misread as an unmapped type (SQLETCH311).
	oracleDir := filepath.Join(dir, ".sqletch/cache/oracle")
	original := map[string]string{}
	strip := regexp.MustCompile(`,\s*"(type_)?(schema|kind)": "[^"]*"|,\s*"(type_)?labels": \[[^\]]*\]`)
	err = filepath.WalkDir(oracleDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		original[p] = string(b)
		return os.WriteFile(p, []byte(strip.ReplaceAllString(string(b), "")), 0o644)
	})
	if err != nil || len(original) == 0 {
		t.Fatalf("walk oracle cache: %v (%d entries)", err, len(original))
	}
	if code, out := offlineCheck(); code == cli.ExitOK || strings.Contains(out, "SQLETCH311") {
		t.Fatalf("a pre-design entry must miss (needing the DB), not hit: exit %d\n%s", code, out)
	}
	// Connected: re-described and rewritten with the metadata. (Last
	// on purpose: the reconnect resets the schema, so the rewritten
	// entries carry new OIDs while the cached catalog keeps the old ones
	// — a pre-existing mixed-epoch effect that widens nullability, not
	// something this test is about.)
	if out := generate(); strings.Contains(out, "SQLETCH") {
		t.Fatalf("re-describing generate reported diagnostics:\n%s", out)
	}
	for p := range original {
		if got, _ := os.ReadFile(p); strings.Contains(string(got), `"event_kind"`) && !strings.Contains(string(got), `"e"`) {
			t.Errorf("entry %s was not re-described with its enum metadata:\n%s", p, got)
		}
	}
	if code, out := offlineCheck(); code != cli.ExitOK || !strings.Contains(out, "offline: yes") {
		t.Fatalf("after re-describing, warm check must pass offline again: exit %d\n%s", code, out)
	}
}

func assertPatternsIn(t *testing.T, src string, patterns ...string) {
	t.Helper()
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(src) {
			t.Errorf("generated code missing pattern %q\n----\n%s", p, src)
		}
	}
}

const enumE2EMain = `package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moznion/go-optional"
	sqletch "github.com/moznion/go-sqletch"

	"sqletchgen/gen"
)

func die(err error) {
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
}

func expect(cond bool, msg string, args ...any) {
	if !cond {
		fmt.Printf("FAIL: "+msg+"\n", args...)
		os.Exit(1)
	}
}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("SQLETCH_ENUM_DSN"))
	die(err)
	defer pool.Close()
	_, err = pool.Exec(ctx, "TRUNCATE ev")
	die(err)
	q := gen.New(pool)

	// Constants, NULLs, multibyte labels.
	id1, err := q.InsertEvent(ctx, gen.InsertEventParams{Kind: gen.EventKindStarted,
		Maybe: optional.Some(gen.EventKindQueued), Mood: optional.Some(gen.Mood済み), Note: optional.None[string]()})
	die(err)
	_, err = q.InsertEvent(ctx, gen.InsertEventParams{Kind: gen.EventKindSkipped,
		Maybe: optional.None[gen.EventKind](), Mood: optional.Some(gen.MoodInProgress)})
	die(err)
	die(q.LegacyInsert(ctx, gen.LegacyInsertParams{Kind: "ANSWERED"}))

	rows, err := q.ListEvents(ctx, gen.ListEventsParams{Limit: 10})
	die(err)
	expect(len(rows) == 3, "rows = %d", len(rows))
	expect(rows[0].ID == id1.ID && rows[0].Kind == gen.EventKindStarted, "row0 = %+v", rows[0])
	expect(rows[0].Maybe.Unwrap() == gen.EventKindQueued && rows[0].Mood.Unwrap() == gen.Mood済み, "row0 = %+v", rows[0])
	expect(rows[1].Maybe.IsNone() && rows[1].Mood.Unwrap() == gen.MoodInProgress, "row1 = %+v", rows[1])
	expect(rows[2].Kind == gen.EventKindAnswered && rows[2].Maybe.IsNone() && rows[2].Mood.IsNone(), "row2 = %+v", rows[2])

	// A guarded enum filter.
	skipped, err := q.ListEvents(ctx, gen.ListEventsParams{Kind: sqletch.Present(gen.EventKindSkipped), Limit: 10})
	die(err)
	expect(len(skipped) == 1 && skipped[0].Kind == gen.EventKindSkipped, "filtered = %+v", skipped)

	// A label added after generation: no regeneration needed (D3).
	_, err = pool.Exec(ctx, "ALTER TYPE event_kind ADD VALUE IF NOT EXISTS 'ABANDONED'")
	die(err)
	_, err = q.InsertEvent(ctx, gen.InsertEventParams{Kind: gen.EventKind("ABANDONED")})
	die(err)
	abandoned, err := q.ListEvents(ctx, gen.ListEventsParams{Kind: sqletch.Present(gen.EventKind("ABANDONED")), Limit: 10})
	die(err)
	expect(len(abandoned) == 1, "abandoned = %+v", abandoned)

	// The value set is the server's: an unknown label is 22P02.
	_, err = q.InsertEvent(ctx, gen.InsertEventParams{Kind: gen.EventKind("typo")})
	var pgErr *pgconn.PgError
	expect(errors.As(err, &pgErr) && pgErr.Code == "22P02", "unknown label: %v", err)

	fmt.Println("ENUM-OK")
}
`
