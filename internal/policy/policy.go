// Package policy implements cross-query policy weaving (design 14,
// spec §"Cross-Query Policies"): a boolean predicate declared once in
// sqletch.yaml, woven after the P1 scan and before rendering into
// every query that touches a designated table. Everything downstream
// of the weave sees an ordinary template — the woven conjunct is
// unconditional skeleton text with the empty guard set.
package policy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Placeholder is the relation-reference marker in a policy predicate:
// the weaver substitutes it with the designated relation's name as
// spelled in the target query (its alias if present, else the table
// name).
const Placeholder = "{}"

// Policy is one compiled policy. The cli layer builds it from
// config.Policy; Validate checks the policy-intrinsic invariants once
// per run.
type Policy struct {
	Name      string
	Tables    []string // bare lowercase identifiers
	Predicate string   // may contain Placeholder and :param refs
	ParamName string   // "" = paramless policy
	ParamType string   // "" = untyped (Tier 1 infers; Tier 2 requires)
	// Kinds are the statement kinds the policy applies to. Empty =
	// select, update, delete (every kind a row filter can scope).
	Kinds []dialect.StmtKind
}

// appliesTo reports whether the policy covers the statement kind.
func (p *Policy) appliesTo(k dialect.StmtKind) bool {
	if len(p.Kinds) == 0 {
		return k == dialect.StmtSelect || k == dialect.StmtUpdate || k == dialect.StmtDelete
	}
	for _, pk := range p.Kinds {
		if pk == k {
			return true
		}
	}
	return false
}

// coversSelect reports whether reads are in scope — the visibility
// check for INSERT … SELECT bodies keys on it (design 14 §D6).
func (p *Policy) coversSelect() bool { return p.appliesTo(dialect.StmtSelect) }

// designates reports whether the policy designates the table name.
// Matching is case-insensitive across all dialects: PostgreSQL folds
// unquoted identifiers anyway, SQLite table names are case-insensitive,
// and on MySQL the fold errs toward scoping more, never less.
func (p *Policy) designates(table string) bool {
	for _, t := range p.Tables {
		if strings.EqualFold(t, table) {
			return true
		}
	}
	return false
}

// bareIdentRe is the §11.3 restriction: names bound to Placeholder
// must need no dialect quoting.
var bareIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// lowerIdentRe is the shape required of policy names, table names,
// and the parameter name in the config.
var lowerIdentRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// KindNames maps applies_to config strings to statement kinds.
var KindNames = map[string]dialect.StmtKind{
	"select": dialect.StmtSelect,
	"update": dialect.StmtUpdate,
	"delete": dialect.StmtDelete,
}

// Validate checks policy-intrinsic invariants: identifier shapes,
// parameter references, and — via the dialect frontend — that the
// predicate parses as exactly one complete boolean expression.
// Diagnostics carry SQLETCH303 against the config path (design 14 §D4:
// a defect in the policy itself is the config's fault, reported once).
func Validate(profile dialect.LexerProfile, fe dialect.Frontend, pols []Policy, configPath string) []diagnostics.Diagnostic {
	span := diagnostics.Span{File: configPath}
	var diags []diagnostics.Diagnostic
	bad := func(format string, args ...any) {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodePolicyInvalid, span, format, args...))
	}
	seen := map[string]bool{}
	for i, p := range pols {
		id := p.Name
		if id == "" {
			id = fmt.Sprintf("policies[%d]", i)
		}
		if !lowerIdentRe.MatchString(p.Name) {
			bad("%s: policy name must be a snake_case identifier ([a-z][a-z0-9_]*)", id)
		}
		if seen[p.Name] {
			bad("%s: duplicate policy name", id)
		}
		seen[p.Name] = true
		if len(p.Tables) == 0 {
			bad("%s: a policy needs at least one designated table", id)
		}
		for _, t := range p.Tables {
			if !lowerIdentRe.MatchString(t) {
				bad("%s: designated table %q must be a bare lowercase identifier", id, t)
			}
		}
		if strings.TrimSpace(p.Predicate) == "" {
			bad("%s: a policy needs a predicate", id)
			continue
		}
		if p.ParamName != "" && !lowerIdentRe.MatchString(p.ParamName) {
			bad("%s: parameter name %q must be a snake_case identifier", id, p.ParamName)
		}
		diags = append(diags, validatePredicate(profile, fe, &p, id, span)...)
	}
	return diags
}

