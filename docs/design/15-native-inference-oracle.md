# sqletch Design — 15: Native-inference oracle backend

**Status: ACCEPTED — in implementation (phase 1 started
2026-08-02).** This document expands the recorded notes (spec
§"Oracle backends" stage 3; spec "Future Roadmap";
`08-later-phases.md` §"Beyond 1.0") into a design. The decisions
D1–D8 (§6) were settled with the user on 2026-08-02, each on the
recommended option; the settled outcome is recorded inline per item.
Reflection into `docs/spec.md` happens with the phase that ships the
first user-visible surface (the D1 config key, phase 2) — phase 1 is
test infrastructure only and changes no observable behavior.

The native-inference backend is a self-implemented `dialect.Oracle`
(à la sqlc): `Describe` answered by sqletch's own resolution and
typing over a catalog built from the schema DDL, with no database
process anywhere — not even in-process. Per the spec's staging it is
deliberately the *last* backend, pursued only where no embedded real
engine exists and only because the project has accumulated a corpus of
real-engine answers to test against. **First and only initial target:
MySQL** — SQLite already runs the real engine in-process, and
PostgreSQL has a designed embedded path (doc 09) that preserves "the
database is the type checker" outright.

## 1. What this buys — the benefit case

1. **MySQL is the last dialect whose cold run needs a server.** A cold
   `generate`/`check` on SQLite is fully in-process today, and on
   PostgreSQL will be once doc 09 ships. On MySQL it needs Docker or a
   DSN, and no embeddable real MySQL exists to close that gap the doc
   09 way. Native inference is the only remaining route to
   "clone, `go generate`, done" on all three dialects.

2. **The corpus makes it buildable without being written blind.**
   Every committed oracle cache entry is already exactly the required
   ground-truth triple — `schema_fp`, `rendered_sql`, and the full
   `Desc` (params, columns, source identity) in canonical JSON
   (doc 04 §3, `internal/cache/store.go`). Every conformance and
   property-test run produces more. A native backend is therefore
   differential-tested against the real engine's answers from day one;
   nothing extra needs collecting. This is the precondition the spec
   set for attempting the backend at all.

3. **Tier 2 has already paid most of the cost.** The MySQL protocol
   types no parameters, so `-- @param` annotations are mandatory on
   every bind parameter today (`internal/dialect/mysql/oracle.go`,
   `driver.annotationsRequired`). The native backend inherits that
   verbatim: its parameter half is annotation resolution, zero
   inference. The pipeline likewise already has a discipline for
   result columns the oracle cannot type — `-- @column` hints behind
   `driver.columnHintsRequired`, built for SQLite decltype gaps
   (`internal/cli/driver.go:37`). The native backend composes existing
   mechanisms; it does not invent new annotation machinery.

4. **CI and contributor friction.** The devdb E2E suite keeps its real
   MySQL container regardless (§7 — the differential gate *requires*
   it), but user-facing cold runs, docs examples, and downstream CI
   stop needing one.

What it does **not** buy, stated up front: it is the first backend
that replaces, rather than relocates, the real engine — the design
principle "the database is the type checker" is substituted by
"sqletch's inference, continuously proven equal to the database on
everything it accepts". The soundness of that substitution rests
entirely on §2's fail-closed rule plus §7's differential gate, and the
subset it accepts in v1 is deliberately narrow. It also does nothing
for planner-stage verification (§5.3): `EXPLAIN`-level assurance keeps
requiring a real engine.

## 2. Soundness stance — fail closed

Premise P1 (pinned parameter types hold at runtime) and the
verification claim (every reachable shape prepares) are only as good
as the oracle's answers. A native backend can fail in two directions:

- **Accepting what the engine would reject** — an unverified query
  ships and fails at execution time. This is the catastrophic
  direction: it silently voids the compiler's core promise.
- **Typing differently than the engine** — codegen bakes a wrong bind
  or scan type; failure surfaces at execution or as data corruption.

