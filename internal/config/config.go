// Package config loads and validates sqletch.yaml. Strict decoding:
// unknown keys are errors, required fields are named in messages.
// See docs/design/07-cli-config.md.
package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/lexer"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// maxConfigBytes bounds the sqletch.yaml a run will read. A config file
// is authored by hand and never legitimately large; the cap is
// deliberately generous (real configs are a few KB) yet forecloses an
// attacker-planted giant file OOMing the process on `sqletch lsp`
// workspace-open or in CI.
const maxConfigBytes = 1 << 20 // 1 MiB

// maxExpandedNodes bounds the node count a YAML document may expand to
// once anchors/aliases are resolved. YAML alias fan-out is exponential
// ("billion laughs"): a ~600-byte file can name ~1e9 nodes and hang the
// decoder for tens of seconds, and DisallowUnknownField does NOT
// short-circuit amplification aimed at a KNOWN typed field
// (static_expansion.queries). The cap is far above any realistic flat
// config (bounded by maxConfigBytes to well under 1e6 nodes) yet
// rejects a bomb after O(input) work, before any expansion happens.
const maxExpandedNodes = 1 << 20

// maxNestingDepth bounds the structural nesting depth a YAML document may
// reach — BOTH flow-collection nesting (open `[`/`{` tokens) and compact
// single-line block-context nesting (a run of block indicators `- `/`? ` at
// strictly increasing columns). This is a SECOND, independent DoS vector
// from alias fan-out: goccy/go-yaml's parser is superlinear (≈O(n^2)) in
// MEMORY in nesting depth alone, with no aliases involved. A deeply nested
// document sized well UNDER maxConfigBytes drives the parser (or, once the
// parse fails, the error formatter) to gigabytes and OOM-kills the process —
// and the alias guard cannot help, because boundYAMLExpansion must itself
// call parser.ParseBytes first, so the parse IS the blow-up. The depth is
// therefore checked by a linear-time pre-scan over goccy's OWN lexer BEFORE
// any goccy parse touches the bytes (the billion-laughs guard included).
// Real configs nest only a few levels; the cap is deliberately generous so
// an ordinary document is never rejected, yet forecloses the OOM.
//
// BOTH flow AND compact block nesting cost ~1–2 bytes per level and so
// reach a pathological depth under the size cap, so both are counted. Only
// INDENTATION-based (multi-line) block nesting is left unbounded here: each
// extra indented level costs one more leading space on every subsequent
// line, so a document's indentation depth is bounded by ~sqrt(2·len) ≈ 1400
// at the size cap, which goccy parses in linear time and a few milliseconds
// (measured). The earlier claim that ALL block depth is sqrt-bounded was
// FALSE — it holds only for the indentation form, not the compact
// single-line `- - - … x` / `? ? ? … x` forms, which nest one level per two
// bytes and were a depth-guard BYPASS until counted here.
const maxNestingDepth = 100

