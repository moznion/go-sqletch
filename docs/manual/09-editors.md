# Editor support

## Language server

```console
$ sqletch lsp     # stdio; started by your editor, not by hand
```

- **Diagnostics as you type** — the same `SQLETCHnnn` codes as the
  CLI, produced by the same analysis code (they cannot disagree).
- **Go-to-definition** on `:param` → its `-- @param` annotation (or
  first occurrence).
- **Strictly offline**: the server never opens a database. Checks
  that need oracle answers run only for queries fully covered by the
  committed cache; a cold cache degrades coverage, never correctness.
  Run `sqletch generate` once and the LSP picks the cache up on the
  next edit.

The server reads `sqletch.yaml` from the workspace root. Any LSP
client works; configure it to run `sqletch lsp` for SQL files.

Templates authored in a `//sqletch:query` const inside a `.go` file
get the same diagnostics, at the right Go line and column — the
analysis works on byte offsets into the real file. What they do **not**
get yet is syntax highlighting: the grammars below cover `.sql` files,
not templates embedded in Go raw strings.

## VS Code

The extension in `editors/vscode` bundles both halves:

- construct highlighting as an injection over the built-in SQL
  grammar (`@…` constructs, `:param` refs, `-- name:` / `-- @param` /
  `-- @column` directives);
- an LSP client for `sqletch lsp` (`sqletch.path` to locate the
  binary, `sqletch.lsp.enabled` to toggle).

## tree-sitter (Neovim, Helix, Zed, …)

`editors/tree-sitter-sqletch` parses the template structure (construct
blocks are real nodes — folding and textobjects work) and injects SQL
runs into your editor's SQL grammar. Standard `highlights.scm` /
`injections.scm` queries ship with it.
