// Package config loads and validates sqletch.yaml. Strict decoding:
// unknown keys are errors, required fields are named in messages.
// See docs/design/07-cli-config.md.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

type Config struct {
	Version       int        `yaml:"version"`
	Dialect       string     `yaml:"dialect"`
	ServerVersion string     `yaml:"server_version"`
	Database      Database   `yaml:"database"`
	Schema        Schema     `yaml:"schema"`
	Queries       []string   `yaml:"queries"`
	Output        Output     `yaml:"output"`
	Cache         Cache      `yaml:"cache"`
	Overrides     []Override `yaml:"overrides"`
	Expansion     Expansion  `yaml:"static_expansion"`
	TreeCaps      TreeCaps   `yaml:"filter_tree_caps"`

	// Dir is the directory containing sqletch.yaml; all relative paths
	// resolve against it. Not part of the YAML.
	Dir string `yaml:"-"`
	// Path is the config file itself, so later phases can attach
	// diagnostics to it (e.g. SQLETCH200). Not part of the YAML.
	Path string `yaml:"-"`
}

type Database struct {
	DSN string `yaml:"dsn"`
	// Oracle selects the type-oracle backend: "server" (default; a
	// dev database serves cache misses) or "native" (sqletch's own
	// corpus-validated inference — MySQL only, design 15). Strict by
	// decision D1: no fallback, and no DSN to fall back to.
	Oracle string `yaml:"oracle"`
}

// Oracle backend names.
const (
	OracleServer = "server"
	OracleNative = "native"
)

// NativeOracle reports whether the native-inference backend is
// selected.
func (c Config) NativeOracle() bool { return c.Database.Oracle == OracleNative }

type Schema struct {
	Files []string `yaml:"files"`
}

type Output struct {
	Package string `yaml:"package"`
	Path    string `yaml:"path"`
}

type Cache struct {
	Path string `yaml:"path"`
}

type Override struct {
	Query    string `yaml:"query"`
	Column   string `yaml:"column"`
	Nullable *bool  `yaml:"nullable"`
}

// Expansion configures strict static expansion: listed queries are
// materialized shape-by-shape into .sql files and dispatch to
// precomposed SQL instead of composing at runtime.
type Expansion struct {
	Queries   []string `yaml:"queries"`
	MaxShapes int      `yaml:"max_shapes"`
}

// TreeCaps bounds @filter-tree values at runtime; the values are baked
// into generated code.
type TreeCaps struct {
	MaxNodes int `yaml:"max_nodes"`
	MaxDepth int `yaml:"max_depth"`
}

func (c Config) Expanded(query string) bool {
	return slices.Contains(c.Expansion.Queries, query)
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads, env-expands, and validates the configuration.
func Load(path string) (Config, []diagnostics.Diagnostic) {
	span := diagnostics.Span{File: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, []diagnostics.Diagnostic{diagnostics.Errorf(
			diagnostics.CodeConfigParse, span, "cannot read config: %v", err)}
	}
	raw = envRe.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := envRe.FindSubmatch(m)[1]
		return []byte(os.Getenv(string(name)))
	})

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, []diagnostics.Diagnostic{diagnostics.Errorf(
			diagnostics.CodeConfigParse, span, "invalid config: %v", err)}
	}
	cfg.Dir = filepath.Dir(path)
	cfg.Path = path

	var diags []diagnostics.Diagnostic
	invalid := func(format string, args ...any) {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodeConfigInvalid, span, format, args...))
	}
	if cfg.Version != 1 {
		invalid("version must be 1 (got %d)", cfg.Version)
	}
	if cfg.Dialect != "postgres" && cfg.Dialect != "mysql" && cfg.Dialect != "sqlite" {
		invalid("dialect must be \"postgres\", \"mysql\", or \"sqlite\" (got %q)", cfg.Dialect)
	}
	if cfg.ServerVersion == "" {
		invalid("server_version is required (it pins the oracle and keys the cache)")
	}
	switch cfg.Database.Oracle {
	case "":
		cfg.Database.Oracle = OracleServer
	case OracleServer:
	case OracleNative:
		// Design 15 D1: strict native — only where no embedded real
		// engine exists (MySQL), and never silently backed by a server.
		if cfg.Dialect != "mysql" {
			invalid("database.oracle: \"native\" is only available for dialect \"mysql\" (%s has a real-engine backend; see docs/design/15-native-inference-oracle.md)", cfg.Dialect)
		}
		if cfg.Database.DSN != "" {
			invalid("database.dsn is meaningless with database.oracle: \"native\" (no server is ever contacted); remove one of the two")
		}
	default:
		invalid("database.oracle must be \"server\" or \"native\" (got %q)", cfg.Database.Oracle)
	}
	if len(cfg.Schema.Files) == 0 {
		invalid("schema.files is required (ordered globs of plain .sql files)")
	}
	if len(cfg.Queries) == 0 {
		invalid("queries is required (globs of template .sql files)")
	}
	if cfg.Output.Package == "" {
		invalid("output.package is required")
	}
	if cfg.Output.Path == "" {
		invalid("output.path is required")
	}
	if cfg.Cache.Path == "" {
		cfg.Cache.Path = ".sqletch/cache"
	}
	if cfg.Expansion.MaxShapes == 0 {
		cfg.Expansion.MaxShapes = 256
	}
	if cfg.TreeCaps.MaxNodes == 0 {
		cfg.TreeCaps.MaxNodes = 32
	}
	if cfg.TreeCaps.MaxDepth == 0 {
		cfg.TreeCaps.MaxDepth = 8
	}
	if cfg.TreeCaps.MaxNodes < 1 || cfg.TreeCaps.MaxDepth < 1 {
		invalid("filter_tree_caps values must be positive")
	}
	for i, o := range cfg.Overrides {
		if o.Query == "" || o.Column == "" || o.Nullable == nil {
			invalid("overrides[%d] needs query, column, and nullable", i)
		}
	}
	return cfg, diags
}

// Abs resolves a config-relative path.
func (c Config) Abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// ExpandGlobs resolves config-relative globs into a sorted, duplicate-
// free path list; a pattern matching nothing is an error (a typoed
// glob silently matching zero files is the classic footgun).
func (c Config) ExpandGlobs(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, pat := range patterns {
		matches, err := filepath.Glob(c.Abs(pat))
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("glob %q matches no files", pat)
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// NullOverridesFor collects the per-column nullability overrides of
// one query.
func (c Config) NullOverridesFor(query string) map[string]bool {
	var out map[string]bool
	for _, o := range c.Overrides {
		if o.Query == query && o.Nullable != nil {
			if out == nil {
				out = map[string]bool{}
			}
			out[o.Column] = *o.Nullable
		}
	}
	return out
}