type Config struct {
	Version       int          `yaml:"version"`
	Dialect       string       `yaml:"dialect"`
	ServerVersion string       `yaml:"server_version"`
	Database      Database     `yaml:"database"`
	Schema        Schema       `yaml:"schema"`
	Queries       []string     `yaml:"queries"`
	Output        Output       `yaml:"output"`
	Cache         Cache        `yaml:"cache"`
	Overrides     []Override   `yaml:"overrides"`
	Expansion     Expansion    `yaml:"static_expansion"`
	Verification  Verification `yaml:"verification"`
	TreeCaps      TreeCaps     `yaml:"filter_tree_caps"`
	Policies      []Policy     `yaml:"policies"`

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

// Verification bounds the work `check --exhaustive` will do. It is a
// config key rather than a flag because it decides whether a CI gate
// passes: a project's verification budget must be the same on every
// machine that runs the check, not a property of who typed the command.
type Verification struct {
	MaxShapes int `yaml:"max_shapes"`
}

// DefaultVerificationMaxShapes is the shape budget `check --exhaustive`
// gets when the config says nothing.
const DefaultVerificationMaxShapes = 4096

// TreeCaps bounds @filter-tree values at runtime; the values are baked
// into generated code.
type TreeCaps struct {
	MaxNodes int `yaml:"max_nodes"`
	MaxDepth int `yaml:"max_depth"`
}

// Policy declares one cross-query policy (spec §"Cross-Query
// Policies"): a predicate woven at compile time into every query that
// touches a designated table, plus the enforcement that no reachable
// shape goes unscoped. Shape checks that need the dialect (predicate
// probing, identifier rules) live in policy.Validate; Load checks only
// the config-level vocabulary.
type Policy struct {
	Name      string      `yaml:"name"`
	Tables    []string    `yaml:"tables"`
	Predicate string      `yaml:"predicate"`
	Param     PolicyParam `yaml:"param"`
	// AppliesTo restricts the statement kinds the policy covers;
	// empty means select, update, and delete. INSERT … VALUES is
	// never a policy target (no rows are filtered).
	AppliesTo []string `yaml:"applies_to"`
}

// PolicyParam declares the policy predicate's parameter. Type is
// required on Tier 2 dialects (their oracles cannot type parameter
// slots) and asserted like a `-- @param` hint on Tier 1.
type PolicyParam struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

func (c Config) Expanded(query string) bool {
	return slices.Contains(c.Expansion.Queries, query)
}

// Load reads and validates the configuration. Config values are
// literal: there is deliberately no ${VAR} environment-variable
// expansion (removed as a secret-exfiltration / SSRF vector — a cloned
// repo could otherwise splice the caller's environment, including
// secrets, into database.dsn and point it at an attacker host). An
// operator who wants the dev-database DSN to come from the environment
// should leave database.dsn empty and let the driver's own libpq/DSN
// environment variables (PGHOST, MYSQL_DSN, …) take effect, or template
// the config outside sqletch.
func Load(path string) (Config, []diagnostics.Diagnostic) {
	span := diagnostics.Span{File: path}
	raw, err := readConfigCapped(path)
	if err != nil {
		return Config{}, []diagnostics.Diagnostic{diagnostics.Errorf(
			diagnostics.CodeConfigParse, span, "cannot read config: %v", err)}
	}

	// Bound YAML structural nesting depth BEFORE any goccy parse: goccy's
	// parser is superlinear in memory in nesting depth alone (no aliases
	// needed), so a deeply nested flow collection under the size cap can
	// OOM the process. This pre-scan is a linear-time walk over goccy's own
	// lexer and MUST precede both boundYAMLExpansion (which parses to count
	// nodes) and the decode below — either parse is the blow-up.
	if depth, over := exceedsNestingDepth(raw); over {
		return Config{}, []diagnostics.Diagnostic{diagnostics.Errorf(
			diagnostics.CodeConfigParse, span,
			"invalid config: YAML structural nesting is too deep (reached depth %d, cap %d): deeply nested flow collections ([[[…]]]) drive the YAML parser into superlinear memory and can OOM the process (a denial of service) even under the %d-byte size cap, so the document is refused before it is parsed",
			depth, maxNestingDepth, maxConfigBytes).
			WithHint("sqletch configs nest only a few levels deep; flatten the document (this cap is far above any legitimate config)")}
	}

	// Bound YAML alias expansion BEFORE decoding: a billion-laughs bomb
	// amplifies through DisallowUnknownField because it targets a known
	// typed field, so the decoder must never see it. This pre-scan is
	// O(input) — it parses the document (aliases unresolved) and sums the
	// expanded node count, rejecting before any exponential blow-up.
	if err := boundYAMLExpansion(raw); err != nil {
		return Config{}, []diagnostics.Diagnostic{diagnostics.Errorf(
			diagnostics.CodeConfigParse, span, "invalid config: %v", err)}
	}

	var cfg Config
	// DisallowUnknownField reproduces yaml.v3's KnownFields(true);
	// duplicate map keys are rejected by goccy/go-yaml's default
	// (AllowDuplicateMapKey is the opt-out), matching yaml.v3 too.
	// Format the error with yamlErrorMessage, NEVER %v/err.Error(): goccy's
	// err.Error() renders a source annotation in O(n^2) MEMORY in the
	// document's nesting depth, so a deep-but-cheap-to-parse document (a
	// compact block-sequence/complex-key bomb) turns a rejected config into
	// gigabytes of allocation — a denial of service. yamlErrorMessage omits
	// the annotation, which is microseconds and a few dozen bytes.
	if err := yaml.UnmarshalWithOptions(raw, &cfg, yaml.DisallowUnknownField()); err != nil {
		return Config{}, []diagnostics.Diagnostic{diagnostics.Errorf(
			diagnostics.CodeConfigParse, span, "invalid config: %s", yamlErrorMessage(err))}
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
	if cfg.Verification.MaxShapes == 0 {
		cfg.Verification.MaxShapes = DefaultVerificationMaxShapes
	}
	if cfg.Verification.MaxShapes < 1 {
		invalid("verification.max_shapes must be positive")
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
	badPolicy := func(format string, args ...any) {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodePolicyInvalid, span, format, args...))
	}
	for i, p := range cfg.Policies {
		id := p.Name
		if id == "" {
			id = fmt.Sprintf("policies[%d]", i)
		}
		for _, k := range p.AppliesTo {
			switch k {
			case "select", "update", "delete":
			case "insert":
				badPolicy("%s: applies_to cannot include %q — INSERT filters no rows; an INSERT … SELECT reading a designated table is rejected separately", id, k)
			default:
				badPolicy("%s: applies_to values must be select, update, or delete (got %q)", id, k)
			}
		}
		if p.Param.Type != "" && p.Param.Name == "" {
			badPolicy("%s: param.type without param.name", id)
		}
	}

	// cache.path and output.path drive every write sqletch performs.
	// A committed relative path climbing out of the project with `..`
	// is the clone-and-run write-redirection vector, so it is refused;
	// the `..` test is purely lexical, so it misses a committed DIRECTORY
	// symlink whose path stays in-tree but whose real target escapes; the
	// symlink-aware pass below closes that (a cloned repo can commit
	// `link -> /outside` and point the field at `link/...`).
	//
	// warnAbsolute distinguishes the two kinds of path this policy covers.
	// For generated-output paths an absolute path is a deliberate operator
	// choice that only WARNS ("output belongs in the repo"). For a SQLite
	// database.dsn an absolute path is entirely normal — a dev database
	// legitimately lives outside the tree (often /tmp) and is not
	// generated output — so it must be accepted silently; only the sneaky
	// in-tree-LOOKING escapes (relative `..`, symlinked directory) are the
	// committed-repo attack vector worth refusing there.
	checkPath := func(field, p string, warnAbsolute bool) {
		if p == "" {
			return
		}
		if filepath.IsAbs(p) {
			if warnAbsolute {
				diags = append(diags, diagnostics.Warnf(diagnostics.CodePathEscape, span,
					"%s %q is an absolute path: sqletch will write outside the project directory", field, p).
					WithHint("prefer a project-relative path so generated output stays inside the repository"))
			}
			return
		}
		resolved := filepath.Clean(filepath.Join(cfg.Dir, p))
		rel, err := filepath.Rel(cfg.Dir, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodePathEscape, span,
				"%s %q escapes the project directory (resolves to %q): a relative path climbing out with `..` is refused because a cloned repository could otherwise redirect writes to arbitrary locations", field, p, resolved).
				WithHint("keep %s inside the project directory", field))
			return
		}
		if real, outside := resolvesOutsideRoot(cfg.Dir, resolved); outside {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodePathEscape, span,
				"%s %q escapes the project directory through a symlink (resolves to %q): a cloned repository could otherwise commit a directory symlink to redirect writes to arbitrary locations", field, p, real).
				WithHint("keep %s inside the project directory and remove any symlinked components", field))
		}
	}
	checkPath("cache.path", cfg.Cache.Path, true)
	checkPath("output.path", cfg.Output.Path, true)
	// For SQLite, database.dsn is a FILE PATH that generate/check creates
	// and opens, so a committed in-tree-looking path that escapes the
	// project is the same clone-and-run redirection risk as the output
	// paths. But an ABSOLUTE dev-database path is a normal operator choice
	// (not generated output), so it is accepted without the absolute-path
	// warning — warning here would break the common `dsn: /abs/dev.sqlite3`
	// setup, since config-load diagnostics are fatal to the run. The URI
	// spellings (`:memory:`, `file:`) are not paths and are exempt,
	// matching cli.sqliteDSNPath; for the server dialects the DSN is a
	// connection URL and must not be path-checked at all.
	if cfg.Dialect == "sqlite" {
		if dsn := cfg.Database.DSN; dsn != "" && dsn != ":memory:" &&
			!strings.HasPrefix(dsn, "file:") {
			checkPath("database.dsn", dsn, false)
		}
	}

	return cfg, diags
}

