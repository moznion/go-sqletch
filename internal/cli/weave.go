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
//
// maxRenderings bounds the verification rendering set: Renderings
// materialises one full-SQL copy per rendering, and its count grows
// with the number of @choose/@order-by/@filter-tree blocks, so a
// crafted template could otherwise exhaust memory here — before any
// max_shapes cap downstream. A projected count over the budget is
// refused with SQLETCH302 (a plain error diagnostic, so the LSP
// degrades rather than crashing) and the renderings are never built.
// The bound is verification.max_shapes: the rendering count never
// exceeds the shape space, so a template inside the verification budget
// is never refused here. A non-positive budget disables the check.
func scanChecks(drv driver, pols []policy.Policy, q *template.QueryTemplate, maxRenderings int) (policy.Result, []ast.Rendering, []diagnostics.Diagnostic, error) {
	diags := rules.CheckLexical(drv.profile, q)
	wres := policy.Weave(drv.profile, drv.frontend, pols, q)
	diags = append(diags, wres.Diags...)
	if n := ast.RenderingCount(drv.profile, wres.Query); maxRenderings > 0 && n > maxRenderings {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodeExpansionLarge,
			wres.Query.HeaderSpan,
			"%s expands to %d verification renderings, over the shape budget of %d; refusing to materialise them (each is a full copy of the query, so the set would exhaust memory)",
			wres.Query.Name, n, maxRenderings).
			WithHint("reduce the number of @choose/@order-by/@filter-tree blocks in this query, or raise verification.max_shapes"))
		return wres, nil, diags, nil
	}
	rs, err := ast.Renderings(drv.profile, wres.Query)
	if err != nil {
		return wres, nil, diags, err
	}
	diags = append(diags, rules.CheckR1(drv.profile, drv.frontend, wres.Query, rs)...)
	return wres, rs, diags, nil
}
