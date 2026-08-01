// Command sqletch is the statically verified, dynamically composed SQL
// compiler for Go. See docs/spec.md for the concept and
// docs/design/ for the implementation design.
package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/moznion/go-sqletch/internal/cli"
)

// version is overridable at build time
// (-ldflags="-X main.version=v1.2.3"); otherwise it resolves from the
// module build info, so `go install …@vX.Y.Z` reports the right
// version with no release tooling.
var version = ""

func resolvedVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

func main() {
	var configPath string
	var jsonFormat bool

	root := &cobra.Command{
		Use:           "sqletch",
		Short:         "statically verified, dynamically composed SQL for Go",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "sqletch.yaml", "path to sqletch.yaml")
	root.PersistentFlags().BoolVar(&jsonFormat, "json", false, "emit diagnostics as JSON lines")

	root.AddCommand(&cobra.Command{
		Use:   "generate",
		Short: "compile templates, run type extraction, emit Go",
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(cli.Generate(context.Background(), configPath, jsonFormat, os.Stdout, os.Stderr))
		},
	})

	var exhaustive bool
	check := &cobra.Command{
		Use:   "check",
		Short: "verify only (offline on cache hit)",
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(cli.Check(context.Background(), configPath, exhaustive, jsonFormat, os.Stdout, os.Stderr))
		},
	}
	check.Flags().BoolVar(&exhaustive, "exhaustive", false,
		"prepare and EXPLAIN every enumerable shape (needs the dev DB)")
	root.AddCommand(check)

	var enumerate, analyze bool
	explain := &cobra.Command{
		Use:   "explain [query...]",
		Short: "show guards, cases, types, and shape counts per query",
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(cli.Explain(context.Background(), configPath, args, enumerate, analyze, os.Stdout, os.Stderr))
		},
	}
	explain.Flags().BoolVar(&enumerate, "enumerate", false,
		"print every reachable SQL shape (no database needed)")
	explain.Flags().BoolVar(&analyze, "analyze", false,
		"EXPLAIN every reachable shape on the dev DB and print the plans")
	root.AddCommand(explain)

	var fmtCheck bool
	fmtCmd := &cobra.Command{
		Use:   "fmt",
		Short: "canonicalize template files (construct layout, anchors)",
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(cli.Fmt(configPath, fmtCheck, os.Stdout, os.Stderr))
		},
	}
	fmtCmd.Flags().BoolVar(&fmtCheck, "check", false,
		"list files that would change; exit 1 if any")
	root.AddCommand(fmtCmd)

	root.AddCommand(&cobra.Command{
		Use:   "lsp",
		Short: "run the language server over stdio (diagnostics, go-to-definition; offline)",
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(cli.LSP(configPath, os.Stdin, os.Stdout, os.Stderr))
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "print the sqletch version",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Println("sqletch " + resolvedVersion())
		},
	})

	if err := root.Execute(); err != nil {
		os.Exit(cli.ExitEnvironment)
	}
}
