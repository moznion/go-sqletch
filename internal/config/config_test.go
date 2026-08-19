package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

const validYAML = `version: 1
dialect: postgres
server_version: "16"
database:
  dsn: postgres://x
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
`

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", validYAML)
	cfg, diags := Load(path)
	if len(diags) != 0 {
		t.Fatalf("diags: %+v", diags)
	}
	if cfg.Database.DSN != "postgres://x" {
		t.Errorf("DSN = %q", cfg.Database.DSN)
	}
	if cfg.Cache.Path != ".sqletch/cache" {
		t.Errorf("cache default: %q", cfg.Cache.Path)
	}
	if cfg.Dir != dir {
		t.Errorf("Dir = %q, want %q", cfg.Dir, dir)
	}
	if got := cfg.Abs("db/schema.sql"); got != filepath.Join(dir, "db/schema.sql") {
		t.Errorf("Abs = %q", got)
	}
}

// TestLoad_NoEnvExpansion pins the security decision that config values
// are literal: a ${VAR} in the DSN is NOT expanded from the environment
// (removing that expansion closed a secret-exfiltration / SSRF vector
// where a cloned repo could splice the caller's env into the DSN). The
// literal must survive verbatim, and the referenced variable — even
// when set — must have no effect.
func TestLoad_NoEnvExpansion(t *testing.T) {
	t.Setenv("SQLETCH_TEST_CONFIG_DSN", "postgres://attacker.example/db")
	dir := t.TempDir()
	y := strings.Replace(validYAML,
		"dsn: postgres://x", "dsn: postgres://x:${SQLETCH_TEST_CONFIG_DSN}@h/db", 1)
	cfg, diags := Load(write(t, dir, "sqletch.yaml", y))
	if len(diags) != 0 {
		t.Fatalf("diags: %+v", diags)
	}
	if cfg.Database.DSN != "postgres://x:${SQLETCH_TEST_CONFIG_DSN}@h/db" {
		t.Errorf("env expansion must be disabled; DSN = %q", cfg.Database.DSN)
	}
}

