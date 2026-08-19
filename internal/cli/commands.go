package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
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
	dir := cfg.Abs(filepath.Join(filepath.Dir(cfg.Cache.Path), "explain"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(errW, "sqletch: no explain data (run `sqletch generate` first): %v\n", err)
		return ExitEnvironment
	}
	want := map[string]bool{}
	for _, n := range queryNames {
		want[n] = true
	}
	printed := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".json")]
		if len(want) > 0 && !want[name] {
			continue
		}
		data, err := cache.ReadFileCapped(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
		var d explainData
		if err := json.Unmarshal(data, &d); err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
		printExplain(out, d)
		printed++
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

func explainEnumerate(cfg config.Config, queryNames []string, capN int, out, errW io.Writer) int {
	drv := driverFor(cfg)
	profile := drv.profile
	paths, err := cfg.ExpandGlobs(cfg.Queries)
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	want := map[string]bool{}
	for _, n := range queryNames {
		want[n] = true
	}
	scanner := template.NewScanner(profile)
	printed := 0
	capped := &Result{Sources: map[string][]byte{}}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
		file, diags := scanner.ScanFile(p, src)
		if diagnostics.HasErrors(diags) {
			printBare(errW, diags, false)
			return ExitDiagnostics
		}
		capped.Sources[p] = src
		for _, q := range file.Queries {
			if len(want) > 0 && !want[q.Name] {
				continue
			}
			keys, truncated := shape.EnumerateExpand(q, capN, drv.expandIn)
			for _, k := range keys {
				r, err := ast.RenderShape(profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
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
			printed++
		}
	}
	if printed == 0 {
		fmt.Fprintf(errW, "sqletch: no matching queries\n")
		return ExitDiagnostics
	}
	PrintDiags(errW, capped, false)
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

	paths, err := cfg.ExpandGlobs(cfg.Queries)
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	want := map[string]bool{}
	for _, n := range queryNames {
		want[n] = true
	}
	scanner := template.NewScanner(profile)
	printed := 0
	capped := &Result{Sources: map[string][]byte{}}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
		file, diags := scanner.ScanFile(p, src)
		if diagnostics.HasErrors(diags) {
			printBare(errW, diags, false)
			return ExitDiagnostics
		}
		capped.Sources[p] = src
		for _, q := range file.Queries {
			if len(want) > 0 && !want[q.Name] {
				continue
			}
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
			printed++
		}
	}
	if printed == 0 {
		fmt.Fprintf(errW, "sqletch: no matching queries\n")
		return ExitDiagnostics
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
	paths, err := cfg.ExpandGlobs(cfg.Queries)
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	changed := 0
	for _, p := range paths {
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
