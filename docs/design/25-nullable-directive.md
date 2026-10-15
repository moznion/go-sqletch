# sqletch Design — 25: `-- @nullable` directive

Status: ACCEPTED — owner decisions D1–D3 settled 2026-10-14;
implemented. D4–D9 are the implementation choices made within them,
recorded here for review.

## 1. Motivation

A result column's nullability can be forced only from sqletch.yaml
(`overrides: [{query, column, nullable}]`). The declaration is about a
query — usually an application invariant visible in its SQL, such as
a `WHERE a.id IS NOT NULL` filtering away a LEFT JOIN's
null-extension — yet it lives in a different file, keyed by a query
name. This design adds the in-template spelling.

```sql
-- name: BooksWithAuthor :many
-- @nullable author_name: false
SELECT b.id, a.name AS author_name
FROM books AS b LEFT JOIN authors AS a ON a.id = b.author_id
WHERE a.id IS NOT NULL;
```

## 2. Owner decisions

- **D1 — syntax.** A separate directive, `-- @nullable <column>:
  true|false`, mirroring the config's `nullable: <bool>`. NOT an
  extension of `-- @column`: that directive is about TYPES (it supplies
  them on Tier 2 and asserts them against the oracle on Tier 1,
  SQLETCH216), and nullability is a different fact with a different
  failure direction. Both `true` and `false` are accepted, like the
  config.
- **D2 — conflict with the config.** The same (query, column) in
  sqletch.yaml `overrides` AND a directive is an error (SQLETCH319)
  **even when the two agree**: one declaration per column, so the two
  cannot drift apart silently on a later edit, and there is no
  precedence rule to remember.
- **D3 — the config key stays.** `overrides` is not deprecated; both
  spellings coexist.

## 3. Semantics

Exactly a config override's (design 05 §3 step 1): the forced verdict
is applied, by result-column NAME, in every verified rendering before
any analysis, so the nullable-most union over renderings returns it.
`true` generates `optional.Option[T]`, `false` a plain `T`.

It is an author ASSERTION, outside the soundness argument in the same
way a config override always was: the analyzer never narrows from it,
the author does. The failure direction is loud — a wrong `false` makes
the driver refuse to scan the first NULL into the plain field (pinned
on all three engines by `TestNullableDirective*` in the devdb suite);
it never yields a silent zero value. Parameter nullability (design 20)
is untouched: the directive names result columns only.

## 4. Implementation choices

- **D4 — grammar.** Loose `^--\s*@nullable\b` decides "this is the
  directive" (so a malformed one is diagnosed, not ignored as a
  comment); the accepted form is
  `^--\s*@nullable\s+([a-z][a-z0-9_]*)\s*:\s*(true|false)\s*$` — one
  column per directive, the `@column` identifier class, lowercase
  literals only.
- **D5 — SQLETCH018.** Malformed, non-boolean, or a second directive
  for the same column (even agreeing — the `@timeout` rule) is a
  scanner error that records nothing; the first directive stands.
- **D6 — hygiene.** The config's SQLETCH312 (dead: no such result
  column) and SQLETCH313 (ambiguous: several same-named columns) are
  reused as warnings, spanned at the directive. They are computed in
  `cli.resolvedChecks` (against the maximal rendering's columns) so the
  LSP reports them from a warm cache; the config's own hygiene stays in
  `pipeline.Run`, spanned at the config file.
- **D7 — conflict check placement.** SQLETCH319 needs no catalog: it
  runs per query in the scan phase of BOTH `pipeline.Run` and
  `OfflineChecker.Check` (`cli.nullOverrideConflictDiags`), spanned at
  the directive, naming the config file. The config entry keys on a
  query name, so with several targets a config entry conflicts with a
  directive on any same-named query.
- **D8 — one merge seam.** `cli.effectiveNullOverrides` assembles a
  query's forced verdicts from both sources; `nullability` takes them as
  `map[string]Verdict` and adopts them verbatim, so `explain` shows
  where each was declared (`forced by sqletch.yaml overrides` /
  `forced by -- @nullable`). The old reason text named a nonexistent
  `null_overrides` key; it and the 312/313 messages were corrected.
- **D9 — cache.** Like every directive the comment stays in the
  skeleton, so adding or editing one re-keys the query's oracle entries
  (one cold `generate`). Verdicts are never cached (design 05 §4), and
  the directive never enters the fingerprint.

## 5. Editors

TextMate rule `nullable-directive` and tree-sitter node
`nullable_directive` (`column_name`, `nullable_value` → `@boolean`);
see design 11 §2.