// TestLoad_PathEscape pins the M6 fix: a committed RELATIVE cache/output
// path climbing out of the project with `..` is refused (SQLETCH306
// error) because a cloned repo could redirect writes; an ABSOLUTE path
// is a deliberate operator choice and only warns; an in-tree relative
// path is unaffected.
func TestLoad_PathEscape(t *testing.T) {
	hasCode := func(diags []diagnostics.Diagnostic, sev diagnostics.Severity) bool {
		for _, d := range diags {
			if d.Code == diagnostics.CodePathEscape && d.Severity == sev {
				return true
			}
		}
		return false
	}

	t.Run("relative escape is an error", func(t *testing.T) {
		dir := t.TempDir()
		y := strings.Replace(validYAML, "path: gen", "path: ../../evil", 1)
		_, diags := Load(write(t, dir, "sqletch.yaml", y))
		if !hasCode(diags, diagnostics.Error) {
			t.Fatalf("want SQLETCH306 error, got %+v", diags)
		}
		if !diagnostics.HasErrors(diags) {
			t.Error("a relative escape must fail the load")
		}
	})

	t.Run("absolute path only warns", func(t *testing.T) {
		dir := t.TempDir()
		abs := filepath.Join(t.TempDir(), "out")
		y := strings.Replace(validYAML, "path: gen", "path: "+abs, 1)
		_, diags := Load(write(t, dir, "sqletch.yaml", y))
		if !hasCode(diags, diagnostics.Warning) {
			t.Fatalf("want SQLETCH306 warning, got %+v", diags)
		}
		if diagnostics.HasErrors(diags) {
			t.Error("an absolute path must not fail the load")
		}
	})

	t.Run("in-tree relative is clean", func(t *testing.T) {
		dir := t.TempDir()
		y := strings.Replace(validYAML, "path: gen", "path: sub/gen", 1)
		_, diags := Load(write(t, dir, "sqletch.yaml", y))
		for _, d := range diags {
			if d.Code == diagnostics.CodePathEscape {
				t.Errorf("in-tree path must not flag: %+v", d)
			}
		}
	})

	t.Run("relative cache.path escape is an error", func(t *testing.T) {
		dir := t.TempDir()
		y := validYAML + "cache:\n  path: ../escape\n"
		_, diags := Load(write(t, dir, "sqletch.yaml", y))
		if !hasCode(diags, diagnostics.Error) {
			t.Fatalf("want SQLETCH306 error for cache.path, got %+v", diags)
		}
	})

	// A committed DIRECTORY symlink whose lexical path stays in-tree (no
	// `..`) but whose real target escapes the project must be refused:
	// this is the clone-and-run write-redirection the lexical check alone
	// misses (a cloned repo commits `link -> /outside`, then points
	// output.path/cache.path at `link/...`).
	t.Run("symlinked directory component escape is an error", func(t *testing.T) {
		for _, field := range []string{"output.path", "cache.path"} {
			t.Run(field, func(t *testing.T) {
				dir := t.TempDir()
				outside := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				var y string
				switch field {
				case "output.path":
					y = strings.Replace(validYAML, "path: gen", "path: link/gen", 1)
				case "cache.path":
					y = validYAML + "cache:\n  path: link/cache\n"
				}
				_, diags := Load(write(t, dir, "sqletch.yaml", y))
				if !hasCode(diags, diagnostics.Error) {
					t.Fatalf("want SQLETCH306 error for %s via symlinked dir, got %+v", field, diags)
				}
				if !diagnostics.HasErrors(diags) {
					t.Errorf("a symlinked-dir escape must fail the load")
				}
			})
		}
	})

	// No false positive when the PROJECT itself is legitimately reached
	// through a symlinked ancestor (macOS /tmp -> /private/tmp, a repo
	// under a symlinked home): both sides resolve into the same real
	// tree, so an in-tree relative path stays contained.
	t.Run("symlinked project root is not a false positive", func(t *testing.T) {
		real := t.TempDir()
		linkParent := t.TempDir()
		proj := filepath.Join(linkParent, "proj")
		if err := os.Symlink(real, proj); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		// Config lives in the symlinked project dir; output.path is a
		// plain in-tree subdir.
		_, diags := Load(write(t, proj, "sqletch.yaml", validYAML))
		for _, d := range diags {
			if d.Code == diagnostics.CodePathEscape {
				t.Errorf("in-tree path under a symlinked project root must not flag: %+v", d)
			}
		}
	})
}

// For SQLite, database.dsn is a file path that generate/check creates
// and opens, so it is subject to the same clone-and-run escape policy as
// output.path/cache.path. The URI spellings are exempt; server-dialect
// DSNs (connection URLs) are never path-checked.
func TestLoad_SQLiteDSNPathEscape(t *testing.T) {
	hasEscapeError := func(diags []diagnostics.Diagnostic) bool {
		for _, d := range diags {
			if d.Code == diagnostics.CodePathEscape && d.Severity == diagnostics.Error {
				return true
			}
		}
		return false
	}
	sqliteYAML := func(dsn string) string {
		return "version: 1\ndialect: sqlite\nserver_version: \"3.50\"\n" +
			"database:\n  dsn: " + dsn + "\n" +
			"schema:\n  files: [db/schema.sql]\nqueries: [queries/*.sql]\n" +
			"output:\n  package: gen\n  path: gen\n"
	}

	t.Run("relative escape is an error", func(t *testing.T) {
		dir := t.TempDir()
		_, diags := Load(write(t, dir, "sqletch.yaml", sqliteYAML("../../evil.db")))
		if !hasEscapeError(diags) {
			t.Fatalf("want SQLETCH306 error for sqlite database.dsn, got %+v", diags)
		}
	})

	t.Run("symlinked directory escape is an error", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		_, diags := Load(write(t, dir, "sqletch.yaml", sqliteYAML("link/dev.sqlite")))
		if !hasEscapeError(diags) {
			t.Fatalf("want SQLETCH306 error for sqlite dsn via symlinked dir, got %+v", diags)
		}
	})

	t.Run("URI spellings are exempt", func(t *testing.T) {
		for _, dsn := range []string{":memory:", "file:dev.db?mode=memory"} {
			dir := t.TempDir()
			_, diags := Load(write(t, dir, "sqletch.yaml", sqliteYAML(dsn)))
			for _, d := range diags {
				if d.Code == diagnostics.CodePathEscape {
					t.Errorf("dsn %q must be exempt from the path check: %+v", dsn, d)
				}
			}
		}
	})

	t.Run("in-tree relative dsn is clean", func(t *testing.T) {
		dir := t.TempDir()
		_, diags := Load(write(t, dir, "sqletch.yaml", sqliteYAML("db/dev.sqlite")))
		for _, d := range diags {
			if d.Code == diagnostics.CodePathEscape {
				t.Errorf("in-tree sqlite dsn must not flag: %+v", d)
			}
		}
	})
}

