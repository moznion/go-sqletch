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
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/moznion/sqletch/internal/diagnostics"
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

	// Dir is the directory containing sqletch.yaml; all relative paths
	// resolve against it. Not part of the YAML.
	Dir string `yaml:"-"`
}

type Database struct {
	DSN string `yaml:"dsn"`
}

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

	var diags []diagnostics.Diagnostic
	invalid := func(format string, args ...any) {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodeConfigInvalid, span, format, args...))
	}
	if cfg.Version != 1 {
		invalid("version must be 1 (got %d)", cfg.Version)
	}
	if cfg.Dialect != "postgres" {
		invalid("dialect must be %q in v0.1 (got %q)", "postgres", cfg.Dialect)
	}
	if cfg.ServerVersion == "" {
		invalid("server_version is required (it pins the oracle and keys the cache)")
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
