package dialect

import "fmt"

// JoinType classifies how a FROM relation is introduced.
type JoinType int

const (
	JoinBase JoinType = iota // plain FROM item, not a join
	JoinInner
	JoinLeft
	JoinRight
	JoinFull
	JoinCross
	JoinOther
)

func (j JoinType) String() string {
	switch j {
	case JoinBase:
		return "FROM item"
	case JoinInner:
		return "INNER JOIN"
	case JoinLeft:
		return "LEFT JOIN"
	case JoinRight:
		return "RIGHT JOIN"
	case JoinFull:
		return "FULL JOIN"
	case JoinCross:
		return "CROSS JOIN"
	}
	return "join"
}

// RelRef is one relation of the statement's FROM/target clauses.
type RelRef struct {
	Alias string // alias if present, else ""
	Table string // relation name ("" for subselects etc.)
	Loc   int    // byte offset of the relation in the parsed SQL
	Join  JoinType
}

// StmtKind is the statement class sqletch supports.
type StmtKind int

const (
	StmtOther StmtKind = iota
	StmtSelect
	StmtUpdate
	StmtInsert
	StmtDelete
)

// Tree is the narrow dialect-AST facade the rules engine consumes.
// Deliberately minimal: extending it is a compile-visible act.
type Tree interface {
	StmtCount() int
	Kind() StmtKind
	Relations() []RelRef
	// TopConjunctLocs returns the byte locations of the statement's
	// top-level WHERE conjuncts (AND-flattened).
	TopConjunctLocs() []int
	// OrderByLocs returns byte locations of statement-level ORDER BY
	// item expressions.
	OrderByLocs() []int
	HasDistinctOn() bool
	HasLockingClause() bool
}

// ParseError reports a dialect parse failure at a byte offset into the
// parsed SQL (rendered text; callers map it back to the template).
type ParseError struct {
	Pos int
	Msg string
}

func (e *ParseError) Error() string { return fmt.Sprintf("parse error at %d: %s", e.Pos, e.Msg) }

// Frontend parses rendered SQL and answers node-completeness probes
// (design 02 §4). Probe methods return nil when the fragment forms
// exactly one complete node of its slot.
type Frontend interface {
	Parse(sql string) (Tree, error)
	ProbeExpr(expr string) error
	ProbeJoinItem(item string) error
	ProbeOrderBy(clause string) error
}