func TestLoad_UnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", validYAML+"typo_key: true\n")
	_, diags := Load(path)
	if len(diags) == 0 || diags[0].Code != diagnostics.CodeConfigParse {
		t.Fatalf("want SQLETCH300 for unknown key, got %+v", diags)
	}
}

// TestLoad_DuplicateKey pins the strict-decode property the yaml.v3 →
// goccy/go-yaml migration relies on: duplicate map keys are rejected
// by goccy's default (AllowDuplicateMapKey is the opt-out we must
// never pass), matching yaml.v3's behavior.
func TestLoad_DuplicateKey(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", validYAML+"version: 1\n")
	_, diags := Load(path)
	if len(diags) == 0 || diags[0].Code != diagnostics.CodeConfigParse {
		t.Fatalf("want SQLETCH300 for duplicate key, got %+v", diags)
	}
}

func TestLoad_Validation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // message fragment
	}{
		{"missing version", strings.Replace(validYAML, "version: 1\n", "", 1), "version"},
		{"wrong dialect", strings.Replace(validYAML, "dialect: postgres", "dialect: oracle", 1), "dialect"},
		{"missing server_version", strings.Replace(validYAML, "server_version: \"16\"\n", "", 1), "server_version"},
		{"missing schema", strings.Replace(validYAML, "schema:\n  files: [db/schema.sql]\n", "", 1), "schema.files"},
		{"missing queries", strings.Replace(validYAML, "queries: [queries/*.sql]\n", "", 1), "queries"},
		{"missing output package", strings.Replace(validYAML, "  package: gen\n", "", 1), "output.package"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := write(t, dir, "sqletch.yaml", tt.yaml)
			_, diags := Load(path)
			found := false
			for _, d := range diags {
				if d.Code == diagnostics.CodeConfigInvalid && strings.Contains(d.Message, tt.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("want SQLETCH301 mentioning %q, got %+v", tt.want, diags)
			}
		})
	}
}

func TestLoad_TreeCaps(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", validYAML)
	cfg, diags := Load(path)
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if cfg.TreeCaps.MaxNodes != 32 || cfg.TreeCaps.MaxDepth != 8 {
		t.Errorf("default caps = %+v", cfg.TreeCaps)
	}

	path2 := write(t, dir, "custom.yaml", validYAML+"filter_tree_caps:\n  max_nodes: 64\n  max_depth: 12\n")
	cfg2, diags := Load(path2)
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if cfg2.TreeCaps.MaxNodes != 64 || cfg2.TreeCaps.MaxDepth != 12 {
		t.Errorf("custom caps = %+v", cfg2.TreeCaps)
	}

	path3 := write(t, dir, "bad.yaml", validYAML+"filter_tree_caps:\n  max_nodes: -1\n")
	if _, diags := Load(path3); !hasConfigCode(diags, diagnostics.CodeConfigInvalid) {
		t.Errorf("negative caps must be SQLETCH301, got %+v", diags)
	}
}

