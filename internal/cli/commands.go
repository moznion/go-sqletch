package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/moznion/sqletch/internal/config"
	"github.com/moznion/sqletch/internal/diagnostics"
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
// at generate time — no database, no recompilation.
func Explain(configPath string, queryNames []string, out, errW io.Writer) int {
	cfg, diags := config.Load(configPath)
	if len(diags) > 0 {
		printBare(errW, diags, false)
		return ExitDiagnostics
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
