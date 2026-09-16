package config

import "testing"

// `require_annotation` is per policy (design 14 §12.3, owner decision
// 2026-09-16), so a set can be rolled out one policy at a time.
func TestLoad_PolicyRequireAnnotation(t *testing.T) {
	yaml := validYAML + `policies:
  - name: tenant_scope
    tables: [orders]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint
    require_annotation: true
  - name: soft_delete
    tables: [orders]
    predicate: "{}.deleted_at IS NULL"
`
	dir := t.TempDir()
	cfg, diags := Load(write(t, dir, "sqletch.yaml", yaml))
	if len(diags) != 0 {
		t.Fatalf("diags: %+v", diags)
	}
	if !cfg.Policies[0].RequireAnnotation {
		t.Errorf("require_annotation not decoded: %+v", cfg.Policies[0])
	}
	// Absent means off: existing configs keep compiling unchanged.
	if cfg.Policies[1].RequireAnnotation {
		t.Errorf("require_annotation defaulted to true: %+v", cfg.Policies[1])
	}
}
