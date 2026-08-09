# sqletch Design — 01: Template Scanner (Phase P1)

Deliverable: `internal/template` — turn a `.sql` template file into
`[]*QueryTemplate` (skeleton chunks + construct blocks with exact
spans), with all lexical-level errors reported. No database, no SQL
parser; this phase must be fully unit-testable in isolation.

Prerequisites: none (first phase).
Depends on: `internal/dialect` (the `LexerProfile` interface only),
`internal/diagnostics`.

## 1. Lexer profile

The scanner walks dialect SQL without parsing it. Everything
dialect-specific about *lexing* is captured in:

```go
// internal/dialect
type LexerProfile interface {
    // NextToken returns the next raw token starting at src[pos:].
    // Kinds the scanner cares about: String, Comment, LineComment,
    // Ident, QuotedIdent, Operator, ParamRef, Cast, LParen, RParen,
    // Comma, Semicolon, Keyword, Other.
    NextToken(src []byte, pos int) (Token, error)
}

type Token struct {
    Kind  TokenKind
    Start, End int
    Text  string // uppercased for Keyword; verbatim otherwise
}
```

The postgres profile (in `dialect/postgres/lexer.go`) handles:
single-quoted strings with `''` escapes and `E'...'`, dollar-quoted
strings (`$tag$ ... $tag$`), `--` line comments, nested `/* */`
comments, double-quoted identifiers, `::` casts (so `:param::text`
lexes as ParamRef + Cast + Ident), `:name` param refs, and operators
(`@>`, `<@`, `@@` come out as Operator tokens and can never be
mistaken for constructs, which are matched *before* operator lexing —
see §2).

The profile is intentionally *not* a full SQL lexer: it only needs to
be correct about token *boundaries* (so constructs, params, and clause
keywords are never found inside strings/comments/identifiers).

## 2. Construct recognition

Constructs are matched at token boundaries, before the profile's
operator rule, by exact lowercase match against the fixed vocabulary:

```
@if-present( ident ("," ident)* )   ... @endif
@choose( ident )                    @case( ident ) ... [@default ...] @end
```

v0.1 vocabulary: `@if-present`, `@endif`, `@choose`, `@case`,
`@default`, `@end`. Anything else starting with `@` falls through to
the profile (PostgreSQL operators; later, MySQL variables — hyphenated
construct names are invalid variable syntax there, per the spec).

Matching rule: `@` followed immediately by a known keyword followed by
`(` or a token boundary. `@if-presentx` is not a construct (falls
through); `@if-present (` with a space is diagnostic `SQLETCH001`
("construct arguments must follow immediately") — strictness keeps the
grammar unambiguous and formattable.

Guard-argument idents must be `snake_case` (`[a-z][a-z0-9_]*`);
violations are `SQLETCH002`.

## 3. Query splitting and annotations

A file is split on `-- name: <Name> :<annotation>` header comments
(sqlc-compatible). Rules:

- Header must precede any non-comment token of its query. Statements
  without a header: `SQLETCH003`.
- `<Name>` must be a valid exported Go identifier after PascalCase
  mapping; duplicates per output package: `SQLETCH004` (checked in P6
  across files, and per file here).
- One statement per query (a single `;` terminator, optional for the
  last query in a file). Multiple statements under one header:
  `SQLETCH005`.

## 4. Clause-context tracking

The scanner maintains a coarse clause context to (a) record where each
construct appeared, (b) lift separators correctly, and (c) reject
constructs at non-slot positions *early* with good messages (the
authoritative check is R1 in P2; the scanner's rejection is a fast
path covering the obvious cases).

Context is tracked only at paren depth 0 (top level of the statement),
transitioning on keywords:

| Keyword seen (depth 0) | Context becomes |
|---|---|
| `SELECT` | `Projection` |
| `FROM` | `From` |
| `JOIN` / `LEFT` / `RIGHT` / `FULL` / `CROSS` / `INNER` | stays `From` |
| `WHERE` | `Where` |
| `GROUP` | `GroupBy` |
| `HAVING` | `Having` |
| `ORDER` | `OrderBy` |
| `LIMIT` / `OFFSET` / `FETCH` / `FOR` | `Tail` |
| `UPDATE` | `UpdateTarget` → `Set` after `SET` |
| `INSERT` | `InsertTarget` → `InsertColumns` after `(` … |

Inside parens (subqueries, function calls) the context is `Nested`,
and **any construct there is `SQLETCH006`** — constructs are top-level
only (R1). This is precise at scanner level because paren depth is
exact even without parsing.

