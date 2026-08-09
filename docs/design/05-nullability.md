# sqletch Design — 05: Nullability Analysis (Phase P5)

Deliverable: `internal/nullability` — decide, per result column, Go
pointer-ness under the spec's **per-shape-sound discipline**: narrow
only through the skeleton; guarded fragments never narrow; the result
is a correct upper bound on nullability for *every* reachable shape.

Prerequisites: P2 (tree facade), P4 (Desc with source-column refs,
Catalog).

## 1. Contract

```go
// internal/nullability
func Analyze(t *template.QueryTemplate, max ast.Rendering,
    tree dialect.Tree, desc dialect.Desc, cat *cache.Catalog,
    overrides config.NullOverrides) []bool // per result column: nullable?
```

Correctness invariant (enforced by the property test, §5): if
`Analyze` says column i is non-nullable, then **no reachable shape**
can return NULL in column i. False positives (claiming nullable when
actually always non-null) are permitted and expected — they cost a
pointer, not a runtime panic.

## 2. Relation nullability (skeleton-only)

Classify every relation in the maximal tree:

- Skeleton relations: nullable-side ⟺ on the null-extended side of a
  *skeleton* outer join (right side of LEFT JOIN, either side of FULL —
  FULL only occurs in skeleton; guarded joins are INNER/LEFT by R2).
- **Guarded joins: ignored entirely.** They are INNER/LEFT and
  contribute no result columns (R2), so they can filter rows but never
  null-extend any column that reaches the result. This single line is
  what makes the analysis shape-independent — do not "improve" it by
  reasoning about guarded INNER joins implying non-nullness (that was
  review counterexample F1(b); it breaks per-shape soundness).

## 2a. Provenance trust (soundness delta, 2026-08)

Rule 2 below narrows from `SrcRel` — the engine's report of which
base table a result column came from. The original implementation
trusted that report unconditionally and cross-referenced it against a
*name-based* reading of the FROM list; the deterministic adversarial
suite (`TestNullabilitySoundnessAdversarial` and its MySQL/SQLite
variants) proved four NULL-into-value counterexamples in that gap:

- PostgreSQL's `resorigtbl` (and SQLite's column-origin API) resolve
  THROUGH derived tables and CTEs to base-table OIDs the FROM list
  never mentions — a null-extended derived table narrowed as if its
  base table were joined directly.
- A schema-qualified join (`LEFT JOIN aux.orgs`) lost its qualifier in
  `RelRef` and marked the *wrong* same-named table as null-extended.
- `GROUP BY ROLLUP/CUBE/GROUPING SETS` nulls grouping columns in
  super-aggregate rows regardless of catalog NOT NULL.
- SQLite attributes compound-select (UNION) output to the first
  branch's table.

SrcRel narrowing is therefore gated on **provenance trust**:

- **Presence**: the source OID must be accounted for by a skeleton
  FROM relation, resolved schema-aware (`RelRef.Schema` +
  `Catalog.LookupQualified`; an explicit qualifier never falls back to
  another schema). An unaccounted OID fails SAFE to nullable — this
  also covers FROM constructs the frontend does not model
  (e.g. an unrecognized range item simply never becomes present).
- **Kill-switches** (`Tree.HasOpaqueProvenance`,
  `Tree.HasGroupingSets`): a derived table in FROM, a CTE, a set
  operation, or a grouping set disables SrcRel narrowing for the
  whole statement — presence alone cannot save the dual-instance case
  (the same table joined directly *and* wrapped in a null-extended
  derived table). On MySQL/SQLite a database-qualified table
  reference is also opaque: their wire attribution is name-based and
  database-blind. INSERT is exempt on PostgreSQL (RETURNING columns
  are attributed to the target relation only); sublinks are not
  FROM-reachable and stay transparent.

Views need no special case under this rule: PostgreSQL and MySQL
report the *view's own* identity (whose catalog rows carry the
engine's per-view nullability), and SQLite resolves through to a base
table that then fails the presence check.

The index-based expression whitelist (rule 3) is likewise gated on
exact alignment: any star target item, or a length mismatch between
target items and described columns, disables it — a `count(*)` target
must never vouch for a differently-indexed column.

## 3. Column decision

Per result column, in order:

1. Config override (`nullable: true/false` per query.column) wins.
2. `Desc.Columns[i].SourceRel` set (direct column reference — pgx
   gives TableOID/AttrNumber): look up `Catalog` column.
   - column `NotNull` **and** its relation is present and trusted
     (§2a) **and** not nullable-side (§2) → non-nullable.
   - else nullable.
3. Expression columns (no source ref): **nullable**, except a small
   total-function whitelist evaluated on the maximal tree's target
   expression:
   - `count(*)`/`count(x)` → non-nullable
   - `coalesce(...)` with any argument that is a non-nullable column
     (rule 2) or a non-NULL literal → non-nullable
   - `now()`, `current_*` → non-nullable
   - boolean `EXISTS(...)` → non-nullable
   The whitelist lives in one table (`whitelist.go`) with the explicit
   note that additions require a per-shape-soundness argument, not
   just "PostgreSQL docs say it's not null on some path".
4. **No WHERE-based narrowing, ever** — not even from skeleton
   predicates in v0.1. (Skeleton-predicate narrowing like
   `WHERE x IS NOT NULL` is per-shape sound and may be added later;
   it is omitted from v0.1 to keep the first release conservative.
   Guarded-predicate narrowing is *never* sound — review
   counterexample F1(a) — and the code structure must not make it an
   easy "optimization" to slip in: the analyzer receives only the
   skeleton tree view, not fragment bodies.)

`@choose` in ORDER BY (v0.1's only choose slot) cannot affect result
columns; the per-case nullable-most union becomes real work only with
the v0.2 projection slot (08).

## 4. Output usage

P6 maps `nullable=false` → value field + direct `Scan`; `nullable=true`
→ pointer field (`*string`, `*time.Time`, …; pgx handles NULL→nil).
The analysis result is recorded in the oracle cache entry
(`columns[].nullable`) so warm runs skip `Analyze` recomputation only
if inputs (catalog fp) match — cheap either way, but keeps cache
entries self-describing for `explain`.

## 5. Testing & acceptance criteria

- Unit fixtures reproducing the review counterexamples as *must-stay-
  nullable* cases: guarded `IS NOT NULL` conjunct (F1a), guarded INNER
  join over a nullable FK (F1b) — both must yield nullable.
- Skeleton LEFT JOIN → right-side columns nullable even when
  catalog-NOT NULL; skeleton INNER join keeps NOT NULL.
- Whitelist cases: `count(*)`, `coalesce(col, 0)`,
  `coalesce(nullable_col)` (stays nullable).
- Property test extension (in the `-tags devdb` suite): for each corpus
  template, execute representative shapes against seeded data
  engineered to produce NULLs wherever possible; scanning into the
  generated structs must never fail with a NULL-into-value error.
- Deterministic adversarial soundness suite
  (`internal/e2e/nullability_soundness_devdb_test.go`, one per
  dialect): each case pairs a template with fixed seed data that
  forces NULL into a suspect column; a NULL observed in a
  claimed-non-nullable column fails the test. The §2a
  counterexamples live here as permanent regressions; extend this
  suite first when touching the analyzer.
- Acceptance: Use Case 1 row struct fields match the spec sketch
  (all non-pointer — `users` columns NOT NULL in the example schema);
  a doctored schema with nullable `status` flips exactly that field to
  `*string`.
