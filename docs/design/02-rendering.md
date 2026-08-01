# sqletch Design — 02: Renderings, Frontend, Source Maps, R1 (Phase P2)

Deliverable: `internal/ast` + `internal/dialect/postgres` (frontend
half) — build the maximal and per-case renderings, parse them with
pg_query, tie fragments to AST nodes via source maps, and enforce R1
(slot legality + node completeness). Still no database.

Prerequisites: P1 (`QueryTemplate`).

## 1. Renderings

```go
// internal/ast
type Rendering struct {
    Kind      RenderKind // Maximal | ChooseCase{blockIdx, caseIdx}
    SQL       string     // placeholders already $n
    ParamsSeq []string   // template param name per placeholder index
    Map       SourceMap
}
```

`Render(t *QueryTemplate, sel CaseSelection) Rendering` walks
`t.Items` in order and emits:

- Skeleton chunks: verbatim.
- `@if-present` (all active in every rendering — maximal inclusion):
  - `SlotWhereConjunct`: `AND (` + body + `)` — the composer-owned
    separator and **mandatory parentheses** (spec P2). The same
    wrapping is used at runtime; the shared conformance test (see 00)
    pins this.
  - `SlotJoinItem`: body verbatim (no separator, no parens).
- `@choose`: the selected case's body (maximal = case 0; `@default`
  counts as the last selectable case). Non-selected cases contribute
  nothing.
- Fragments joined by single `\n` (P2: line-comment safety).
- `:name` → `$n`: numbered in first-occurrence order *within this
  rendering*; `ParamsSeq[n-1]` records the name. The rewrite happens
  token-wise using the lexer profile (never regex), so params inside
  strings/comments are untouched.

The set of renderings for v0.1: 1 maximal + (cases−1) per `@choose`
block (each non-first case substituted into the otherwise-maximal
query). `@default` is one of the cases (spec: verified like any case).

Renderings are deterministic; `Rendering.SQL` for the maximal query is
also the input to the oracle cache key (04).

## 2. Source map

```go
type SourceMap struct{ segs []Seg } // sorted by rendered offset
type Seg struct {
    ROff, RLen int  // rendered range
    TOff       int  // template byte offset (same file)
    Synth      bool // composer-synthesized text (AND, parens, \n, $n)
}
func (m SourceMap) ToTemplate(rOff int) (tOff int, synth bool)
```

Built during rendering: every verbatim copy appends a 1:1 segment;
every synthesized token appends a `Synth` segment pointing at the
nearest enclosing construct's span (so an error inside synthesized
parens still lands on the fragment). Placeholder rewrites map `$n`
back to the `:name` token span.

All downstream position translation (parse errors, oracle errors,
rule diagnostics) goes through `ToTemplate` — there is exactly one
implementation of position math in the codebase.

## 3. Frontend interface and pg_query integration

```go
// internal/dialect
type Frontend interface {
    Parse(sql string) (Tree, error)      // error carries byte offset
    // Probe helpers for node-completeness (see §4):
    ProbeExpr(expr string) error         // parses as ONE a_expr?
    ProbeJoinItem(item string) error     // parses as ONE join item?
    ProbeOrderBy(clause string) error    // parses as ORDER BY clause?
}
```

`dialect/postgres` implements `Parse` with `pg_query.Parse` (protobuf
tree). `Tree` is a thin dialect-neutral facade over what P3 needs —
not a full AST abstraction:

```go
type Tree interface {
    Relations() []RelRef      // FROM items: alias, table name, join
                              // type, location, nullable-side flag
    ColumnRefs() []ColRef     // every ColumnRef: fields, location
    TargetList() []TargetItem // projection: name/star/location
    OrderByLocs() []int       // statement-level ORDER BY item locations
    HasDistinctOn() bool
    HasLockingClause() bool   // FOR UPDATE/SHARE (planner-check, P3)
    Utility() bool            // non-DML (rejected)
}
```

