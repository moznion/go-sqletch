// Command sqletch is the statically verified, dynamically composed SQL
// compiler for Go. See PROJECT_INSTRUCTION.md for the concept and
// docs/design/ for the implementation design.
package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/moznion/sqletch/internal/cli"
)

var version = "0.1.0-dev"

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

	root.AddCommand(&cobra.Command{
		Use:   "explain [query...]",
		Short: "show guards, cases, types, and shape counts per query",
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(cli.Explain(configPath, args, os.Stdout, os.Stderr))
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "print the sqletch version",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Println("sqletch " + version)
		},
	})

	if err := root.Execute(); err != nil {
		os.Exit(cli.ExitEnvironment)
	}
}