// maxYAMLErrorLen bounds the rendered length of a goccy YAML error in a
// diagnostic. The source-annotation-free form is already tiny for the known
// bombs, but a pathological single-line message must never bloat a
// diagnostic either.
const maxYAMLErrorLen = 4096

// yamlErrorMessage renders a goccy/go-yaml parse/decode error for a
// user-facing diagnostic WITHOUT its source annotation, and NEVER via
// err.Error()/%v. goccy's err.Error() is FormatError(err, colored=false,
// inclSource=true): rendering the source annotation costs O(n^2) MEMORY in
// the document's nesting depth. A compact but deeply nested document
// (`extra: - - - … x` or `extra: ? ? ? … x`) parses cheaply yet drives that
// annotation to multiple gigabytes — the same superlinear blow-up the
// nesting pre-scan guards against, relocated into ERROR FORMATTING. The
// annotation-free FormatError(err, false, false) is microseconds and a few
// dozen bytes; the result is additionally length-capped for safety.
func yamlErrorMessage(err error) string {
	msg := yaml.FormatError(err, false, false)
	if len(msg) > maxYAMLErrorLen {
		msg = msg[:maxYAMLErrorLen] + "… (truncated)"
	}
	return msg
}

// readConfigCapped reads path but refuses more than maxConfigBytes, so a
// committed giant sqletch.yaml cannot OOM the process. It reads at most
// maxConfigBytes+1 and rejects if that ceiling is reached, so the bound
// holds even if the file grows after an initial stat (no size TOCTOU).
func readConfigCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxConfigBytes {
		return nil, fmt.Errorf("config file exceeds the %d-byte cap", maxConfigBytes)
	}
	return data, nil
}

