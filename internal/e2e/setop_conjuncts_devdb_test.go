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
	"github.com/moznion/go-sqletch/internal/devdb"
)

// TestSetOpBranchConjunctsEndToEnd runs a keyset-paginated UNION ALL
// whose branches each carry an optional `@if-present(after)` conjunct
// (spec R1, set-operation operands) through the real CLI pipeline —
// scan, policy weave (design 14 §13: WHERE for inner occurrences, ON
// for the null-extended subscriber side), R1, resolved checks,
// enforcement, oracle, codegen — and then executes the generated code
// on PostgreSQL.
//
// The seed data is adversarial for both features at once: a second
// company reuses the SAME room, room-group, and user UUIDs, and owns a
// subscriber row on the first company's PRIVATE conversation. An
// unscoped branch returns the other company's conversations; an
// unscoped ON clause lets the foreign subscriber make the private
// conversation visible through `s.uuid IS NOT NULL`; a mis-composed
// guard either ignores `after` or drops a branch's filter.
func TestSetOpBranchConjunctsEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_DSN"),
		AllowDestructive: true,
		ServerVersion:    "16",
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer cleanup()

	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", chatSchemaSQL)
	writeFile(t, dir, "queries/chat.sql", chatConversationsQuery)
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
cache:
  path: .sqletch/cache
policies:
  - name: chat_tenant_scope
    tables:
      - chat_conversations
      - chat_conversation_managers
      - chat_conversation_subscribers
      - chat_dm_registers
    predicate: "{}.company_uuid = :company_uuid"
    param:
      name: company_uuid
      type: uuid
    applies_to: [select, update, delete]
`)
	configPath := filepath.Join(dir, "sqletch.yaml")

	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, configPath, false, cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("cold generate: exit %d\n%s%s", code, out.String(), errW.String())
	}
	gen := readFile(t, dir, "gen/list_visible_conversations.sql.gen.go")
	// Woven text is its own fragment (zero-width synthesized skeleton),
	// so assert per fragment: branches 1–2 carry c in WHERE and the
	// null-extended s in its ON; branch 3 its three inner occurrences.
	for frag, n := range map[string]int{
		`" AND (s.company_uuid = :company_uuid)"`: 2,
		`" (c.company_uuid = :company_uuid) AND"`: 2,
		`" (s.company_uuid = :company_uuid) AND (c.company_uuid = :company_uuid) AND (d.company_uuid = :company_uuid) AND"`: 1,
		`Kind: runtime.Guarded, GuardMask: 0x1`: 3,
	} {
		if got := strings.Count(gen, frag); got != n {
			t.Errorf("generated fragments: %s occurs %d times, want %d", frag, got, n)
		}
	}
	if t.Failed() {
		t.Fatalf("generated code:\n%s", gen)
	}

	// Warm offline re-check over the committed cache.
	out.Reset()
	errW.Reset()
	if code := cli.Check(ctx, configPath, false, false, cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("warm check: exit %d\n%s%s", code, out.String(), errW.String())
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	ver := func(mod string) string {
		m := regexp.MustCompile(regexp.QuoteMeta(mod) + ` (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
		if m == nil {
			t.Fatalf("%s version not found in parent go.mod", mod)
		}
		return m[1]
	}
	goMod := "module sqletchgen\n\ngo 1.24\n\nrequire (\n" +
		"\tgithub.com/jackc/pgx/v5 " + ver("github.com/jackc/pgx/v5") + "\n" +
		"\tgithub.com/moznion/go-optional " + ver("github.com/moznion/go-optional") + "\n" +
		"\tgithub.com/moznion/go-sqletch v0.0.0\n)\n\n" +
		"replace github.com/moznion/go-sqletch => " + repoRoot + "\n"
	writeFile(t, dir, "go.mod", goMod)
	parentSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "go.sum", string(parentSum))
	writeFile(t, dir, "main.go", setOpConjunctsMain)

	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "SQLETCH_TEST_DSN="+dsn)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	run("mod", "tidy")
	if o := run("run", "."); !strings.Contains(o, "SETOP-CONJUNCTS-E2E-OK") {
		t.Fatalf("generated module run did not report success:\n%s", o)
	}
}