// verification.max_shapes is the shape budget `check --exhaustive`
// spends; unset means the default, and the value must be raisable so a
// query whose shape space is large is verifiable at all.
func TestLoad_VerificationMaxShapes(t *testing.T) {
	dir := t.TempDir()
	cfg, diags := Load(write(t, dir, "sqletch.yaml", validYAML))
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if cfg.Verification.MaxShapes != DefaultVerificationMaxShapes {
		t.Errorf("default verification.max_shapes = %d, want %d",
			cfg.Verification.MaxShapes, DefaultVerificationMaxShapes)
	}

	cfg2, diags := Load(write(t, dir, "custom.yaml", validYAML+"verification:\n  max_shapes: 100000\n"))
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if cfg2.Verification.MaxShapes != 100000 {
		t.Errorf("custom verification.max_shapes = %d, want 100000", cfg2.Verification.MaxShapes)
	}

	bad := write(t, dir, "bad.yaml", validYAML+"verification:\n  max_shapes: -1\n")
	if _, diags := Load(bad); !hasConfigCode(diags, diagnostics.CodeConfigInvalid) {
		t.Errorf("negative verification.max_shapes must be SQLETCH301, got %+v", diags)
	}
}

func hasConfigCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestExpandGlobs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries/b.sql", "x")
	write(t, dir, "queries/a.sql", "x")
	cfg := Config{Dir: dir}

	paths, err := cfg.ExpandGlobs([]string{"queries/*.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "a.sql") {
		t.Errorf("paths = %v (must be sorted)", paths)
	}
	if _, err := cfg.ExpandGlobs([]string{"nope/*.sql"}); err == nil {
		t.Error("empty glob must be an error")
	}
}

func TestNullOverridesFor(t *testing.T) {
	yes := true
	cfg := Config{Overrides: []Override{
		{Query: "Q", Column: "c", Nullable: &yes},
		{Query: "Other", Column: "d", Nullable: &yes},
	}}
	got := cfg.NullOverridesFor("Q")
	if len(got) != 1 || got["c"] != true {
		t.Errorf("overrides = %v", got)
	}
	if cfg.NullOverridesFor("None") != nil {
		t.Error("no overrides must yield nil")
	}
}

func TestLoad_OracleBackend(t *testing.T) {
	mysqlYAML := strings.Replace(strings.Replace(validYAML,
		"dialect: postgres", "dialect: mysql", 1),
		"server_version: \"16\"", "server_version: \"8.4\"", 1)
	noDSN := func(y string) string {
		return strings.Replace(y, "database:\n  dsn: postgres://x\n", "", 1)
	}
	withOracle := func(y, backend string) string {
		return strings.Replace(y, "database:\n", "database:\n  oracle: "+backend+"\n", 1)
	}

	t.Run("defaults to server", func(t *testing.T) {
		dir := t.TempDir()
		cfg, diags := Load(write(t, dir, "sqletch.yaml", mysqlYAML))
		if len(diags) != 0 {
			t.Fatalf("unexpected diags: %+v", diags)
		}
		if cfg.Database.Oracle != OracleServer || cfg.NativeOracle() {
			t.Fatalf("default backend must be server, got %q", cfg.Database.Oracle)
		}
	})
	t.Run("native on mysql without dsn is valid", func(t *testing.T) {
		dir := t.TempDir()
		y := "database:\n  oracle: native\n" + noDSN(mysqlYAML)
		cfg, diags := Load(write(t, dir, "sqletch.yaml", y))
		if len(diags) != 0 {
			t.Fatalf("unexpected diags: %+v", diags)
		}
		if !cfg.NativeOracle() {
			t.Fatal("NativeOracle() must report true")
		}
	})

	invalid := []struct {
		name string
		yaml string
		want string
	}{
		{"native on postgres", "database:\n  oracle: native\n" + noDSN(validYAML), "only available for dialect \"mysql\""},
		{"native with dsn", withOracle(mysqlYAML, "native"), "database.dsn is meaningless"},
		{"unknown backend", withOracle(mysqlYAML, "quantum"), "must be \"server\" or \"native\""},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			_, diags := Load(write(t, dir, "sqletch.yaml", tt.yaml))
			found := false
			for _, d := range diags {
				if d.Code == diagnostics.CodeConfigInvalid && strings.Contains(d.Message, tt.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("want SQLETCH301 mentioning %q, got %+v", tt.want, diags)
			}
		})
	}
}
