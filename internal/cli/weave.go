package cli

import (
	"slices"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/rules"
	"github.com/moznion/go-sqletch/internal/template"
)

// compilePolicies converts cfg.Policies into the weaver's form and
// validates the set once per run (SQLETCH303, attributed to the
// config file — design 14 §D4). Any defect disables the whole set:
// weaving with a half-valid policy list would scope some queries and
// not others while the config is being fixed.
func compilePolicies(drv driver, cfg config.Config) ([]policy.Policy, []diagnostics.Diagnostic) {
	if len(cfg.Policies) == 0 {
		return nil, nil
	}
	pols := make([]policy.Policy, 0, len(cfg.Policies))
	for _, p := range cfg.Policies {
		var kinds []dialect.StmtKind
		for _, k := range p.AppliesTo {
			if sk, ok := policy.KindNames[k]; ok {
				kinds = append(kinds, sk)
			}
		}
		pols = append(pols, policy.Policy{
			Name:      p.Name,
			Tables:    slices.Clone(p.Tables),
			Predicate: p.Predicate,
			ParamName: p.Param.Name,
			ParamType: p.Param.Type,
			Kinds:     kinds,
		})
	}
	diags := policy.Validate(drv.profile, drv.frontend, pols, cfg.Path)
	if diagnostics.HasErrors(diags) {
		return nil, diags
	}
	return pols, diags
}

// scanChecks is the per-query catalog-free sequence shared by
// pipeline.Run and the OfflineChecker (design 14 §11.5): the lexical
// checks (R6/R9) on the UNWOVEN template — template validity never
// depends on configuration — then policy weaving, then renderings and
// R1 on the WOVEN result. Every downstream phase must consume the
// returned Result.Query.
func scanChecks(drv driver, pols []policy.Policy, q *template.QueryTemplate) (policy.Result, []ast.Rendering, []diagnostics.Diagnostic, error) {
	diags := rules.CheckLexical(drv.profile, q)
	wres := policy.Weave(drv.profile, drv.frontend, pols, q)
	diags = append(diags, wres.Diags...)
	rs, err := ast.Renderings(drv.profile, wres.Query)
	if err != nil {
		return wres, nil, diags, err
	}
	diags = append(diags, rules.CheckR1(drv.profile, drv.frontend, wres.Query, rs)...)
	return wres, rs, diags, nil
}