The rule that governs the whole design: **anything outside the
modeled subset is refused, never guessed.** The refusal is a
diagnostic (§8) that names both escape hatches — switch the backend to
`server`, or bring the construct into the subset (add the annotation,
add the alias, restructure the DDL). A native backend that guesses on
uncertainty is strictly worse than no native backend, because its
failures are invisible until production. Rejecting *more* than the
engine costs a diagnostic and an escape hatch; accepting more than the
engine costs the soundness argument. All v1 choices below (annotation-
required expression columns, CREATE-TABLE-only DDL, alias-required
output names) buy their small scope with this rule.

## 3. Position in the architecture

The backend implements `dialect.Oracle`
(`internal/dialect/oracle.go:63`) and is constructed by
`driver.acquire` (`internal/cli/driver.go:38`) — the seam doc 04 §1
reserved for exactly this ("Backend selection is config, not code").
Nothing above the interface changes:

- The pipeline flow (doc 04 §4), cross-rendering agreement
  (`SQLETCH210`/`211`), nullability (which keys on
  `ColumnDesc.SrcRel/SrcAtt` and the catalog's `NotNull`), codegen,
  and the runtime are all consumers of `Desc` and `cache.Catalog` and
  cannot tell backends apart.
- The cache neither records nor keys on the backend. `Fingerprint`
  stays `sha256(dialect ‖ server_version ‖ schema inputs)` and
  `queryHash` stays `sha256(fp ‖ rendered SQL)`
  (`internal/cache/store.go:35`, `:75`). **The backend must not enter
  the fingerprint** — the whole point is that a native-warmed cache
  and a server-warmed cache are interchangeable.

That interchangeability is a hard requirement, not an aspiration:
**for every input it accepts, the native backend's cache entry must be
byte-identical to the server backend's** — same `Desc`, same synthetic
OIDs, same canonical JSON. Byte-identity of cache entries is the
production form of the differential test (§7), the same way
`TestComposeConformance` pins `runtime.Compose` to `ast.RenderShape`.

Concretely, `Snapshot` must reproduce the server snapshot's synthetic
numbering exactly: table OIDs 1-based in table-name order, column att
numbers = ordinal positions, `Schema` = the database name
(`internal/dialect/mysql/oracle.go:130`, and the `ORDER BY
table_name, ordinal_position` in `snapshotQuery`). Note the server
orders by information_schema's collation — the catalog builder must
match that ordering rule (byte-wise on the stored names is the
proposal; verified by the differential gate).

## 4. Scope of inference in v1 — resolution, not typing

The insight that makes v1 tractable: on MySQL, **the oracle's typing
duties are already mostly annotation-shaped**, and what remains is
name resolution against the catalog.

| `Desc` field | Server backend source | Native v1 source |
| --- | --- | --- |
| `Params[i]` | untyped by protocol; annotation-filled downstream | identical — zero slots, annotation-filled downstream |
| `Columns[i].Name` | protocol `name` field | alias, or column name for direct refs (D4 for the rest) |
| `Columns[i].Type` | protocol type code + flags | catalog `column_type` for direct refs; `-- @column` for expressions (D3) |
| `Columns[i].SrcRel/SrcAtt` | `org_table`/`org_name` → catalog lookup | resolution → the same catalog entry |

So the v1 describe engine is: parse the rendering with the existing
TiDB-parser frontend, resolve the relation set and every output
expression against the DDL-built catalog, expand `*`/`t.*` in catalog
order, take types from the catalog for direct column references, and
demand `-- @column` for everything else. The encoded
`TypeRef.OID` (wire type code + unsigned/binary flag bits) comes from
the same `TypeByName` path annotations already use — no new type
encoding.

What the native backend must also replicate is the *rejection*
surface of `COM_STMT_PREPARE`, because prepare failures are how the
pipeline reports broken renderings (`SQLETCH202`): unknown
table/column, ambiguous unqualified reference, wrong function arity in
the checked subset, `*` with no FROM, and so on. These checks overlap
`rules.CheckResolved` (R3) by design — but the oracle must stay
self-contained (`internal/rules` is a *consumer* of oracle results;
the oracle cannot depend on it), and R3 deliberately skips positions
the native engine cannot skip (unqualified refs in subquery scopes).
Shared machinery, if any falls out, moves *down* into the frontend's
tree utilities, not across.

Widening beyond this subset — inferring `count(*)` as `bigint`,
arithmetic result types, `COALESCE` — is explicitly a later, separate
phase (D3), gated per-construct on corpus agreement. MySQL's implicit
type coercion rules are a swamp (the `decimal`/`double` promotion
table, signedness propagation, the `utf8mb4` vs binary split this
codebase already handles for TEXT/BLOB); v1 refuses the swamp
entirely.

## 5. Components

### 5.1 Catalog builder (`internal/dialect/mysql`, native half)

Parses the ordered schema inputs (`cfg.Schema` globs — see D6 for why
`schema_setup_cmd` is out) with the TiDB parser, which handles DDL
with the same `test_driver` import the frontend already carries.
v1 subset:

- `CREATE TABLE`: column names, types (spelled as
  information_schema's `column_type` would spell them, since that is
  what `TypeByName` and the differential gate compare), `NOT NULL`,
  `DEFAULT`, `AUTO_INCREMENT` (→ `HasDefault`, matching the
  `snapshotQuery` predicate), `PRIMARY KEY` (columns become
  `NotNull`, as MySQL does implicitly).
- `DROP TABLE IF EXISTS`, `SET`, and other statements with no catalog
  effect in a fresh schema: ignored.
- Everything else — `ALTER TABLE`, `CREATE VIEW`, generated columns,
  triggers, `CREATE TABLE … SELECT` — is **fail-closed** (D5) with a
  diagnostic naming the statement and the escape hatches.

The builder's output is a `cache.Catalog` byte-identical (after
canonical JSON) to what `Snapshot` over a server that ran the same DDL
produces — asserted directly by a devdb differential test.

