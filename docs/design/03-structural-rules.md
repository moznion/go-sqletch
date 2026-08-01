# sqletch Design — 03: Structural Rules R2–R9 (Phase P3)

Deliverable: `internal/rules` complete — guard/scope analysis and all
remaining rule checks, split into a catalog-free pass (runs before the
oracle) and a catalog-dependent pass (runs after, using the catalog
snapshot from 04). R1 is specified in 02.

Prerequisites: P1, P2. Catalog-dependent pass additionally consumes
`cache.Catalog` (04) — pipeline interleave is described in 00.

## 1. Entry points

```go
// internal/rules
func CheckLexical(t *template.QueryTemplate,
    rs []ast.Rendering) []diagnostics.Diagnostic
    // R5, R6, R7(v0.2 stub), R9; plus R1 helpers from 02.

func CheckResolved(t *template.QueryTemplate,
    rs []ast.Rendering, cat *cache.Catalog) []diagnostics.Diagnostic
    // R2 (star expansion), R3 (resolution-based scope), R8,
    // planner-sensitive combination checks.
```

Both return *all* findings (no fail-fast) so users fix batches.

## 2. Guard model

```go
type GuardSet map[template.GuardAtom]struct{}
func (g GuardSet) Superset(of GuardSet) bool
```

Each fragment carries its `GuardSet` (from P1). `@choose` cases,
future `@order-by` keys and `@filter-tree` predicates have the empty
set by definition — represented as `GuardSet(nil)`, and
`nil.Superset(nonEmpty) == false` makes the "cases may not reference
optional joins" rule fall out of the same code path as R3 proper.

## 3. R9 — parameter discipline (catalog-free)

Input: `t.Params` occurrence data from P1. Algorithm per param `p`:

