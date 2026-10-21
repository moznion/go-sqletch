package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// plantOutsideTarget creates a directory outside the project holding a
// pre-existing `signin/gen/SearchUsers.json` — exactly the file an
// explain write through a planted `.sqletch/explain/app` link would
// replace — and returns the directory and the sentinel file's path.
func plantOutsideTarget(t *testing.T) (string, string) {
	t.Helper()
	outside := t.TempDir()
	victim := filepath.Join(outside, "signin", "gen", "SearchUsers.json")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("SENTINEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	return outside, victim
}

func assertUntouched(t *testing.T, victim string) {
	t.Helper()
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("victim file vanished: %v", err)
	}
	if string(got) != "SENTINEL" {
		t.Fatalf("explain data was written through a committed symlink: victim now %q", got)
	}
}

// F5: a cloned repository commits `.sqletch/explain/app -> <outside>`.
// The explain write for target slug `app/signin/gen` must not follow
// the link (it would atomically replace a file outside the project);
// it is refused with SQLETCH306.
func TestRun_ExplainWriteRefusesCommittedSymlink(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(projectDir, outside string) (linkPath, linkTarget string)
	}{
		{"intermediate component under explain/", func(dir, outside string) (string, string) {
			return filepath.Join(dir, ".sqletch", "explain", "app"), outside
		}},
		{"explain directory itself", func(dir, outside string) (string, string) {
			// explain -> outside means explain/app/signin/gen/… lands in
			// outside/app/signin/gen/…; plant the victim one level down.
			return filepath.Join(dir, ".sqletch", "explain"), filepath.Join(outside, "root")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeFanOutProject(t, "")
			outside, victim := plantOutsideTarget(t)
			linkPath, linkTarget := tc.link(cfg.Dir, outside)
			if tc.name == "explain directory itself" {
				v2 := filepath.Join(linkTarget, "app", "signin", "gen", "SearchUsers.json")
				if err := os.MkdirAll(filepath.Dir(v2), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(v2, []byte("SENTINEL"), 0o600); err != nil {
					t.Fatal(err)
				}
				victim = v2
			}
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(linkTarget, linkPath); err != nil {
				t.Fatal(err)
			}
			res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			assertUntouched(t, victim)
			if !hasCode(res.Diags, diagnostics.CodePathEscape) {
				t.Fatalf("want SQLETCH306 for the symlinked explain path, got %+v", res.Diags)
			}
			// Nothing new appeared beside the victim either.
			entries, err := os.ReadDir(filepath.Dir(victim))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("files were created outside the project: %v", entries)
			}
		})
	}
}

// A symlink whose target stays inside the project is still refused:
// the explain tree is sqletch-owned derived output, and following any
// committed link there would let a repository redirect it onto files it
// does not own.
func TestRun_ExplainWriteRefusesInProjectSymlink(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	elsewhere := filepath.Join(cfg.Dir, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.Dir, ".sqletch", "explain", "app")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !hasCode(res.Diags, diagnostics.CodePathEscape) {
		t.Fatalf("want SQLETCH306, got %+v", res.Diags)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("explain data written through an in-project symlink: %v", entries)
	}
}

// A leftover regular file at the explain path (or a symlink at the
// LEAF) is replaced, not written through — unchanged from before, but
// pinned here so the os.Root rewrite keeps it.
func TestRun_ExplainWriteReplacesLeafSymlink(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	_, victim := plantOutsideTarget(t)
	leaf := filepath.Join(cfg.Dir, ".sqletch", "explain", "app", "signin", "gen", "SearchUsers.json")
	if err := os.MkdirAll(filepath.Dir(leaf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, leaf); err != nil {
		t.Fatal(err)
	}
	mustGenerate(t, cfg)
	assertUntouched(t, victim)
	fi, err := os.Lstat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("leaf is %v, want a regular file", fi.Mode())
	}
}
