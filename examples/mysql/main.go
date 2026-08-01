// A runnable sqletch MySQL showcase. Point SQLETCH_MYSQL_DSN at a
// disposable MySQL database (go-sql-driver format,
// e.g. "user:pass@tcp(127.0.0.1:3306)/demo") and `go run .`.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"

	gen "github.com/moznion/sqletch/examples/mysql/gen"
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
		Status: gen.Ptr("active"),
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

	fmt.Println("PATCH update (nickname only):")
	n, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{
		ID:       1,
		Nickname: gen.Ptr("allie"),
	})
	must(err)
	fmt.Printf("  %d row(s) updated\n", n)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
