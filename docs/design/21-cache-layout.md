# sqletch Design — 21: Committed-cache layout and pruning

Status: settled 2026-09-16 (owner decisions D1–D4 below), implemented.

Supersedes the layout half of `04-type-oracle.md` §3/§3.1. Nothing
here changes the *verification model*: the cache key, the
store-and-compare discipline, and the offline-computable fingerprint
are exactly as specified. What changes is where an entry lives on
disk, and that a `generate` run now removes what it did not write.

## 1. The problem: a committed artifact nobody can review

The cache is committed on purpose (spec, "Phase 4"): it is what makes
`check`/`generate` offline in CI. But it is also a file tree in every
pull request, and the v0.5 layout made that tree unreadable:

```
.sqletch/cache/
  catalog-<fp24>.json
  env-<fp24>.json
  oracle/<sha256(fp ‖ renderedSQL)[:24]>.json      one per rendering
```

Four independent consequences, all of them review damage:

1. **A file name is a function of the file's content.** Change a
   template and the entry does not change — it is replaced by a
   differently named file. Git sees delete+add, never modify, and
   rename detection cannot help (the content changed too). The diff
   carries no hint of which query it belongs to.
2. **The schema fingerprint is part of every name.** One DDL line
   re-keys *every* entry in the repository at once: 100% churn for a
   change that may alter no query's types at all.
3. **Nothing is ever removed** (§3 promised pruning; it was never
   implemented). A PR that edits a query adds entries and leaves the
   superseded ones behind forever, so the tree is a union of every
   schema and every template revision the project has ever had, with
   no way to tell live from dead.
4. Consequently the reviewer's only honest options are "approve
   blind" or "read 80 hash-named JSON files".

The count of entries was never the problem. The verified rendering set
is small and structural (maximal, one per extra `@choose` ordinal, one
per `@order-by @default`, one per `@in` on expanding dialects, one per
`@filter-tree`) — `examples/postgres` has 18 entries for 10 queries.
The problem is that every one of them is anonymous and re-keyed on
every edit.

## 2. Decisions

**D1 — the schema fingerprint leaves the path.** It stays *in* every
file as the compared key; it no longer appears in any file name. One
committed cache therefore describes exactly one schema state.

