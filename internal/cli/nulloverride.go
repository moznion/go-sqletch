package cli

import (
	"fmt"
	"sort"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/nullability"
	"github.com/moznion/go-sqletch/internal/template"
)

// Forced-verdict reasons, surfaced by `explain` next to the analyzer's
// own: an author-forced type names where it was declared.
const (
	reasonConfigOverride    = "forced by sqletch.yaml overrides"
	reasonNullableDirective = "forced by -- @nullable"
)

// effectiveNullOverrides is the one place a query's forced nullability
// is assembled (design 25): sqletch.yaml `overrides` entries for its
// name plus its `-- @nullable` directives. A column declared in both is
// SQLETCH319 (nullOverrideConflictDiags), which stops the run before
// this result is used, so the order of the two loops never decides a
// verdict.
func effectiveNullOverrides(cfg config.Config, q *template.QueryTemplate) map[string]nullability.Verdict {
	fromCfg := cfg.NullOverridesFor(q.Name)
	if len(fromCfg) == 0 && len(q.NullOverrides) == 0 {
		return nil
	}
	out := make(map[string]nullability.Verdict, len(fromCfg)+len(q.NullOverrides))
	for name, v := range fromCfg {
		out[name] = nullability.Verdict{Nullable: v, Reason: reasonConfigOverride}
	}
	for name, d := range q.NullOverrides {
		out[name] = nullability.Verdict{Nullable: d.Nullable, Reason: reasonNullableDirective}
	}
	return out
}

// nullOverrideConflictDiags reports every column whose nullability a
// query forces from both sqletch.yaml and a directive. Agreement is no
// excuse: two declarations of one fact drift apart silently on the
// next edit, so each column gets exactly one. Catalog-free, so the
// pipeline and the LSP both run it from their scan phase.
func nullOverrideConflictDiags(cfg config.Config, q *template.QueryTemplate) []diagnostics.Diagnostic {
	if len(q.NullOverrides) == 0 {
		return nil
	}
	fromCfg := cfg.NullOverridesFor(q.Name)
	var diags []diagnostics.Diagnostic
	for _, name := range sortedKeys(q.NullOverrides) {
		if _, both := fromCfg[name]; !both {
			continue
		}
		diags = append(diags, diagnostics.Errorf(diagnostics.CodeNullOverrideConflict,
			q.NullOverrides[name].Span,
			"the nullability of column %q of query %s is declared twice, by this @nullable and by an `overrides` entry in %s; a column's forced nullability has exactly one declaration, so the two cannot silently drift apart",
			name, q.Name, cfg.Path).
			WithHint("keep one: delete the `{query: %s, column: %s}` entry from %s's `overrides`, or delete this directive", q.Name, name, cfg.Path))
	}
	return diags
}

// overrideHygieneDiags flags sqletch.yaml `overrides` entries that
// cannot do what the user intended: a key naming no result column is
// dead config (a typo or a renamed column), and a key matching several
// same-named result columns forces ALL of them — overrides are
// name-keyed, not position-keyed.
func overrideHygieneDiags(cfg config.Config, query string,
	overrides map[string]bool, desc dialect.Desc) []diagnostics.Diagnostic {

	return nullOverrideHygiene(sortedKeys(overrides), desc, func(string) diagnostics.Span {
		return diagnostics.Span{File: cfg.Path}
	}, func(name string) string {
		return fmt.Sprintf("the `overrides` entry for query %s, column %q,", query, name)
	})
}

// directiveHygieneDiags is overrideHygieneDiags for `-- @nullable`
// directives, pointed at the directive. It runs inside resolvedChecks
// so the editor reports it too.
func directiveHygieneDiags(q *template.QueryTemplate, desc dialect.Desc) []diagnostics.Diagnostic {
	return nullOverrideHygiene(sortedKeys(q.NullOverrides), desc, func(name string) diagnostics.Span {
		return q.NullOverrides[name].Span
	}, func(name string) string {
		return fmt.Sprintf("@nullable %q", name)
	})
}

// nullOverrideHygiene is the shared SQLETCH312/313 judgment over the
// maximal rendering's result columns; names must be sorted.
func nullOverrideHygiene(names []string, desc dialect.Desc,
	span func(name string) diagnostics.Span, subject func(name string) string) []diagnostics.Diagnostic {

	if len(names) == 0 {
		return nil
	}
	count := map[string]int{}
	for _, c := range desc.Columns {
		count[c.Name]++
	}
	var diags []diagnostics.Diagnostic
	for _, name := range names {
		switch {
		case count[name] == 0:
			diags = append(diags, diagnostics.Warnf(diagnostics.CodeOverrideUnknownColumn, span(name),
				"%s names no result column of the query — the override is dead", subject(name)).
				WithHint("check for a typo or a renamed output alias"))
		case count[name] > 1:
			diags = append(diags, diagnostics.Warnf(diagnostics.CodeOverrideAmbiguousColumn, span(name),
				"%s matches %d result columns of that name — overrides are name-keyed and force all of them",
				subject(name), count[name]).
				WithHint("give the columns distinct output aliases and override the intended one"))
		}
	}
	return diags
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
