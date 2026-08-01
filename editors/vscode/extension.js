// sqletch VS Code extension: starts `sqletch lsp` (diagnostics +
// go-to-definition, docs/design/10-lsp.md) for SQL documents. The
// construct highlighting is a TextMate injection grammar declared in
// package.json and needs no code.
'use strict';

const { workspace } = require('vscode');
const { LanguageClient } = require('vscode-languageclient/node');

let client;

exports.activate = function activate() {
  const cfg = workspace.getConfiguration('sqletch');
  if (!cfg.get('lsp.enabled', true)) {
    return;
  }
  client = new LanguageClient(
    'sqletch',
    'sqletch',
    {
      command: cfg.get('path', 'sqletch'),
      args: ['lsp'],
    },
    {
      documentSelector: [{ language: 'sql' }],
    },
  );
  // start() surfaces spawn failures (missing binary) as a client
  // error notification; the grammar keeps working regardless.
  client.start();
};

exports.deactivate = function deactivate() {
  return client ? client.stop() : undefined;
};
