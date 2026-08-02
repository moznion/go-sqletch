// The examples app exercises the generated API end to end. Regenerate
// with `go generate ./...` (needs Docker or SQLETCH_DSN).
package main

//go:generate go run github.com/moznion/go-sqletch/cmd/sqletch generate --config sqletch.yaml

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"

	gen "github.com/moznion/go-sqletch/examples/postgres/gen"
	"github.com/moznion/go-sqletch/runtime"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("SQLETCH_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	q := gen.New(conn)
	q.OnQuery(func(shapeKey, sql string) {
		log.Printf("shape %s:\n%s", shapeKey, sql)
	})

	users, err := q.SearchUsers(ctx, gen.SearchUsersParams{
		Status: new("active"),
		Sort:   gen.SearchUsersSortCreatedAtDesc,
		Limit:  20,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, u := range users {
		fmt.Printf("%d\t%s\t%s\n", u.ID, u.Email, u.Status)
	}

	// v0.3: typed filters cross the repository boundary as values —
	// never as SQL strings (@filter-tree!).
	scoped, err := q.FilterUsers(ctx, gen.FilterUsersParams{
		Scope: gen.And(
			gen.FilterUsersTenant(1),
			gen.FilterUsersStatusEq("active"),
		),
		Limit: 20,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("scoped: %d users\n", len(scoped))

	// The `!` in @filter-tree!(scope) makes the filter required: a
	// forgotten scope fails before any SQL is sent…
	if _, err := q.FilterUsers(ctx, gen.FilterUsersParams{Limit: 20}); !errors.Is(err, runtime.ErrFilterRequired) {
		log.Fatalf("expected ErrFilterRequired, got %v", err)
	}
	// …and deliberately unfiltered access is one greppable call.
	unscoped, err := q.FilterUsers(ctx, gen.FilterUsersParams{
		Scope: gen.FilterUsersUnscoped(), // renders TRUE
		Limit: 20,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("unscoped: %d users\n", len(unscoped))

	// v0.3: caller-chosen multi-key sort over a closed key set.
	sorted, err := q.ListUsersSorted(ctx, gen.ListUsersSortedParams{
		IncludeBanned: false,
		Sort: []gen.ListUsersSortedSortKey{
			gen.ListUsersSortedSortEmailDesc,
			gen.ListUsersSortedSortCreatedAtAsc,
		},
		Limit: 20,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("sorted: %d users\n", len(sorted))
}
