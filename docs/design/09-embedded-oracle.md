# 09 — Embedded PostgreSQL oracle: WASM spike findings

Status: **spike complete (2026-08)** — this document records the
outcome mandated by 08-later-phases.md ("Explicit spike before
committing: WASM PG maturity, extension availability, cold-start time
budget (< 2s target)"). The runnable harness lives in
`spike/wasm-oracle/` and reproduces every number below.

## Verdict

**Feasible, with one structural caveat.** libpglite's WASI build of
PostgreSQL 16.6 boots in-process under wazero (pure Go, no cgo, no
Docker), and sqletch's **unmodified** pgx-based oracle runs against it
end to end — the design principle "the database is the type checker"
survives the move in-process with zero oracle changes:

| Oracle operation | Result |
| --- | --- |
| `Describe` (extended-protocol Parse/Describe) | ✅ param OIDs and result columns **including SrcRel/SrcAtt source identity** |
| `Plan` (`EXPLAIN (GENERIC_PLAN)`, needs PG 16+) | ✅ engine is 16.6 |
| `PlanText` (raw simple query) | ✅ real plan output |
| `Snapshot` (catalog query) | ✅ types, NOT NULL, defaults (after schema bootstrap, see below) |
| `ServerVersion` | ✅ `16.6` |
| DDL incl. identity columns / sequences | ✅ on a clean data dir |
| MD5 auth handshake via pgx | ✅ |

The caveat: **the current WASI build has no in-instance error
recovery** — see "The error-recovery caveat" below.

## Measurements (Apple Silicon macOS, wazero 1.12)

| Phase | Cold | Warm (on-disk compile cache) |
| --- | --- | --- |
| wazero compile of `postgres.wasi` (22 MB) | 2.26 s | 0.73 s |
| instantiate + `_start` + `pg_initdb` (existing data dir) | 0.31–0.47 s | 0.31 s |
| pgx connect (startup + md5 + ready) | 4 ms | 4 ms |
| **Total to first query** | **3.3 s** | **1.69 s** |
| `Describe` / `Plan` / `PlanText` | 1–8 ms each | same |
| Reboot after a trap (fresh instance, same compiled module) | ~0.5 s | same |

**The < 2 s cold-start budget is met in the steady state** (compile
cache is per-machine persistent); the very first run pays one-time
~3.3 s plus a ~14 MB bundle download.

## The working recipe

The bundle: `electric-sql/pglite-bindings` `16.x/pglite-wasi.tar.gz`
(~14 MB) — `postgres.wasi` (pure WASI preview-1, **zero non-WASI
imports**, so wazero needs no Emscripten shims) plus a pre-initdb'd
`PGDATA`. Boot:

- argv `/tmp/pglite/bin/postgres --single postgres`; env
  `ENVIRONMENT=wasi-embed`, `PREFIX=/tmp/pglite`,
  `PGDATA=/tmp/pglite/base`, `PGSYSCONFDIR=/tmp/pglite`,
  `PGUSER=postgres`, `PGDATABASE=template1`, `MODE=REACT`, `REPL=N`,
  `TZ=UTC`, `PGTZ=UTC`, `PATH=/tmp/pglite/bin`.
- Preopen the extracted `tmp/` as `/tmp` and a host dir containing a
  `urandom` file as `/dev`.
- After `_start`: `pg_initdb()`, then `use_socketfile()`, then
  `use_wire(1)`.
- **Transport is the socket-file pump, not the CMA**: write wire bytes
  to `<PGDATA>/.s.PGSQL.5432.lock.in`, atomically rename to `.in`,
  call `interactive_one()`, then read-and-delete `.s.PGSQL.5432.out`
  (loop until absent). The CMA path failed to flush auth replies in
  this build; the file path is also what the community Rust bindings
  (`kshcherban/pglite-rust-bindings`) productionize against real
  PostgreSQL clients.
- Wrap that pump in a `net.Conn` and hand it to pgx via
  `ConnConfig.DialFunc` — everything above pgconn is untouched.
- Credentials: `postgres` / `password` (md5; recovered by reversing
  the bundle's PGPASSFILE hash: `md5("password"+"postgres")`).
- Session bootstrap: the bundled template1 has **no `public` schema**
  (`current_schema()` resolves to `pg_catalog`, where accidental DDL
  lands and the catalog snapshot rightly ignores it). Run
  `CREATE SCHEMA IF NOT EXISTS public; SET search_path = public` after
  every connect, before applying user schema.

## The error-recovery caveat

The WASI build disables PostgreSQL's setjmp/longjmp error recovery
("sjlj exception handler off"): **every PostgreSQL ERROR escalates to
`abort()` → a wasm trap that kills the instance.** The `clear_error`
export is Emscripten-only (confirmed empirically — the instance stays
unusable). This matters because erroring queries are a first-class
oracle input (diagnosing broken templates is the product).

Mitigation, measured: treat a trapped instance as disposed — re-extract
the data directory, re-instantiate from the cached compiled module,
re-apply schema (~0.5 s + schema DDL). This maps exactly onto the
devdb disposability contract the pipeline already has. Two rules make
it sound:

1. **Never reuse a data dir that lived through a trap.** Aborts leave
   torn state behind; we reproduced catalog/index corruption
   (`invalid table access` inside `_bt_compare`) by rebooting on a
   dirty dir. Always re-extract from the pristine bundle.
2. One error costs one reboot; the pipeline should Describe the
   maximal rendering first so a broken template fails fast, and the
   committed cache already keeps repeat runs off the oracle entirely.

## Ecosystem status (2026-08)

- `electric-sql/pglite-bindings` (the bundle source) is explicitly
  WIP / "do not use in production". 16.x is self-contained; the 17.x
  directory ships only a prefix tarball (initdb decoupled in the
  PGlite v0.4 architecture) without a prebuilt `postgres.wasi`.
- PGlite v0.4 (2026-03) announced **libpglite** — a native library
  built from Postgres source with official multi-language bindings —
  as the planned successor. When that ships, it is the obvious
  replacement for this recipe (and likely fixes error recovery, which
  works in the Emscripten/JS builds via `clear_error`).
- Community Rust bindings productionize today's recipe; the
  `pglite-oxide` lineage has grown into a native-first embedded
  PostgreSQL 18 effort ("Oliphaunt"), further evidence the embedded-PG
  space is consolidating.
- Extension availability in the WASI bundle: **plpgsql only**. The
  PGlite JS distribution's extension story (pgvector, PostGIS) has not
  reached the WASI bundle. Templates relying on extension types would
  need the server backend.

## Recommendation

1. **Do not ship `database.backend: embedded` on today's bundle.** The
   WIP upstream status, the error-recovery caveat, and the
   single-source 16.x snapshot make it a support liability as a
   default. The spike harness stays in-repo (`spike/wasm-oracle/`,
   wazero is a pure-Go dependency) as the regression check for
   revisiting.
2. **Revisit when libpglite ships official bindings** (PGlite v0.4
   roadmap). The integration seam is proven: the oracle needs nothing
   dialect-visible — `devdb` selection per 08 (`backend: server |
   embedded`) plus a bundle fetch/cache, an engine pool that reboots
   on trap, and the session bootstrap above.
3. **If "no Docker" pressure arrives before libpglite matures**,
   auto-fetched native PostgreSQL binaries (zonky/embedded-postgres
   style) are the pragmatic interim backend: same oracle, full error
   recovery, ~1 s startup — at the cost of per-platform binaries
   instead of one wasm file.

## Spike-visible engine quirks (for the eventual implementation)

- Replies only materialize during `interactive_one()` ticks; a reader
  must drain after each write rather than block on a background
  producer.
- `interactive_read()` can return stale lengths after a failed flush;
  the file transport sidesteps this entirely.
- The engine is single-connection; `Terminate` handling is a no-op
  (drop the instance instead).
- `/dev/urandom` must exist as a preopened file (WASI `random_get`
  alone is not enough — the engine opens the device path directly).
- 32-bit address space (wasm32): irrelevant at oracle scale.
