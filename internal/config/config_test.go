package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// pre-scan, in O(input), never parsed. The depth used here (200000,
// ~400 KB, still under the 1 MiB cap) is one that on the UNFIXED code is
// grossly superlinear — measured ~3s at depth 120k and OOM near the cap —
// so the deadline both proves the fix works and guarantees a regression
// fails by TIMEOUT rather than wedging (or OOMing) CI. Do not remove the
// deadline: without the pre-scan this input takes the parser to gigabytes.
func TestLoad_YAMLDeepNestingRejected(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", flowNest(200000))

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
	yaml := validYAML + "database: " + strings.Repeat("{a: ", 200000) + "1" + strings.Repeat("}", 200000) + "\n"
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
