package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/lexer"

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

	// An absolute dev-database path is a normal operator choice (a dev DB
	// legitimately lives outside the tree, e.g. under /tmp) and is NOT
	// generated output, so unlike output.path it must NOT even warn —
	// config-load diagnostics are fatal to the run, so a warning here
	// would break the common `dsn: /abs/dev.sqlite3` setup.
	t.Run("absolute dsn is clean (no warning)", func(t *testing.T) {
		dir := t.TempDir()
		abs := filepath.Join(t.TempDir(), "dev.sqlite3")
		_, diags := Load(write(t, dir, "sqletch.yaml", sqliteYAML(abs)))
		for _, d := range diags {
			if d.Code == diagnostics.CodePathEscape {
				t.Errorf("absolute sqlite dsn must not produce a path diagnostic: %+v", d)
			}
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

// aliasBomb builds a "billion laughs" sqletch.yaml: nested YAML anchors
// whose aliases fan out so a ~600-byte file expands to ~fan^levels nodes
// during decode. The amplification targets the KNOWN typed field
// static_expansion.queries, so DisallowUnknownField does not short it.
func aliasBomb(levels, fan int) string {
	var b strings.Builder
	b.WriteString(validYAML)
	b.WriteString("static_expansion:\n  queries:\n  - &l0 \"lol\"\n")
	for i := 1; i <= levels; i++ {
		fmt.Fprintf(&b, "  - &l%d [", i)
		for j := 0; j < fan; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "*l%d", i-1)
		}
		b.WriteString("]\n")
	}
	return b.String()
}

// TestLoad_YAMLAliasBomb pins the DoS fix: a tiny alias-amplification
// config must be REFUSED quickly (SQLETCH300), never expanded. The
// deadline guarantees a regression cannot hang CI — a hung Load makes
// the test fail by timeout rather than wedging the suite.
func TestLoad_YAMLAliasBomb(t *testing.T) {
	dir := t.TempDir()
	// ~9^9 (~387M) expansion in ~500 bytes.
	path := write(t, dir, "sqletch.yaml", aliasBomb(9, 9))

	done := make(chan []diagnostics.Diagnostic, 1)
	go func() {
		_, diags := Load(path)
		done <- diags
	}()

	select {
	case diags := <-done:
		refused := false
		for _, d := range diags {
			if d.Code == diagnostics.CodeConfigParse && d.Severity == diagnostics.Error {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("alias bomb must be refused with SQLETCH300, got %+v", diags)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load hung on an alias bomb (DoS regression): it must reject before expanding")
	}
}

// TestLoad_LargeFlatConfigLoads makes sure the bomb guard does not
// reject a legitimate large-but-flat config (many queries, no aliases).
func TestLoad_LargeFlatConfigLoads(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(validYAML)
	b.WriteString("static_expansion:\n  queries:\n")
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "  - q%d\n", i)
	}
	_, diags := Load(write(t, dir, "sqletch.yaml", b.String()))
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse {
			t.Fatalf("large flat config wrongly refused: %+v", d)
		}
	}
}

// flowNest builds a sqletch.yaml whose queries value is a flow collection
// nested `depth` levels deep (`[[[ … ]]]`). One byte per level, so a deep
// document stays well under maxConfigBytes — yet goccy/go-yaml's parser is
// superlinear in MEMORY in nesting depth (no aliases involved), so parsing
// it OOM-kills the process. depth 20000 (~40 KB) allocates ~1.28 GiB;
// near-cap (~500k deep, ~1 MB) OOM-kills after ~71s. This is why the depth
// pre-scan MUST run before any goccy parse.
func flowNest(depth int) string {
	return validYAML + "queries: " + strings.Repeat("[", depth) + strings.Repeat("]", depth) + "\n"
}

