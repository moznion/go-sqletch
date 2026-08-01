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

filter_tree_caps:              # @filter-tree limits, baked into generated code
  max_nodes: 32                # default
  max_depth: 8                 # default
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
- **`schema.files`** are plain SQL, applied in glob order. The
  concatenation (plus dialect and server_version) fingerprints the
  cache: any change re-verifies affected queries.
- **`cache.path`** is the only part of `.sqletch/` you commit — it is
  what makes `check`, warm `generate`, and the LSP work with no
  database. The sibling directories are derived output that an offline
  `generate` rewrites from that cache (`.sqletch/explain/`, consumed by
  `sqletch explain`; `.sqletch/expanded/`, the static-expansion audit
  surface), so they belong in `.gitignore`.
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
- **`filter_tree_caps`** bound caller-built trees; exceeding them
  returns `runtime.ErrTreeTooLarge` before any SQL is composed.