v0.1 accepts constructs in: `From` (`@if-present` join item), `Where`
(`@if-present` conjunct), `OrderBy` (`@choose`). Constructs in any
other context: `SQLETCH007` with a message naming the allowed slots
(this also cleanly rejects the projection example from
the spec's Rejected Examples).

## 5. Separator lifting

Per P2 (spec premise), the leading separator belongs to the composer,
not the fragment. In a `Where` block the scanner requires the body to
start with `AND` (token, not substring) and strips it (`Sep = SepAnd`).
Bodies not starting with `AND`: `SQLETCH008` ("write the conjunct as
`AND <predicate>`; sqletch owns the separator"). Join items have
`SepNone`. (Comma lifting arrives with SET/INSERT slots in v0.2 — the
`Sep` enum already includes `SepComma`.)

The stored `Body` is the verbatim bytes after the lifted separator,
trimmed of leading/trailing blank lines but **not** re-indented
(fragment bytes are sacred; P2 premise).

Nested constructs inside a body (a guard within a guard, R5) are
`SQLETCH012`, with the multi-parameter-guard rewrite as the hint; the
scanner skips the nested block with a marker stack so one mistake does
not cascade.

## 6. `@choose` block structure

Grammar checks done here (lexically):

- At least one `@case`. `@default` at most once, last. `SQLETCH009`.
- Case names unique per block, `snake_case`. `SQLETCH002`/`SQLETCH009`.
- In `OrderBy` context, each case body must itself start with
  `ORDER BY` (token check) or be empty (for `@default` only). Empty
  non-default cases: `SQLETCH009`. The composer owns nothing inside a
  case body in v0.1 — the whole `ORDER BY …` clause text is the case
  body, and clause omission (R6) = emitting an empty default. (This
  matches the spec: `ORDER BY` is omissible; the "clause keyword owned
  by composer" mechanics start mattering with v0.2 slots.)

## 7. Parameter collection

Every `ParamRef` token inside skeleton and bodies is recorded into
`QueryTemplate.Params` with the spans of all its occurrences and
whether each occurrence is inside a guarded block (and which guards).
This is raw data for R9 (P3); the scanner itself only rejects
malformed names (`SQLETCH002`) and positional refs (`$1`, `?` —
`SQLETCH011`, "named parameters only").

## 8. Guard-bit assignment and encoding limits

Guard atoms get bits in first-appearance document order (stable across
runs by construction). >64 atoms: `SQLETCH010`. Multiple blocks with
the same guard *set* share nothing structurally (they remain separate
fragments) but naturally share bits per atom.

`checkEncodingLimits` enforces the shape key's other two bounds under
the same code: **≤64 `@order-by` keys per block** (sequence elements
pack as `key<<1|desc` into a uint8, and the used-key masks in
`shape.orderOptions` and `runtime.OrderSeq` are 64 bits) and **≤255
`@choose` ordinals per block**, counting the `@default` body
(`ShapeKey.Choices` is one uint8 per block). It also bounds the *bind
plan*: **≤32767 parameters per query** (`SQLETCH013`), because
`runtime.Bind.Idx` is an int16.

These are refusals, not policy knobs: past them the encoding truncates
and composes a *different* query — the wrong `@choose` case, the wrong
sort column, the wrong bound value — with no error at any layer.
`runtime` re-checks the shape-key bounds (`ErrShapeKeyLimit`) so a
codegen/scanner disagreement surfaces as an error instead of wrong SQL,
and `internal/codegen` pins the two ends' constants against each other.

## 9. Output invariants (checked by debug assertions + tests)

- Concatenating `Skeleton.Text` and construct raw spans in order
  reproduces the input file byte-for-byte (nothing lost, nothing
  invented).
- Every `Span` is within file bounds, non-overlapping, ordered.
- No construct tokens remain inside any `Skeleton.Text` or `Body`.
- **Diagnostic spans are in bounds too**, not just item spans. Several
  "empty body" diagnostics point at the character *after* a marker
  (`span(p, p+1)`); when the marker ends at EOF there is no such
  character. Because consumers index the source with these spans — the
  excerpt renderer, and the LSP's UTF-16 position conversion — the
  clamp lives in `fileScan.span`, the single span constructor, making
  the invariant structural rather than a per-call-site obligation.

## 10. Testing & acceptance criteria

- Table-driven token tests for the postgres lexer profile: strings
  (incl. dollar-quoted with tags), nested comments, `::` vs `:name`,
  `@>` vs `@if-present`.
- Golden tests: `testdata/scanner/*.sql` → `*.golden.json` (the
  serialized `QueryTemplate`). Corpus must include every example from
  the spec (accepted ones parse; each Rejected Example
  yields its documented diagnostic where the scanner is the detecting
  layer).
- Fuzz test: `FuzzScan` must never panic and must uphold the §9
  invariants on arbitrary input — item-span contiguity on input that
  scans successfully, and in-bounds diagnostic spans (plus a panic-free
  `RenderExcerpt`, whose caret geometry does rune arithmetic over bytes
  that need not be valid UTF-8) on input that does not. It runs **every
  lexer profile** per input: the quoting rules are what differ between
  dialects (dollar quoting, backticks, bracket quoting) and are exactly
  where a scanner mis-tracks state, so a postgres-only target left two
  of the three unexercised. Crashing inputs are committed under
  `testdata/fuzz/FuzzScan/` to pin the regression; CI uploads any it
  finds as an artifact, since the runner's copy is otherwise discarded.
- Acceptance: all Use Case 1/3 templates scan into the expected
  structure; diagnostics carry correct spans (verified by golden
  `.diag` files rendering file excerpts).
