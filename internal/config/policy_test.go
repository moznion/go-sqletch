package config

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

const policyYAML = validYAML + `policies:
  - name: tenant_scope
    tables: [orders, order_items]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint
    applies_to: [select, update, delete]
  - name: soft_delete
    tables: [orders]
    predicate: "{}.deleted_at IS NULL"
`

func TestLoad_Policies(t *testing.T) {
	t.Setenv("SQLETCH_TEST_CONFIG_DSN", "postgres://x")
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", policyYAML)
	cfg, diags := Load(path)
	if len(diags) != 0 {
		t.Fatalf("diags: %+v", diags)
	}
	if len(cfg.Policies) != 2 {
		t.Fatalf("got %d policies", len(cfg.Policies))
	}
	p := cfg.Policies[0]
	if p.Name != "tenant_scope" || len(p.Tables) != 2 ||
		p.Predicate != "{}.tenant_id = :tenant_id" ||
		p.Param.Name != "tenant_id" || p.Param.Type != "bigint" ||
		len(p.AppliesTo) != 3 {
		t.Errorf("policy decoded wrong: %+v", p)
	}
	if q := cfg.Policies[1]; q.Param.Name != "" || len(q.AppliesTo) != 0 {
		t.Errorf("paramless policy decoded wrong: %+v", q)
	}
}

func TestLoad_PolicyVocabulary(t *testing.T) {
	t.Setenv("SQLETCH_TEST_CONFIG_DSN", "postgres://x")
	cases := []struct {
		name    string
		yamlAdd string
		wantMsg string
	}{
		{
			name: "applies_to insert",
			yamlAdd: `policies:
  - name: p
    tables: [t]
    predicate: "{}.x = 1"
    applies_to: [insert]
`,
			wantMsg: "INSERT filters no rows",
		},
		{
			name: "applies_to unknown",
			yamlAdd: `policies:
  - name: p
    tables: [t]
    predicate: "{}.x = 1"
    applies_to: [merge]
`,
			wantMsg: "must be select, update, or delete",
		},
		{
			name: "param type without name",
			yamlAdd: `policies:
  - name: p
    tables: [t]
    predicate: "{}.x = 1"
    param:
      type: bigint
`,
			wantMsg: "param.type without param.name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := write(t, dir, "sqletch.yaml", validYAML+tc.yamlAdd)
			_, diags := Load(path)
			found := false
			for _, d := range diags {
				if d.Code == diagnostics.CodePolicyInvalid && strings.Contains(d.Message, tc.wantMsg) {
					found = true
				}
			}
			if !found {
				t.Errorf("no SQLETCH303 mentioning %q in %+v", tc.wantMsg, diags)
			}
		})
	}
}

// A config written for a policies-aware sqletch is rejected loudly by
// binaries whose Config predates the key (strict decoding); the
// inverse — this binary reading an old config — must stay silent.
func TestLoad_NoPoliciesKeyStaysValid(t *testing.T) {
	t.Setenv("SQLETCH_TEST_CONFIG_DSN", "postgres://x")
	dir := t.TempDir()
	path := write(t, dir, "sqletch.yaml", validYAML)
	cfg, diags := Load(path)
	if len(diags) != 0 || len(cfg.Policies) != 0 {
		t.Fatalf("plain config regressed: %+v %+v", cfg.Policies, diags)
	}
}