// exceedsNestingDepth reports whether raw's flow-collection nesting depth
// exceeds maxNestingDepth, returning the depth at which the cap was first
// passed. It must run BEFORE any goccy parse: goccy's parser is superlinear
// in memory in nesting depth, so a document like `queries: [[[[ … ]]]]` —
// one byte per level, easily fitting under maxConfigBytes — can drive the
// parser to gigabytes and OOM the process (independent of the alias-fan-out
// bomb).
//
// The depth is counted over the token stream from goccy's OWN lexer
// (lexer.Tokenize), a linear-time tokenizer. Using the SAME tokenizer the
// parser is built on is what makes the count SOUND — quotes, comments, and
// `|`/`>` block scalars are classified exactly as the parser will classify
// them, so a bracket the parser treats as string content is never miscounted
// and a bracket the parser treats as structural is never masked. A
// hand-rolled byte quote/comment state machine (an earlier implementation)
// could DIVERGE from the parser and either miss a structural bracket — a
// stray unbalanced quote in a plain scalar (`server_version: a"`) put it into
// a permanent mask state that hid all later `[`/`{`, a depth-guard BYPASS —
// or over-count a literal bracket inside a `|`/`>` block scalar, a false
// reject. Neither is possible when the parser's own lexer does the
// classifying.
//
// Two nesting mechanisms are counted:
//
//   - FLOW collections: each open token (SequenceStart `[`, MappingStart `{`)
//     increments a running depth, each close token (SequenceEnd `]`,
//     MappingEnd `}`) decrements it.
//
//   - Compact single-line BLOCK nesting: a run of block-indicator tokens
//     (SequenceEntry `-`, MappingKey `?`) at STRICTLY increasing columns with
//     no value between them nests one collection level per indicator at ~2
//     bytes each (`extra: - - - … x`, `extra: ? ? ? … x`) — producing NO
//     flow-start tokens, so this form was a depth-guard BYPASS that drove the
//     decoder (and, via err.Error(), the error formatter) into the same
//     superlinear blow-up. The STRICTLY-increasing-column test is what
//     distinguishes nesting from siblings: a normal multi-item list emits a
//     value token between entries (which resets the run) and empty list items
//     (`-\n-\n`) repeat the SAME column, so a legitimate list is never
//     miscounted. Any non-indicator token, or a flow open/close, resets the
//     block run.
//
// The reported depth is flow depth plus the current block run, and the cap
// fires on whichever mechanism (or their sum) reaches it first. Only
// INDENTATION-based (multi-line) block nesting is left uncounted — it is
// linear-cost and sqrt-bounded by the size cap; see maxNestingDepth.
//
// Linearity/memory: on any of these bombs the lexer is O(len(raw)) in time
// and token count; the 1 MiB size cap (readConfigCapped) backstops the
// token-slice memory. Measured at the cap: bounded (~hundreds of ms, a few
// hundred MiB peak), versus the gigabytes/OOM the parser or error formatter
// spends on the same input.
func exceedsNestingDepth(raw []byte) (int, bool) {
	depth := 0    // running flow-collection depth
	blockRun := 0 // length of the current strictly-increasing block-indicator run
	prevCol := -1 // column of the previous block indicator in the run
	maxDepth := 0
	note := func(d int) bool {
		if d > maxDepth {
			maxDepth = d
		}
		return d > maxNestingDepth
	}
	for _, tk := range lexer.Tokenize(string(raw)) {
		switch tk.Type {
		case token.SequenceStartType, token.MappingStartType:
			depth++
			blockRun, prevCol = 0, -1
			if note(depth) {
				return depth, true
			}
		case token.SequenceEndType, token.MappingEndType:
			if depth > 0 {
				depth--
			}
			blockRun, prevCol = 0, -1
		case token.SequenceEntryType, token.MappingKeyType:
			col := -1
			if tk.Position != nil {
				col = tk.Position.Column
			}
			if col > prevCol {
				blockRun++
			} else {
				blockRun = 1
			}
			prevCol = col
			if note(depth + blockRun) {
				return depth + blockRun, true
			}
		default:
			blockRun, prevCol = 0, -1
		}
	}
	return maxDepth, false
}