### 5.2 Describe engine

Input: rendered SQL (one shape). Steps:

1. Parse with the existing frontend; a parse failure is an
   `OracleError` with the node offset (the frontend already carries
   byte offsets for expressions; relation locations use the lexical
   recovery already built for R3).
2. Build the top-level scope from the relation set; resolve every
   result expression. Subqueries in the FROM clause or SELECT list are
   outside the v1 subset (fail closed) — scalar subqueries and derived
   tables need full inner-scope typing, exactly the machinery v1
   refuses to half-build. (R3's documented skip of unqualified refs in
   subquery scopes is a *rule* relaxation; an oracle cannot relax —
   it must answer or refuse.)
3. Expand stars in catalog order; type direct references from the
   catalog; match `-- @column` hints to remaining output positions;
   any expression column without a hint is refused (D3).
4. Count `?` placeholders and emit that many zero `TypeRef`s — the
   annotation-fill downstream is unchanged.
5. Emit `Columns` with `SrcRel`/`SrcAtt` for direct references, zero
   for hinted expressions — the same shape the server backend emits,
   feeding P5 nullability identically.

Determinism: the engine is a pure function of (catalog, SQL,
hints) — no maps ranged into output, property-tested byte-stable.

### 5.3 `Plan` — the honest gap

There is no planner. Options:

- (a) `Plan` = success after `Describe`-level validation, and the
  manual documents that planner-stage assurance (the `EXPLAIN` half of
  `check --exhaustive` and the property test) requires a server or
  embedded backend.
- (b) `Plan` returns "unsupported", making `check --exhaustive` an
  error under the native backend.
- (c) Best-effort static approximation of planner errors.

**Recommendation: (a).** (c) is guessing, which §2 forbids. (b) makes
the exhaustive prepare-check unavailable just because the explain leg
is — but `--exhaustive` under a native backend then *silently* proves
less than under a server, which needs at least a printed notice.
The conformance/property suite in this repo's CI keeps running against
real MySQL either way, so the project-level EXPLAIN backstop never
weakens; the question is only what a *user's* `--exhaustive` claims.
**DECISION NEEDED** on (a)-with-notice vs (b).

### 5.4 `ServerVersion`

Returns the pinned `cfg.ServerVersion` — under a native backend the
pin *is* the modeled engine, there is nothing to disagree with, so
`devdb.VersionMismatchError`/`SQLETCH200` cannot fire. Consequence
worth stating in the manual: the pin selects which version's semantics
the corpus was collected against; bumping the pin invalidates the
fingerprint and demands re-validation, which is exactly the right
behavior inherited for free.

## 6. Decisions the spec must make

### D1 — Configuration surface and fallback policy

Backend selection needs a config key; proposed:

