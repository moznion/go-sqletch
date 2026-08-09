# sqletch.yaml reference

Strict decoding: unknown keys are errors (SQLETCH300). `${VAR}`
expands from the environment anywhere in the file. Relative paths
resolve against the file's directory.

```yaml
version: 1                     # required, must be 1
dialect: postgres              # required: postgres | mysql | sqlite
server_version: "16"           # required: pins the oracle AND keys the cache

database:
  dsn: ${SQLETCH_DEV_DSN}      # optional (see below)

schema:
  files: [db/schema.sql]       # required: ordered globs of plain DDL

queries: [queries/*.sql]       # required: globs of template files;
                               # `.go` paths are read for //sqletch:query consts

output:
  package: gen                 # required: generated package name
  path: gen                    # required: output directory

cache:
  path: .sqletch/cache         # default shown; COMMIT this directory

overrides:                     # per-column nullability overrides
  - query: SearchUsers
    column: nickname
    nullable: false

static_expansion:              # strict static expansion (opt-in per query)
  queries: [SearchUsers]
  max_shapes: 256              # default

verification:                  # budget for `check --exhaustive`
  max_shapes: 4096             # default

filter_tree_caps:              # @filter-tree limits, baked into generated code
  max_nodes: 32                # default
  max_depth: 8                 # default

policies:                      # cross-query policies (see the policies chapter)
  - name: tenant_scope
    tables: [orders]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint             # required on MySQL/SQLite
    applies_to: [select, update, delete]   # default: all three
```

## Field notes

- **`server_version`** does three jobs: selects the auto-managed dev
  database image, is validated against whatever DSN you point at
  (mismatch = SQLETCH200), and is part of the cache fingerprint — the
  oracle's answers are pinned to a version. PostgreSQL/MySQL compare
  the major version; SQLite compares a dotted prefix (`"3.50"`
  matches `3.50.x`).
- **`database.dsn`** is per-dialect: a PostgreSQL URL, a go-sql-driver
  MySQL DSN, or a SQLite file path. Empty means auto-managed:
  a disposable container (PostgreSQL/MySQL) or a temp file (SQLite).
  Whatever it points at is treated as **disposable** — sqletch resets
  it (drops the public schema / all tables) before applying
  `schema.files`. Never point it at data you care about.
- **`database.oracle`** selects the type-oracle backend: `server`
  (default) or `native` — sqletch's own corpus-validated inference,
  MySQL only, no server anywhere (setting a `dsn` alongside it is a
  config error). See [Dialects](04-dialects.md#the-native-oracle-backend-databaseoracle-native)
  for the discipline it demands and what `check --exhaustive` proves
  under it.
- **`schema.files`** are plain SQL, applied in glob order. The
  concatenation (plus dialect and server_version) fingerprints the
  cache: any change re-verifies affected queries.
- **`cache.path`** is the only part of `.sqletch/` you commit — it is
  what makes `check`, warm `generate`, and the LSP work with no
  database. The sibling directories are derived output that an offline
  `generate` rewrites from that cache (`.sqletch/explain/`, consumed by
  `sqletch explain`; `.sqletch/expanded/`, the static-expansion audit
  surface), so they belong in `.gitignore`. Alongside the catalog and
  the oracle entries, the cache directory holds one `env-<fp>.json`
  per fingerprint recording the server a run actually connected to —
  see [environment drift](#server-environment-drift) below.
- **`queries`** globs may list `.sql` template files, `.go` files
  holding `//sqletch:query` consts, or both; the input form follows the
  extension (see [the template language](02-template-language.md)).
  Query names are global across every file and both forms.
- **`overrides`** force a result column's nullability where the
  analysis is conservative (the analyzer never narrows from optional
  fragments by design — see the manual's runtime chapter).
- **`static_expansion`**: listed queries are materialized
  shape-by-shape into `.sqletch/expanded/<query>/*.sql` (an audit
  surface) and dispatch via a precomposed table instead of composing
  at runtime. Refused for unbounded shape spaces (`@filter-tree`
  anywhere; `@in` on MySQL/SQLite) with SQLETCH302.
- **`verification.max_shapes`** is how many shapes of one query
  `check --exhaustive` will prepare and plan. A query that reaches more
  fails the check (SQLETCH010, exit 1) rather than being verified
  partway — raise the key to give it the budget it needs. It lives in
  the config, not on the command line, because it decides whether a CI
  gate passes: every machine running the check must spend the same
  budget.
- **`filter_tree_caps`** bound caller-built trees; exceeding them
  returns `runtime.ErrTreeTooLarge` before any SQL is composed.
- **`policies`** declare predicates woven at compile time into every
  query touching the designated tables, with per-query opt-outs and
  an enforcement check — the whole story is
  [Cross-query policies](12-policies.md). Malformed declarations are
  SQLETCH303. A config using `policies:` is rejected by pre-policy
  sqletch binaries (strict decoding) — the desired failure direction.

## Server environment drift

`server_version` pins a *major*, so 16.4 and 16.9 both satisfy
`"16"` — and the committed cache cannot tell entries typed by one
from entries typed by the other. Every run that contacts a server
therefore records what it connected to in `env-<fp>.json`, beside the
cache it produced, and refuses to extend a cache that came from a
different server:

```console
$ sqletch check --exhaustive
sqletch.yaml:1:1: error[SQLETCH203]: the committed oracle cache was generated against server version 16.4 (16.4 (Debian 16.4-1.pgdg120+1)) but this run connected to 16.9
help: regenerate the whole cache against one server (delete .sqletch/cache and re-run), or pass --allow-server-drift to accept a cache built from both
```

Notes:

- **The comparison is the numeric version only.** `16.4` on Alpine and
  `16.4 (Debian …)` are the same server as far as this check is
  concerned; changing base images is not drift.
- **Only runs that connect can see it.** A warm `check` is offline by
  design and stays that way. `generate` on a cache miss and
  `check --exhaustive` always connect — if you want a CI lane that
  catches drift, that is the one to run against a database.
- **`--allow-server-drift`** (on `generate` and `check`) downgrades it
  to a warning and adopts the connected server. The result is a cache
  no single environment produced; that is why it is a flag you type,
  not a setting you can leave on.
- **Caches committed before this existed have no record**, so the next
  connecting run simply adopts its server. Deleting the file is always
  safe and means the same thing.
