package lsp

import (
	"fmt"
	"net/url"
	"path/filepath"
)

// The LSP type subset this server speaks. Field names and casing
// follow the protocol spec; only the members sqletch uses are modeled
// (unknown incoming members are ignored by encoding/json).

type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"` // UTF-16 code units (negotiated encoding)
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

const (
	severityError   = 1
	severityWarning = 2
)

type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// syncFull: the client sends whole-document contents on every change.
const syncFull = 1

type ServerCapabilities struct {
	PositionEncoding   string `json:"positionEncoding"`
	TextDocumentSync   int    `json:"textDocumentSync"`
	DefinitionProvider bool   `json:"definitionProvider"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   ServerInfo         `json:"serverInfo"`
}

type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type DidOpenParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

type ContentChange struct {
	Text string `json:"text"`
}

type DidChangeParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	// Full sync: the last element carries the complete new content.
	ContentChanges []ContentChange `json:"contentChanges"`
}

type DidCloseParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

type DidSaveParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

type DefinitionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

const (
	messageError   = 1
	messageWarning = 2
)

type ShowMessageParams struct {
	Type    int    `json:"type"`
	Message string `json:"message"`
}

func pathToURI(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

func uriToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("lsp: bad document URI %q: %w", uri, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("lsp: non-file document URI %q", uri)
	}
	return filepath.Clean(u.Path), nil
}
