# Compatibility and versioning

sqletch follows semantic versioning on a single Go module,
`github.com/moznion/go-sqletch`. The published version is currently
**v0.x**, and semantic versioning promises nothing below v1.0.0. This
chapter therefore states two things: the contract v1.0.0 will freeze,
so that upgrading becomes a decision you can make from the version
number alone, and what may still change under you before then.

`sqletch version` prints the release you have.

## Where v0.x stands

Every surface named below is implemented and intended to be final —
the freeze audit behind them is
[`docs/design/12-v1.md`](../design/12-v1.md). What v0.x withholds is
the *promise*, not the feature:

- Any release may make a breaking change to any of those surfaces: the
  template language, the generated API, the `runtime` package,
  `sqletch.yaml`, the CLI, the meanings of diagnostic codes.
- Breaking changes are called out in the release notes, but the
  version number alone will not warn you — under semver a v0 major
  carries no such signal, so v0.0.1 → v0.0.2 may break you.
- Pin an exact version, and read the release notes when you bump.

Everything under *The promises* below therefore reads as **stable from
v1.0.0 on**. Nothing there is weaker in intent today; it is simply not
yet backed by the version number.

## The promises (from v1.0.0)

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

- Anything at all, before v1.0.0 — see *Where v0.x stands* above. The
  list below is what stays unpromised once v1.0.0 ships.
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
While the version is v0.x it may instead be a deliberate breaking
change; the release notes say which.

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