// boundYAMLExpansion rejects a config whose YAML anchors/aliases expand
// beyond maxExpandedNodes ("billion laughs" DoS). It parses the document
// (goccy/go-yaml does NOT resolve aliases while parsing, so this is
// O(input)) and computes each node's expanded size, resolving every
// alias to its already-seen anchor's size — anchors always precede their
// aliases in a valid document, so a single document-order pass suffices.
// The per-node accumulation short-circuits once the cap is exceeded, so
// the computation itself stays bounded even for an astronomical bomb.
// A parse failure is left to the real decoder, which reports the precise
// syntax error.
func boundYAMLExpansion(raw []byte) error {
	file, err := parser.ParseBytes(raw, 0)
	if err != nil {
		return nil // let UnmarshalWithOptions surface the syntax error
	}
	anchors := map[string]int64{}
	for _, doc := range file.Docs {
		if expandedNodeSize(doc.Body, anchors) > maxExpandedNodes {
			return fmt.Errorf("YAML anchor/alias expansion exceeds the %d-node cap: this is the shape of a \"billion laughs\" denial-of-service (a tiny file that expands to billions of nodes); remove the alias fan-out", maxExpandedNodes)
		}
	}
	return nil
}

// expandedNodeSize returns the number of nodes n contributes once every
// alias it reaches is resolved, memoizing each anchor's size in anchors.
// The result saturates near maxExpandedNodes: aggregate nodes stop
// summing children once they pass the cap, so a bomb is detected without
// ever materializing the expansion.
func expandedNodeSize(n ast.Node, anchors map[string]int64) int64 {
	if n == nil {
		return 1
	}
	switch t := n.(type) {
	case *ast.AnchorNode:
		size := expandedNodeSize(t.Value, anchors)
		if t.Name != nil {
			if tok := t.Name.GetToken(); tok != nil {
				anchors[tok.Value] = size
			}
		}
		return size
	case *ast.AliasNode:
		if t.Value != nil {
			if tok := t.Value.GetToken(); tok != nil {
				if size, ok := anchors[tok.Value]; ok {
					return size
				}
			}
		}
		return 1
	case *ast.SequenceNode:
		size := int64(1)
		for _, v := range t.Values {
			size += expandedNodeSize(v, anchors)
			if size > maxExpandedNodes {
				return size
			}
		}
		return size
	case *ast.MappingNode:
		size := int64(1)
		for _, v := range t.Values {
			size += expandedNodeSize(v, anchors)
			if size > maxExpandedNodes {
				return size
			}
		}
		return size
	case *ast.MappingValueNode:
		return expandedNodeSize(t.Key, anchors) + expandedNodeSize(t.Value, anchors)
	case *ast.DocumentNode:
		return expandedNodeSize(t.Body, anchors)
	default:
		return 1
	}
}

