package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/config"
	"github.com/moznion/sqletch/internal/devdb"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
)

// Exit codes (design 07 §2): 0 ok, 1 diagnostics, 2 environment.
const (
	ExitOK          = 0
	ExitDiagnostics = 1
	ExitEnvironment = 2
)

// Generate implements `sqletch generate`.
func Generate(ctx context.Context, configPath string, jsonFormat bool, out, errW io.Writer) int {
	return runPipeline(ctx, configPath, ModeGenerate, jsonFormat, out, errW)
}

// Check implements `sqletch check [--exhaustive]`.
func Check(ctx context.Context, configPath string, exhaustive, jsonFormat bool, out, errW io.Writer) int {
	mode := ModeCheck
	if exhaustive {
		mode = ModeCheckExhaustive
	}
	return runPipeline(ctx, configPath, mode, jsonFormat, out, errW)
}

func runPipeline(ctx context.Context, configPath string, mode Mode, jsonFormat bool, out, errW io.Writer) int {
	cfg, diags := config.Load(configPath)
	if len(diags) > 0 {
		printBare(errW, diags, jsonFormat)
		return ExitDiagnostics
	}
	res, err := Run(ctx, cfg, mode)
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
	fmt.Fprintf(out, "sqletch: %d queries ok (oracle cache: %d hits, %d misses; offline: %s)\n",
		res.QueryCount, res.OracleHits, res.OracleMiss, offline)
	if mode == ModeCheckExhaustive {
		fmt.Fprintf(out, "sqletch: exhaustive: %d shapes prepared and planned\n", res.ShapesTotal)
	}
	return ExitOK
}

// Explain implements `sqletch explain [query…]` from the data written
// at generate time — no database, no recompilation. With enumerate,
// it prints every reachable shape's SQL instead (scan + render only,
// still no database).
func Explain(ctx context.Context, configPath string, queryNames []string, enumerate, analyze bool, out, errW io.Writer) int {
	cfg, diags := config.Load(configPath)
	if len(diags) > 0 {
		printBare(errW, diags, false)
		return ExitDiagnostics
	}
	if analyze {
		return explainAnalyze(ctx, cfg, queryNames, out, errW)
	}
	if enumerate {
		return explainEnumerate(cfg, queryNames, out, errW)
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
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
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
	fmt.Fprintln(w)
}

func printBare(w io.Writer, diags []diagnostics.Diagnostic, jsonFormat bool) {
	res := &Result{Diags: diags, Sources: map[string][]byte{}}
	PrintDiags(w, res, jsonFormat)
}

const enumerateCap = 4096

func explainEnumerate(cfg config.Config, queryNames []string, out, errW io.Writer) int {
	profile := postgres.Profile{}
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
		for _, q := range file.Queries {
			if len(want) > 0 && !want[q.Name] {
				continue
			}
			keys, truncated := shape.Enumerate(q, enumerateCap)
			for _, k := range keys {
				r, err := ast.RenderShape(profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
				if err != nil {
					fmt.Fprintf(errW, "sqletch: %v\n", err)
					return ExitEnvironment
				}
				fmt.Fprintf(out, "-- %s shape %s\n%s\n\n", q.Name, k, strings.TrimSpace(r.SQL))
			}
			if truncated {
				fmt.Fprintf(out, "-- %s: enumeration truncated at %d shapes\n\n", q.Name, enumerateCap)
			}
			printed++
		}
	}
	if printed == 0 {
		fmt.Fprintf(errW, "sqletch: no matching queries\n")
		return ExitDiagnostics
	}
	return ExitOK
}

const analyzeCap = 64

// explainAnalyze runs EXPLAIN (GENERIC_PLAN) for every enumerable
// shape against the dev database and prints the plans.
func explainAnalyze(ctx context.Context, cfg config.Config, queryNames []string, out, errW io.Writer) int {
	profile := postgres.Profile{}
	schemaPaths, err := cfg.ExpandGlobs(cfg.Schema.Files)
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	var schemaSQL []string
	for _, p := range schemaPaths {
		content, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(errW, "sqletch: %v\n", err)
			return ExitEnvironment
		}
		schemaSQL = append(schemaSQL, string(content))
	}
	conn, cleanup, err := devdb.Acquire(ctx, devdb.Config{
		DSN:           cfg.Database.DSN,
		ServerVersion: cfg.ServerVersion,
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		fmt.Fprintf(errW, "sqletch: %v\n", err)
		return ExitEnvironment
	}
	defer cleanup()
	oracle := postgres.NewOracle(conn)

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
		for _, q := range file.Queries {
			if len(want) > 0 && !want[q.Name] {
				continue
			}
			keys, truncated := shape.Enumerate(q, analyzeCap)
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
				fmt.Fprintf(out, "-- %s: analysis truncated at %d shapes\n\n", q.Name, analyzeCap)
			}
			printed++
		}
	}
	if printed == 0 {
		fmt.Fprintf(errW, "sqletch: no matching queries\n")
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
		formatted, fdiags := template.Format(postgres.Profile{}, p, src)
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
