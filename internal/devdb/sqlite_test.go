package devdb

import (
	"context"
	"testing"
)

func TestAcquireSQLite_FTS5Schema(t *testing.T) {
	conn, cleanup, err := AcquireSQLite(context.Background(), Config{
		SchemaSQL: []string{`
CREATE VIRTUAL TABLE docs USING fts5(body);
INSERT INTO docs(body) VALUES ('hello world');
`},
	})
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("apply FTS5 schema: %v", err)
	}

	stmt, _, err := conn.Prepare("SELECT body FROM docs WHERE docs MATCH 'hello'")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	if !stmt.Step() {
		t.Fatalf("query FTS5 table: %v", stmt.Err())
	}
	if got := stmt.ColumnText(0); got != "hello world" {
		t.Errorf("body = %q, want hello world", got)
	}
}