// resolvesOutsideRoot reports whether target — after following symlinks in
// its existing ancestry — lands outside root, and returns the resolved real
// path. root (the project directory) is itself symlink-resolved so a project
// legitimately reached through a symlinked ancestor (macOS /tmp ->
// /private/tmp, a repo under a symlinked home) is not a false positive.
// Callers must already have ruled out a lexical `..` escape; this adds only
// the symlinked-directory-component case.
func resolvesOutsideRoot(root, target string) (string, bool) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	real := resolveExistingPrefix(target)
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return real, true
	}
	return real, false
}

// resolveExistingPrefix resolves symlinks over the longest existing prefix
// of path (the leaf usually does not exist yet — it is what sqletch is about
// to create) and rejoins the not-yet-existing remainder lexically, so a
// symlinked directory component anywhere in the existing ancestry is
// followed while a missing leaf does not defeat resolution.
func resolveExistingPrefix(path string) string {
	path = filepath.Clean(path)
	remainder := ""
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(path)
		if parent == path {
			// Reached the filesystem root with nothing resolvable; fall
			// back to the lexical join.
			return filepath.Join(path, remainder)
		}
		remainder = filepath.Join(filepath.Base(path), remainder)
		path = parent
	}
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
//
// Every match is path-escape-checked (SQLETCH306): the read paths
// (cli/commands.go, pipeline.go via os.ReadFile) consume this list
// unchecked, so a committed `queries: ["../../etc/*.conf"]` or a
// symlinked-directory glob would otherwise read arbitrary host-readable
// files on a clone-and-run `check`/`generate` and disclose them through
// catalog/scan and diagnostic excerpts. The check mirrors Load's
// write-path policy (lexical `..` plus symlinked-component resolution)
// against the project directory.
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
			if err := c.checkMatchInRoot(pat, m); err != nil {
				return nil, err
			}
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// checkMatchInRoot refuses a glob match that resolves outside the project
// directory — lexically (a `..`-climbing pattern) or through a symlinked
// directory component whose real target escapes. match is the absolute
// path filepath.Glob returned (it exists, so symlink resolution is
// exact). The error cites SQLETCH306 so the path-escape refusal is
// recognizable even though ExpandGlobs's channel is a plain error.
func (c Config) checkMatchInRoot(pat, match string) error {
	resolved := filepath.Clean(match)
	rel, err := filepath.Rel(c.Dir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s: glob %q matches %q, which escapes the project directory: a cloned repository could otherwise read arbitrary host files through queries/schema.files; keep globs inside the project directory",
			diagnostics.CodePathEscape, pat, resolved)
	}
	if real, outside := resolvesOutsideRoot(c.Dir, resolved); outside {
		return fmt.Errorf("%s: glob %q matches %q, which escapes the project directory through a symlink (resolves to %q): a cloned repository could otherwise read arbitrary host files; keep globs inside the project directory and remove any symlinked components",
			diagnostics.CodePathEscape, pat, resolved, real)
	}
	return nil
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