const chatSchemaSQL = `CREATE TABLE chat_conversations (
    uuid            uuid PRIMARY KEY,
    company_uuid    uuid NOT NULL,
    label           text,
    visibility      text NOT NULL,
    kind            text NOT NULL,
    scope           text NOT NULL,
    room_group_uuid uuid,
    room_uuid       uuid,
    archived_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE chat_conversation_subscribers (
    uuid              uuid PRIMARY KEY,
    company_uuid      uuid NOT NULL,
    conversation_uuid uuid NOT NULL,
    user_uuid         uuid NOT NULL,
    subscribed_at     timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE chat_conversation_managers (
    uuid              uuid PRIMARY KEY,
    company_uuid      uuid NOT NULL,
    conversation_uuid uuid NOT NULL,
    user_uuid         uuid NOT NULL
);
CREATE TABLE chat_dm_registers (
    conversation_uuid uuid PRIMARY KEY,
    company_uuid      uuid NOT NULL,
    member_user_uuids uuid[] NOT NULL
);
`

// The user-facing shape: three per-scope listings combined into one
// keyset page. Columns are aligned across operands, and the ORDER BY /
// LIMIT belong to the whole UNION ALL.
const chatConversationsQuery = `-- name: ListVisibleConversations :many
SELECT
    c.uuid, c.label, c.visibility, c.kind, c.scope,
    c.room_group_uuid, c.room_uuid, c.created_at, c.updated_at,
    s.uuid AS subscription_uuid, s.subscribed_at,
    NULL::uuid[] AS member_user_uuids
FROM chat_conversations AS c
LEFT JOIN chat_conversation_subscribers AS s
    ON s.conversation_uuid = c.uuid
    AND s.user_uuid = :user_uuid
WHERE c.scope = 'room'
  AND c.room_uuid = :room_uuid
  AND c.archived_at IS NULL
  AND (c.visibility = 'public' OR s.uuid IS NOT NULL)
@if-present(after)
  AND c.uuid > :after
@endif
UNION ALL
SELECT
    c.uuid, c.label, c.visibility, c.kind, c.scope,
    c.room_group_uuid, c.room_uuid, c.created_at, c.updated_at,
    s.uuid AS subscription_uuid, s.subscribed_at,
    NULL::uuid[] AS member_user_uuids
FROM chat_conversations AS c
LEFT JOIN chat_conversation_subscribers AS s
    ON s.conversation_uuid = c.uuid
    AND s.user_uuid = :user_uuid
WHERE c.scope = 'room-group'
  AND c.room_group_uuid = :room_group_uuid
  AND c.archived_at IS NULL
  AND (c.visibility = 'public' OR s.uuid IS NOT NULL)
@if-present(after)
  AND c.uuid > :after
@endif
UNION ALL
SELECT
    c.uuid, c.label, c.visibility, c.kind, c.scope,
    NULL::uuid AS room_group_uuid, NULL::uuid AS room_uuid, c.created_at, c.updated_at,
    s.uuid AS subscription_uuid, s.subscribed_at,
    d.member_user_uuids
FROM chat_conversation_subscribers AS s
JOIN chat_conversations AS c ON c.uuid = s.conversation_uuid
JOIN chat_dm_registers AS d ON d.conversation_uuid = c.uuid
WHERE s.user_uuid = :user_uuid
  AND c.scope = 'dm'
  AND c.archived_at IS NULL
  AND :user_uuid = ANY (d.member_user_uuids)
@if-present(after)
  AND s.conversation_uuid > :after
@endif
ORDER BY 1
LIMIT :limit;
`

