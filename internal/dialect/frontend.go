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
// visibility input (design 14 §11.1) and the nullability analyzer's
// poisoning input (design 05 §2b). References to CTE *names* are
// deliberately included: a CTE shadowing a policy-designated table is
// conservatively treated as touching it (a false positive is a loud
// diagnostic with an opt-out, never a silent leak). Loc is -1 where
// the parser exposes no offset.
type TableRef struct {
	Name   string
	Schema string // explicit schema/database qualifier, "" when unqualified
	Loc    int
}

// SubRel is one FROM-reachable derived table, wrapped in its own Tree
// facade so the nullability analyzer can recurse instead of
// distrusting the whole statement (design 05 §2b).
type SubRel struct {
	Alias        string
	NullableSide bool // on a null-extended side of the ENCLOSING level
	Tree         Tree
}

// CTEDef is one WITH-list definition.
type CTEDef struct {
	Name      string
	Recursive bool
	// Tree is the facade over the body, nil when the body is not a
	// plain query (a data-modifying CTE): such bodies expose only
	// RETURNING rows via a target list the engine attributes to the
	// base tables the DML reads.
	Tree Tree
	// PoisonTables lists every base-table name a data-modifying body
	// (nil Tree) mentions. PostgreSQL attributes a wCTE column through
	// GetCTETargetList — the RETURNING list — to a base table's OID,
	// and that table may sit on a null-extended side of a join inside
	// the DML (e.g. RETURNING a RIGHT JOIN's null-extended side). The
	// analyzer must POISON these OIDs so no clean OUTER instance of the
	// same table can vouch for the null-extended provenance (design 05
	// §2b). Populated only by the PostgreSQL facade — MySQL/SQLite CTE
	// bodies are SELECT-only, so it stays nil there.
	PoisonTables []TableRef
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
	// ScopeAliases is the set of effective relation names (alias if
	// present, else table name) introduced by the subquery scope(s)
	// ENCLOSING this reference — the union across every enclosing
	// subquery level, but NOT the top-level statement's own FROM. It is
	// nil for a top-level reference. R3 uses it to resolve a qualified
	// reference innermost-first: a qualifier found here is bound by a
	// nearer scope (SQL resolves innermost-first), so the reference does
	// not touch a same-named top-level relation and its guard need not
	// be re-derived. A correlated reference — whose qualifier is a
	// top-level relation absent from every enclosing subquery FROM — is
	// NOT listed here and is still checked. Facades under-collect rather
	// than over-collect (set-operation branch FROMs are omitted): a
	// missing name only preserves the pre-existing (sound) check, an
	// extra name would wrongly suppress one.
	ScopeAliases []string
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
	// HasSetOperation reports a statement-level set operation
	// (UNION/INTERSECT/EXCEPT, SQLite compound selects). Engines may
	// attribute set-op output to a branch's base table (SQLite: the
	// FIRST branch), which no per-branch analysis can license —
	// SrcRel narrowing is off for the level (design 05 §2b).
	HasSetOperation() bool
	// DerivedRels returns the statement's own FROM-reachable derived
	// tables, each wrapped in a sub-facade for recursive analysis.
	DerivedRels() []SubRel
	// CTEs returns the statement's WITH-list definitions in order.
	CTEs() []CTEDef
	// HasUnresolvableProvenance reports that the ORACLE's column
	// attribution for this statement can cross-resolve to a wrong
	// catalog entry: MySQL/SQLite attribute by BARE table name with
	// no database qualifier, so any db-qualified reference anywhere
	// in the statement (subqueries included) poisons every
	// name-keyed attribution. PostgreSQL attributes by OID and always
	// reports false (design 05 §2a).
	HasUnresolvableProvenance() bool
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
