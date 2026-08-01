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
  dsn: ${SQLETCH_DSN}        # optional; env expansion supported
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

`internal/config.Load` performs strict decoding (unknown keys are
`SQLETCH300`), env expansion (`${VAR}` only, no shell), and validation
(`SQLETCH301`: mutually exclusive/required combinations named in the
message). The loaded config carries its own canonical hash — a config
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
enumerate every query's shapes (config `explain_cap`, default 4096;
exceeding it fails with guidance) and Describe+EXPLAIN each against
the dev DB — always requires the DB by definition.

**explain** — renders from `.sqletch/explain/*.json` (06): per query,
its guards (param → bit), choose blocks/cases, shape count, param and
column types with nullability, maximal SQL. No DB, no recompilation.
(`--enumerate` and `--analyze` are v0.2/v0.3 flags; the command
structure reserves them.)

Exit codes: 0 ok, 1 diagnostics reported, 2 environment failure
(config unreadable, DB unreachable, cache write failure). CI can
distinguish "your SQL is wrong" from "infra flaked".

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
- `--format json` on all commands emits one JSON object per diagnostic
  (code, file, span, message, hint) for editor/CI integration — this
  is also the future LSP's data source, so it ships in v0.1.
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
  diagnostics; env expansion; unknown-key strictness.
- CLI: table-driven command tests over `examples/` using the in-proc
  entry points; `--format json` schema pinned by golden files.
- Renderer: golden text output incl. the multibyte-column caret case.
- E2E (`-tags devdb`): cold `generate` → commit cache → warm `check`
  offline → mutate a query → `check` fails with the right code →
  `--exhaustive` over examples passes.
- Acceptance: a new user can clone `examples/`, run
  `go generate ./... && go test ./...` with only Docker present, and
  read every failure message without consulting source code. That
  sentence is the v0.1 release bar.
