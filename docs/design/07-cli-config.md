# sqletch Design — 07: CLI, Configuration, Diagnostics Output (Phase P7)

Deliverable: `cmd/sqletch`, `internal/cli`, `internal/config`,
diagnostics rendering — the v0.1 user surface. After this phase v0.1
is feature-complete and releasable.

Prerequisites: P1–P6.

## 1. Configuration (`sqletch.yaml`)

```yaml
version: 1
dialect: postgres            # required; v0.1: postgres only
server_version: "16.4"       # required; cache-key + container tag + pin
database:
  # dsn: postgres://…        # optional, LITERAL only (no ${VAR} expansion);
                             # empty = auto-managed disposable container.
                             # SQLite: a FILE PATH, resolved against the
                             # config dir like every other path (":memory:"
                             # and "file:…" URIs pass through)
  container: true            # default when dsn empty
schema:
  files:                     # exactly one of files / setup_cmd
    - db/migrations/*.sql    # ordered by sorted path
  # setup_cmd: "goose -dir db/migrations postgres $SQLETCH_DSN up"
  # fingerprint_globs:       # required with setup_cmd
  #   - db/migrations/*.sql
queries:
  - queries/**/*.sql
output:
  package: gen
  path: internal/gen
cache:
  path: .sqletch/cache       # committed to VCS
runtime:
  statement_cache: text      # text | prepared
  statement_cache_size: 256
overrides:                   # per-query escape hatches
  - query: SearchUsers
    column: status
    nullable: true
```

`internal/config.Load` performs strict decoding (unknown keys and
duplicate keys are `SQLETCH300`; goccy/go-yaml with
`DisallowUnknownField` — migrated off the archived gopkg.in/yaml.v3,
2026-08) and validation (`SQLETCH301`: mutually exclusive/required
combinations named in the message). Config values are **literal**:
there is deliberately no `${VAR}` environment expansion (removed
2026-08 as a secret-exfiltration / SSRF vector — a cloned repo could
otherwise splice the caller's environment, including secrets, into
`database.dsn` and point it at an attacker host). An operator who
wants the DSN from the environment leaves `database.dsn` empty and
relies on the driver's own DSN environment variables, or templates the
config file outside sqletch. The loaded config carries its own canonical hash — a config
change that affects renderings or keys (dialect, server_version,
schema) invalidates cache naturally through the fingerprint.

## 2. Commands

```
sqletch generate [--config sqletch.yaml]
sqletch check    [--exhaustive] [--config …]
sqletch explain  [query-name…] [--config …]
sqletch version
```

**generate** — full pipeline (00 §Pipeline), writes generated package
+ explain data + cache updates. Exit 0 only if zero diagnostics of
severity error. Designed for `//go:generate sqletch generate`.

**check** — pipeline minus codegen writes (codegen still runs
in-memory to catch `SQLETCH3xx`). Offline iff all cache lookups hit;
prints `offline: yes|no (n misses)` in verbose mode so CI logs show
when a container was needed. `--exhaustive`: after normal checks,
enumerate every query's shapes (config `verification.max_shapes`,
default 4096; exceeding it is `SQLETCH304` and leaves that query
unverified, with the key named in the hint) and Describe+EXPLAIN each
against the dev DB — always requires the DB by definition.

The cap is a **config key, not a flag**, because it decides whether a
CI gate passes: the verification budget must be identical on every
machine that runs the check, not a property of who typed the command.
That is the same line `--max-shapes` sits on the other side of — it
only governs how much `explain` shows you right now, and builds
nothing. A query that outgrows the budget fails the check with the key
named in the hint, so raising it is a deliberate, reviewable edit to
`sqletch.yaml`; per-query shapes must stay verifiable, so the cap
exists to stop one query stalling the run, not to bound what may be
verified. (Earlier drafts of this doc called the key `explain_cap`.
That name was never implemented, and it was wrong twice over: it
belongs to `check`, not `explain`, and the quantity it caps is the one
`static_expansion.max_shapes` already names.)

**explain** — renders from `.sqletch/explain/*.json` (06): per query,
its guards (param → bit), choose blocks/cases, shape count, param and
column types with nullability, maximal SQL. No DB, no recompilation.
(`--enumerate` and `--analyze` are v0.2/v0.3 flags; the command
structure reserves them.)