```yaml
dialect: mysql
database:
  oracle: native        # default: server
```

- (a) `database.oracle: server | native`, strict: a construct outside
  the subset is a diagnostic, the run fails.
- (b) Same key plus `oracle_fallback: server` — on refusal, acquire a
  dev DB and answer from it (cache still shared).
- (c) Automatic: try native, fall back silently when refused.

**Recommendation: (a) for v1.** (c) is disqualified outright — a
silent fallback means CI without Docker fails where a laptop with
Docker passes, on the same commit, which is the "works on my machine"
class of failure. (b) is coherent (it is how the cache already
behaves: offline until a miss) and is the natural v2, but it doubles
the initial test matrix. `Config` is v1.0-frozen surface, additive
keys only — naming must be settled once. Note `database.dsn` is
meaningless under `native` (validation: setting both is `SQLETCH301`).

**Settled 2026-08-02: (a)** — `database.oracle: server | native`,
strict, no fallback in v1.

### D2 — What `check --exhaustive` claims under native

§5.3: recommendation (a) — `Plan` succeeds after describe-validation,
`--exhaustive` prints a notice that the EXPLAIN leg requires a
server-backed run. The alternative (b) refuses `--exhaustive`
entirely. Either way the manual's verification-model chapter must say
what a native-backed check does and does not prove.

**Settled 2026-08-02: (a) with the printed notice.**

### D3 — Expression result columns: annotate or infer

- (a) v1 requires `-- @column name: type` for every non-direct output
  column, reusing `driver.columnHintsRequired` exactly as SQLite does.
- (b) Implement inference for a whitelisted expression subset
  (aggregates, `COUNT(*)`, literal types) at once.
- (c) Full expression typing à la sqlc.

**Recommendation: (a), with (b) as later corpus-gated widenings.**
(c) blind-reimplements MySQL's coercion rules, which is precisely what
the spec's staged strategy exists to avoid. Each (b) widening lands
only with differential evidence over the corpus plus targeted devdb
cases for its edge behavior (signedness, DECIMAL scale, NULL-ability
of aggregates). The widening path never changes config or annotations
— hints simply become optional-but-asserted (D7) for constructs the
engine learns.

**Settled 2026-08-02: (a)**, with (b) as later corpus-gated
widenings.

### D4 — Output names of expression columns

MySQL names an unaliased expression column with the original
expression text, whitespace and case preserved (`count(*)`,
`o.total + 1`). Reproducing that spelling from a parsed tree is a
fidelity trap (the parser normalizes), and byte-identity of entries
(§3) makes a near-miss a cache-thrashing bug.

- (a) Require `AS alias` on every non-direct output column under the
  native backend; diagnostic shows the rewrite.
- (b) Reproduce MySQL's name derivation from the source text spans.

**Recommendation: (a).** The hint requirement (D3) already touches
every such column; requiring the alias alongside is marginal, and (b)
is exactly the kind of cosmetic-but-load-bearing reimplementation that
would consume the differential budget on names instead of types.
Generated row-struct fields need stable names anyway — an aliased
column is better sqletch style on every backend.

**Settled 2026-08-02: (a)** — `AS` required on non-direct output
columns under the native backend.

### D5 — The DDL subset

§5.1 proposes `CREATE TABLE` (+ no-op statements) only, everything
else fail-closed. The sharp edge is `ALTER TABLE`: migration-shaped
schema directories (`001_create.sql`, `002_add_column.sql`) are
common. Supporting ALTER means implementing MySQL's alter semantics
(column ordering with `AFTER`, implicit NOT NULL changes) in the
builder.

- (a) CREATE-only; the diagnostic tells migration users to point
  `schema:` at a consolidated dump.
- (b) CREATE + the common ALTER subset (ADD/DROP/MODIFY COLUMN).

**Recommendation: (a) for v1**, because the differential gate makes
(b) safe to add later — an ALTER-handling bug shows up as a catalog
byte-diff, so the subset can grow with evidence rather than up front.

**Settled 2026-08-02: (a)** — CREATE-only in v1.

### D6 — `schema_setup_cmd` is incompatible

