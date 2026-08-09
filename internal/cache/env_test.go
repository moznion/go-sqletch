package cache

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestNumericVersionPrefix(t *testing.T) {
	// The comparison value is the leading dotted-numeric run: build
	// suffixes and packaging noise differ between images of the same
	// server and must NOT read as a drifted environment.
	cases := []struct{ raw, want string }{
		{"16.4", "16.4"},
		{"16.4 (Debian 16.4-1.pgdg120+1)", "16.4"},
		{"16.4 (Ubuntu 16.4-1.pgdg22.04+1)", "16.4"},
		{"8.0.36-log", "8.0.36"},
		{"8.4.0", "8.4.0"},
		{"3.50.1", "3.50.1"},
		{"17devel", "17"},
		{"16beta1", "16"},
		{"16.", "16"},
		{"16..4", "16..4"}, // not our business to normalize; only compared
		{"", ""},
		{"unknown", ""},
	}
	for _, c := range cases {
		if got := NumericVersionPrefix(c.raw); got != c.want {
			t.Errorf("NumericVersionPrefix(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestNumericVersionPrefix_SameServerDifferentBuild(t *testing.T) {
	// The motivating pair: identical PostgreSQL, different base image.
	alpine := NumericVersionPrefix("16.4")
	debian := NumericVersionPrefix("16.4 (Debian 16.4-1.pgdg120+1)")
	if alpine != debian {
		t.Errorf("same version on different builds must compare equal: %q vs %q", alpine, debian)
	}
	if NumericVersionPrefix("16.9") == alpine {
		t.Error("a different patch version must compare unequal")
	}
}

func TestStore_EnvRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	fp := strings.Repeat("ab", 32)
	env := &Env{
		SchemaFP:         fp,
		Dialect:          "postgres",
		OracleBackend:    "server",
		ServerVersion:    "16.4",
		ServerVersionRaw: "16.4 (Debian 16.4-1.pgdg120+1)",
	}
	if err := s.SaveEnv(env); err != nil {
		t.Fatal(err)
	}
	got, ok := s.LoadEnv(fp)
	if !ok {
		t.Fatal("round trip failed")
	}
	if got.ServerVersion != "16.4" || got.ServerVersionRaw != "16.4 (Debian 16.4-1.pgdg120+1)" {
		t.Fatalf("unexpected record: %+v", got)
	}
	if got.Dialect != "postgres" || got.OracleBackend != "server" {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestStore_SaveEnvWithoutFP(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.SaveEnv(&Env{ServerVersion: "16.4"}); err == nil {
		t.Fatal("saving an env record without fingerprint must fail")
	}
}

func TestStore_EnvMissIsNeverAFailure(t *testing.T) {
	// Every way of not having a usable record must read as "no record"
	// (adopt whatever we connect to), never as a drift or an error:
	// caches committed before the sidecar existed must keep working.
	dir := t.TempDir()
	s := NewStore(dir)
	fp := strings.Repeat("ab", 32)

	if _, ok := s.LoadEnv(fp); ok {
		t.Error("absent sidecar must miss")
	}

	if err := s.SaveEnv(&Env{SchemaFP: fp, ServerVersion: "16.4"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadEnv(strings.Repeat("cd", 32)); ok {
		t.Error("wrong fingerprint must miss")
	}

	// Store-and-compare: the filename hash is an index, never identity.
	path := s.envPath(fp)
	doctored := strings.Replace(string(mustRead(t, path)),
		`"schema_fp": "`+fp+`"`, `"schema_fp": "`+strings.Repeat("cd", 32)+`"`, 1)
	if err := os.WriteFile(path, []byte(doctored), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadEnv(fp); ok {
		t.Error("mismatched stored key must be treated as a miss")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadEnv(fp); ok {
		t.Error("unparseable sidecar must be treated as a miss")
	}
}

func TestStore_EnvFormatVersion(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	fp := strings.Repeat("ab", 32)
	if err := s.SaveEnv(&Env{SchemaFP: fp, ServerVersion: "16.4"}); err != nil {
		t.Fatal(err)
	}
	data := mustRead(t, s.envPath(fp))
	if !strings.Contains(string(data), "\"format\": "+strconv.Itoa(FormatVersion)) {
		t.Fatalf("env file missing format marker:\n%s", data)
	}
	bumped := strings.Replace(string(data),
		"\"format\": "+strconv.Itoa(FormatVersion),
		"\"format\": "+strconv.Itoa(FormatVersion+1), 1)
	if err := os.WriteFile(s.envPath(fp), []byte(bumped), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadEnv(fp); ok {
		t.Error("newer-format env record must be a miss")
	}
}

func TestStore_EnvCanonicalJSON(t *testing.T) {
	s := NewStore(t.TempDir())
	fp := strings.Repeat("ab", 32)
	env := &Env{SchemaFP: fp, Dialect: "postgres", OracleBackend: "server", ServerVersion: "16.4"}
	if err := s.SaveEnv(env); err != nil {
		t.Fatal(err)
	}
	first := mustRead(t, s.envPath(fp))
	if err := s.SaveEnv(env); err != nil {
		t.Fatal(err)
	}
	if string(first) != string(mustRead(t, s.envPath(fp))) {
		t.Error("env files must be byte-stable across saves")
	}
	if !strings.HasSuffix(string(first), "\n") {
		t.Error("env files must end with a newline")
	}
}

func TestEnvFileName_DistinctPerFingerprint(t *testing.T) {
	a := EnvFileName(strings.Repeat("ab", 32))
	b := EnvFileName(strings.Repeat("cd", 32))
	if a == b {
		t.Error("distinct fingerprints must name distinct env files")
	}
	if !strings.HasPrefix(a, "env-") || !strings.HasSuffix(a, ".json") {
		t.Errorf("unexpected env file name %q", a)
	}
	// The sidecar must not collide with the catalog for the same fp.
	if a == CatalogFileName(strings.Repeat("ab", 32)) {
		t.Error("env and catalog file names must differ")
	}
}
