package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// expansionTargets is the fan-out project with SearchUsers statically
// expanded, so `generate` writes .sqletch/expanded/<slug>/SearchUsers/.
const expansionTargets = `targets:
  - queries: ["(app/*)/queries/*.sql"]
    output:
      package: gen
      path: $1/gen
static_expansion:
  queries: [SearchUsers]
`

// victimDir creates a directory OUTSIDE the project holding a sentinel
// file, standing in for e.g. $HOME/Documents.
func victimDir(t *testing.T) (dir, sentinel string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "SearchUsers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel = filepath.Join(dir, "precious.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, sentinel
}

func assertSymlinkRefused(t *testing.T, res *Result, err error, victim, sentinel string) {
	t.Helper()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !hasCode(res.Diags, diagnostics.CodePathEscape) || !diagnostics.HasErrors(res.Diags) {
		t.Errorf("want a SQLETCH306 error, got %+v", res.Diags)
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep me" {
		t.Fatalf("generate deleted/changed a file outside the project through a committed symlink (err=%v)", err)
	}
	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("generate wrote into the symlink target: %v", entries)
	}
}

// F1 (CWE-59): a committed symlink at .sqletch/expanded must not
// redirect the expansion's RemoveAll/MkdirAll/WriteFile outside the
// project. It is refused as SQLETCH306, never silently followed.
func TestRun_ExpandedSymlinkRootRefused(t *testing.T) {
	cfg := writeFanOutProject(t, expansionTargets)
	// The victim sits where expanded/<slug>/SearchUsers lands once
	// .sqletch/expanded is followed into <outside>.
	outside := t.TempDir()
	victim := filepath.Join(outside, "app", "signin", "gen", "SearchUsers")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(victim, "precious.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, ".sqletch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.Dir, ".sqletch", "expanded")); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	assertSymlinkRefused(t, res, err, victim, sentinel)
}

// The same through a symlink one level deeper, at the target slug.
func TestRun_ExpandedSymlinkSlugRefused(t *testing.T) {
	cfg := writeFanOutProject(t, expansionTargets)
	victim, sentinel := victimDir(t)
	slugParent := filepath.Join(cfg.Dir, ".sqletch", "expanded", "app", "signin")
	if err := os.MkdirAll(slugParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(victim), filepath.Join(slugParent, "gen")); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	assertSymlinkRefused(t, res, err, victim, sentinel)
}

// A symlinked query leaf is refused too: removing a symlink is harmless,
// but nothing under the derived tree is ever followed or trusted.
func TestRun_ExpandedSymlinkLeafRefused(t *testing.T) {
	cfg := writeFanOutProject(t, expansionTargets)
	victim, sentinel := victimDir(t)
	leafParent := filepath.Join(cfg.Dir, ".sqletch", "expanded", "app", "signin", "gen")
	if err := os.MkdirAll(leafParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(leafParent, "SearchUsers")); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	assertSymlinkRefused(t, res, err, victim, sentinel)
}

// The benign path still works and replaces the previous run's files.
func TestRun_ExpandedFilesWrittenAndReplaced(t *testing.T) {
	cfg := writeFanOutProject(t, expansionTargets)
	dir := filepath.Join(cfg.Dir, ".sqletch", "expanded", "app", "signin", "gen", "SearchUsers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.sql")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGenerate(t, cfg)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale expansion file survived (err=%v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("no expanded .sql files written")
	}
}
