package main

import (
	"bytes"
	"testing"
)

// Both spellings of the version must render the same line: a release
// script that greps `sqletch --version` and a human typing `sqletch
// version` are entitled to the same answer.
func TestVersionSpellingsAgree(t *testing.T) {
	t.Cleanup(func(old string) func() {
		return func() { version = old }
	}(version))
	version = "v9.9.9-test"

	const want = "sqletch v9.9.9-test\n"
	for _, args := range [][]string{{"--version"}, {"version"}} {
		root := newRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got := out.String(); got != want {
			t.Errorf("%v printed %q, want %q", args, got, want)
		}
	}
}

// The build-time override wins over whatever the build info says, and
// the resolved version is never empty — a blank version line would be
// worse than an honest "devel".
func TestResolvedVersion(t *testing.T) {
	t.Cleanup(func(old string) func() {
		return func() { version = old }
	}(version))

	version = "v1.2.3"
	if got := resolvedVersion(); got != "v1.2.3" {
		t.Errorf("-X main.version must win, got %q", got)
	}

	version = ""
	if got := resolvedVersion(); got == "" {
		t.Error("resolvedVersion must never be empty")
	}
}

// --lint is tri-state on generate and check: absent leaves sqletch.yaml's
// `lint` in charge (nil), --lint / --lint=true force it on, --lint=false
// forces it off.
func TestLintFlagOverride(t *testing.T) {
	for _, name := range []string{"generate", "check"} {
		for args, want := range map[string]string{
			"":             "nil",
			"--lint":       "true",
			"--lint=true":  "true",
			"--lint=false": "false",
		} {
			root := newRootCommand()
			cmd, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			var argv []string
			if args != "" {
				argv = []string{args}
			}
			if err := cmd.ParseFlags(argv); err != nil {
				t.Fatalf("%s %q: %v", name, args, err)
			}
			got := "nil"
			if p := lintOverride(cmd); p != nil {
				got = map[bool]string{true: "true", false: "false"}[*p]
			}
			if got != want {
				t.Errorf("%s %q: override = %s, want %s", name, args, got, want)
			}
		}
	}
}