The native backend cannot run goose/atlas — there is no server to run
them against. `database.oracle: native` therefore requires the
`schema:` globs mode; combining it with `schema_setup_cmd` is a config
validation error (`SQLETCH301`). Recorded as a decision because it is
a visible capability cliff between backends, not just a limitation
note. (No alternative is proposed; executing migration tools against
a sqletch-embedded fake server is out of the question.)

**Settled 2026-08-02: as proposed** — `native` requires `schema:`
globs; the combination with `schema_setup_cmd` is `SQLETCH301`.

### D7 — Hints flip from SUPPLY to ASSERT when a server is present

Under the native backend, `-- @column` hints SUPPLY types the way
`-- @param` does on Tier 2 — nothing checks them. The v0.4 Tier 1
lesson (`SQLETCH213`) is that an unchecked annotation is invisible to
every other phase and fails only at execution time. The differential
harness (§7) is where a server *is* present — so whenever it runs,
hints on expression columns must be ASSERTED against the protocol's
column metadata, engine-wins, mirroring `SQLETCH213` semantics
(new code, §8; `writableName`-style rewrite spelling in the hint).
**DECISION NEEDED** only on surface: does this assert also run during
ordinary server-backed `generate` for users who annotate voluntarily
(recommended: yes — it is the same check, and it hardens the corpus),
or only inside the differential job?

**Settled 2026-08-02: yes** — the assert runs wherever a server
answers, including ordinary server-backed `generate`/`check`.

### D8 — May a native-backed run write the committed cache?

If entries are byte-identical by construction, refusing to write them
protects nothing; but a corpus grown by the system under test is
self-confirming — a wrong native answer, once cached, *becomes* the
ground truth future runs are compared against.

- (a) Native runs read and write the cache like any backend; corpus
  authority comes from the differential CI job, which re-describes
  entries against real MySQL and fails on any byte diff (the entry's
  provenance is irrelevant because the job re-derives, not trusts).
- (b) Native runs read the cache but never write it (cold native runs
  re-infer every time until a server-backed run warms the cache).
- (c) Entries record their producing backend.

**Recommendation: (a).** (c) breaks byte-identity and adds a field the
frozen format does not need. (b) destroys the offline story the
backend exists for. (a) is sound *provided* the differential job
re-derives from `rendered_sql` + schema rather than comparing entries
to each other — which is how it must work anyway (§7). Requires no
`FormatVersion` bump.

**Settled 2026-08-02: (a)** — native runs read and write the cache;
corpus authority is the re-deriving differential CI job.

## 7. Differential testing — the acceptance gate

Per the spec, the backend is "built and continuously
differential-tested against the real engine's answers instead of
written blind". Three harness modes:

1. **Dual-backend live** (`-tags devdb`, CI): for every rendering the
   conformance and property suites already enumerate, run `Describe`
   on both backends and compare — full `Desc` deep-equality (params,
   column names, encoded OIDs, `SrcRel`/`SrcAtt`) *and* canonical
   cache-entry byte-identity; plus `Snapshot` byte-identity per test
   schema. **Error-side agreement is checked with direction-aware
   severity**: engine-rejects/native-accepts is a hard failure (the
   catastrophic direction, §2); engine-accepts/native-refuses must be
   an *intentional* subset exclusion, asserted against an allowlist of
   refusal diagnostics so scope-creep in rejection stays visible.
2. **Corpus replay** (offline, plain `go test ./...`): committed
   caches under `examples/mysql/` and a dedicated
   `testdata/corpus/` (schema + entries captured from real-engine
   runs) replayed against the native backend — schema in, `Describe`
   out, byte-compare against the stored entry. This is the mode that
   runs on every contributor's machine with no Docker, and the mode
   that makes a captured regression permanent: any live-mode
   disagreement gets its triple checked into the corpus, doc 04
   §6-style.
3. **Differential fuzz** (devdb job): fuzz the *query* side against a
   fixed adversarial schema — generate SQL from a small grammar over
   the v1 subset, require verdict agreement (accept+equal or
   both-reject). The existing fuzz targets cover scan/compose; this
   one covers the new inference surface where hand-written cases have
   blind spots (identifier case-insensitivity, alias shadowing, `*`
   column-order edge cases).

