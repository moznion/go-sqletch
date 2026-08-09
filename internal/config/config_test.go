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
  dsn: ${SQLETCH_TEST_CONFIG_DSN}
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

func TestLoad_ValidWithEnvExpansion(t *testing.T) {
	t.Setenv("SQLETCH_TEST_CONFIG_DSN", "postgres://x")
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", validYAML)
	cfg, diags := Load(path)
	if len(diags) != 0 {
		t.Fatalf("diags: %+v", diags)
	}
	if cfg.Database.DSN != "postgres://x" {
		t.Errorf("env expansion: DSN = %q", cfg.Database.DSN)
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
		return strings.Replace(y, "database:\n  dsn: ${SQLETCH_TEST_CONFIG_DSN}\n", "", 1)
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
			t.Setenv("SQLETCH_TEST_CONFIG_DSN", "user:pass@tcp(h:3306)/db")
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
