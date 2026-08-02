# CLI reference

Global flags: `--config sqletch.yaml` (path), `--json` (diagnostics as
JSON lines on stderr).

| Command | What it does |
| --- | --- |
| `sqletch generate` | Full pipeline: scan → rules → oracle (cache-aware) → nullability → emit Go into `output.path`. Unchanged files keep their mtime. |
| `sqletch check` | Everything except writing output. `--exhaustive` additionally prepares AND plans **every enumerable shape** against the dev database (the compositional-verification claim, checked mechanically; needs the database). |
| `sqletch explain` | Per-query summary: guard bits, cases, shape count, parameter and column types, [policy coverage](12-policies.md) (woven / opted out with reason), maximal SQL (reads the data written by the last generate). `--enumerate` prints every shape's SQL (cap 4096). `--analyze` runs the dialect's plan explainer on every shape against the dev database (cap 64). |
| `sqletch fmt` | Canonicalizes construct layout and inserts missing `TRUE` anchors. Skeleton SQL is preserved byte-for-byte; fmt∘fmt = fmt. `--check` lists files that would change (exit 1) instead of writing. |
| `sqletch lsp` | The language server over stdio ([editors](09-editors.md)). Strictly offline. |
| `sqletch version` | Prints the version. |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success. |
| 1 | Diagnostics — something in YOUR files (templates, config) needs fixing. |
| 2 | Environment — database unreachable, version mismatch, unreadable files. Fix the surroundings, not the templates. |

The split is deliberate for CI: a red `1` is a review comment, a red
`2` is an infrastructure page.

## Offline behavior

`generate` and `check` print their cache posture:

```
sqletch: 6 queries ok (oracle cache: 10 hits, 0 misses; offline: yes)
```

`offline: yes` means no database was contacted — guaranteed when the
committed cache covers every rendering and the schema fingerprint
matches. `check --exhaustive` and `explain --analyze` always need the
database.
