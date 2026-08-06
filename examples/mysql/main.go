// A runnable sqletch MySQL showcase. Point SQLETCH_MYSQL_DSN at a
// disposable MySQL database (go-sql-driver format,
// e.g. "user:pass@tcp(127.0.0.1:3306)/demo") and `go run .`.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"

	gen "github.com/moznion/go-sqletch/examples/mysql/gen"
	"github.com/moznion/go-sqletch/runtime"
)

func main() {
	dsn := os.Getenv("SQLETCH_MYSQL_DSN")
	if dsn == "" {
		fmt.Println("set SQLETCH_MYSQL_DSN to a DISPOSABLE MySQL database (go-sql-driver format) and re-run")
		return
	}
	ctx := context.Background()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	schema, err := os.ReadFile("db/schema.sql")
	if err != nil {
		log.Fatal(err)
	}
	for _, stmt := range []string{"DROP TABLE IF EXISTS users", string(schema),
		`INSERT INTO users (email, status, tenant_id, nickname) VALUES
			('alice@example.com', 'active', 1, 'アリス'),
			('bob@example.com',   'active', 1, NULL),
			('carol@example.com', 'banned', 1, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			log.Fatal(err)
		}
	}

	q := gen.New(db)
	q.OnQuery(func(key, sql string) { fmt.Printf("  [shape %s]\n", key) })

	fmt.Println("active users, sorted by email:")
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

	// The `!` makes the filter required: a forgotten scope fails before
	// any SQL is sent, and unfiltered access is one greppable call.
	fmt.Println("filter tree (required mode):")
	// Omitting the scope does not compile, and neither does passing nil
	// (the argument is a value type). The zero Tree is the only way left
	// to arrive here without a decision, and it is refused.
	_, err = q.FilterUsers(ctx, runtime.Tree{}, gen.FilterUsersParams{Limit: 10})
	if !errors.Is(err, runtime.ErrFilterRequired) {
		log.Fatalf("expected ErrFilterRequired, got %v", err)
	}
	fmt.Printf("  nil scope: %v\n", err)
	unscoped, err := q.FilterUsers(ctx, gen.FilterUsersUnscoped(), gen.FilterUsersParams{
		Limit: 10, // Unscoped() is the explicit opt-out — renders TRUE
	})
	must(err)
	fmt.Printf("  Unscoped(): %d users\n", len(unscoped))

	fmt.Println("PATCH update (nickname only):")
	n, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{
		ID:       1,
		Nickname: new("allie"),
	})
	must(err)
	fmt.Printf("  %d row(s) updated\n", n)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
