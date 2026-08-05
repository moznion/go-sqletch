// A zero-setup, runnable sqletch showcase: SQLite needs no server and
// no Docker — `go run .` seeds a throwaway database file and queries
// it through the generated, compile-time-verified API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "github.com/ncruces/go-sqlite3/driver"

	gen "github.com/moznion/go-sqletch/examples/sqlite/gen"
	"github.com/moznion/go-sqletch/runtime"
)

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "sqletch-example-*")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "demo.sqlite3"))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	schema, err := os.ReadFile("db/schema.sql")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users (email, status, tenant_id, nickname) VALUES
		('alice@example.com', 'active', 1, 'アリス'),
		('bob@example.com',   'active', 1, NULL),
		('carol@example.com', 'banned', 1, NULL)`); err != nil {
		log.Fatal(err)
	}

	q := gen.New(db)
	q.OnQuery(func(key, sql string) { fmt.Printf("  [shape %s] %.60s…\n", key, sql) })

	fmt.Println("all users:")
	all, err := q.SearchUsers(ctx, gen.SearchUsersParams{Limit: 10})
	must(err)
	for _, u := range all {
		fmt.Printf("  %d %s (%s)\n", u.ID, u.Email, u.Status)
	}

	fmt.Println("active, sorted by email:")
	active, err := q.SearchUsers(ctx, gen.SearchUsersParams{
		Status: new("active"),
		Sort:   gen.SearchUsersSortEmailAsc,
		Limit:  10,
	})
	must(err)
	for _, u := range active {
		fmt.Printf("  %d %s\n", u.ID, u.Email)
	}

	fmt.Println("in-list (active, banned):")
	in, err := q.UsersInStatuses(ctx, gen.UsersInStatusesParams{
		TenantID: 1,
		Statuses: []string{"active", "banned"},
		Limit:    10,
	})
	must(err)
	for _, u := range in {
		fmt.Printf("  %d %s (%s)\n", u.ID, u.Email, u.Status)
	}

	// @filter-tree!: the filter crosses the call boundary as a typed
	// value over the query's closed predicate vocabulary, never as SQL.
	fmt.Println("filter tree (tenant AND active):")
	scoped, err := q.FilterUsers(ctx, gen.And(gen.FilterUsersTenant(1), gen.FilterUsersStatusEq("active")), gen.FilterUsersParams{
		Limit: 10,
	})
	must(err)
	for _, u := range scoped {
		fmt.Printf("  %d %s\n", u.ID, u.Email)
	}

	fmt.Println("filter tree (banned OR alice*):")
	either, err := q.FilterUsers(ctx, gen.Or(gen.FilterUsersStatusEq("banned"), gen.FilterUsersEmailPrefix("alice")), gen.FilterUsersParams{
		Limit: 10,
	})
	must(err)
	for _, u := range either {
		fmt.Printf("  %d %s\n", u.ID, u.Email)
	}

	// The `!` makes the filter required: a forgotten scope fails before
	// any SQL is sent, and unfiltered access is one greppable call.
	fmt.Println("filter tree (required mode):")
	_, err = q.FilterUsers(ctx, nil, gen.FilterUsersParams{Limit: 10})
	if !errors.Is(err, runtime.ErrFilterRequired) {
		log.Fatalf("expected ErrFilterRequired, got %v", err)
	}
	fmt.Printf("  nil scope: %v\n", err)
	unscoped, err := q.FilterUsers(ctx, gen.FilterUsersUnscoped(), gen.FilterUsersParams{
		Limit: 10, // the explicit opt-out — renders TRUE
	})
	must(err)
	fmt.Printf("  Unscoped(): %d users\n", len(unscoped))

	fmt.Println("counts by status:")
	counts, err := q.CountByStatus(ctx, gen.CountByStatusParams{TenantID: 1})
	must(err)
	for _, c := range counts {
		fmt.Printf("  %s: %d\n", c.Status, c.N)
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
