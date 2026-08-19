package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomic_IgnoresPlantedTempSymlink pins the M5 fix. The old
// writeFile wrote to the PREDICTABLE name `<path>.tmp` with a plain
// os.WriteFile, which follows a symlink: a cloned repo could pre-plant
// `<path>.tmp` as a link to ~/.bashrc / a CI secret and have the write
// corrupt that file. WriteFileAtomic uses os.CreateTemp (O_CREATE|O_EXCL
// + a random suffix), so a planted `<path>.tmp` is neither followed nor
// collided with. This test fails against the old implementation.
func TestWriteFileAtomic_IgnoresPlantedTempSymlink(t *testing.T) {
	dir := t.TempDir()

	secret := filepath.Join(dir, "secret.txt")
	const secretContent = "DO NOT TOUCH"
	if err := os.WriteFile(secret, []byte(secretContent), 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "out.json")
	// The exact temp name the old code used, pre-planted as a symlink.
	if err := os.Symlink(secret, target+".tmp"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	// The secret must be untouched: the write never followed the link.
	if got, _ := os.ReadFile(secret); string(got) != secretContent {
		t.Errorf("planted <path>.tmp symlink was followed: secret = %q", got)
	}
	if got, _ := os.ReadFile(target); string(got) != "payload" {
		t.Errorf("destination content = %q", got)
	}
}

// TestWriteFileAtomic_ReplacesSymlinkAtTarget: a symlink sitting at the
// destination itself is replaced by the rename (not written through).
func TestWriteFileAtomic_ReplacesSymlinkAtTarget(t *testing.T) {
	dir := t.TempDir()

	secret := filepath.Join(dir, "secret.txt")
	const secretContent = "DO NOT TOUCH"
	if err := os.WriteFile(secret, []byte(secretContent), 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "out.json")
	if err := os.Symlink(secret, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if got, _ := os.ReadFile(secret); string(got) != secretContent {
		t.Errorf("symlink target was clobbered: secret = %q", got)
	}
	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("destination is still a symlink; rename should have replaced it")
	}
	if got, _ := os.ReadFile(target); string(got) != "payload" {
		t.Errorf("destination content = %q", got)
	}
}

// TestWriteFileAtomic_NoLeftoverTemp ensures the O_EXCL temp file is not
// left behind on success (it is renamed into place).
func TestWriteFileAtomic_NoLeftoverTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "out.json")
	if err := WriteFileAtomic(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only out.json, got %v", names)
	}
}

// TestReadFileCapped bounds reads so an attacker-planted giant file at a
// computable cache path cannot OOM the process (M9). A file over the cap
// is rejected (→ a cache miss for the Load* callers); a normal file
// reads unchanged. The oversized file is sparse (Truncate), so the test
// touches no real disk.
func TestReadFileCapped(t *testing.T) {
	dir := t.TempDir()

	small := filepath.Join(dir, "small.json")
	if err := os.WriteFile(small, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadFileCapped(small); err != nil || string(data) != "{}" {
		t.Fatalf("small read: data=%q err=%v", data, err)
	}

	big := filepath.Join(dir, "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := ReadFileCapped(big); err == nil {
		t.Errorf("oversized file must be rejected, got nil error")
	}
}
