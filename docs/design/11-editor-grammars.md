# sqletch Design — 11: Editor Grammars (v0.4 editor support)

The second half of 08 §"Editor support": syntax highlighting for
template files, complementing the LSP server (doc 10). No Go code;
everything lives under `editors/`.

## 1. Approach

sqletch templates ARE `.sql` files — the construct vocabulary is a
thin layer over the dialect's SQL. Both grammars therefore **layer
over SQL highlighting instead of re-implementing it**:

- **TextMate (VS Code)**: an *injection grammar* into `source.sql`.
  The editor's existing SQL grammar keeps doing SQL; our patterns are
  injected with left precedence (`L:source.sql`) so construct markers,
  `:params`, and the `-- name:` / `-- @param` / `-- @column`
  directives win over the base grammar's comment/operator rules.
  Ships as a VS Code extension (`editors/vscode`) that also starts
  `sqletch lsp` for diagnostics and go-to-definition.
- **tree-sitter** (`editors/tree-sitter-sqletch`): a grammar that
  parses the *template structure* — query headers, construct blocks
  with their bodies, parameter references, directives — and treats SQL
  runs as opaque `sql_content`. An `injections.scm` query marks that
  content for the host editor's `sql` language, which is tree-sitter's
  standard embedding mechanism (the markdown/ejs pattern). Construct
  blocks are real nodes, so folding and textobjects fall out for free.

## 2. Scope names (TextMate) / captures (tree-sitter)

| Surface | TextMate scope | tree-sitter capture |
| --- | --- | --- |
| `@if-present` `@endif` `@when` `@end` `@choose` `@case` `@default` `@order-by` `@key` `@filter-tree` `@filter-tree!` `@predicate` `@in` | `keyword.control.sqletch` | `@keyword.directive` |
| guard/choose/order/tree parameter names in construct heads | `variable.parameter.sqletch` | `@variable.parameter` |
| `:param` references | `variable.parameter.sqletch` | `@variable.parameter` |
| `-- name:` header (query name) | `entity.name.function.sqletch` | `@function` |
| header annotation (`:many` …) | `storage.modifier.sqletch` | `@keyword.modifier` |
| `-- @param` / `-- @column` directive keyword | `keyword.other.directive.sqletch` | `@keyword.directive` |
| directive type names | `storage.type.sqletch` | `@type` |
| `@when` literals | `constant.sqletch` | `@constant` |

## 3. Testing

Grammar bugs are regressions like any other, so both grammars carry
executable tests, run via npx (Node is a dev-only dependency; nothing
lands in the Go module):

- tree-sitter: `npx tree-sitter-cli generate && npx tree-sitter-cli
  test` over `test/corpus/*.txt` — the standard corpus format pins the
  parse tree of representative templates (every construct, adversarial
  strings/comments containing `@` and `:`).
- TextMate: `npx vscode-tmgrammar-test` over `tests/*.sql` with inline
  scope assertions.

CI gets a `grammars` job running both. The generated tree-sitter
parser (`src/`) is committed, as consumers require it.

## 4. Non-goals

- No dialect-specific SQL highlighting differences (the base SQL
  grammar owns SQL; constructs are dialect-independent).
- No semantic tokens over LSP (the grammars cover highlighting;
  revisit only if injection proves insufficient somewhere).
- Publishing (VS Code Marketplace, npm) is a release-process concern,
  not part of this doc.
