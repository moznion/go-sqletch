package cli

import (
	"path/filepath"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// A malformed `-- @timeout` reaches the editor through the LSP's
// offline analysis seam (design 23): it is a scanner diagnostic, so no
// catalog or cache is needed to report it.
func TestOfflineChecker_BadTimeout(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"queries/a.sql": "-- name: FindT :many\n-- @timeout 0s\nSELECT t.id FROM t;\n",
		"queries/b.sql": "-- name: FindU :many\n-- @timeout 500ms\nSELECT u.id FROM u;\n",
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Diags[filepath.Join(cfg.Dir, "queries", "a.sql")]
	if !hasCode(a, diagnostics.CodeBadTimeout) {
		t.Errorf("a.sql must carry SQLETCH017, got %v", a)
	}
	for _, d := range a {
		if d.Code == diagnostics.CodeBadTimeout && d.Span.File != filepath.Join(cfg.Dir, "queries", "a.sql") {
			t.Errorf("SQLETCH017 span file = %q", d.Span.File)
		}
	}
	if b := res.Diags[filepath.Join(cfg.Dir, "queries", "b.sql")]; len(b) != 0 {
		t.Errorf("b.sql should be clean, got %v", b)
	}
}
