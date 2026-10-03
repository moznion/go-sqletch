package cli

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// The examples opt into lints (`lint: true`) and must stay free of
// every one of them — SQLETCHL005 included, which needs the catalog.
// The committed cache makes that checkable in plain `go test`: a
// `check` over a copy of each example must run fully offline (proof the
// catalog-dependent pass ran from the cache, no database involved) and
// report no SQLETCHLnnn.
func TestExamplesAreLintClean(t *testing.T) {
	for _, name := range []string{"postgres", "mysql", "sqlite"} {
		t.Run(name, func(t *testing.T) {
			dir := copyExample(t, filepath.Join("..", "..", "examples", name))
			cfg, diags := config.Load(filepath.Join(dir, "sqletch.yaml"))
			if diagnostics.HasErrors(diags) {
				t.Fatalf("config: %v", diags)
			}
			if !cfg.Lint {
				t.Fatal("examples must set lint: true")
			}
			res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Offline {
				t.Fatal("check must be served entirely by the committed cache (regenerate the example)")
			}
			for _, d := range res.Diags {
				if d.Severity == diagnostics.Error || strings.HasPrefix(string(d.Code), "SQLETCHL") {
					t.Errorf("%s: %s", d.Code, d.Message)
				}
			}
		})
	}
}

// copyExample copies an example project (minus its disposable dev
// database file) into a temp dir, so a check run cannot touch the
// committed tree.
func copyExample(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if strings.HasSuffix(rel, ".sqlite3") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}
