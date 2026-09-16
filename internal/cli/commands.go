package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/gosrc"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/shape"
	"github.com/moznion/go-sqletch/internal/template"
)

// Exit codes (design 07 §2): 0 ok, 1 diagnostics, 2 environment.
const (
	ExitOK          = 0
	ExitDiagnostics = 1
	ExitEnvironment = 2
)

// Generate implements `sqletch generate`.
func Generate(ctx context.Context, configPath string, jsonFormat bool, opts RunOptions, out, errW io.Writer) int {
	return runPipeline(ctx, configPath, ModeGenerate, jsonFormat, opts, out, errW)
}

// Check implements `sqletch check [--exhaustive]`.
func Check(ctx context.Context, configPath string, exhaustive, jsonFormat bool, opts RunOptions, out, errW io.Writer) int {
	mode := ModeCheck
	if exhaustive {
		mode = ModeCheckExhaustive
	}
	return runPipeline(ctx, configPath, mode, jsonFormat, opts, out, errW)
}

func runPipeline(ctx context.Context, configPath string, mode Mode, jsonFormat bool, opts RunOptions, out, errW io.Writer) int {
	cfg, diags := config.Load(configPath)
	if len(diags) > 0 {
		printBare(errW, diags, jsonFormat)
		return ExitDiagnostics
	}
	res, err := Run(ctx, cfg, mode, opts)
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	PrintDiags(errW, res, jsonFormat)
	if diagnostics.HasErrors(res.Diags) {
		return ExitDiagnostics
	}
	offline := "no"
	if res.Offline {
		offline = "yes"
	}
	backend := ""
	if cfg.NativeOracle() {
		backend = "; oracle: native"
	}
	fmt.Fprintf(out, "sqletch: %d queries ok (oracle cache: %d hits, %d misses; offline: %s%s)\n",
		res.QueryCount, res.OracleHits, res.OracleMiss, offline, backend)
	if mode == ModeCheckExhaustive {
		if res.NativePlan {
			// D2 (design 15): the native backend has no planner, so an
			// exhaustive run proves less — say so rather than imply it.
			fmt.Fprintf(out, "sqletch: exhaustive: %d shapes verified by native inference (no EXPLAIN pass; planner coverage needs database.oracle: \"server\")\n", res.ShapesTotal)
		} else {
			fmt.Fprintf(out, "sqletch: exhaustive: %d shapes prepared and planned\n", res.ShapesTotal)
		}
	}
	return ExitOK
}

// ExplainOptions carries the `sqletch explain` flags.
type ExplainOptions struct {
	Enumerate bool // print every reachable shape's SQL (offline)
	Analyze   bool // EXPLAIN every reachable shape on the dev DB
	// MaxShapes caps shape enumeration for Enumerate/Analyze; 0 takes
	// the mode's default (enumerateCap / analyzeCap).
	MaxShapes int
	// AllowDestructive confirms a user-supplied database.dsn is
	// disposable so --analyze may reset its schema (H1); ignored by the
	// offline modes, which never connect.
	AllowDestructive bool
}

// Explain implements `sqletch explain [query…]` from the data written
// at generate time — no database, no recompilation. With enumerate,
// it prints every reachable shape's SQL instead (scan + render only,
// still no database).
func Explain(ctx context.Context, configPath string, queryNames []string, opts ExplainOptions, out, errW io.Writer) int {
	if opts.MaxShapes < 0 {
		fmt.Fprintf(errW, "sqletch: --max-shapes must be a positive shape count (got %d)\n", opts.MaxShapes)
		return ExitEnvironment
	}
	cfg, diags := config.Load(configPath)
	if len(diags) > 0 {
		printBare(errW, diags, false)
		return ExitDiagnostics
	}
	if opts.Analyze {
		return explainAnalyze(ctx, cfg, queryNames, shapeCap(opts.MaxShapes, analyzeCap), opts.AllowDestructive, out, errW)
	}
	if opts.Enumerate {
		return explainEnumerate(cfg, queryNames, shapeCap(opts.MaxShapes, enumerateCap), out, errW)
	}
	root := cfg.Abs(filepath.Join(filepath.Dir(cfg.Cache.Path), "explain"))
	want := map[string]bool{}
	for _, n := range queryNames {
		want[n] = true
	}
	// Explain data is namespaced by target (design 19 §5), so a name
	// can legitimately exist in several packages: print every match,
	// each under its target, rather than picking one arbitrarily.
	var targets []string
	byTarget := map[string][]explainData{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".json" {
			return nil
		}
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		if len(want) > 0 && !want[name] {
			return nil
		}
		data, err := cache.ReadFileCapped(p)
		if err != nil {
			return err
		}
		var ed explainData
		if err := json.Unmarshal(data, &ed); err != nil {
			return err
		}
		target := filepath.ToSlash(filepath.Dir(p))
		if rel, err := filepath.Rel(root, filepath.Dir(p)); err == nil {
			target = filepath.ToSlash(rel)
		}
		if _, seen := byTarget[target]; !seen {
			targets = append(targets, target)
		}
		byTarget[target] = append(byTarget[target], ed)
		return nil
	})
	if err != nil {
		fmt.Fprintf(errW, "sqletch: no explain data (run `sqletch generate` first): %v\n", err)
		return ExitEnvironment
	}
	sort.Strings(targets)
	printed := 0
	for _, t := range targets {
		if len(targets) > 1 {
			fmt.Fprintf(out, "# target %s\n", t)
		}
		for _, ed := range byTarget[t] {
			printExplain(out, ed)
			printed++
		}
	}
	if printed == 0 {
		fmt.Fprintf(errW, "sqletch: no matching queries\n")
		return ExitDiagnostics
	}
	return ExitOK
}

