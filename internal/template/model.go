// Package template implements phase P1: scanning .sql template files
// into a structured model (constant skeleton + guarded fragments) with
// exact source spans. See docs/design/01-template-scanner.md.
package template

import (
	"github.com/moznion/sqletch/internal/diagnostics"
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
	SlotSetItem // v0.2: an UPDATE SET assignment
)

// GuardAtom identifies one guard condition. v0.1 has presence atoms
// only; Op/Value are reserved for @when (v0.3).
type GuardAtom struct {
	Param string
	Op    string // "" in v0.1
	Value string // "" in v0.1
}

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

// Occurrence is one :name bind appearance of a parameter.
type Occurrence struct {
	Span         diagnostics.Span
	Guards       []GuardAtom // guard set of the enclosing fragment; nil in skeleton
	InChooseCase bool        // inside a @choose case body (empty guard set, R3)
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
}

type QueryFile struct {
	Path    string
	Queries []*QueryTemplate
}
