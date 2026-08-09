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
	Alias  string // alias if present, else ""
	Table  string // relation name ("" for subselects etc.)
	Schema string // explicit schema/database qualifier, "" when unqualified
	// Only marks a `FROM ONLY table` reference (PostgreSQL): the scan
	// excludes inheritance children, so child rows cannot undermine
	// the parent's NOT NULL declarations.
	Only bool
	Loc  int // byte offset of the relation in the parsed SQL
	Join JoinType
	// NullableSide reports whether this relation sits on a
	// null-extended side of an outer join in this statement (right of
	// LEFT, left of RIGHT, either side of FULL) — the nullability
	// analysis input.
	NullableSide bool
}

// TableRef is one base-table name referenced anywhere in the
// statement, including subquery and CTE bodies — the policy weaver's
// visibility input (design 14 §11.1). References to CTE *names* are
// deliberately included: a CTE shadowing a policy-designated table is
// conservatively treated as touching it (a false positive is a loud
// diagnostic with an opt-out, never a silent leak). Loc is -1 where
// the parser exposes no offset.
type TableRef struct {
	Name string
	Loc  int
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

// ColRef is one column reference anywhere in the statement.
type ColRef struct {
	Fields     []string // qualified parts, e.g. ["u", "id"] or ["id"]
	Star       bool     // reference ends in * (e.g. u.*)
	Loc        int      // byte offset in the parsed SQL
	InSubquery bool     // inside a sublink/derived table/CTE scope
}

// TargetItem is one projection entry.
type TargetItem struct {
	Name      string // output alias ("" if none)
	Star      bool   // item is * or qualifier.*
	Qualifier string // "u" for u.*; "" for bare *
	FuncName  string // lowercased function name when the item is a bare call
	// Total marks an expression that can never evaluate to NULL
	// regardless of data: a non-NULL literal, EXISTS, an IS [NOT]
	// NULL / boolean test, a non-null value function, a cast of a
	// total expression, or coalesce with at least one total argument.
	// Data-INDEPENDENT only — a column reference never makes an
	// expression total (its nullability is the analyzer's job).
	Total bool
	// AggArg is the qualified path of the call's single bare-column
	// argument (["u","org_id"] or ["org_id"]) when the item is a
	// plain aggregate call over exactly one column with no FILTER
	// clause and no OVER clause; nil otherwise. The nullability
	// analyzer combines it with GROUP BY presence: an aggregate over
	// a non-nullable column of a non-null-extended relation is
	// non-null when every output row's group is non-empty.
	AggArg []string
	Loc    int
}

// Tree is the narrow dialect-AST facade the rules engine consumes.
// Deliberately minimal: extending it is a compile-visible act.
type Tree interface {
	StmtCount() int
	Kind() StmtKind
	Relations() []RelRef
	// DeepTables reports every base-table name referenced anywhere in
	// the statement — subqueries and CTE bodies included, unlike
	// Relations, which stops at the statement's own FROM/target
	// clauses. The policy weaver compares the two to reject designated
	// tables in positions it cannot scope (design 14 §D6).
	DeepTables() []TableRef
	ColumnRefs() []ColRef
	TargetItems() []TargetItem
	// TopConjunctLocs returns the byte locations of the statement's
	// top-level WHERE conjuncts (AND-flattened).
	TopConjunctLocs() []int
	// HavingConjunctLocs is TopConjunctLocs for the statement-level
	// HAVING clause (empty when the statement has none).
	HavingConjunctLocs() []int
	// OrderByLocs returns byte locations of statement-level ORDER BY
	// item expressions.
	OrderByLocs() []int
	HasDistinctOn() bool
	HasLockingClause() bool
	// HasFetchWithTies reports FETCH FIRST … WITH TIES, which makes
	// the ORDER BY clause mandatory (@order-by then needs a @default).
	HasFetchWithTies() bool
	// HasOpaqueProvenance reports whether a result column's
	// source-relation identity (Desc.Columns[].SrcRel) may have been
	// attributed through a construct Relations() cannot model: a
	// derived table in FROM, a CTE, or a set operation. Engines
	// resolve column origins THROUGH those constructs to base tables,
	// so when this is true, SrcRel-based nullability narrowing is
	// unsound and must be disabled (design 05 §2a).
	HasOpaqueProvenance() bool
	// HasGroupingSets reports ROLLUP / CUBE / GROUPING SETS in the
	// statement-level GROUP BY: super-aggregate rows null out grouping
	// columns regardless of catalog NOT NULL, so SrcRel-based
	// narrowing is unsound while one is present (design 05 §2a).
	HasGroupingSets() bool
	// HasGroupBy reports a statement-level GROUP BY clause (of any
	// form). With one present — and no grouping sets — every output
	// row aggregates a NON-EMPTY group, which is what lets strict
	// aggregates over non-nullable columns narrow (design 05 §3a).
	HasGroupBy() bool
	// NotNullConjuncts returns the bare column reference of every
	// depth-0 statement-level WHERE conjunct of the exact form
	// `col IS NOT NULL`. Loc is the conjunct's byte offset in the
	// parsed SQL — the analyzer uses it to require the conjunct to be
	// SKELETON text (present in every shape) before narrowing.
	NotNullConjuncts() []ColRef
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
	ProbeOrderByKey(expr string) error
	ProbeGroupBy(clause string) error
	ProbeSetItem(item string) error
	ProbeInsertValue(expr string) error
}
