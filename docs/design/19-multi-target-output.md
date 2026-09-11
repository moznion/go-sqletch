# sqletch Design — 19: Multiple output targets (`targets:`)

Status: implemented (2026-09). Breaking change to `sqletch.yaml`
(v0.x; `docs/manual/11-compatibility.md` withholds the promise until
v1.0.0). Supersedes the top-level `queries:` / `output:` pair of
design 07 §1; design 06 §1's "per config output target (one Go
package)" now reads "per **resolved target**".

## 1. What changes

One `sqletch.yaml` could previously emit exactly one Go package. A
project that wants generated code to live next to the package that
uses it (per feature, per app in a monorepo) had to split the config
and run `sqletch generate --config …` once per output, duplicating
`dialect`, `schema`, `server_version`, and the policy set, and paying
for one dev database per run.

The config now carries a **list of targets**, each pairing query
globs with an output package, and each glob may carry **capture
groups** whose matched text is substituted into the output path — so
one entry fans out into one generated package per matched directory.

```yaml
version: 1
dialect: postgres
server_version: "16"
schema: { files: [db/schema.sql] }
cache: { path: .sqletch/cache }

targets:
  - queries:
      - "(api/internal/app/*/db/queries)/*.sql"
      - "(api/internal/app/*/db/queries)/*.go"
    output:
      package: gen
      path: $1/gen
  - queries: [shared/queries/*.sql]
    output: { package: shared, path: internal/db/shared }
```

Everything above the emission layer is **unchanged**: the schema
fingerprint, the committed cache, the dev database, the oracle, and
policy weaving are per-*run*, not per-target. Two targets in one
config share one cache and one dev-database acquisition — the thing
splitting the config could never do.

```
scan → weave(14) → render → rules → oracle → nullability ─┬→ codegen → target A
                                        (per run, shared) └→ codegen → target B
```

## 2. Configuration surface

```
targets:                       # required, non-empty
  - queries: [<pattern>, …]    # required, non-empty
    output:
      package: <go-ident>      # required; may contain $n
      path: <dir>              # required; may contain $n
```

The top-level `queries:` and `output:` keys are **removed**. They are
still decoded (into deprecated fields) for one purpose: to emit a
`SQLETCH301` naming the rewrite instead of goccy's generic
"unknown field" (§7).

`overrides`, `static_expansion.queries`, `policies`, `cache`,
`schema`, `database`, `verification`, and `filter_tree_caps` stay
top-level and apply to the whole run:

- **`policies`** are table-based, so they weave into every target
  alike. This is deliberate — splitting output packages must not be a
  way to escape a tenant-scoping policy.
- **`overrides` / `static_expansion.queries`** key on a query NAME,
  which is only unique per target (§4). They apply to every matching
  query in every target, and resolution warns (`SQLETCH317`) when a
  name matches in more than one target, so an override that silently
  hit two queries is visible.

## 3. Patterns: recursive globs with captures

