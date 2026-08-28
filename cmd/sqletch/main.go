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

// versionLine is the template both spellings of the version render:
// the binary's name and the resolved version, nothing else.
const versionLine = "sqletch {{.Version}}"

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
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(cli.ExitEnvironment)
	}
}

// newRootCommand builds the whole command tree. It exists as a
// function so tests can execute a command without going through
// os.Exit.
func newRootCommand() *cobra.Command {
	var configPath string
	var jsonFormat bool

	root := &cobra.Command{
		Use:           "sqletch",
		Short:         "statically verified, dynamically composed SQL for Go",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       resolvedVersion(),
	}
	// `--version` and the `version` subcommand print the same line: one
	// spelling of the version, whichever the user reaches for.
	root.SetVersionTemplate(versionLine + "\n")
	root.PersistentFlags().StringVar(&configPath, "config", "sqletch.yaml", "path to sqletch.yaml")
	root.PersistentFlags().BoolVar(&jsonFormat, "json", false, "emit diagnostics as JSON lines")

	// The escape hatch is a flag, never a config key: accepting a cache
	// built from two servers must stay visible on the command line (and
	// in CI logs), not be disarmed once and forgotten.
	const driftFlagUsage = "accept a committed cache generated against a different server version (SQLETCH203 becomes a warning)"

	// Same reasoning as --allow-server-drift: a repo-controlled config
	// key could silently arm a destructive reset of a database the
	// developer pointed sqletch at, so the confirmation is a flag that
	// stays visible on the command line and in CI logs.
	const destructiveFlagUsage = "confirm the database at a user-supplied database.dsn is disposable, letting sqletch drop and recreate its schema (required for a cold run against a dsn you set; SQLETCH204 otherwise)"

	var generateDrift, generateDestructive bool
	generate := &cobra.Command{
		Use:   "generate",
		Short: "compile templates, run type extraction, emit Go",
		Run: func(cmd *cobra.Command, args []string) {
			opts := cli.RunOptions{AllowServerDrift: generateDrift, AllowDestructive: generateDestructive}
			os.Exit(cli.Generate(context.Background(), configPath, jsonFormat, opts, os.Stdout, os.Stderr))
		},
	}
	generate.Flags().BoolVar(&generateDrift, "allow-server-drift", false, driftFlagUsage)
	generate.Flags().BoolVar(&generateDestructive, "allow-destructive", false, destructiveFlagUsage)
	root.AddCommand(generate)

	var exhaustive, checkDrift, checkDestructive bool
	check := &cobra.Command{
		Use:   "check",
		Short: "verify only (offline on cache hit)",
		Run: func(cmd *cobra.Command, args []string) {
			opts := cli.RunOptions{AllowServerDrift: checkDrift, AllowDestructive: checkDestructive}
			os.Exit(cli.Check(context.Background(), configPath, exhaustive, jsonFormat, opts, os.Stdout, os.Stderr))
		},
	}
	check.Flags().BoolVar(&exhaustive, "exhaustive", false,
		"prepare and EXPLAIN every enumerable shape (needs the dev DB)")
	check.Flags().BoolVar(&checkDrift, "allow-server-drift", false, driftFlagUsage)
	check.Flags().BoolVar(&checkDestructive, "allow-destructive", false, destructiveFlagUsage)
	root.AddCommand(check)

	var enumerate, analyze, explainDestructive bool
	var maxShapes int
	explain := &cobra.Command{
		Use:   "explain [query...]",
		Short: "show guards, cases, types, and shape counts per query",
		Run: func(cmd *cobra.Command, args []string) {
			opts := cli.ExplainOptions{Enumerate: enumerate, Analyze: analyze, MaxShapes: maxShapes, AllowDestructive: explainDestructive}
			os.Exit(cli.Explain(context.Background(), configPath, args, opts, os.Stdout, os.Stderr))
		},
	}
	explain.Flags().BoolVar(&enumerate, "enumerate", false,
		"print every reachable SQL shape (no database needed)")
	explain.Flags().BoolVar(&analyze, "analyze", false,
		"EXPLAIN every reachable shape on the dev DB and print the plans")
	explain.Flags().IntVar(&maxShapes, "max-shapes", 0,
		"cap shape enumeration for --enumerate/--analyze (0 = the mode's default); "+
			"--enumerate warns when it stops at the cap, --analyze fails")
	explain.Flags().BoolVar(&explainDestructive, "allow-destructive", false, destructiveFlagUsage)
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

	return root
}
