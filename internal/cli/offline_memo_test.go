package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// A second Check with an unchanged schema must not re-read (and
// re-hash) the schema files: the catalog is memoized behind a cheap
// stat signature. Editing the schema invalidates the memo.
func TestOfflineChecker_CatalogMemoAvoidsReReads(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/a.sql": validQuery})
	c := NewOfflineChecker(cfg)

	if _, err := c.Check(nil); err != nil {
		t.Fatal(err)
	}
	afterFirst := c.schemaReads
	if afterFirst == 0 {
		t.Fatal("first Check should have read the schema at least once")
	}

	if _, err := c.Check(nil); err != nil {
		t.Fatal(err)
	}
	if c.schemaReads != afterFirst {
		t.Errorf("unchanged Check re-read the schema: reads went %d -> %d", afterFirst, c.schemaReads)
	}

	// Editing the schema (different length so the change is detected
	// even under coarse mtime) must invalidate the memo.
	schemaPath := filepath.Join(cfg.Dir, "db", "schema.sql")
	if err := os.WriteFile(schemaPath,
		[]byte("CREATE TABLE t (id bigint NOT NULL, x text, extra text);\n"+
			"CREATE TABLE u (id bigint NOT NULL, name text, score bigint);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(nil); err != nil {
		t.Fatal(err)
	}
	if c.schemaReads <= afterFirst {
		t.Errorf("schema edit should have forced a re-read: reads still %d", c.schemaReads)
	}
}

// The memo must not pin a cold result: once the catalog appears (a
// `generate` run), the same checker picks it up and runs the
// catalog-dependent pass.
func TestOfflineChecker_CatalogMemoColdToWarm(t *testing.T) {
	const scoped = `-- name: FindUser :many
SELECT t.id, u.name FROM t
@if-present(min)
  LEFT JOIN u ON u.id = t.id AND u.score > :min
@endif
WHERE TRUE
@choose(sort)
@case(id_desc)
ORDER BY t.id DESC
@default
ORDER BY t.id ASC
@end
;
`
	cfg := writeOfflineProject(t, map[string]string{"queries/q.sql": scoped})
	c := NewOfflineChecker(cfg)

	res, err := c.Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if anyCode(res, diagnostics.CodeScopeViolation) {
		t.Fatalf("cold cache must not run the resolution pass: %v", res.Diags)
	}

	// Commit the catalog + oracle entries (as `generate` would), then
	// re-check with the SAME checker.
	warmCache(t, cfg, "queries/q.sql", "all")
	res, err = c.Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !anyCode(res, diagnostics.CodeScopeViolation) {
		t.Fatalf("warm cache must surface the resolution diagnostic after the memo invalidates: %v", res.Diags)
	}
}
