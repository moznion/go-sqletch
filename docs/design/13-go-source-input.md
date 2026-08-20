# sqletch Design — 13: Go-source template input

An **additive** second input form: templates may live in a
`//sqletch:query` const inside a `.go` file instead of a `.sql` file.
`.sql` remains the documented default; nothing about the existing form
changes.

## 1. Motivation and non-goals

The template language and the generated API are unchanged — this doc
moves *where the template text is authored*, nothing else.

- **Motivation**: co-location. The template sits next to the
  repository-layer code that uses it, so a query and its caller are
  renamed, reviewed, and moved together.
- **Not a goal**: SQL built from Go control flow. Conditionality stays
  in the template's constructs. The const requirement below is what
  makes that structural rather than conventional.
- **Not a goal**: user-declared row/params types. Codegen still emits
  them exactly as for `.sql` input.
- **Not a goal**: `go/types`. Extraction is *syntactic only*, so the
  target package need not compile — which matters because it may
  reference generated symbols that do not exist yet.

Because the frontend (`internal/template` onward) consumes the same
bytes either way, **P1/P2 and R1–R9 are untouched**: the same template
text produces the same skeleton, renderings, oracle queries, cache
entries, and generated code, whichever file it was authored in.

## 2. Authoring form

```go
//sqletch:query
const searchUsersSQL = `
-- name: SearchUsers :many
SELECT u.id, u.email FROM users AS u
WHERE TRUE
@if-present(organization_id)
  AND u.organization_id = :organization_id
@endif
;
`
```

The literal's contents are a template *file*: byte-for-byte the same
grammar, `-- name:` headers included, multiple queries per literal
allowed.

Three restrictions, each with a diagnostic:

| Restriction | Why |
| --- | --- |
| Marker on a `const` declaration (not `var`) | Immutability at the declaration level is what makes "the SQL is a constant" structural — a `var` could be reassigned, and the template text would no longer describe what runs. |
| Value is a **raw** string literal (backquoted) | Interpreted-string escapes break the 1:1 byte mapping §3 depends on; and SQL is full of backslashes. |
| Value is a single literal, not a concatenation | v1 scope. Concatenation of constants is sound in principle but has no offset-preserving view. |

The marker may sit on a single `const` spec or on a `const (…)` block,
in which case it applies to every spec in the block.

An unmarked const is ignored: opting a file into the `queries:` globs
must not silently make every backquoted string in it a template.

## 3. The masked, truncated view

Diagnostics are `{File, Start, End}` byte offsets, and every consumer —
`Render`/`RenderExcerpt` (`internal/diagnostics`), the LSP's UTF-16
conversion, the source map — resolves them against the file's bytes.
So the extractor does **not** hand the scanner a substring. For each
marked literal it builds a view of the *whole file*:

```
.go file:   [ package … const x = ` ][ template text ][ ` func … ]
view:       [ spaces, newlines kept  ][ template text ]
                                                      ^ EOF (truncated)
```

- Bytes before the literal are replaced by `' '`, except `'\n'` which
  is kept, so both byte offsets **and** line numbers are identical to
  the real file.
- The view is truncated at the literal's end, so the trailing Go code
  cannot be absorbed into the last query's extent (a query runs to the
  next header or EOF).
- One view per marked literal; each is scanned separately and the
  resulting `QueryFile`s are merged.

Consequences: no span shifting exists to get wrong, and excerpts point
at the real `.go` line with a correct caret. The whitespace prefix is
inert to the lexer, and no `-- name:` header can appear in it.

Cost is kept linear in the file, not O(literals × file size):

- **Memory**: one backing buffer is blanked once and reused across
  every view (re-blanked after each), so scratch memory is O(file
  size), not a full prefix copy per literal.
- **CPU**: the view is truncated at the literal's end *and* handed to
  the scanner with the literal's start offset via
  `template.Scanner.ScanFileFrom(path, view, start)`, so the scanner
  begins at the literal instead of re-lexing — and re-copying, as one
  giant whitespace token's `Text` — the blank prefix once per const.
  Because the skipped prefix is scan-inert trivia, the result is
  byte-identical to scanning the whole `[0,end)` view from 0
  (`ScanFileFrom` with `start == 0` *is* `ScanFile`); the equality is
  pinned by `TestScanFileFromEqualsScanFile`. Without it a file of
  thousands of marked consts was O(consts × file size) quadratic CPU,
  DoS-able by a malicious repo and, through `cli.scanSource`, a hang of
  the LSP on file-open.

## 4. Wiring

`config.Queries` globs already carry paths; the input form is chosen by
extension (`.go` → extractor, otherwise → read as-is). No new config
key.

Both scan sites go through one shared helper so the CLI and the LSP can
never diverge:

- `cli.pipeline.Run` — the generate/check pipeline
- `cli.OfflineChecker.analyzeFile` — the LSP's analysis seam

`res.Sources[path]` keeps the **original** `.go` bytes (excerpts must
show real Go source), never a view.

## 5. Diagnostics

New codes in the scanner band:

| Code | Condition |
| --- | --- |
| `SQLETCH020` | `.go` file does not parse |
| `SQLETCH021` | `//sqletch:query` on something other than a `const` declaration |
| `SQLETCH022` | marked const's value is not a raw string literal |
| `SQLETCH023` | marked const declares no value / more values than names |

Every message names the rule and its rationale, and hints the
compliant rewrite, per the project-wide diagnostic contract.

## 6. Deliberately deferred

- Editor support for templates inside Go strings (injection into Go
  raw strings for tree-sitter/TextMate). Until then, authoring in
  `.go` loses template highlighting; the LSP diagnostics work, because
  they are offset-based and the views preserve offsets.
- Constant *expressions* (`const q = base + tail`).
- `//go:generate` ergonomics: `sqletch generate` is invoked exactly as
  before; a `//go:generate sqletch generate` line is a user
  convenience, not a mechanism this design depends on.