`internal/pathpat` replaces `filepath.Glob` for query patterns.
(`schema.files` keeps `config.ExpandGlobs`/`filepath.Glob`: its order
is the fingerprint's order and nothing about it wants captures.)

**Syntax.** Segments are separated by `/`. Within a segment, `*`, `?`
and `[…]` have `path.Match` meaning. A segment that is exactly `**`
matches **zero or more** path segments; `**` mixed with other
characters in one segment (`a/**x`) is a compile error rather than a
silent `*`. A pattern is project-relative; an absolute pattern or one
with a `..` segment is refused at load (`SQLETCH306` territory — the
clone-and-run read-redirection vector of design 07 §1).

**Captures.** `(` … `)` around a whole number of segments; `$1`…`$9`
(also `${1}`) in `output.path` / `output.package` substitute the
matched text, `$$` is a literal `$`. Captures **must align to segment
boundaries** and may not nest — intra-segment captures buy nothing
for the directory-fan-out use case and would force a character-level
matcher. Captured text is always `/`-separated and project-relative,
so a substituted path is byte-identical on every platform.

**Why not `{…}`**: `{` opens a YAML flow mapping, so
`query: {a/**/b}/*.sql` is a YAML parse error before sqletch sees it;
`(` needs no quoting. (Settled 2026-09 with the owner.)

**Why not doublestar**: no capture support, so the pattern would have
to be matched twice by two different engines. One engine that both
enumerates and captures cannot drift from itself.

**Enumeration.** `Pattern.Walk` descends only into directories a
remaining pattern segment can still match, so a pattern with a
literal prefix reads a handful of directories. Under a `**` the walk
is unavoidably a subtree walk, so it prunes `.git`, `node_modules`,
`vendor`, and dot-directories (a literal segment naming one still
matches: pruning applies only to the `**` expansion), follows
directory symlinks once each (a resolved-path visited set — loops
terminate), and stops at `maxWalkEntries`. The trusted-config threat
model (spec §Threat model) rates a self-authored pattern bomb LOW;
the cap exists so a stray `**` cannot hang `sqletch lsp`.

`Walk` also returns every directory it read, which the LSP uses as a
memo signature (§6).

## 4. Resolution: `config.ResolveTargets`

One function, called by `pipeline.Run` AND the LSP's
`OfflineChecker`, so the two can never disagree about which file
belongs to which package — the discipline `cli.scanChecks` and
`cli.resolvedChecks` already apply to the checking phases.

It returns `[]ResolvedTarget{Package, Path, Files}` plus diagnostics:

| Situation | Code | Severity |
|---|---|---|
| pattern matches no file | `SQLETCH316` | **warning** (a monorepo app may not exist yet; a target with no files emits nothing at all) |
| expanded `output.path` escapes the project | `SQLETCH306` | error |
| expanded `output.package` is not a Go identifier | `SQLETCH301` | error |
| one file matched by two targets | `SQLETCH314` | error |
| two targets → same path, different package | `SQLETCH315` | error |
| a query name lives in more than one target (overrides/static_expansion) | `SQLETCH317` | warning |

Two patterns that expand to the **same** `(package, path)` merge into
one target: the grouping key is the expanded output, not the config
entry. That is what lets `.sql` and `.go` patterns — or two source
directories — feed one package, and it is why captures are allowed in
every pattern of an entry rather than in a special singular `query:`
key. Every `$n` an entry's output references must exist in **every**
pattern of that entry (checked at load, no filesystem access).

Targets are ordered by expanded path, and files within a target are
sorted — determinism is not conditional on directory order.

`output.path` validation moves from `Load` to resolution whenever it
contains `$`: the final path is not knowable offline. A `$`-free path
is still checked at load, so the common config fails early.

## 5. Pipeline

`cli.Run` scans per target (`names` is a per-target map, so
`SQLETCH004` is scoped to the output package — two packages may each
define `GetUser`), then runs one shared oracle/nullability phase over
all queries, then emits **per target**: `codegen.Generate` once per
target, written under that target's directory.

Derived, non-authoritative outputs mirror the target path so two
same-named queries cannot overwrite each other:

```
.sqletch/explain/<target-path>/<Query>.json
.sqletch/expanded/<target-path>/<query>/<shape>.sql
```

`sqletch explain NAME` walks that tree and prints every match, with
the target path as a heading when more than one target is present.

**Stale cleanup** (`generate` only, settled with the owner 2026-09):
after writing a target, any `*.gen.go` in its directory that this run
did not write is removed — a deleted query no longer leaves a stale
method compiled into the package. Only `*.gen.go` is ever removed,
never a hand-written file, never a directory. A target whose path is
outside the project directory (the absolute-path case `Load` only
warns about) is **skipped with a warning**: sqletch does not delete
files outside the repository it was pointed at. `check` never
deletes. Targets that vanish entirely (a captured directory removed)
are not tracked — with the `path: $1/gen` idiom the generated
directory is inside the captured directory and disappears with it.

## 6. LSP

`OfflineChecker` resolves targets on each snapshot, checks the union
of all targets' files (plus open overlay buffers that match no
target), and scopes duplicate-name detection per target.

Re-resolving on every keystroke would re-walk the tree, so the
resolution is memoized against the directory list `Walk` returned:
the memo is reused while every recorded directory's stat signature
(size + mtime — a directory's mtime changes when an entry is added or
removed) is unchanged, plus the config file's own signature. This is
the same discipline as the existing `catMemo`. A resolution error
degrades to "no targets" and never takes the server down (design 10).

## 7. Migration

`version:` stays `1`. A config carrying the old keys gets:

```
SQLETCH301 sqletch.yaml: top-level `queries`/`output` were replaced by `targets`
  hint: targets:
          - queries: [queries/*.sql]
            output: { package: gen, path: gen }
```

Both spellings are never accepted at once: the presence of either
legacy key is an error even when `targets` is also present, so there
is no ambiguity about which one won.

## 8. Tests

- `internal/pathpat`: segment/`**`/capture matching, compile errors
  (unbalanced/nested/mis-aligned captures, `a/**x`, absolute, `..`),
  symlink-loop termination, entry cap, walk-pruning, determinism, and
  a property test that `Walk` over a generated tree agrees with a
  brute-force match of every file.
- `internal/config`: legacy-key diagnostic, `$n` arity validation,
  identifier validation, resolution diagnostics 306/314/315/316/317,
  merge-by-expanded-output, ordering.
- `internal/cli`: two targets in one run (same query name in both),
  per-target emission, stale `*.gen.go` cleanup (and that a
  hand-written file survives), explain/expanded namespacing, explain
  across targets, LSP per-target duplicate scoping and memo reuse.
- `internal/e2e` (devdb): a two-target project generates two packages
  that both compile and run against the real database.
