# sqletch Design — 04: Type Oracle, Dev DB, Catalog, Cache (Phase P4)

Deliverable: `internal/dialect` (Oracle interface),
`internal/dialect/postgres` (oracle half), `internal/devdb`,
`internal/cache`. After this phase the pipeline produces typed
queries, works offline on cache hits, and the shape property test
exists.

Prerequisites: P1–P3 (renderings; catalog-dependent rules run after
this phase per the interleave in 00).

## 1. Interfaces

```go
// internal/dialect
type Oracle interface {
    // Describe prepares (never executes) the SQL and reports types.
    Describe(ctx context.Context, sql string) (Desc, error)
    // Snapshot dumps the catalog for offline analysis.
    Snapshot(ctx context.Context) (*cache.Catalog, error)
    // Plan runs EXPLAIN (used by check --exhaustive / property test).
    Plan(ctx context.Context, sql string) error
    ServerVersion(ctx context.Context) (string, error)
}
```

Postgres implementation: one `pgx.Conn`;
`Describe` = `conn.Prepare(ctx, name, sql)` →
`pgconn.StatementDescription{ParamOIDs []uint32, Fields
[]pgconn.FieldDescription}`. `FieldDescription` carries `Name`,
`TableOID`, `TableAttributeNumber`, `DataTypeOID` — the
`TableOID/AttrNumber` pair is the input P5's nullability analysis
keys on. Prepared statements are closed (DEALLOCATE) after describe.

```go
type Desc struct {
    Params  []TypeRef            // by placeholder position
    Columns []ColumnDesc         // Name, TypeRef, SourceRel (oid,attnum) or zero
}
type TypeRef struct{ OID uint32; Name string } // name resolved via catalog
```

**Oracle backends** (spec: staged de-dependency): `Oracle` is the
backend seam. v0.1 ships the server-backed implementation only;
`devdb` decides what it connects to. The embedded-engine backend
(v0.4) implements the same four methods in-process — nothing above
this interface changes. Backend selection is config, not code.

## 2. `internal/devdb`

```go
func Acquire(ctx context.Context, cfg config.Database)
    (conn *pgx.Conn, cleanup func(), err error)
```

- `cfg.DSN` set → connect, verify pinned `server_version` matches
  (`SQLETCH200` on mismatch — the pin is authoritative, not the
  server), apply schema, return.
- else → testcontainers `postgres:<server_version>` with a tmpfs data
  dir, apply schema, return; `cleanup` terminates the container.
- Schema application: `cfg.Schema` (ordered globs of plain `.sql`) —
  executed file-by-file in a transaction each; or
  `cfg.SchemaSetupCmd` — run via `os/exec` with `SQLETCH_DSN` in env,
  non-zero exit is fatal. Exactly one of the two must be set
  (`SQLETCH301` config validation).

`Acquire` is called lazily: only when a cache miss occurs (see §4
flow). `check` on a warm cache never calls it.

### 2.1 Destructive-reset guard (`SQLETCH204`)

Applying the schema first **resets** it — `DROP SCHEMA public CASCADE`
on PostgreSQL, drop every table on MySQL, drop every table/view on
SQLite — so runs are idempotent. That is safe only for a *disposable*
database, but `database.dsn` comes from `sqletch.yaml`, which is
repo-controlled: a cloned project could aim it at a database the
developer cares about, and a cold `generate`/`check`/`explain --analyze`
would wipe it before printing anything.

So the reset is gated:

- **A database sqletch provisioned itself** (empty `dsn` → a fresh
  container, temp file, or `:memory:`) is disposable by construction and
  always resets. This is the common path and is unchanged.
- **A user-supplied `dsn`** does NOT reset by default: `devdb`'s
  `Acquire*` returns `*DestructiveResetError` (mapped to `SQLETCH204`
  against `config.Path` by `cli.destructiveResetDiag`, in both
  `pipeline.Run` and `explain --analyze`). Nothing is dropped.
- The escape hatch is **`--allow-destructive`** — a flag, deliberately
  not a config key (same reasoning as `--allow-server-drift` in §3.1: a
  repo-controlled key could disarm the guard invisibly). It threads
  through `RunOptions`/`ExplainOptions` → `devdb.Config.AllowDestructive`
  and confirms the operator accepts the reset.