func printExplain(w io.Writer, d explainData) {
	fmt.Fprintf(w, "%s\n", d.Name)
	fmt.Fprintf(w, "  shapes: %s\n", d.ShapeCount)
	if len(d.Guards) > 0 {
		fmt.Fprintln(w, "  guards:")
		for _, g := range d.Guards {
			fmt.Fprintf(w, "    %s\n", g)
		}
	}
	for _, c := range d.Chooses {
		fmt.Fprintf(w, "  choose %s\n", c)
	}
	if len(d.Params) > 0 {
		fmt.Fprintln(w, "  params:")
		for _, p := range d.Params {
			fmt.Fprintf(w, "    %s\n", p)
		}
	}
	if len(d.Columns) > 0 {
		fmt.Fprintln(w, "  columns:")
		for _, c := range d.Columns {
			fmt.Fprintf(w, "    %s\n", c)
		}
	}
	if len(d.Policies) > 0 {
		fmt.Fprintln(w, "  policies:")
		for _, pc := range d.Policies {
			switch pc.Status {
			case "opted_out":
				fmt.Fprintf(w, "    %s: opted out (%s)\n", pc.Name, pc.Reason)
			default:
				fmt.Fprintf(w, "    %s: woven (%s)\n", pc.Name, strings.Join(pc.Conjuncts, " AND "))
			}
		}
	}
	// The maximal rendering, verbatim from the last generate (design 07
	// §3): the report's whole point is that you read the SQL sqletch
	// compiled, woven conjuncts included, without recompiling it.
	if sql := strings.TrimSpace(d.MaximalSQL); sql != "" {
		fmt.Fprintln(w, "  maximal SQL:")
		for _, line := range strings.Split(sql, "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	fmt.Fprintln(w)
}

func printBare(w io.Writer, diags []diagnostics.Diagnostic, jsonFormat bool) {
	res := &Result{Diags: diags, Sources: map[string][]byte{}}
	PrintDiags(w, res, jsonFormat)
}

const enumerateCap = 4096

// shapeCap resolves the --max-shapes flag against a mode's default.
func shapeCap(flag, dflt int) int {
	if flag > 0 {
		return flag
	}
	return dflt
}

// shapeCapDiag reports an enumeration that stopped at the cap.
//
// The severity is the caller's, and the split is deliberate: plain
// `explain` is an inspection command that never claimed to show
// everything, so it warns; `--analyze` claims planner coverage over the
// shape space, so truncation there is a failure. What makes it a
// failure rather than a smaller sample is that the enumeration walk
// stops at the lexicographically first N guard combinations — the
// later guard bits are never planned at all, so the shapes a user sees
// are systematically biased, not representative.
func shapeCapDiag(q *template.QueryTemplate, capN int, sev diagnostics.Severity, verb string) diagnostics.Diagnostic {
	mk := diagnostics.Errorf
	if sev == diagnostics.Warning {
		mk = diagnostics.Warnf
	}
	return mk(diagnostics.CodeShapeCapReached, q.HeaderSpan,
		"%s reaches more than %d shapes; %s stopped at the cap", q.Name, capN, verb).
		WithHint("raise it with --max-shapes N; shapes are enumerated in guard-bitmask order, " +
			"so the ones left out are the later guard combinations, not a random sample")
}

// queryFiles resolves the union of every target's template files, for
// the commands that work over the whole workspace (fmt, explain). They
// do not care which package a file belongs to — only the pipeline and
// the LSP do — but they must see exactly the same file set, so they
// go through the same resolution seam.
func queryFiles(cfg config.Config, errW io.Writer) ([]string, int, bool) {
	resolution, diags := cfg.ResolveTargets()
	if diagnostics.HasErrors(diags) {
		printBare(errW, diags, false)
		return nil, ExitDiagnostics, false
	}
	return resolution.Files, ExitOK, true
}

// wovenTemplates is the shape-enumerating commands' entry into the
// pipeline: every target's template files, scanned and then WOVEN with
// the configured policies (design 14 §2).
//
// `explain --enumerate` and `explain --analyze` sit downstream of the
// weave arrow just like rendering and the oracle do, so they must
// consume the woven template. Rendering the scanned template directly
// would print — and plan — SQL that the program never executes: the
// unscoped form of a query the compiler scopes, which is exactly the
// statement an audit of this surface is trying to rule out.
//
// A defective policy set disables every policy for the run
// (compilePolicies returns none, design 14 §D4), so continuing here
// would emit that same unscoped SQL with only a warning attached:
// refuse instead, as generate and check do. Per-query weave failures
// (SQLETCH125) are returned in the Result for the caller to report —
// the shapes are still worth printing, and the caller owns the exit
// code.
func wovenTemplates(cfg config.Config, drv driver, queryNames []string, errW io.Writer) ([]*template.QueryTemplate, *Result, int, bool) {
	paths, code, ok := queryFiles(cfg, errW)
	if !ok {
		return nil, nil, code, false
	}
	pols, polDiags := compilePolicies(drv, cfg)
	res := &Result{Diags: polDiags, Sources: map[string][]byte{}}
	if diagnostics.HasErrors(polDiags) {
		PrintDiags(errW, res, false)
		return nil, nil, ExitDiagnostics, false
	}
	want := map[string]bool{}
	for _, n := range queryNames {
		want[n] = true
	}
	scanner := template.NewScanner(drv.profile)
	var queries []*template.QueryTemplate
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return nil, nil, ExitEnvironment, false
		}
		file, diags := scanSource(scanner, p, src)
		if diagnostics.HasErrors(diags) {
			printBare(errW, diags, false)
			return nil, nil, ExitDiagnostics, false
		}
		res.Sources[p] = src
		for _, q := range file.Queries {
			if len(want) > 0 && !want[q.Name] {
				continue
			}
			wres := policy.Weave(drv.profile, drv.frontend, pols, q)
			res.Diags = append(res.Diags, wres.Diags...)
			queries = append(queries, wres.Query)
		}
	}
	return queries, res, ExitOK, true
}

