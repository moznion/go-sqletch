package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

func validateOne(t *testing.T, p Policy) []diagnostics.Diagnostic {
	t.Helper()
	return Validate(postgres.Profile{}, postgres.Frontend{}, []Policy{p}, "sqletch.yaml")
}

func TestValidate_OK(t *testing.T) {
	cases := []Policy{
		tenantPolicy(),
		{Name: "soft_delete", Tables: []string{"orders"}, Predicate: "{}.deleted_at IS NULL"},
		{Name: "no_placeholder", Tables: []string{"orders"}, Predicate: "current_user = 'app'"},
	}
	for _, p := range cases {
		if diags := validateOne(t, p); len(diags) != 0 {
			t.Errorf("%s: unexpected diagnostics %+v", p.Name, diags)
		}
	}
}

func TestValidate_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		p       Policy
		wantMsg string
	}{
		{"bad policy name", Policy{Name: "Tenant-Scope", Tables: []string{"t"}, Predicate: "{}.x = 1"}, "snake_case"},
		{"no tables", Policy{Name: "p", Predicate: "{}.x = 1"}, "at least one designated table"},
		{"quoted table name", Policy{Name: "p", Tables: []string{`"Orders"`}, Predicate: "{}.x = 1"}, "bare lowercase identifier"},
		{"empty predicate", Policy{Name: "p", Tables: []string{"t"}, Predicate: "  "}, "needs a predicate"},
		{"undeclared param", Policy{Name: "p", Tables: []string{"t"}, Predicate: "{}.x = :other", ParamName: "tenant_id"}, "references :other"},
		{"param never used", Policy{Name: "p", Tables: []string{"t"}, Predicate: "{}.x = 1", ParamName: "tenant_id"}, "never appears"},
		{"param without declaration", Policy{Name: "p", Tables: []string{"t"}, Predicate: "{}.x = :tenant_id"}, "declares no parameter"},
		{"two conjuncts", Policy{Name: "p", Tables: []string{"t"}, Predicate: "a = 1) OR (b = 2"}, "one complete boolean expression"},
		{"not an expression", Policy{Name: "p", Tables: []string{"t"}, Predicate: "ORDER BY x"}, "one complete boolean expression"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := validateOne(t, tc.p)
			if len(diags) == 0 {
				t.Fatal("expected SQLETCH303, got none")
			}
			for _, d := range diags {
				if d.Code != diagnostics.CodePolicyInvalid {
					t.Errorf("code = %s, want SQLETCH303", d.Code)
				}
				if d.Span.File != "sqletch.yaml" {
					t.Errorf("span attributes to %q, want the config path", d.Span.File)
				}
			}
			found := false
			for _, d := range diags {
				if strings.Contains(d.Message, tc.wantMsg) {
					found = true
				}
			}
			if !found {
				t.Errorf("no diagnostic mentions %q in %+v", tc.wantMsg, diags)
			}
		})
	}
}

func TestValidate_DuplicateName(t *testing.T) {
	p := tenantPolicy()
	diags := Validate(postgres.Profile{}, postgres.Frontend{}, []Policy{p, p}, "sqletch.yaml")
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "duplicate policy name") {
			found = true
		}
	}
	if !found {
		t.Errorf("duplicate name not rejected: %+v", diags)
	}
}
