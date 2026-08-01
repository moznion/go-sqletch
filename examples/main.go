// The examples app exercises the generated API end to end. Regenerate
// with `go generate ./...` (needs Docker or SQLETCH_DSN).
package main

//go:generate go run github.com/moznion/sqletch/cmd/sqletch generate --config sqletch.yaml

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"

	gen "github.com/moznion/sqletch/examples/gen"
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
		Status: gen.Ptr("active"),
		Sort:   gen.SearchUsersSortCreatedAtDesc,
		Limit:  20,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, u := range users {
		fmt.Printf("%d\t%s\t%s\n", u.ID, u.Email, u.Status)
	}
}