Both enumerating modes are capped — `--enumerate` at 4096 shapes,
`--analyze` at 64 (it plans each shape against the DB) — and
`--max-shapes N` overrides either. Hitting a cap is **SQLETCH304, on
stderr through the diagnostic channel**, never an SQL comment on
stdout: stdout is the shape stream, and `explain > shapes.sql` must
stay clean. SQLETCH304 is the one code for "shape enumeration stopped
at its cap" wherever it happens — `check --exhaustive` reports the same
code against `verification.max_shapes`. Only the cap's owner and the
severity differ, and both are in the message.

The severity splits by what the mode claims. `--enumerate` is
inspection — it offered to print shapes, so a cap is a *warning* and
the exit code stays 0. `--analyze` reads as planner coverage over the
shape space, so a cap is an *error* (exit 1): `shape.Enumerate` walks
guard bitmasks in ascending order, so truncation does not yield a
smaller sample but a biased one — the high guard bits are never planned
at all, and "every shape plans acceptably" was never established. Other
queries are still analyzed before the command exits, so one oversized
query does not hide the rest.

Exit codes: 0 ok, 1 diagnostics reported, 2 environment failure
(config unreadable, DB unreachable, cache write failure). CI can
distinguish "your SQL is wrong" from "infra flaked". A `server_version`
pin that does not match the connected engine is on the *diagnostic*
side of that line — it is a mistake in `sqletch.yaml`, so it surfaces
as SQLETCH200 against the config file (exit 1), not as an environment
failure.

## 3. Diagnostics rendering

Single renderer in `internal/diagnostics`:

```
queries/users.sql:12:7: error[SQLETCH115]: "organization_id" resolves to
column "ou.organization_id" of the optional join guarded by
`organization_id` (queries/users.sql:6), but this predicate is not
guarded by it.
   |
12 |   AND organization_id = :organization_id
   |       ^^^^^^^^^^^^^^^
help: move this predicate inside @if-present(organization_id)
```

- Format: `file:line:col` (computed from byte offset + a lazily built
  line index), stable code, message, source excerpt with caret span,
  `help:` from the Hint field.
- `--json` (a persistent root flag; earlier drafts of this doc called
  it `--format json`) emits one JSON object per line per diagnostic for
  editor/CI integration — this is also the LSP's data source, so it
  ships in v0.1. The key set is exactly `code`, `severity`, `file`,
  `line`, `col`, `message`, `hint`; `col` counts **bytes**, unlike the
  excerpt renderer's rune-aligned caret and the LSP's UTF-16 code
  units. It is a machine contract: `internal/cli/jsondiag_test.go` pins
  the key set field for field, so any change breaks a test rather than
  a downstream editor. `generate` and `check` honour the flag through
  both `PrintDiags` and `printBare`; `explain` does not take it.
- Diagnostics sorted by (file, offset, code) for stable output.

## 4. Command wiring rules

`cmd/sqletch/main.go` contains cobra boilerplate only; each command is
a function in `internal/cli` taking `(config.Config, io.Writer)` and
returning `(diags, error)` — fully testable without process spawning.
Color output via a tiny local helper (no third-party color dep),
disabled when not a TTY or `NO_COLOR`.

## 5. Example project (`examples/`)

A minimal but real app: schema (users/organizations), the three v0.1
use-case queries, `sqletch.yaml`, a `main.go` exercising the generated
API, and a taskfile target `make regen`. Serves as: living
documentation, the e2e CI fixture, and the sqlc-coexistence smoke
(the example includes one sqlc-generated query sharing the tx).

## 6. Testing & acceptance criteria

- Config: golden valid/invalid fixtures → exact `SQLETCH30x`
  diagnostics; literal-value handling (a `${VAR}` in the DSN is NOT
  expanded — `TestLoad_NoEnvExpansion`); unknown-key strictness.
- CLI: table-driven command tests over `examples/` using the in-proc
  entry points; the `--json` key set and value semantics pinned in
  `jsondiag_test.go`, including the exit-code mapping for SQLETCH200
  (a bad `server_version` pin is exit 1, not exit 2).
- Renderer: golden text output incl. the multibyte-column caret case.
- E2E (`-tags devdb`): cold `generate` → commit cache → warm `check`
  offline → mutate a query → `check` fails with the right code →
  `--exhaustive` over examples passes.
- Acceptance: a new user can clone `examples/`, run
  `go generate ./... && go test ./...` with only Docker present, and
  read every failure message without consulting source code. That
  sentence is the v0.1 release bar.