The guard sits immediately before the reset, after connecting and the
version check (both of which write nothing), so a refusal leaves the
target untouched. `SQLETCH204` reaches `--json`/editors like any coded
diagnostic. The error carries only the engine name, never the DSN
(which may embed credentials).

## 3. `internal/cache` — committed cache

Layout (path from config, default `.sqletch/cache/`, committed to VCS):

```
.sqletch/cache/
  catalog-<fp>.json        one per schema fingerprint
  oracle/<qh>.json         one per rendering
  env-<fp>.json            §3.1: generation-environment record (not a key)
```

**Schema fingerprint** `fp = sha256(dialect ‖ server_version ‖
concat(sorted schema inputs: path + content))` — offline-computable
(spec requirement). With `schema_setup_cmd`, inputs are the files
matched by `cfg.SchemaFingerprintGlobs` (config-required in that mode:
sqletch cannot guess what goose/atlas reads).

**Oracle entry** `qh = sha256(fp ‖ rendering.SQL)`:

```json
{
  "schema_fp": "…", "rendered_sql": "…",       // full inputs stored
  "params": [{"oid":25,"name":"text"}, …],
  "columns": [{"name":"id","oid":20,"type":"int8",
               "src_rel":"public.users","src_att":1}, …]
}
```

Store-and-compare (spec: "hashes are an index, not identity"): on
read, `rendered_sql` and `schema_fp` are compared byte-wise against
the current values; mismatch = treated as miss, entry rewritten.
Canonical JSON (sorted keys, LF, trailing newline) for clean diffs;
`generate` prunes entries whose `qh` no longer corresponds to any
rendering (keeps the committed dir from accreting garbage).

**Untrusted-tree hardening.** The cache tree is committed, so a cloned
repository can plant files at these *fingerprint-derived, hence
attacker-computable* paths. Two defences (`internal/cache`):

- **Bounded reads** (`ReadFileCapped`, `MaxFileBytes` = 64 MiB): every
  cache read (`LoadCatalog`/`LoadOracle`/`LoadEnv`, and the `explain`
  reader) reads at most `MaxFileBytes+1` and rejects beyond it, so a
  giant file planted at a hit path is a miss, never an OOM — the bound
  holds even if the file grows after stat (no size TOCTOU).
- **Symlink-safe atomic writes** (`WriteFileAtomic`): writes go through
  `os.CreateTemp` (O_CREATE|O_EXCL + random suffix) then `Rename`, so a
  pre-planted symlink at the *predictable* old `<path>.tmp` name — or at
  the destination itself — is never followed; the rename replaces a
  destination symlink rather than writing through it. All committed-tree
  writers (cache entries, generated `.go`, `expanded/`, `explain/`) use
  it.

### 3.1 Generation-environment record (`env-<fp>.json`)

The fingerprint pins the *pinned* `server_version` (a major, e.g.
`"16"`), so two servers that satisfy the same pin — 16.4 and 16.9 —
produce entries the cache cannot tell apart. The sidecar records what
a run actually connected to, so a later run can:

```json
{
  "format": 1, "schema_fp": "…",
  "dialect": "postgres", "oracle_backend": "server",
  "server_version": "16.4",
  "server_version_raw": "16.4 (Debian 16.4-1.pgdg120+1)"
}
```

**It is not a cache key, and must never become one.** The fingerprint
has to stay offline-computable (spec requirement) and the version of a
server we have not contacted cannot enter it; putting it in would also
mean every patch bump invalidates the committed cache, destroying the
offline-CI property the cache exists for. It lives outside
`catalog-<fp>.json` and `oracle/<qh>.json` for a second reason: those
files are pinned byte-identical across oracle backends by
`internal/corpus` (design 15 §7.2), and a backend that contacts no
server cannot reproduce a connection-derived byte.

Rules:

- **Compared value = the leading dotted-numeric run** of what the
  server reported (`cache.NumericVersionPrefix`). PostgreSQL spells
  16.4 as `16.4` on Alpine and `16.4 (Debian …)` on Debian, MySQL
  appends `-log`; comparing raw strings would report a base-image
  change as drift. The raw string is recorded so the diagnostic can
  name the builds.