const setOpConjunctsMain = `package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/moznion/go-optional"
	"github.com/moznion/go-sqletch"

	gen "sqletchgen/gen"
)

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}

const (
	coA   = "aaaaaaaa-0000-0000-0000-000000000000"
	coB   = "bbbbbbbb-0000-0000-0000-000000000000"
	user  = "00000000-0000-0000-0000-0000000000ee"
	room  = "00000000-0000-0000-0000-00000000000f"
	group = "00000000-0000-0000-0000-00000000000c"
)

func conv(n string) string { return "00000000-0000-0000-0000-0000000000" + n }

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("SQLETCH_TEST_DSN"))
	die(err)
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, ` + "`" + `
		INSERT INTO chat_conversations (uuid, company_uuid, label, visibility, kind, scope, room_group_uuid, room_uuid, archived_at) VALUES
		  ('00000000-0000-0000-0000-0000000000a1', 'aaaaaaaa-0000-0000-0000-000000000000', 'a-public',  'public',  'k', 'room',       NULL, '00000000-0000-0000-0000-00000000000f', NULL),
		  ('00000000-0000-0000-0000-0000000000a2', 'aaaaaaaa-0000-0000-0000-000000000000', NULL,        'private', 'k', 'room',       NULL, '00000000-0000-0000-0000-00000000000f', NULL),
		  ('00000000-0000-0000-0000-0000000000a3', 'aaaaaaaa-0000-0000-0000-000000000000', 'a-hidden',  'private', 'k', 'room',       NULL, '00000000-0000-0000-0000-00000000000f', NULL),
		  ('00000000-0000-0000-0000-0000000000a4', 'aaaaaaaa-0000-0000-0000-000000000000', 'a-group',   'public',  'k', 'room-group', '00000000-0000-0000-0000-00000000000c', NULL, NULL),
		  ('00000000-0000-0000-0000-0000000000a5', 'aaaaaaaa-0000-0000-0000-000000000000', 'a-dm',      'private', 'k', 'dm',         NULL, NULL, NULL),
		  ('00000000-0000-0000-0000-0000000000a6', 'aaaaaaaa-0000-0000-0000-000000000000', 'a-archived','public',  'k', 'room',       NULL, '00000000-0000-0000-0000-00000000000f', now()),
		  ('00000000-0000-0000-0000-0000000000b1', 'bbbbbbbb-0000-0000-0000-000000000000', 'b-public',  'public',  'k', 'room',       NULL, '00000000-0000-0000-0000-00000000000f', NULL),
		  ('00000000-0000-0000-0000-0000000000b4', 'bbbbbbbb-0000-0000-0000-000000000000', 'b-group',   'public',  'k', 'room-group', '00000000-0000-0000-0000-00000000000c', NULL, NULL),
		  ('00000000-0000-0000-0000-0000000000b5', 'bbbbbbbb-0000-0000-0000-000000000000', 'b-dm',      'private', 'k', 'dm',         NULL, NULL, NULL);
		INSERT INTO chat_conversation_subscribers (uuid, company_uuid, conversation_uuid, user_uuid) VALUES
		  ('00000000-0000-0000-0000-000000000102', 'aaaaaaaa-0000-0000-0000-000000000000', '00000000-0000-0000-0000-0000000000a2', '00000000-0000-0000-0000-0000000000ee'),
		  ('00000000-0000-0000-0000-000000000105', 'aaaaaaaa-0000-0000-0000-000000000000', '00000000-0000-0000-0000-0000000000a5', '00000000-0000-0000-0000-0000000000ee'),
		  -- company B's subscriber row on company A's PRIVATE conversation a3
		  ('00000000-0000-0000-0000-000000000203', 'bbbbbbbb-0000-0000-0000-000000000000', '00000000-0000-0000-0000-0000000000a3', '00000000-0000-0000-0000-0000000000ee'),
		  ('00000000-0000-0000-0000-000000000205', 'bbbbbbbb-0000-0000-0000-000000000000', '00000000-0000-0000-0000-0000000000b5', '00000000-0000-0000-0000-0000000000ee');
		INSERT INTO chat_dm_registers (conversation_uuid, company_uuid, member_user_uuids) VALUES
		  ('00000000-0000-0000-0000-0000000000a5', 'aaaaaaaa-0000-0000-0000-000000000000', ARRAY['00000000-0000-0000-0000-0000000000ee'::uuid]),
		  ('00000000-0000-0000-0000-0000000000b5', 'bbbbbbbb-0000-0000-0000-000000000000', ARRAY['00000000-0000-0000-0000-0000000000ee'::uuid]);
	` + "`" + `)
	die(err)

	q := gen.New(conn)
	list := func(company string, after sqletch.Omittable[string], limit int64) string {
		rows, err := q.ListVisibleConversations(ctx, gen.CompanyUUID(company), gen.ListVisibleConversationsParams{
			UserUUID: user, RoomUUID: room, RoomGroupUUID: group, After: after, Limit: limit,
		})
		die(err)
		var ids []string
		for _, r := range rows {
			id := r.UUID.TakeOr("?")
			ids = append(ids, id[len(id)-2:])
		}
		return strings.Join(ids, ",")
	}
	expect := func(got, want, msg string) {
		if got != want {
			fmt.Fprintf(os.Stderr, "EXPECT FAILED: %s: got [%s], want [%s]\n", msg, got, want)
			os.Exit(1)
		}
	}

	none := sqletch.Omittable[string]{}
	// a1 public room, a2 private room subscribed, a4 public group, a5 dm.
	// a3 is private and only company B's subscriber points at it; a6 is
	// archived; b* belong to company B despite the shared room/group/user.
	expect(list(coA, none, 50), "a1,a2,a4,a5", "company A, first page")
	expect(list(coA, sqletch.Present(conv("a2")), 50), "a4,a5", "company A, after a2 (every branch honors the cursor)")
	expect(list(coA, sqletch.Present(conv("a4")), 50), "a5", "company A, after a4 (the dm branch's cursor)")
	expect(list(coA, none, 2), "a1,a2", "company A, LIMIT applies to the whole union")
	expect(list(coB, none, 50), "b1,b4,b5", "company B sees only its own")
	_ = optional.None[string]()

	fmt.Println("SETOP-CONJUNCTS-E2E-OK")
}
`