The trade-off, accepted: caches for two different schema states can no
longer coexist in one working tree, so switching between branches
whose DDL differs needs a database again (it did not before, if both
branches' entries happened to be committed). This is the same
direction as pruning: keeping several generations side by side is
precisely what made the tree a landfill.

**D2 — one file per rendering**, as before. The alternative (one file
per query holding all its shapes) halves the file count but turns "a
new shape appeared" — the thing a reviewer most wants to see — into a
modification buried inside an existing file, and makes two developers
touching different shapes of one query conflict.

**D3 — pruning runs on `generate` only**, mirroring the `*.gen.go`
sweep in `cli.removeStaleGenerated`. `check` keeps its current
behavior (it fills misses, it deletes nothing): a CI `check` must not
start mutating the tree it is checking.

**D4 — migration is a format bump, not a migration tool.**
`cache.FormatVersion` goes to 2, so every v1 file is a miss (the
format field is checked before anything else, as always). The first
`generate` after upgrading needs a database, writes the new tree, and
prunes the old one in the same run. v0.x, breaking, documented — the
same treatment doc 19 gave the `targets:` change.

## 3. Layout

```
.sqletch/cache/
  catalog.json                                  the schema snapshot
  env.json                                      §3.1 sidecar
  oracle/<target-slug>/<query>/<shape>.json     one per rendering
```

`<target-slug>` is `config.ResolvedTarget.Slug()` — the same spelling
the derived-output trees already use (`.sqletch/explain/<slug>/`,
`.sqletch/expanded/<slug>/<query>/`, doc 19), which already folds away
the escapes a path component cannot carry. `<query>` is the query name
from the `-- name:` header, which the grammar restricts to
`[A-Za-z][A-Za-z0-9_]*`. Target slug plus query name is exactly the
uniqueness scope the compiler enforces (SQLETCH004: a name is unique
per *target*).

`<shape>` names the rendering within its query:

| rendering            | name                                     |
| -------------------- | ---------------------------------------- |
| maximal              | `maximal`                                |
| `@choose` ordinal    | `case-<param>-<case-name>` (`…-default`) |
| `@order-by @default` | `order-default-<param>`                  |
| `@in` at arity 0     | `in-empty-<param>`                       |
| `@filter-tree` empty | `tree-empty-<param>`                     |

Construct parameters and case names are snake_case identifiers
(SQLETCH002), so they are path-safe by construction. Two constructs
may legitimately share a parameter name, so `ast.Renderings` appends
`-<block-index>` to *every* member of a colliding group — deterministic,
and absent from the common case. Uniqueness within a query is asserted
by a test over the enumeration; the path builder additionally folds any
non-`[A-Za-z0-9_-]` byte to `_` as defence in depth, since these
segments end up in a filesystem path.

Effect on the diff:

- Editing a query modifies the files of the shapes it affects, in
  place, under a path that names the query.
- A new shape is one added file whose name says which construct
  created it; a deleted shape is one removed file.
- A DDL change modifies `catalog.json` (where the reviewer can read
  the actual column change) plus, in every entry, the one
  `"schema_fp"` line — and the type lines of whichever queries were
  genuinely affected. That last set is the review signal, and it is
  now visible instead of being buried under a full-tree rewrite.

### 3.1 What is a key and what is a name

Unchanged: an entry is keyed by `(schema fingerprint, rendered SQL)`,
both stored in the file, both compared byte-wise on load; any mismatch
is a miss. The path is now a *name*, not a hash — it is how the loader
finds a candidate, and it is never evidence of what the candidate is.
An entry left at `oracle/gen/get_user/maximal.json` by an older
template is read, fails the rendered-SQL comparison, and is refilled,
exactly as a stale hash-named entry was.

The spec sentence "hashes are an index, never identity" is satisfied a
fortiori: the load path no longer involves a hash at all. The logical
key in spec §"Phase 3" ("keyed by (dialect, server version, schema
fingerprint, rendered query hash)") is unaffected — that key was never
the file name.

The untrusted-tree hardening of §3 (`ReadFileCapped`,
`WriteFileAtomic`) is unchanged and still load-bearing: a cloned
repository can plant a file at any of these paths, and they are now
*more* predictable than before, not less.

## 4. Pruning (`generate`)

After a successful `generate`, the run knows the complete live set:
`catalog.json`, `env.json`, and one oracle path per (target, query,
rendering) — including the hits, not just the misses it filled.
Everything else under the cache directory is garbage from an older
schema, an older template, a deleted query, or the pre-2 layout.

Rules, mirroring `removeStaleGenerated`:

- Only on `ModeGenerate`, and only when the run produced no error
  diagnostics. `check`, `check --exhaustive`, `explain --analyze` and
  the LSP never delete anything.
- **Only files sqletch itself writes** — the same discipline
  `removeStaleGenerated` applies to `*.gen.go`. That is `.json` files
  under `oracle/`, plus `catalog.json`, `env.json` and their v1
  `catalog-<fp>.json` / `env-<fp>.json` predecessors at the cache root.
  A `.gitignore`, a README, an unrelated `package.json`, another tool's
  subdirectory: all survive. `cache.path` is config, and a config that
  points the cache at a directory somebody else also owns must not turn
  `generate` into a shredder.
- Only regular files, so a planted symlink is left alone rather than
  followed.
- Directories that become empty under `oracle/` are removed, so a
  renamed target or query leaves no husk.
- A cache directory outside the project is not swept at all; it
  reports `SQLETCH306` as a warning, the same refusal
  `removeStaleGenerated` gives an out-of-project output directory.

`env.json` is unconditionally live: it may have been written by an
earlier run and a warm offline `generate` never touches it (§3.1 — the
sidecar is only read and written where a server is contacted).

## 5. Corpus impact

`internal/corpus` cases are committed cache trees, so they adopt the
same layout and the same `FormatVersion`. Cases that are copies of a
real project's cache (`examples-mysql`) keep that project's real
paths. Captured cases have no templates and therefore no query or
shape of their own: they use the synthetic target `_corpus`, one
"query" per captured statement (`agree-<NNN>` by position in the
agree-set, or a descriptive name for hand-built cases), and the shape
`maximal`. `Case.Entries` carries the `cache.OracleRef` the entry was
found at, so `SQLETCH_UPDATE_CORPUS=1` rewrites contents in place.

Corpus authority is unchanged: ground truth is re-derivation against
the real engine, never an entry's path or provenance.

## 6. Non-goals

- **Hashing `rendered_sql` instead of storing it.** It would shrink
  every entry, and it would replace a byte comparison with a hash
  comparison on the one check that keeps a stale entry from being
  misread. Explicitly refused: the spec's "hashes are an index, never
  identity" is a rule, not a performance note.
- **A `sqletch cache migrate` command** (D4).
- **Grouping several renderings per file** (D2).
- **Making `check` prune** (D3).