- **Checked only where a server is contacted** — inside
  `cli`'s `acquireOracle`, before the first miss is filled, so a
  refusal leaves the committed tree untouched. A warm offline `check`
  never gets there and never looks; `check --exhaustive` always
  connects and is therefore the drift-detection lane for CI.
  `explain --analyze` writes no entries and passes no sink.
- **Disagreement = SQLETCH203**, error by default,
  `--allow-server-drift` (a flag, never a config key) downgrades it to
  a warning and adopts the connected server.
- **Silence is the default for anything less than a confirmed
  disagreement**: no record (a cache committed before this file
  existed, or a first generate) means adopt. `oracle_backend` is
  recorded for forensics but never compared — server and native are
  required to produce identical bytes, so a backend difference is
  either a no-op or a sqletch bug for the corpus gates to catch.
- Accepting drift produces a cache no single environment produced
  (old entries from the old server, new ones from the new). Only the
  flag can create that state; per-entry provenance would be needed to
  distinguish it, and is deliberately not implemented.

**Catalog model** (consumed by rules/R3, R2 star expansion, P5):

```go
type Catalog struct {
    SchemaFP string
    Tables   []Table // schema, name, oid
}
type Table struct{ Schema, Name string; OID uint32; Cols []Column }
type Column struct{ Name string; Att int16; TypeOID uint32;
                    NotNull bool; HasDefault bool }
```

Snapshot query: single SELECT over `pg_class` ⋈ `pg_attribute` ⋈
`pg_namespace` (user schemas only, `attnum > 0`, not dropped), plus a
`pg_type` name map. Serialized to `catalog-<fp>.json`.

## 4. Pipeline flow (cache-aware)

```
fp := Fingerprint(cfg)
cat, ok := cache.LoadCatalog(fp)
descs := map[rendering]Desc{}
misses := renderings whose oracle entry misses/mismatches
if !ok || len(misses) > 0 {
    conn := devdb.Acquire(...)          // the only DB touchpoint
    if !ok { cat = oracle.Snapshot(); cache.SaveCatalog(cat) }
    for r := range misses { descs[r] = oracle.Describe(r.SQL); save }
}
load remaining descs from cache
```

Describe errors are mapped through the rendering's source map; the
postgres driver pattern-matches "could not determine data type of
parameter $n" and attaches the `:param::type` hint (`SQLETCH201`).

## 5. Cross-rendering agreement (the R2/R9 type half)

After all `Desc`s are in hand (from cache or live):

- **Result columns**: every rendering must agree on column names,
  order, and type OIDs with the maximal rendering → `SQLETCH210`
  (spec: `@choose` case type agreement; nullability union is P5).
- **Params**: for each template param, collect the OID at every
  (rendering, position) it occupies via `ParamsSeq`; all must agree →
  `SQLETCH211` (names both renderings; e.g. a param typed text in one
  ORDER BY case and int8 in another).
- The agreed OID per param is the **pinned type** (premise P1) that
  codegen bakes into the bind path (06).

## 6. Property test (soundness harness)

`internal/oracle_test` + `testdata/` corpus: for every template,
`shape.Enumerate` (cap: 512 per template in CI) → compose via the P2
renderer → `Describe` **and** `Plan` each shape against the dev DB
(spec: prepare + EXPLAIN; EXPLAIN catches planner-stage failures).
Runs under a build tag (`-tags devdb`) in CI with a real container.
Any failure is, by definition, a compiler bug: the failing (template,
shape) pair gets checked into `testdata/regression/`.

`sqletch check --exhaustive` is the same harness exposed to users over
their own queries (07).

## 7. Testing & acceptance criteria

- Unit: fingerprint stability (path ordering, content change, version
  change each flip it); store-and-compare miss on doctored hash
  collision; canonical-JSON byte stability.
- Integration (`-tags devdb`): describe Use Case 1's four renderings →
  expected OIDs (incl. `LIMIT` param = int8, per spec example);
  undetermined-param template yields `SQLETCH201` with hint; version
  pin mismatch yields `SQLETCH200`.
- Cache round-trip: cold `generate` (container) → delete container →
  warm `check` fully offline succeeds; touch schema file → `check`
  correctly demands a DB.
- Acceptance: property test green over the v0.1 corpus on PostgreSQL
  {pinned version}; CI wall time for warm path < 5s (no container).