1. Partition occurrences: unguarded (skeleton / case bodies) vs
   guarded (with each occurrence's `GuardSet`).
2. `p` **optional** ⟺ occurrences ≥1 and every occurrence's guard set
   contains atom `p`. Set `Optional=true`, allocate pointer field.
3. If `p` has an unguarded occurrence **and** is used as a guard atom
   anywhere → `SQLETCH110` (vacuous guard; cites both spans).
4. If `p` is a guard atom but binds in **no** fragment guarded by it →
   `SQLETCH111` ("guard parameter never binds; its Go type is
   uninferable — pure control parameters are @when's role (v0.3)").
5. Occurrences only inside fragments guarded by *other* atoms, or only
   inside `@choose` cases → required, non-pointer; mark
   `UnusedWhenInactive` for doc-comment generation in P6.
6. `@choose` params are control params: required unless `@default`
   exists; they must **not** also appear as `:name` bind refs
   (`SQLETCH112` — the enum is not a SQL value).

Ordering note: R9 is purely lexical (occurrence × guard bookkeeping),
which is why it runs pre-oracle; type agreement across renderings is
the oracle's job (04).

## 4. R6 — anchored clauses (catalog-free)

v0.1 has two omissible-clause situations to check:

- WHERE: if **every** conjunct is guarded and there is no
  unconditional conjunct, require the literal anchor `TRUE` as first
  conjunct — detected on the maximal tree (first conjunct is a `TRUE`
  Const) — else `SQLETCH113` with the `WHERE TRUE` hint. (The spec
  convention; `fmt` will auto-insert in v0.2.)
- ORDER BY via `@choose` without `@default`: nothing to check (param
  required ⇒ clause always present). With empty `@default`: the
  clause-omitted rendering was already parse-verified in P2.

SET / INSERT-list anchoring is v0.2 (08); the check function's switch
is written over `Slot` so the cases slot in without restructuring.

## 5. R7 — paired guards (stub)

v0.1 has no INSERT slots; `CheckLexical` contains the rule ID and a
guard that any `SlotInsertColumn`/`SlotInsertValue` fragment (cannot
be produced by the v0.1 scanner) is an internal error. Full algorithm
documented in 08.

## 6. R3 — resolution-based guarded scope (catalog-dependent)

The heart of this phase. Needs a *reference resolver* over the maximal
tree:

```go
type Resolver struct {
    rels []RelInfo // from Tree.Relations(): alias, table, guard set
                   // (join fragments know their GuardSet; skeleton
                   // relations have empty guard set), nullable side
    cat  *cache.Catalog
}
// Resolve maps every ColRef to a RelInfo (or reports ambiguity).
```

Resolution algorithm (mirrors PostgreSQL's, restricted to what
templates can contain):

- Qualified `a.col`: match alias, else table name. Unknown: the
  maximal parse already failed, so this is an internal error here.
- Unqualified `col`: candidate = every rel whose catalog column set
  contains `col`. 0 candidates → internal error (parse caught it);
  ≥2 → already a PostgreSQL ambiguity error at parse/describe time —
  but note pg_query alone doesn't do semantic analysis, so **the
  resolver must report ambiguity itself** (`SQLETCH114`) rather than
  assume the DB flagged it: `check` may run offline from cache where
  Describe never re-runs.

R3 check: for every `ColRef` resolving to a relation introduced by a
guarded join fragment `J`, the enclosing fragment's `GuardSet` must be
a superset of `J`'s. Enclosing fragment = the fragment whose rendered
range contains the ColRef's location (source-map lookup); skeleton and
`@choose` case bodies have empty guard sets ⇒ violation. Diagnostic
`SQLETCH115` (the spec's flagship error message — resolved column,
join span, guard hint).

Also here: `FOR UPDATE`/`FOR SHARE` (Tree.HasLockingClause) combined
with any guarded LEFT join → `SQLETCH116` (planner-sensitive
combination, per spec's known-list; the list lives in one table in
`plannerchecks.go`).

## 7. R2 — star expansion & shape constancy (catalog-dependent)

- `SELECT *` / `qualifier.*` in the target list: expand against the
  resolver's relation list. If expansion includes any column from a
  guarded join's relation → `SQLETCH117` (R2: optional joins
  contribute no result columns). Otherwise **codegen uses the
  expanded, explicit column list** (deterministic output even if the
  catalog gains columns later — the cache pins the catalog).
- Join type restriction (INNER/LEFT) was enforced in P2 (`SQLETCH101`)
  where join type is visible; re-asserted here as a defensive check.
- Result-shape constancy across renderings (same column names, count)
  is verified in 04 against Describe outputs (`SQLETCH210`) — R2's
  *type-level* half lives with the oracle; the rule doc notes the
  split.

## 8. R8 — case-local names (catalog-dependent)

v0.1's only case bodies are whole `ORDER BY` clauses, which cannot
define aliases in PostgreSQL — so R8 reduces to an assertion plus one
real check: an ORDER BY case may reference **output aliases** defined
in the skeleton target list (legal; resolver knows the target list) or
skeleton/guard-appropriate columns (R3 path). Case bodies referencing
an alias defined in *another* case is impossible in v0.1 (no
alias-defining slots) — the full R8 machinery (name-definition
tracking per case) is specified in 08 for the v0.2 projection slot.

## 9. Shape enumeration (`internal/shape`)

Not a rule, but delivered in this phase for `explain`/`--exhaustive`:

```go
func Enumerate(t *template.QueryTemplate, cap int) iter.Seq[ShapeKey]
func Count(t *template.QueryTemplate) *big.Int // guards×cases product
```

Enumeration composes via the same `ast.Render` path (a `ShapeKey`
selects fragments instead of "all") — reusing the P2 renderer for
arbitrary shapes is what makes `check --exhaustive` and the property
test cheap to build.

## 10. Testing & acceptance criteria

- The complete Rejected Examples corpus from the spec as
  `.diag` golden tests (R3 unqualified-reference case, vacuous guard,
  unanchored WHERE, nesting → these must produce exactly the
  documented codes at the documented spans).
- Soundness-oriented unit tests: R3 with alias chains (guarded join B
  referencing guarded join A), `@choose` case referencing a guarded
  join (must fail — the empty-guard-set path), unqualified ref
  resolving to a guarded join (must fail), same ref with proper guard
  (must pass).
- R9 truth table test: (unguarded?, self-guarded?, other-guarded?,
  is-guard?, binds-under-self?) × expected classification/diagnostic.
- Enumeration: Use Case 1 counts 64; iteration is duplicate-free and
  matches `Count`.
- Acceptance: all Use Case templates pass `CheckLexical` +
  `CheckResolved` cleanly against a fixture catalog; the corpus diag
  set is byte-stable.