Deliberately narrow: P3 consumes exactly this; extending the facade is
a compile-visible act, keeping the "nothing outside dialect/postgres
imports pg_query" rule honest.

pg_query gives start locations only. We do **not** compute node end
offsets; every check that would need extents is instead formulated as
a probe (§4) or a location-membership test ("is this location inside
fragment F's rendered range" — start locations suffice for that).

## 4. R1: slot legality and node completeness

Implemented in `internal/rules` (file `r1.go`) but executed in this
phase's pipeline position.

**Slot legality.** For every fragment, cross-check the scanner's
clause context against the parsed maximal tree:

- `SlotWhereConjunct`: fragment's rendered range must fall inside the
  statement's WHERE (verified via the wrapping parens' location being
  a direct BoolExpr(AND) argument of the WHERE qual — location
  membership on the top-level qual's arg list).
- `SlotJoinItem`: the fragment range must coincide with exactly one
  `RelRef` of join type INNER or LEFT (`SQLETCH101` for RIGHT/FULL —
  R2's join-type restriction is enforced here where join type is
  visible).
- `SlotOrderBy` (`@choose`): each case rendering's ORDER BY item
  locations must all lie within the case body's rendered range, and
  the maximal tree without the clause parses when `@default` is empty
  (that rendering exists iff an empty default is declared: render it
  and parse — this *is* the omission-validity check for v0.1).

**Node completeness** (fragment = one complete AST node) by probing:

- WHERE conjunct: `ProbeExpr(body)` parses
  `SELECT WHERE (<body>)` — wait, concretely: `SELECT 1 WHERE (<body>)`
  must parse AND the parenthesized qual must be the entire WHERE qual
  (single node). Combined with mandatory parens at composition, a
  probe-passing body is one node in every context. Failure:
  `SQLETCH102` ("fragment must be a single predicate; got trailing
  tokens / multiple clauses").
- Join item: `ProbeJoinItem` parses `SELECT 1 FROM sqletch_probe_t
  <item>` and requires exactly 2 relations and no WHERE. `SQLETCH102`.
- `@choose` ORDER BY case: `ProbeOrderBy` parses
  `SELECT 1 <case-body>` requiring the body to contribute only an
  ORDER BY clause. `SQLETCH102`.

Probes use placeholder-rewritten bodies (params → `$n`) so `:name`
never reaches pg_query. Probe parse errors are mapped through the
fragment's source map.

**Parse of every rendering.** All renderings must parse
(`SQLETCH100`, position mapped). A rendering that parses as a utility
statement or as >1 statement: `SQLETCH103`.

## 5. Failure-mode notes (implementation gotchas)

- pg_query locations are byte offsets into the *rendered* SQL — always
  translate before showing users; tests must include a multibyte
  (UTF-8) template to pin this.
- `WHERE TRUE AND (frag)` : pg_query flattens nested ANDs into one
  BoolExpr args list — the membership test must handle both the
  flattened and nested forms (normalize by walking BoolExpr(AND)
  recursively into a conjunct list).
- Renderings with zero placeholders still need `ParamsSeq` non-nil
  (empty) for cache-key stability.

## 6. Testing & acceptance criteria

- Golden renderings: `testdata/render/*.sql` → `*.maximal.golden.sql`
  + `*.case-N.golden.sql`. Byte-exact.
- Source-map tests: for a corpus of (rendered offset → expected
  template offset) probes, including offsets inside synthesized text.
- R1 tests: every Rejected Example lands its documented code; a
  crafted `AND a = 1 OR b = 2` conjunct triggers `SQLETCH102`
  (node-completeness); RIGHT JOIN fragment triggers `SQLETCH101`.
- Multibyte template test (Japanese comments in SQL) for position math.
- Acceptance: Use Case 1 produces 4 byte-stable renderings; all parse;
  R1 passes; the shared compose-conformance fixture (00) is seeded
  with these renderings.
