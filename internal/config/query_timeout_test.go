package config

import (
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// query_timeout.default (design 23) is the deadline every generated
// method gets unless its query says `-- @timeout`. Unset means no
// default; a set value must be a positive Go duration (SQLETCH318) —
// never silently treated as "no timeout".
func TestLoad_QueryTimeoutDefault(t *testing.T) {
	dir := t.TempDir()
	cfg, diags := Load(write(t, dir, "sqletch.yaml", validYAML))
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if cfg.QueryTimeout.DefaultDuration != 0 {
		t.Errorf("unset default = %v, want 0 (none)", cfg.QueryTimeout.DefaultDuration)
	}

	for in, want := range map[string]time.Duration{
		"2s":     2 * time.Second,
		`"2s"`:   2 * time.Second,
		"1m30s":  90 * time.Second,
		"750ms":  750 * time.Millisecond,
		"1.5s":   1500 * time.Millisecond,
		" 3s   ": 3 * time.Second,
	} {
		cfg, diags := Load(write(t, dir, "custom.yaml", validYAML+"query_timeout:\n  default: "+in+"\n"))
		if len(diags) != 0 {
			t.Fatalf("%q: %v", in, diags)
		}
		if cfg.QueryTimeout.DefaultDuration != want {
			t.Errorf("%q: default = %v, want %v", in, cfg.QueryTimeout.DefaultDuration, want)
		}
	}

	for _, in := range []string{"0s", "0", "-1s", "500", "fast", "none", `""`, "1 s", "9999999h"} {
		_, diags := Load(write(t, dir, "bad.yaml", validYAML+"query_timeout:\n  default: "+in+"\n"))
		if !hasConfigCode(diags, diagnostics.CodeBadDefaultTimeout) {
			t.Errorf("query_timeout.default %q must be SQLETCH318, got %+v", in, diags)
		}
	}

	// Unknown sub-keys are a parse error like everywhere else.
	_, diags = Load(write(t, dir, "unk.yaml", validYAML+"query_timeout:\n  per_query: 1s\n"))
	if !hasConfigCode(diags, diagnostics.CodeConfigParse) {
		t.Errorf("unknown query_timeout key must be SQLETCH300, got %+v", diags)
	}
}
