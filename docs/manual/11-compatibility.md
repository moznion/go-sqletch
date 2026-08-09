# Compatibility and versioning

sqletch follows semantic versioning on a single Go module,
`github.com/moznion/go-sqletch`. This chapter states what stays stable
for the whole of v1 and what may still change under you, so that
upgrading is a decision you can make from the version number alone.

`sqletch version` prints the release you have.

## The promises

### Template language

Constructs and directives keep their meaning. A template that compiles
under one v1 release compiles under every later v1 release and produces
the same SQL for the same inputs.

New constructs and new slots may be added. Additions never change what
existing templates do — a template only sees new behavior if you write
the new syntax.

### Generated code

The generated API shape is fixed: `New(DBTX)`, `Queries`, `WithTx`,
`Querier`, `OnQuery`, one params struct / row struct / method per
query, and the per-query enums and predicate constructors described in
[generated code and the runtime](07-runtime-and-generated-code.md).

Regenerating with a newer v1 sqletch may **add** surface, but does not
break code that compiles against the old output — except where your own
template change forces it (renaming a query, adding a result column,
making a parameter optional). Those are your edits, visible in your
diff.

`DBTX` keeps matching sqlc's interface for the same driver flavor, so
sqlc- and sqletch-generated code keep sharing connections and
transactions.

### The `runtime` package

Go API compatibility for all of v1. The package has two faces, and the
distinction matters for upgrades:

- **The user-facing API** — `Tree`, `And`/`Or`, `TreeCaps`, the
  sentinel errors — is stable in the ordinary sense. Write against it
  freely.
- **The generated-code contract** — fragment tables, shape keys,
  composers — is public only because your generated package lives
  outside sqletch's module. It is not an API for you to call, and
  conformance is guaranteed **per version pair**.

Therefore: **after upgrading sqletch, re-run `sqletch generate`.**
Mixing generated code from one release with a `runtime` from another is
not a supported configuration, even though it may compile.

### Diagnostics

**Codes are stable identifiers; message wording is not.** `SQLETCH115`
means the same rule violation forever, so it is safe to grep for,
match on in `--json` output, or suppress in tooling. The English
sentence next to it may be rewritten at any time to explain the problem
better.

Every code that exists is documented in the
[diagnostics reference](08-diagnostics.md), and a test in the
repository fails if a code is added without documenting it or
documented after being removed — the table cannot drift from the code.

### `sqletch.yaml`

Existing keys keep their meaning. New optional keys may be added.
Unknown keys remain **errors**, not warnings: strict decoding is what
turns a typo into a build failure instead of a silently ignored
setting.

### The CLI

The commands, their flags, and the exit codes in the
[CLI reference](06-cli.md) are stable — in particular the 0 / 1 / 2
split (success / your files are wrong / your environment is wrong),
which CI configurations depend on. The `--json` diagnostic object keeps
its keys.

Human-readable stdout formatting is *not* a stable interface; parse
`--json`, not the summary lines.

### The committed cache

The cache is **an offline optimization and never a source of truth.**
Two consequences:

- **Deleting `.sqletch/cache/` is always safe.** The next cold run
  rebuilds it against the dev database.
- Cache files are self-describing (they carry a `"format"` field). A
  file whose format this release does not recognize is treated as a
  **miss**, so it is re-described rather than misread. Format evolution
  can therefore never produce a wrong answer — at worst it costs one
  cold run.

Entries store their full keys and compare them on read; hashes are an
index, never an identity.

The cache also records the server each fingerprint's entries were
generated against (`env-<fp>.json`). That record is **not** a key: it
never affects hits or misses, and deleting it only costs the next
connecting run its ability to notice that the entries came from
somewhere else (SQLETCH203 — see
[the config chapter](05-config.md#server-environment-drift)). Caches
committed before the record existed keep working unchanged.

## What is not promised

- Message text of diagnostics, and the layout of human-readable CLI
  output.
- The internal packages (`internal/...`) — not importable, by
  construction.
- The `runtime` machinery used by generated code, across versions;
  regenerate instead.
- Byte-identical *cache file contents* across releases. Byte-identical
  *generated code and composed SQL* for identical inputs within one
  release is guaranteed — determinism is a design invariant.
- SQL semantics of your queries. sqletch verifies against the schema
  you compile with; production schema drift is a risk it does not
  currently detect (see the spec's *Beyond v1.0*).

## Upgrading checklist

1. Bump the sqletch version.
2. Run `sqletch generate` — always, even if no template changed.
3. Run `sqletch check`; a clean run means every reachable shape still
   verifies, offline, against the committed cache.
4. Compile and test your application as usual.

If step 2 or 3 surfaces a new diagnostic on an unchanged template, that
is a bug in the release — the promises above say it should not happen.

### One-time step: `server_version` now means what it says

The pin used to be compared by **major only** on PostgreSQL and
MySQL — everything you wrote after the first dot was discarded, so
`server_version: "8.4"` quietly accepted a MySQL 8.0 server. It is now
a dotted prefix on every dialect (what SQLite always did).

Nothing changes for a major-only pin (`"16"`, `"8"`), which is the
common case. If you pinned a minor, a run that connects may now report
SQLETCH200 where it used to pass — that is the pin doing the job it
looked like it was doing. Either point the DSN at a server the pin
actually describes, or shorten the pin (the diagnostic's hint spells
both options). Note that changing the pin re-keys the cache: it is a
fingerprint input, so the next run is cold.

### One-time step: the `.gen.go` rename

Generated files used to be `<query>.sql.go`, `db.go`, `querier.go`;
they are now `<query>.sql.gen.go`, `db.gen.go`, `querier.gen.go`.
`generate` writes the new files but never deletes files it did not
write, so the first regeneration after upgrading leaves the old ones in
place and the package fails to compile with duplicate declarations.
Delete them once:

```console
rm gen/*.sql.go gen/db.go gen/querier.go   # only if they predate the rename
```

The API is unchanged — same package, same types, same methods — so no
call site moves.

---

The engineering record behind these promises — which surfaces were
audited before the v1 freeze, and why each was frozen as-is — is in
[`docs/design/12-v1.md`](../design/12-v1.md).