// TestLoad_YAMLDeepNestingRejected pins the deep-nesting DoS fix: a deeply
// nested flow document is refused with SQLETCH300 by the raw-byte depth
// pre-scan, in O(input), never parsed. The depth used here (100000, ~200 KB,
// under the 256 KiB cap so the depth guard — not the size cap — is what fires)
// is one that on the UNFIXED code is grossly superlinear — measured ~3s at
// depth 120k and OOM near the old cap — so the deadline both proves the fix
// works and guarantees a regression fails by TIMEOUT rather than wedging (or
// OOMing) CI. Do not remove the deadline: without the pre-scan this input
// takes the parser to gigabytes.
func TestLoad_YAMLDeepNestingRejected(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", flowNest(100000))

	done := make(chan []diagnostics.Diagnostic, 1)
	go func() {
		_, diags := Load(path)
		done <- diags
	}()

	select {
	case diags := <-done:
		refused := false
		for _, d := range diags {
			if d.Code == diagnostics.CodeConfigParse && d.Severity == diagnostics.Error &&
				strings.Contains(d.Message, "nesting is too deep") {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("deep-nesting doc must be refused with SQLETCH300 (nesting too deep), got %+v", diags)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Load did not reject a deeply nested doc within 2s (deep-nesting OOM DoS regression): the raw-byte depth pre-scan must refuse it before any goccy parse")
	}
}

// TestLoad_YAMLDeepNestingCurlyRejected pins that the pre-scan also bounds
// flow-MAPPING nesting ({{{ … }}}), not just sequences.
func TestLoad_YAMLDeepNestingCurlyRejected(t *testing.T) {
	dir := t.TempDir()
	yaml := validYAML + "database: " + strings.Repeat("{a: ", 40000) + "1" + strings.Repeat("}", 40000) + "\n"
	path := write(t, dir, "sqletch.yaml", yaml)

	done := make(chan []diagnostics.Diagnostic, 1)
	go func() {
		_, diags := Load(path)
		done <- diags
	}()

	select {
	case diags := <-done:
		refused := false
		for _, d := range diags {
			if d.Code == diagnostics.CodeConfigParse && strings.Contains(d.Message, "nesting is too deep") {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("deep flow-mapping doc must be refused with SQLETCH300, got %+v", diags)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Load did not reject a deeply nested flow-mapping doc within 2s")
	}
}

// TestLoad_ModestNestingLoads makes sure the depth cap does not reject an
// ordinary config that uses a handful of flow-collection levels and string
// values that merely CONTAIN brackets — the pre-scan must mask brackets
// inside quoted strings and comments, and tolerate balanced brackets in
// plain scalars, so a real config still loads.
func TestLoad_ModestNestingLoads(t *testing.T) {
	dir := t.TempDir()
	yaml := `version: 1
dialect: postgres
server_version: "16" # a comment with a [bracket] { that must not count
database:
  dsn: "postgres://x?opt=[a,b,c]"
schema:
  files: [db/schema.sql]
queries: [queries/*.sql, "lit[0][1][2]"]
output:
  package: gen
  path: gen
overrides:
  - {query: q, column: c, nullable: true}
`
	_, diags := Load(write(t, dir, "sqletch.yaml", yaml))
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse {
			t.Fatalf("modestly nested config wrongly refused: %+v", d)
		}
	}
}

// TestLoad_YAMLStrayQuoteBypassRejected pins the HIGH bypass fix: a stray
// unbalanced quote in a plain scalar (`server_version: a"`) must NOT hide
// the deep flow nesting on a later line. goccy's parser treats those
// brackets as STRUCTURAL (the quote is part of a plain scalar, not a string
// delimiter), so if the depth guard is fooled into masking them Load falls
// through to the superlinear parse the guard exists to stop. The guard must
// classify tokens exactly as goccy's own lexer does, so the nesting is seen
// and the doc refused with SQLETCH300 fast. The deadline both proves the fix
// and makes a regression fail by TIMEOUT rather than OOMing CI.
func TestLoad_YAMLStrayQuoteBypassRejected(t *testing.T) {
	dir := t.TempDir()
	// A stray double-quote in a plain scalar, then a deep flow collection on
	// the next line. On the fooled guard the brackets are invisible and the
	// parse blows up; the fix sees depth ~100000 and refuses.
	yaml := "version: 1\ndialect: postgres\nserver_version: a\"\n" +
		"queries: " + strings.Repeat("[", 100000) + strings.Repeat("]", 100000) + "\n"
	path := write(t, dir, "sqletch.yaml", yaml)

	done := make(chan []diagnostics.Diagnostic, 1)
	go func() {
		_, diags := Load(path)
		done <- diags
	}()

	select {
	case diags := <-done:
		refused := false
		for _, d := range diags {
			if d.Code == diagnostics.CodeConfigParse && d.Severity == diagnostics.Error &&
				strings.Contains(d.Message, "nesting is too deep") {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("stray-quote deep-nesting doc must be refused with SQLETCH300 (nesting too deep), got %+v", diags)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Load did not reject the stray-quote deep-nesting bypass within 2s: a stray quote must not mask structural flow brackets from the depth guard")
	}
}

// TestLoad_YAMLBlockScalarBracketsLoad pins the LOW false-reject fix: a
// VALID config whose value is a `|` block scalar containing many literal
// `[` must LOAD — those brackets are string content, not flow-collection
// opens, so counting them structurally wrongly rejected the document. The
// depth guard must skip block-scalar bodies (goccy's lexer emits them as a
// single string token), so 200 literal `[` inside a block scalar do not
// trip the cap.
func TestLoad_YAMLBlockScalarBracketsLoad(t *testing.T) {
	dir := t.TempDir()
	// server_version is a string field; give it a `|` block scalar whose
	// 200 lines each carry unbalanced `[` — far past maxNestingDepth if the
	// guard miscounted them structurally. The document is otherwise valid
	// and must load with no SQLETCH300 nesting refusal.
	yaml := "version: 1\ndialect: postgres\nserver_version: |\n" +
		strings.Repeat("  [[[[[ literal brackets in a block scalar\n", 200) +
		"database:\n  dsn: postgres://x\nschema:\n  files: [db/schema.sql]\n" +
		"queries: [queries/*.sql]\noutput:\n  package: gen\n  path: gen\n"
	path := write(t, dir, "sqletch.yaml", yaml)
	_, diags := Load(path)
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse && strings.Contains(d.Message, "nesting is too deep") {
			t.Fatalf("valid block-scalar config wrongly refused for nesting depth: %+v", d)
		}
	}
}

// TestExceedsNestingDepth unit-tests the raw-byte pre-scan directly: it
// must count only structural (unquoted, uncommented) brackets, and must
// not run away on a legitimate scalar that contains brackets.
func TestExceedsNestingDepth(t *testing.T) {
	deep := strings.Repeat("[", maxNestingDepth+5)
	cases := []struct {
		name string
		in   string
		over bool
	}{
		{"flat", "queries: [a, b, c]", false},
		{"nested-ok", "queries: " + strings.Repeat("[", maxNestingDepth) + strings.Repeat("]", maxNestingDepth), false},
		{"too-deep", "queries: " + deep, true},
		{"curly-too-deep", "x: " + strings.Repeat("{", maxNestingDepth+1), true},
		{"brackets-in-double-quote", `x: "` + deep + `"`, false},
		{"brackets-in-single-quote", "x: '" + deep + "'", false},
		{"brackets-in-comment", "x: 1 # " + deep, false},
		{"balanced-plain-scalar", "x: a[0][1][2][3]", false},
		{"escaped-single-quote", "x: 'it''s [fine]'", false},
		// HIGH: a stray quote in a plain scalar must NOT mask the deep
		// nesting that follows — goccy parses those brackets structurally.
		{"stray-quote-then-deep", "server_version: a\"\nqueries: " + deep, true},
		{"stray-single-quote-then-deep", "server_version: a'\nqueries: " + deep, true},
		// LOW: literal brackets inside a `|`/`>` block scalar are string
		// content and must not be counted structurally.
		{"block-literal-brackets", "server_version: |\n" + strings.Repeat("  "+deep+"\n", 3), false},
		{"block-folded-brackets", "server_version: >\n" + strings.Repeat("  "+deep+"\n", 3), false},
		// RELOCATED DoS: compact single-line block nesting emits no flow-start
		// tokens but still nests one level per indicator — it must be counted.
		{"compact-block-seq-too-deep", "extra: " + strings.Repeat("- ", maxNestingDepth+5) + "x", true},
		{"compact-complex-key-too-deep", "extra: " + strings.Repeat("? ", maxNestingDepth+5) + "x", true},
		{"compact-block-seq-shallow", "extra: - - - x", false},
		// A normal multi-item list nests only one level: a value token between
		// entries resets the run, so many siblings never accumulate depth.
		{"flat-block-list", "extra:\n" + strings.Repeat("  - item\n", maxNestingDepth+50), false},
		// Empty list items repeat the SAME column, not strictly increasing —
		// they are siblings, not nesting, and must not be counted.
		{"empty-list-items", "extra:\n" + strings.Repeat("-\n", maxNestingDepth+50), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, over := exceedsNestingDepth([]byte(tc.in)); over != tc.over {
				t.Errorf("exceedsNestingDepth(%q) over = %v, want %v", tc.in, over, tc.over)
			}
		})
	}
}

// TestExpandGlobs_PathEscape pins the MEDIUM fix: a glob whose matches
// resolve outside the project directory is refused (SQLETCH306), so a
// clone-and-run cannot read arbitrary host files through queries/
// schema.files. An in-tree glob still expands.
func TestExpandGlobs_PathEscape(t *testing.T) {
	parent := t.TempDir()
	// A file the attacker wants to read, OUTSIDE the project dir.
	if err := os.WriteFile(filepath.Join(parent, "secret.conf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Dir: dir}

	t.Run("relative escaping glob is refused", func(t *testing.T) {
		_, err := cfg.ExpandGlobs([]string{"../secret*.conf"})
		if err == nil {
			t.Fatal("a glob escaping the project directory must be refused")
		}
		if !strings.Contains(err.Error(), string(diagnostics.CodePathEscape)) {
			t.Errorf("error must cite SQLETCH306, got %v", err)
		}
	})

	t.Run("symlinked directory escape is refused", func(t *testing.T) {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(parent, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		_, err := cfg.ExpandGlobs([]string{"link/secret*.conf"})
		if err == nil {
			t.Fatal("a glob escaping through a symlinked directory must be refused")
		}
		if !strings.Contains(err.Error(), string(diagnostics.CodePathEscape)) {
			t.Errorf("error must cite SQLETCH306, got %v", err)
		}
	})

	t.Run("in-tree glob still expands", func(t *testing.T) {
		write(t, dir, "queries/a.sql", "x")
		write(t, dir, "queries/b.sql", "x")
		paths, err := cfg.ExpandGlobs([]string{"queries/*.sql"})
		if err != nil {
			t.Fatalf("in-tree glob must succeed: %v", err)
		}
		if len(paths) != 2 {
			t.Errorf("paths = %v", paths)
		}
	})
}

// blockSeqBomb builds a config whose value is a COMPACT single-line block
// sequence nested `depth` levels deep (`extra: - - - … x`). Each `- ` nests
// one collection level at 2 bytes, so ~500k levels fit under the 1 MiB cap —
// yet this form emits NO flow-start tokens, so before the fix the flow-only
// depth guard returned over=false (a BYPASS) and the invalid document fell
// through to yaml.UnmarshalWithOptions, whose err.Error() renders a source
// annotation in O(n^2) MEMORY in depth (measured 40k→437MiB, 80k→1687MiB —
// superlinear; ~1 MB cap → ~264 GB → OOM). The depth guard now counts this
// compact block nesting, and Load formats the error without the annotation.
func blockSeqBomb(depth int) string {
	return validYAML + "extra: " + strings.Repeat("- ", depth) + "x\n"
}

// complexKeyBomb is blockSeqBomb's complex-mapping-key twin (`extra: ? ? ? …
// x`): the `? ` complex-key indicator nests one mapping level per two bytes,
// the same compact-block DoS via a different indicator token.
func complexKeyBomb(depth int) string {
	return validYAML + "extra: " + strings.Repeat("? ", depth) + "x\n"
}

// loadAsync runs Load(path) with a deadline so a superlinear/OOM regression
// fails the test by TIMEOUT rather than wedging or OOM-killing the runner.
func loadAsync(t *testing.T, path string, deadline time.Duration) []diagnostics.Diagnostic {
	t.Helper()
	done := make(chan []diagnostics.Diagnostic, 1)
	go func() {
		_, diags := Load(path)
		done <- diags
	}()
	select {
	case diags := <-done:
		return diags
	case <-time.After(deadline):
		t.Fatalf("Load did not return within %s (superlinear/OOM DoS regression)", deadline)
		return nil
	}
}

func refusedNestingTooDeep(diags []diagnostics.Diagnostic) bool {
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse && d.Severity == diagnostics.Error &&
			strings.Contains(d.Message, "nesting is too deep") {
			return true
		}
	}
	return false
}

// TestLoad_YAMLBlockSeqBombRejected pins the RELOCATED-DoS fix (layer 2, the
// depth guard): a compact single-line block-sequence bomb near the size cap
// must be refused with SQLETCH300 (nesting too deep) by the raw-byte
// pre-scan, in O(input), never handed to the decoder. On the UNFIXED code
// this form bypassed the flow-only guard (over=false) and reached
// UnmarshalWithOptions + err.Error(), which is grossly superlinear and OOMs
// near the cap — so the deadline both proves the fix and makes a regression
// fail by TIMEOUT rather than OOMing CI.
func TestLoad_YAMLBlockSeqBombRejected(t *testing.T) {
	dir := t.TempDir()
	// ~200 KB (under the 256 KiB cap so the depth guard, not the size cap,
	// fires): ~100k nesting levels.
	path := write(t, dir, "sqletch.yaml", blockSeqBomb(100000))
	if !refusedNestingTooDeep(loadAsync(t, path, 2*time.Second)) {
		t.Fatal("near-cap compact block-sequence bomb must be refused with SQLETCH300 (nesting too deep)")
	}
}

// TestLoad_YAMLComplexKeyBombRejected pins the same for the compact
// complex-key form (`extra: ? ? ? … x`).
func TestLoad_YAMLComplexKeyBombRejected(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", complexKeyBomb(100000))
	if !refusedNestingTooDeep(loadAsync(t, path, 2*time.Second)) {
		t.Fatal("near-cap compact complex-key bomb must be refused with SQLETCH300 (nesting too deep)")
	}
}

// TestLoad_BlockBombBoundedAlloc pins that Load's cost on the block-sequence
// bomb is BOUNDED (linear), not the O(n^2) of the unfixed code. It measures
// allocation at two depths on the OLD superlinear curve (20k→437MiB,
// 40k→1687MiB before the fix) and asserts BOTH that each stays under a
// generous linear ceiling AND that doubling the depth does not ~quadruple
// the allocation. On the unfixed code either fixed ceiling is blown; this is
// the canary that a regression cannot OOM the runner because these depths
// survive even superlinearly.
func TestLoad_BlockBombBoundedAlloc(t *testing.T) {
	dir := t.TempDir()
	measure := func(depth int) uint64 {
		path := write(t, dir, "sqletch.yaml", blockSeqBomb(depth))
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		if !refusedNestingTooDeep(loadAsync(t, path, 2*time.Second)) {
			t.Fatalf("block bomb depth=%d must be refused with SQLETCH300", depth)
		}
		runtime.ReadMemStats(&m1)
		return (m1.TotalAlloc - m0.TotalAlloc) / (1 << 20) // MiB
	}
	small := measure(20000)
	large := measure(40000)
	// Generous linear ceiling: the fixed guard uses a few MiB here; the
	// unfixed err.Error() path used 437/1687 MiB at these depths.
	const ceilingMiB = 128
	if large > ceilingMiB {
		t.Fatalf("Load allocated %d MiB at depth 40000 (cap %d MiB): O(n^2) error-formatting DoS regression (unfixed ≈1687 MiB)", large, ceilingMiB)
	}
	// Doubling depth must not ~quadruple allocation (superlinear witness).
	if small > 0 && large > 4*small {
		t.Fatalf("Load allocation grew superlinearly: %d MiB at 20000 → %d MiB at 40000 (> 4x)", small, large)
	}
}

// TestYAMLErrorMessageBounded is the DIRECT test of the primary fix: the
// annotation-free yamlErrorMessage is small and fast on a deeply nested
// invalid document, while goccy's err.Error() (the old %v path) renders the
// source annotation in O(n^2) memory and is dramatically larger. This pins
// that Load must never format a goccy error with %v/err.Error().
func TestYAMLErrorMessageBounded(t *testing.T) {
	// Deep enough that err.Error()'s annotation is unmistakably larger, but
	// small enough that computing err.Error() once here stays quick.
	var cfg Config
	err := yaml.UnmarshalWithOptions([]byte(blockSeqBomb(30000)), &cfg, yaml.DisallowUnknownField())
	if err == nil {
		t.Fatal("a compact block-sequence bomb must fail to decode")
	}

	start := time.Now()
	msg := yamlErrorMessage(err)
	elapsed := time.Since(start)

	if len(msg) > maxYAMLErrorLen+len("… (truncated)") {
		t.Fatalf("yamlErrorMessage is not length-bounded: %d bytes", len(msg))
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("yamlErrorMessage took %s (must be sub-annotation, near-instant)", elapsed)
	}
	// The old path (err.Error() == FormatError with source annotation) must
	// be far larger — this documents the amplifier the fix removes.
	if full := err.Error(); len(full) <= len(msg)*4 {
		t.Fatalf("expected err.Error() (%d bytes) to dwarf the annotation-free message (%d bytes)", len(full), len(msg))
	}
}

// manyKeysBomb builds a config whose body is a FLAT block map of `keys`
// entries (`k0: 1\nk1: 1\n…`). Every entry sits at depth 0/1, so the document
// sails past maxNestingDepth, and each entry is only ~7 bytes, so ~150k keys
// fit under the 1 MiB size cap — yet goccy/go-yaml's parser builds the mapping
// in O(n^2) MEMORY in the WIDTH of the map: measured on the pre-fix code
// 20k→416ms/3.3GiB, 40k→1.35s/12.7GiB, ~170k (≈1 MB) → ~90 GB → OOM-kill.
// Neither the depth cap (nesting is shallow) nor the size cap (bytes are few)
// sees it; only the total-token cap does.
func manyKeysBomb(keys int) string {
	var b strings.Builder
	b.WriteString(validYAML)
	for i := 0; i < keys; i++ {
		fmt.Fprintf(&b, "k%d: 1\n", i)
	}
	return b.String()
}

// flatSeqBomb builds a config whose body is a FLAT block sequence of `entries`
// empty items (`extra:\n-\n-\n…`). Each `-\n` is 2 bytes, so ~450k entries fit
// under the 1 MiB size cap, all at depth 1 — yet goccy's parser is O(n^2) TIME
// in the sequence's width: measured pre-fix 50k→349ms, 100k→1.3s, 200k→5.06s,
// ~450k (≈900 KB) → ~25s hang. This is the flat-width sibling of manyKeysBomb
// on the TIME axis; the total-token cap bounds both.
func flatSeqBomb(entries int) string {
	var b strings.Builder
	b.WriteString(validYAML)
	b.WriteString("extra:\n")
	for i := 0; i < entries; i++ {
		b.WriteString("-\n")
	}
	return b.String()
}

func refusedTooComplex(diags []diagnostics.Diagnostic) bool {
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse && d.Severity == diagnostics.Error &&
			strings.Contains(d.Message, "too structurally complex") {
			return true
		}
	}
	return false
}

// TestLoad_YAMLFlatManyKeysBombRejected pins the FLAT-WIDTH DoS fix (memory
// axis): a near-cap flat block map of tens of thousands of keys must be
// refused with SQLETCH300 (too structurally complex) by the raw-byte
// total-token pre-scan, in O(input), never handed to the parser. On the
// pre-fix code this shape is depth 0/1 (guardOver=false), sails past
// maxNestingDepth AND the size cap, and reaches parser.ParseBytes /
// UnmarshalWithOptions, which build the map in O(n^2) MEMORY and OOM-kill the
// process near the cap — so the deadline both proves the fix and makes a
// regression fail by TIMEOUT rather than OOMing CI.
func TestLoad_YAMLFlatManyKeysBombRejected(t *testing.T) {
	dir := t.TempDir()
	// ~20k keys ≈ 190 KB (under the 256 KiB byte cap), ~60k tokens — so the
	// total-token cap, not the size cap, is what refuses it.
	src := manyKeysBomb(20000)
	if len(src) > maxConfigBytes {
		t.Fatalf("test bomb %d bytes exceeds the byte cap — it would be rejected by size, not tokens", len(src))
	}
	path := write(t, dir, "sqletch.yaml", src)
	if !refusedTooComplex(loadAsync(t, path, 2*time.Second)) {
		t.Fatal("near-cap flat many-keys bomb must be refused with SQLETCH300 (too structurally complex)")
	}
}

// TestLoad_YAMLFlatSeqBombRejected pins the same fix on the TIME axis: a
// near-cap flat block sequence of empty entries must be refused fast. Pre-fix
// this is depth 1 (guardOver=false) and drives parser.ParseBytes into O(n^2)
// TIME (~25s at the cap).
func TestLoad_YAMLFlatSeqBombRejected(t *testing.T) {
	dir := t.TempDir()
	// ~100k entries ≈ 200 KB (under the 256 KiB byte cap), ~100k tokens.
	src := flatSeqBomb(100000)
	if len(src) > maxConfigBytes {
		t.Fatalf("test bomb %d bytes exceeds the byte cap", len(src))
	}
	path := write(t, dir, "sqletch.yaml", src)
	if !refusedTooComplex(loadAsync(t, path, 2*time.Second)) {
		t.Fatal("near-cap flat block-sequence bomb must be refused with SQLETCH300 (too structurally complex)")
	}
}

// TestLoad_FlatBombBoundedAlloc pins that Load's cost on the flat many-keys
// bomb is BOUNDED, not the O(n^2) MEMORY of the pre-fix parser. It measures
// allocation at two key counts BOTH over the token cap (so both are rejected)
// that on the OLD code are grossly superlinear (7000 keys ≈ 440 MiB, 14000 ≈
// 1.75 GiB before the fix) yet SURVIVE even superlinearly, so a regression
// fails by an allocation-ceiling assertion rather than OOM-killing the runner.
// With the total-token cap both counts are rejected by the O(input) token scan
// for a few MiB, so BOTH the fixed ceiling AND the no-quadrupling check hold;
// on the unfixed code the ceiling is blown.
func TestLoad_FlatBombBoundedAlloc(t *testing.T) {
	dir := t.TempDir()
	measure := func(keys int) uint64 {
		path := write(t, dir, "sqletch.yaml", manyKeysBomb(keys))
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		if !refusedTooComplex(loadAsync(t, path, 2*time.Second)) {
			t.Fatalf("flat many-keys bomb keys=%d must be refused with SQLETCH300", keys)
		}
		runtime.ReadMemStats(&m1)
		return (m1.TotalAlloc - m0.TotalAlloc) / (1 << 20) // MiB
	}
	small := measure(7000)  // ~21k tokens, just over the 20k cap
	large := measure(14000) // ~42k tokens
	// Generous linear ceiling: the fixed guard tokenizes once for a few MiB;
	// the unfixed parser used ~440/1750 MiB at these counts.
	const ceilingMiB = 128
	if large > ceilingMiB {
		t.Fatalf("Load allocated %d MiB at 14000 keys (cap %d MiB): O(n^2) flat-width DoS regression (unfixed ≈1750 MiB)", large, ceilingMiB)
	}
	if small > 0 && large > 4*small {
		t.Fatalf("Load allocation grew superlinearly: %d MiB at 7000 → %d MiB at 14000 keys (> 4x)", small, large)
	}
}

// TestLoad_ExampleConfigsLoad guards against a false-reject of the shipped
// reference configs: all three examples must load with no config-parse
// diagnostic (they are the ground truth for a legitimate config).
func TestLoad_ExampleConfigsLoad(t *testing.T) {
	for _, ex := range []string{"postgres", "mysql", "sqlite"} {
		path := filepath.Join("..", "..", "examples", ex, "sqletch.yaml")
		_, diags := Load(path)
		for _, d := range diags {
			if d.Code == diagnostics.CodeConfigParse {
				t.Fatalf("example %s config wrongly refused: %+v", ex, d)
			}
		}
	}
}

// TestLoad_LargeLegitConfigLoads guards the total-token cap's margin: a
// synthetic but LEGITIMATE large config — 1000 expansion queries, 200
// overrides, 20 policies — must load without a config-parse diagnostic. Its
// token count (~5000) is reported so the margin over the cap stays visible.
func TestLoad_LargeLegitConfigLoads(t *testing.T) {
	dir := t.TempDir()
	src := largeLegitConfig()
	toks := len(lexer.Tokenize(src))
	if toks >= maxStructuralTokens {
		t.Fatalf("synthetic large-legit config has %d tokens, at/over the %d cap — the cap has too little margin over a legitimate config", toks, maxStructuralTokens)
	}
	t.Logf("large-legit config: %d bytes, %d tokens (cap %d)", len(src), toks, maxStructuralTokens)
	_, diags := Load(write(t, dir, "sqletch.yaml", src))
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse {
			t.Fatalf("large-legit config wrongly refused: %+v", d)
		}
	}
}

// TestExceedsStructuralTokens unit-tests the total-token boundary directly: a
// document with tokens just over the cap is over, one just under is not, and
// the count is over goccy's OWN token stream so it matches what the parser
// will see.
func TestExceedsStructuralTokens(t *testing.T) {
	under := manyKeysBomb(4000) // ~12k tokens, well under the cap
	if _, over := exceedsStructuralTokens([]byte(under)); over {
		t.Fatalf("a %d-token doc must be under the %d cap", len(lexer.Tokenize(under)), maxStructuralTokens)
	}
	over := manyKeysBomb(30000) // ~90k tokens, well over
	if _, ok := exceedsStructuralTokens([]byte(over)); !ok {
		t.Fatalf("a %d-token doc must exceed the %d cap", len(lexer.Tokenize(over)), maxStructuralTokens)
	}
	// The flat sequence form (time axis) is caught by the same count.
	if _, ok := exceedsStructuralTokens([]byte(flatSeqBomb(50000))); !ok {
		t.Fatal("a flat block-sequence bomb must exceed the total-token cap")
	}
}

// tabBomb builds a config whose value is a double-quoted scalar containing
// `tabs` literal TAB bytes (0x09): `extra: "\t\t…x"`. goccy/go-yaml's
// lexer.Tokenize is O(n^2) in the number of literal tabs inside a
// double-quoted scalar, and the tabs produce FEW tokens, so neither the
// total-token cap nor the depth cap can see the blow-up — Tokenize itself
// hangs before either guard runs (measured: ~300 KB of tabs hangs Load for
// >20s). The raw-byte tab pre-scan must refuse this before any goccy call.
// The size is kept comfortably under maxConfigBytes so the reject exercises
// the tab pre-scan itself, not the byte cap.
func tabBomb(tabs int) string {
	return validYAML + "extra: \"" + strings.Repeat("\t", tabs) + "x\"\n"
}

// TestLoad_TabBombRejectedFast pins the tab-tokenizer DoS fix: a
// double-quoted scalar full of literal tabs must be refused with SQLETCH300
// in O(input) by the raw-byte pre-scan, never handed to lexer.Tokenize. On
// the UNFIXED code lexer.Tokenize is O(n^2) in the tab count and hangs before
// any guard runs — so the deadline both proves the fix works and makes a
// regression fail by TIMEOUT rather than wedging CI. The bomb is sized under
// maxConfigBytes (so the byte cap is not what catches it) yet large enough
// that the unfixed tokenizer hangs well past the deadline.
func TestLoad_TabBombRejectedFast(t *testing.T) {
	dir := t.TempDir()
	src := tabBomb(200000) // ~200 KB, under the 256 KiB cap; ~O(n^2) tokenizer hang unfixed
	if len(src) > maxConfigBytes {
		t.Fatalf("tab bomb %d bytes exceeds the byte cap — it would be caught by size, not the tab scan", len(src))
	}
	path := write(t, dir, "sqletch.yaml", src)
	diags := loadAsync(t, path, 2*time.Second)
	if !refusedLiteralTab(diags) {
		t.Fatalf("tab bomb must be refused with SQLETCH300 (literal tab), got %+v", diags)
	}
}

func refusedLiteralTab(diags []diagnostics.Diagnostic) bool {
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse && d.Severity == diagnostics.Error &&
			strings.Contains(d.Message, "literal tab") {
			return true
		}
	}
	return false
}

// TestLoad_ExcessiveTabsRejected pins that a config carrying MORE than
// maxConfigTabs literal tabs is refused with SQLETCH300 — the count-threshold
// guard against goccy's superlinear tokenize path. Below the threshold a tab
// is legal (see TestLoad_TabInCommentLoads / TestLoad_TabInPredicateLoads);
// above it the document is refused before any parse. The tabs are spread
// across harmless comment lines so only the tab-count guard — not the size,
// depth, or token cap — decides.
func TestLoad_ExcessiveTabsRejected(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(validYAML)
	for i := 0; i <= maxConfigTabs; i++ { // maxConfigTabs+1 tabs total
		b.WriteString("#\tc\n")
	}
	diags := loadAsync(t, path2(t, dir, b.String()), time.Second)
	if !refusedLiteralTab(diags) {
		t.Fatalf("a config with >maxConfigTabs literal tabs must be refused with SQLETCH300, got %+v", diags)
	}
}

// TestLoad_TabInCommentLoads pins the regression fix: a legitimate tab used to
// align a comment (`# aligned<TAB>comment`) is valid YAML and MUST load. The
// earlier any-tab reject falsely refused this; the count threshold lets a
// handful of tabs through while still catching a tab bomb.
func TestLoad_TabInCommentLoads(t *testing.T) {
	dir := t.TempDir()
	src := validYAML + "# aligned\tcomment\n# another\talignment\n"
	cfg, diags := Load(path2(t, dir, src))
	if len(diags) != 0 {
		t.Fatalf("a config with tab-aligned comments must load, got %+v", diags)
	}
	if cfg.Version != 1 {
		t.Fatalf("config did not load: %+v", cfg)
	}
}

// TestLoad_TabInPredicateLoads pins that a literal tab inside a quoted scalar
// value (a policy predicate) is valid YAML and MUST load — tabs in scalar
// content are legal, and the count threshold clears a single one. A
// single-quoted scalar is used: goccy/go-yaml's strict decoder has a
// pre-existing quirk with a literal tab inside a DOUBLE-quoted scalar (it
// reports a spurious parse error unrelated to this guard), but single-quoted
// and block scalars carry a tab cleanly.
func TestLoad_TabInPredicateLoads(t *testing.T) {
	dir := t.TempDir()
	src := validYAML + "policies:\n" +
		"  - name: tenant\n" +
		"    tables: [t]\n" +
		"    predicate: '{}.tenant_id\t= :tid'\n" +
		"    param:\n" +
		"      name: tid\n" +
		"      type: bigint\n"
	cfg, diags := Load(path2(t, dir, src))
	if len(diags) != 0 {
		t.Fatalf("a config with a tab in a predicate string must load, got %+v", diags)
	}
	if len(cfg.Policies) != 1 || !strings.Contains(cfg.Policies[0].Predicate, "\t") {
		t.Fatalf("predicate tab not preserved through Load: %+v", cfg.Policies)
	}
}

func path2(t *testing.T, dir, src string) string {
	t.Helper()
	return write(t, dir, "sqletch.yaml", src)
}

// TestLoad_SizeCapBoundary pins the tightened byte cap: a document just over
// maxConfigBytes is refused with SQLETCH300 (read cap) and one just under
// loads. The padding is a YAML comment (no tabs), so only the size cap — not
// the tab scan or a structural guard — decides.
func TestLoad_SizeCapBoundary(t *testing.T) {
	dir := t.TempDir()
	base := validYAML
	// A run of '#'-comment bytes on one line inflates the file without adding
	// structural tokens, tabs, or nesting.
	padTo := func(target int) string {
		pad := target - len(base) - len("\n#\n")
		if pad < 0 {
			t.Fatalf("base config already %d bytes", len(base))
		}
		return base + "\n#" + strings.Repeat("x", pad) + "\n"
	}

	over := padTo(maxConfigBytes + 1)
	if len(over) <= maxConfigBytes {
		t.Fatalf("over-cap fixture is only %d bytes", len(over))
	}
	if diags := loadDiags(Load(path2(t, dir, over))); !hasConfigParse(diags) {
		t.Fatalf("a config over the %d-byte cap must be refused with SQLETCH300, got %+v", maxConfigBytes, diags)
	}

	under := padTo(maxConfigBytes - 1024)
	if len(under) > maxConfigBytes {
		t.Fatalf("under-cap fixture is %d bytes", len(under))
	}
	for _, d := range loadDiags(Load(path2(t, dir, under))) {
		if d.Code == diagnostics.CodeConfigParse {
			t.Fatalf("a config just under the cap wrongly refused: %+v", d)
		}
	}
}

func loadDiags(_ Config, diags []diagnostics.Diagnostic) []diagnostics.Diagnostic {
	return diags
}

func hasConfigParse(diags []diagnostics.Diagnostic) bool {
	for _, d := range diags {
		if d.Code == diagnostics.CodeConfigParse && d.Severity == diagnostics.Error {
			return true
		}
	}
	return false
}

// TestConfigSizesUnderCap documents the margin the 256 KiB cap leaves over
// every legitimate config: the three shipped examples and a synthetic large
// config (1000 queries + 200 overrides + 20 policies) are all far under the
// cap. If a plausible legitimate config could approach the cap this test —
// and the design decision behind the tighter cap — would need revisiting.
func TestConfigSizesUnderCap(t *testing.T) {
	for _, ex := range []string{"postgres", "mysql", "sqlite"} {
		path := filepath.Join("..", "..", "examples", ex, "sqletch.yaml")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Size() >= maxConfigBytes {
			t.Fatalf("example %s is %d bytes, at/over the %d cap", ex, info.Size(), maxConfigBytes)
		}
		t.Logf("example %s: %d bytes (cap %d, ~%.0fx margin)", ex, info.Size(), maxConfigBytes, float64(maxConfigBytes)/float64(info.Size()))
	}
	large := largeLegitConfig()
	if len(large) >= maxConfigBytes {
		t.Fatalf("synthetic large-legit config is %d bytes, at/over the %d cap — the cap has too little margin over a legitimate config", len(large), maxConfigBytes)
	}
	t.Logf("large-legit config: %d bytes (cap %d, ~%.0fx margin)", len(large), maxConfigBytes, float64(maxConfigBytes)/float64(len(large)))
}

// largeLegitConfig builds the synthetic large-but-legitimate config shared by
// the size-margin and load tests: 1000 expansion queries, 200 overrides, and
// 20 policies.
func largeLegitConfig() string {
	var b strings.Builder
	b.WriteString(validYAML)
	b.WriteString("static_expansion:\n  queries:\n")
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&b, "  - q%d\n", i)
	}
	b.WriteString("overrides:\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "  - {query: q%d, column: c, nullable: true}\n", i)
	}
	b.WriteString("policies:\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "  - name: p%d\n    tables: [t%d]\n    predicate: \"{}.x = :y\"\n    param:\n      name: y\n      type: bigint\n", i, i)
	}
	return b.String()
}