Ship gate for v1: modes 1–2 green over the full existing MySQL
conformance corpus, plus the E2E proof that matters most — a cold
`generate` under the native backend produces a module byte-identical
to the server-backed output, and that module's generated-run E2E
(NULL-heavy seeds, multibyte) passes against real MySQL.

## 8. Diagnostics

The `2xx` band is free at 203–209 and from 214; config errors reuse
`SQLETCH301`. Every code lands in `docs/manual/08-diagnostics.md`
(`diagnostics.manual_test.go` enforces). Spans: refusals in a query
map through the source map like any oracle error (`OracleError.Pos`
is already the mechanism); catalog-builder refusals point at the
schema file offset (schema files are config-referenced inputs, and
`Span.File` already carries arbitrary paths, per doc 14 D4).

| Code | Meaning |
| --- | --- |
| `SQLETCH214` | The native backend refuses a query construct outside its modeled subset (subquery output, un-hinted expression column, unaliased expression column). Message names the construct; hint shows the annotation/alias rewrite and the `database.oracle: server` escape. |
| `SQLETCH215` | The native catalog builder refuses a DDL statement outside its subset (ALTER, VIEW, generated column). Span into the schema file. |
| `SQLETCH216` | A `-- @column` hint disagrees with the engine's column metadata (server present, D7). Engine wins; never a silent override — the `SQLETCH213` rule applied to columns. |

## 9. Testing plan

Per `CLAUDE.md`: test-first, every layer, rejected inputs asserted to
their code.

- **Catalog builder unit**: DDL → `cache.Catalog` goldens (canonical
  JSON); PRIMARY KEY/AUTO_INCREMENT/`NotNull`/`HasDefault` semantics;
  each refused DDL form → `SQLETCH215` with span; table-ordering rule.
- **Describe engine unit**: per-construct goldens over a fixture
  catalog (direct refs, qualified/unqualified, star and `t.*` order,
  aliases, hinted expressions); every refusal → `SQLETCH214`;
  byte-determinism property.
- **Differential** (§7 modes 1–3) — the layer that carries the
  soundness weight; direction-aware error assertions included.
- **Cache**: cold native `generate` → warm fully-offline `check`;
  native-then-server and server-then-native runs over the same inputs
  produce zero cache churn (byte-identity in practice, D8).
- **E2E** (devdb): natively-generated module executes against real
  MySQL over the adversarial seed data — types must actually bind and
  scan, which is the end-to-end meaning of P1.
- **Conformance**: `TestComposeConformance` untouched — the backend
  sits entirely behind the oracle seam and composition never changes.

## 10. Non-goals

- **PostgreSQL and SQLite native backends.** Both have a real-engine
  in-process answer (shipped for SQLite, designed in doc 09 for PG).
  Native inference exists for the dialect with no such option; it is
  not a direction the other dialects should drift toward.
- **Planner fidelity** (§5.3) — never claimed.
- **Nullability inference** — sqletch already owns nullability on
  every dialect; the native backend feeds the same catalog
  `NotNull` + `SrcRel`/`SrcAtt` inputs and changes nothing.
- **LSP integration beyond the cache.** A native backend could serve
  cache misses live in the editor (it is offline by nature, so doc
  10's "never opens a DB" contract is arguably preserved), but it
  changes the LSP's cost profile and determinism story; recorded as a
  possible follow-up, out of scope here.

## 11. Phasing

1. **Corpus capture harness first** (test-first at the feature scale):
   the `testdata/corpus/` format and the replay runner (§7.2), seeded
   from existing example caches and a captured conformance run —
   merged before any inference code, so every subsequent PR is judged
   by it.
2. Config surface (D1) + catalog builder (§5.1) with its unit and
   differential-snapshot tests.
3. Describe engine (§5.2) in the v1 subset + `SQLETCH214`/`215` +
   corpus replay green; dual-backend and fuzz modes wired into the
   devdb CI job.
4. D7 assert (`SQLETCH216`), E2E cold-run/byte-identical-module gate,
   manual chapter (backend selection, what `--exhaustive` proves,
   the annotation discipline delta), `explain` reporting the active
   backend.
5. Deferred: expression-inference widenings (D3b, corpus-gated,
   one construct class at a time); `oracle_fallback: server` (D1b);
   LSP live-miss serving.
