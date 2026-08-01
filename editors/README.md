# Editor support

- `vscode/` — VS Code extension: TextMate injection grammar over
  `source.sql` plus a client for `sqletch lsp`.
- `tree-sitter-sqletch/` — tree-sitter grammar: template constructs as
  real nodes, SQL runs injected into the host editor's `sql` grammar
  (Neovim, Helix, Zed, …).

Design: `docs/design/11-editor-grammars.md`.
