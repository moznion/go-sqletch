package cli

import (
	"fmt"
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

// The per-file memo is keyed by path, never evicted, and fed overlay
// paths straight from LSP client didOpen notifications — a hostile or
// buggy client streaming ever-distinct URIs must not grow it without
// bound. The cap clears the memo wholesale; correctness is unaffected
// because entries are validated by content hash on read.
func TestOfflineChecker_MemoSizeCap(t *testing.T) {
	cfg := writeOfflineProject(t, nil)
	c := NewOfflineChecker(cfg)

	// Content with no queries: the per-path analysis is cheap (no
	// rendering), which is exactly the cheapest flood a client can send.
	src := []byte("-- hostile buffer, no queries\n")
	for i := range maxMemoEntries + 100 {
		c.analyzeFile(fmt.Sprintf("/hostile/%d.sql", i), src)
	}
	if len(c.memo) > maxMemoEntries {
		t.Errorf("memo grew to %d entries, cap is %d", len(c.memo), maxMemoEntries)
	}

	// Replacing an EXISTING entry (content change) must not count as
	// growth: fill back to the cap, then re-analyze one path with new
	// content and confirm nothing was cleared.
	for i := range maxMemoEntries - len(c.memo) {
		c.analyzeFile(fmt.Sprintf("/refill/%d.sql", i), src)
	}
	before := len(c.memo)
	c.analyzeFile("/refill/0.sql", []byte("-- edited, still no queries\n"))
	if len(c.memo) != before {
		t.Errorf("in-place replacement changed the memo size: %d -> %d", before, len(c.memo))
	}

	// The clear is invisible to correctness: a real template analyzed
	// after an overflow still scans to a full result.
	c.analyzeFile("/one/more.sql", src) // overflow: wholesale clear
	m := c.analyzeFile("/ws/queries/a.sql", []byte(validQuery))
	if m.file == nil || len(m.file.Queries) != 1 {
		t.Fatalf("post-clear analysis must still work: %+v", m)
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
