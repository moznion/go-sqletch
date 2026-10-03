package config

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// `lint` (design 24 §2, owner decision 2026-10-03) opts the run into the
// SQLETCHLnnn performance lints. Off unless set; a non-boolean value is
// a parse error, never a silent "off".
func TestLoad_Lint(t *testing.T) {
	dir := t.TempDir()
	cfg, diags := Load(write(t, dir, "sqletch.yaml", validYAML))
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if cfg.Lint {
		t.Error("lint must default to off")
	}
	for in, want := range map[string]bool{"true": true, "false": false} {
		cfg, diags := Load(write(t, dir, "set.yaml", validYAML+"lint: "+in+"\n"))
		if len(diags) != 0 {
			t.Fatalf("%q: %v", in, diags)
		}
		if cfg.Lint != want {
			t.Errorf("lint: %s = %v", in, cfg.Lint)
		}
	}
	for _, in := range []string{`"true"`, "1", "on-ish", "{enabled: true}"} {
		_, diags := Load(write(t, dir, "bad.yaml", validYAML+"lint: "+in+"\n"))
		if !hasConfigCode(diags, diagnostics.CodeConfigParse) {
			t.Errorf("lint: %s must be SQLETCH300, got %+v", in, diags)
		}
	}
}