// validatePredicate checks the predicate's parameter references and
// probes it as one complete boolean node, exactly like an @if-present
// conjunct body (R1's discipline applied to config text).
func validatePredicate(profile dialect.LexerProfile, fe dialect.Frontend, p *Policy, id string, span diagnostics.Span) []diagnostics.Diagnostic {
	var diags []diagnostics.Diagnostic
	bad := func(format string, args ...any) {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodePolicyInvalid, span, format, args...))
	}

	// Bind the placeholder to a probe name so the predicate lexes and
	// parses as it will after substitution.
	bound := strings.ReplaceAll(p.Predicate, Placeholder, "sqletch_probe_t")

	// Every :param in the predicate must be the declared parameter —
	// the config declares exactly one — and a declared parameter must
	// actually appear (a missing reference is a typo, and on Tier 2 it
	// would leave an untypable bind).
	refs := paramRefs(profile, bound)
	for _, r := range refs {
		if p.ParamName == "" || r != p.ParamName {
			bad("%s: predicate references :%s, but the policy declares %s",
				id, r, describeParam(p.ParamName))
			return diags
		}
	}
	if p.ParamName != "" && len(refs) == 0 {
		bad("%s: declared parameter %q never appears in the predicate", id, p.ParamName)
	}

	// The probe wraps the predicate in parentheses, so unbalanced
	// input could escape the wrapper — the same lexer-level
	// precondition rules.CheckR1 enforces for fragment bodies.
	if !balancedParens(profile, bound) {
		bad("%s: predicate is not one complete boolean expression: unbalanced parentheses", id)
		return diags
	}
	if err := fe.ProbeExpr(rewriteParams(profile, bound)); err != nil {
		bad("%s: predicate is not one complete boolean expression: %s", id, probeMsg(err))
	}
	return diags
}

// balancedParens reports whether the text's parentheses are balanced
// and the depth never goes negative.
func balancedParens(profile dialect.LexerProfile, body string) bool {
	src := []byte(body)
	depth, pos := 0, 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil {
			return false
		}
		if tok.Kind == dialect.KindEOF {
			return depth == 0
		}
		switch tok.Kind {
		case dialect.KindLParen:
			depth++
		case dialect.KindRParen:
			depth--
			if depth < 0 {
				return false
			}
		}
		pos = tok.End
	}
}

func describeParam(name string) string {
	if name == "" {
		return "no parameter"
	}
	return fmt.Sprintf("parameter %q", name)
}

// paramRefs returns the distinct :param names of body in first-
// occurrence order.
func paramRefs(profile dialect.LexerProfile, body string) []string {
	src := []byte(body)
	var out []string
	seen := map[string]bool{}
	pos := 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return out
		}
		if tok.Kind == dialect.KindParamRef {
			name := tok.Text[1:]
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		pos = tok.End
	}
}

// rewriteParams replaces :name refs with the dialect's placeholders so
// probe inputs are valid dialect SQL (the same discipline as
// rules.rewriteParams; duplicated because rules will grow a dependency
// on this package for enforcement, not the reverse).
func rewriteParams(profile dialect.LexerProfile, body string) string {
	question := dialect.StyleOf(profile) == dialect.PlaceholderQuestion
	src := []byte(body)
	var b strings.Builder
	pos, n := 0, 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			b.Write(src[pos:])
			return b.String()
		}
		if tok.Kind == dialect.KindParamRef {
			b.Write(src[pos:tok.Start])
			if question {
				b.WriteByte('?')
			} else {
				n++
				fmt.Fprintf(&b, "$%d", n)
			}
			pos = tok.End
			continue
		}
		b.Write(src[pos:tok.End])
		pos = tok.End
	}
}

func probeMsg(err error) string {
	if pe, ok := err.(*dialect.ParseError); ok {
		return pe.Msg
	}
	return err.Error()
}
