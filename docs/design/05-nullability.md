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

## 3. Column decision

Per result column, in order:

1. Config override (`nullable: true/false` per query.column) wins.
2. `Desc.Columns[i].SourceRel` set (direct column reference — pgx
   gives TableOID/AttrNumber): look up `Catalog` column.
   - column `NotNull` **and** its relation is not nullable-side (§2)
     → non-nullable.
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
→ `optional.Option[T]` field (go-optional adoption, design 17; the
scan still targets a `*T` temporary — pgx handles NULL→nil — and
converts with `optional.FromNillable`).
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
- Acceptance: Use Case 1 row struct fields match the spec sketch
  (all non-pointer — `users` columns NOT NULL in the example schema);
  a doctored schema with nullable `status` flips exactly that field to
  `*string`.
