# sqletch for Visual Studio Code

Editor support for [sqletch](https://github.com/moznion/go-sqletch) SQL
templates:

- **Construct highlighting** via a TextMate injection grammar layered
  over the built-in SQL highlighting: `@if-present` / `@when` /
  `@choose` / `@order-by` / `@filter-tree` / `@in` blocks, `:param`
  references, and the `-- name:` / `-- @param` / `-- @column`
  directives.
- **Language server**: starts `sqletch lsp` for `SQLETCHnnn`
  diagnostics as you type and go-to-definition on parameters. Point
  `sqletch.path` at the binary if it is not on PATH; disable with
  `sqletch.lsp.enabled: false`.

Grammar tests: `npm run test` (vscode-tmgrammar-test).
