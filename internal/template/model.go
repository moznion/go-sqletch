// Package template implements phase P1: scanning .sql template files
// into a structured model (constant skeleton + guarded fragments) with
// exact source spans. See docs/design/01-template-scanner.md.
package template

import (
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

type Annotation int

const (
	AnnotationInvalid Annotation = iota
	AnnotationOne
	AnnotationMany
	AnnotationExec
	AnnotationExecRows
)

func (a Annotation) String() string {
	switch a {
	case AnnotationOne:
		return ":one"
	case AnnotationMany:
		return ":many"
	case AnnotationExec:
		return ":exec"
	case AnnotationExecRows:
		return ":execrows"
	}
	return ":invalid"
}

// Sep is the composer-owned separator lifted off a fragment body.
type Sep int

const (
	SepNone Sep = iota
	SepAnd
	SepComma // reserved for v0.2 SET/INSERT slots
)

// Slot is the grammatical position a construct occupies. The scanner
// assigns it provisionally from clause context; P2 revalidates against
// the parsed AST (R1).
type Slot int

const (
	SlotUnknown Slot = iota
	SlotWhereConjunct
	SlotJoinItem
	SlotOrderBy
	SlotSetItem        // v0.2: an UPDATE SET assignment
	SlotInsertColumn   // v0.2: an INSERT column-list item (paired, R7)
	SlotInsertValue    // v0.2: an INSERT VALUES row item (paired, R7)
	SlotGroupBy        // v0.2: @choose over whole GROUP BY clauses
	SlotProjExpr       // v0.2: @choose over one projection expression
	SlotHavingConjunct // v0.3: a HAVING conjunct
)

// GuardedItem records one guarded INSERT column/value item for the R7
// pairing check. Name is the column name (column items only).
type GuardedItem struct {
	Name   string
	Guards []GuardAtom
	Span   diagnostics.Span
}

// ValueKind classifies a @when literal (drives the Go type of pure
// control parameters).
type ValueKind int

const (
	ValueNone ValueKind = iota
	ValueString
	ValueInt
	ValueBool
)

// GuardAtom identifies one guard condition: a presence atom
// (@if-present, Op == "") or a value atom (@when, Op "=" or "!=").
// Atoms are compared by equality; identical conditions share a shape
// bit.
type GuardAtom struct {
	Param string
	Op    string
	Value string // Go-side literal (unquoted string, number, true/false)
	Kind  ValueKind
	// RawValue is the literal as written in SQL (e.g. `'a'`, `false`),
	// preserved for `sqletch fmt`.
	RawValue string
}

// IsValue reports whether the atom is a @when value condition.
func (g GuardAtom) IsValue() bool { return g.Op != "" }

// Item is one element of a query template in document order.
// Exactly one of the concrete types below.
type Item interface {
	// Raw is the full byte range this item occupies in the file
	// (constructs include their @… markers). Item ranges of a query
	// are contiguous: concatenated they reproduce the source section.
	Raw() diagnostics.Span
}

type Skeleton struct {
	Text string // verbatim bytes, params still :name
	Span diagnostics.Span
}

func (s *Skeleton) Raw() diagnostics.Span { return s.Span }

type IfPresent struct {
	Guards   []GuardAtom
	Sep      Sep
	Body     string // verbatim (edge-trimmed), separator lifted
	Slot     Slot
	Span     diagnostics.Span // '@if-present' through '@endif'
	BodySpan diagnostics.Span
}

func (i *IfPresent) Raw() diagnostics.Span { return i.Span }

type Choose struct {
	Param   string
	Cases   []ChooseCase // declaration order, excluding default
	Default *ChooseCase  // nil = required parameter
	Slot    Slot
	Span    diagnostics.Span // '@choose' through '@end'
}

func (c *Choose) Raw() diagnostics.Span { return c.Span }

type ChooseCase struct {
	Name string // "" for @default
	Body string // edge-trimmed verbatim; may be empty only for default
	Span diagnostics.Span
}

// OrderBy is the @order-by construct: a closed key set the caller
// orders at runtime (subset, permutation, per-key direction). The
// maximal rendering lists all keys in declaration order; the @default
// body is verified as an extra rendering.
type OrderBy struct {
	Param   string
	Keys    []OrderKey
	Default *ChooseCase // whole ORDER BY clause; may be empty
	Span    diagnostics.Span
}

func (o *OrderBy) Raw() diagnostics.Span { return o.Span }

type OrderKey struct {
	Name string
	Body string // one sort expression
	Span diagnostics.Span
}

// InExpr is the @in construct: `expr @in(:param)` — dialect-complete
// variable-arity membership. On PostgreSQL it renders as a single
// static `= ANY($n)`; expanding dialects (MySQL/SQLite) render
// per-arity `IN (?, …)` lists.
type InExpr struct {
	Param string
	Span  diagnostics.Span
}

func (i *InExpr) Raw() diagnostics.Span { return i.Span }

// FilterTree is the @filter-tree construct: a closed predicate
// vocabulary the caller combines at runtime with AND/OR trees.
// Predicate parameters are constructor arguments, not struct fields.
type FilterTree struct {
	Param      string
	Required   bool // @filter-tree!: nil tree is an error, Unscoped explicit
	Predicates []Predicate
	Span       diagnostics.Span
}

func (f *FilterTree) Raw() diagnostics.Span { return f.Span }

type Predicate struct {
	Name   string
	Body   string   // one boolean expression
	Params []string // distinct :params in first-occurrence order
	Span   diagnostics.Span
}

// Occurrence is one :name bind appearance of a parameter.
type Occurrence struct {
	Span         diagnostics.Span
	Guards       []GuardAtom // guard set of the enclosing fragment; nil in skeleton
	InChooseCase bool        // inside a @choose case body (empty guard set, R3)
	InFilterTree bool        // inside a @predicate body (constructor arg)
}

type Param struct {
	Name        string
	Occurrences []Occurrence
	// GuardBit is set iff the param is used as a presence-guard atom;
	// -1 otherwise.
	GuardBit int
	// Optional is filled by the R9 classification (rules.CheckLexical):
	// true iff every bind appearance lies in fragments guarded by this
	// parameter — a pointer field in the generated params struct.
	Optional bool
}

type QueryTemplate struct {
	Name       string
	Annotation Annotation
	HeaderSpan diagnostics.Span
	Items      []Item
	// Params in first-appearance order (deterministic output).
	ParamOrder []string
	Params     map[string]*Param
	// GuardAtoms in bit order: GuardAtoms[i] has bit i.
	GuardAtoms []GuardAtom
	// InsertColGuards / InsertValGuards collect the guarded INSERT
	// column items and, per VALUES row, the guarded value items — the
	// R7 pairing input. Guarded items are restricted to the tail of
	// their clause (checked at scan time), so sequence equality plus
	// the maximal Describe implies positional alignment in every shape.
	InsertColGuards []GuardedItem
	InsertValGuards [][]GuardedItem
	// TypeHints holds `-- @param name: sqltype` directives: explicit
	// parameter types that override (Tier 1) or supply (Tier 2) the
	// oracle's answer. Values are raw SQL type names resolved by the
	// dialect.
	TypeHints map[string]TypeHint
	// ColumnHints holds `-- @column name: sqltype` directives: result
	// column types for dialects whose oracle cannot type expression
	// columns (SQLite decltype is NULL for any expression). Keyed by
	// the result column's output name.
	ColumnHints map[string]TypeHint
	// WhereKwEnd, TailStart, and StmtEnd are template-source offsets
	// recorded for the policy weaver (design 14 §4.2): WhereKwEnd is
	// the offset just past the statement's top-level WHERE keyword (-1
	// when absent); TailStart is where a synthesized WHERE clause would
	// go when the statement has none — the start of the first
	// GROUP BY/HAVING/ORDER BY/tail/RETURNING clause (or `@order-by`
	// construct), -1 when the statement has no such clause; StmtEnd is
	// the end of the last statement token (excluding any terminating
	// semicolon), the fallback insertion point.
	WhereKwEnd int
	TailStart  int
	StmtEnd    int
	// PolicyOptOuts are the query's `-- @policy-optout: name (reason)`
	// annotations in declaration order (a slice so diagnostics never
	// depend on map iteration order).
	PolicyOptOuts []PolicyOptOut
}

// PolicyOptOut is one `-- @policy-optout` annotation: a deliberate,
// reviewable exemption from a policy, with a mandatory reason.
type PolicyOptOut struct {
	Policy string
	Reason string
	Span   diagnostics.Span
}

// TypeHint is one `-- @param` / `-- @column` directive.
type TypeHint struct {
	SQLType string
	Span    diagnostics.Span
}

type QueryFile struct {
	Path    string
	Queries []*QueryTemplate
}