func explainEnumerate(cfg config.Config, queryNames []string, capN int, out, errW io.Writer) int {
	drv := driverFor(cfg)
	queries, capped, code, ok := wovenTemplates(cfg, drv, queryNames, errW)
	if !ok {
		return code
	}
	if len(queries) == 0 {
		fmt.Fprintf(errW, "sqletch: no matching queries\n")
		return ExitDiagnostics
	}
	for _, q := range queries {
		keys, truncated := shape.EnumerateExpand(q, capN, drv.expandIn)
		for _, k := range keys {
			r, err := ast.RenderShape(drv.profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
			if err != nil {
				fmt.Fprintf(errW, "sqletch: %v\n", err)
				return ExitEnvironment
			}
			fmt.Fprintf(out, "-- %s shape %s\n%s\n\n", q.Name, k, strings.TrimSpace(r.SQL))
		}
		if truncated {
			// Warning, and on stderr: stdout is the shape SQL
			// stream, which `explain > shapes.sql` must keep clean.
			capped.Diags = append(capped.Diags,
				shapeCapDiag(q, capN, diagnostics.Warning, "enumeration"))
		}
	}
	PrintDiags(errW, capped, false)
	// The cap alone stays a warning (exit 0), but a query the weaver
	// could not scope printed shapes that the compiler would reject:
	// the enumeration no longer describes a buildable program, so it
	// fails like every other command on that code.
	if diagnostics.HasErrors(capped.Diags) {
		return ExitDiagnostics
	}
	return ExitOK
}

const analyzeCap = 64

// explainAnalyze runs EXPLAIN (GENERIC_PLAN) for every enumerable
// shape against the dev database and prints the plans.
func explainAnalyze(ctx context.Context, cfg config.Config, queryNames []string, capN int, allowDestructive bool, out, errW io.Writer) int {
	drv := driverFor(cfg)
	profile := drv.profile
	schemaPaths, err := cfg.ExpandGlobs(cfg.Schema.Files)
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	if cfg.NativeOracle() {
		fmt.Fprintf(errW, "sqletch: explain --analyze needs a real engine's planner; the native backend has none (switch to database.oracle: \"server\")\n")
		return ExitEnvironment
	}
	var schema []cache.SchemaFile
	for _, p := range schemaPaths {
		content, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
		schema = append(schema, cache.SchemaFile{Path: p, Content: content})
	}
	// No drift sink: explain --analyze reads plans and writes no cache
	// entries, so there is nothing for a mismatched server to
	// contaminate (SQLETCH203 belongs to the paths that write).
	o, cleanup, err := drv.acquire(ctx, cfg, schema, allowDestructive, nil)
	if d, ok := versionPinDiag(cfg, err); ok {
		// Same user mistake, same code, whichever command hits it.
		PrintDiags(errW, &Result{Diags: []diagnostics.Diagnostic{d}}, false)
		return ExitDiagnostics
	}
	if d, ok := destructiveResetDiag(cfg, err); ok {
		PrintDiags(errW, &Result{Diags: []diagnostics.Diagnostic{d}}, false)
		return ExitDiagnostics
	}
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	defer cleanup()
	oracle, ok := o.(planTexter)
	if !ok {
		fmt.Fprintf(errW, "sqletch: %s oracle does not support explain --analyze\n", cfg.Dialect)
		return ExitEnvironment
	}

	// The WOVEN templates, for the same reason `check` plans them:
	// the plan a policy-scoped query gets is not the plan its unscoped
	// text gets, and the unscoped text is never executed.
	queries, capped, code, ok := wovenTemplates(cfg, drv, queryNames, errW)
	if !ok {
		return code
	}
	if len(queries) == 0 {
		fmt.Fprintf(errW, "sqletch: no matching queries\n")
		return ExitDiagnostics
	}
	for _, q := range queries {
		keys, truncated := shape.EnumerateExpand(q, capN, drv.expandIn)
		for _, k := range keys {
			r, err := ast.RenderShape(profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
			if err != nil {
				fmt.Fprintf(errW, "sqletch: %v\n", err)
				return ExitEnvironment
			}
			plan, err := oracle.PlanText(ctx, r.SQL)
			if err != nil {
				fmt.Fprintf(errW, "sqletch: %s shape %s: %v\n", q.Name, k, err)
				return ExitDiagnostics
			}
			fmt.Fprintf(out, "-- %s shape %s\n%s\n", q.Name, k, plan)
		}
		if truncated {
			// An error: the plans printed are the low guard bits
			// only, so "every shape plans acceptably" was never
			// established. Other queries still get analyzed.
			capped.Diags = append(capped.Diags,
				shapeCapDiag(q, capN, diagnostics.Error, "analysis"))
		}
	}
	if len(capped.Diags) > 0 {
		PrintDiags(errW, capped, false)
		return ExitDiagnostics
	}
	return ExitOK
}

// Fmt implements `sqletch fmt [--check]`: canonicalize template files
// in place, or report the ones that would change.
func Fmt(configPath string, check bool, out, errW io.Writer) int {
	cfg, diags := config.Load(configPath)
	if len(diags) > 0 {
		printBare(errW, diags, false)
		return ExitDiagnostics
	}
	paths, code, ok := queryFiles(cfg, errW)
	if !ok {
		return code
	}
	changed := 0
	for _, p := range paths {
		// A .go template file is Go source that happens to carry
		// `//sqletch:query` consts (design 13); the template formatter
		// canonicalizes SQL and has no business rewriting it.
		if gosrc.IsGoSource(p) {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
		formatted, fdiags := template.Format(driverFor(cfg).profile, p, src)
		if diagnostics.HasErrors(fdiags) {
			printBare(errW, fdiags, false)
			return ExitDiagnostics
		}
		if string(formatted) == string(src) {
			continue
		}
		changed++
		if check {
			fmt.Fprintf(out, "%s\n", p)
			continue
		}
		if err := os.WriteFile(p, formatted, 0o644); err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
	}
	if check && changed > 0 {
		return ExitDiagnostics
	}
	fmt.Fprintf(out, "sqletch: %d file(s) formatted\n", changed)
	return ExitOK
}
