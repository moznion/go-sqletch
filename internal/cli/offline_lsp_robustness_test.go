package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// A file reached both through the config glob (its resolved on-disk
// path) and through an open editor buffer (a symlinked workspace
// spelling, e.g. macOS /tmp vs /private/tmp) must enter the snapshot
// ONCE, with the edited overlay content winning -- not twice, which
// flags every query as a duplicate and checks the stale disk copy.
func TestCheck_SymlinkedOverlayDedupesAndOverlayWins(t *testing.T) {
	const disk = "-- name: FindT :many\nSELECT t.id FROM t;\n"
	// Same query name as on disk (so a failure to dedupe surfaces as a
	// duplicate-name diagnostic) plus an extra query (so we can prove the
	// overlay content, not the disk copy, was the one analyzed).
	const overlaySrc = "-- name: FindT :many\nSELECT t.id FROM t;\n\n" +
		"-- name: Extra :many\nSELECT u.id FROM u;\n"

	cfg := writeOfflineProject(t, map[string]string{"queries/q.sql": disk})

	// A symlink pointing at the project dir gives the file a second,
	// resolved-to-the-same spelling: what an editor opened via a
	// symlinked workspace root would send.
	linkDir := cfg.Dir + "-link"
	if err := os.Symlink(cfg.Dir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(linkDir) })

	overlayPath := filepath.Join(linkDir, "queries", "q.sql")
	res, err := NewOfflineChecker(cfg).Check(map[string][]byte{overlayPath: []byte(overlaySrc)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if anyCode(res, diagnostics.CodeDuplicateQueryName) {
		t.Error("the symlinked overlay path and its glob twin were not deduped: query flagged as a false duplicate")
	}
	file := res.Files[filepath.Clean(overlayPath)]
	if file == nil {
		t.Fatalf("overlay path %q not present in result files", filepath.Clean(overlayPath))
	}
	if len(file.Queries) != 2 {
		t.Fatalf("overlay content was not used (want 2 queries from the buffer, got %d; the stale disk copy has 1)", len(file.Queries))
	}
}

// One unreadable glob-matched file (here a dangling symlink; permission
// denial behaves the same) must degrade to a per-file diagnostic, not
// abort the whole snapshot and freeze every other file's diagnostics.
func TestCheck_UnreadableFileDegradesInsteadOfFreezing(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"queries/good.sql": "-- name: Good :many\nSELECT t.id FROM t;\n",
	})

	broken := filepath.Join(cfg.Dir, "queries", "broken.sql")
	if err := os.Symlink(filepath.Join(cfg.Dir, "queries", "no-such-target"), broken); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatalf("an unreadable file must not fail the whole Check: %v", err)
	}

	goodPath := filepath.Clean(filepath.Join(cfg.Dir, "queries", "good.sql"))
	if res.Files[goodPath] == nil {
		t.Error("the readable file was not checked (the unreadable one froze the snapshot)")
	}
	brokenPath := filepath.Clean(broken)
	if !hasCode(res.Diags[brokenPath], diagnostics.CodeSourceUnreadable) {
		t.Errorf("the unreadable file must carry SQLETCH308; got %v", res.Diags[brokenPath])
	}
}
