package cli

import (
	"io"
	"os"
	"strings"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/lsp"
)

// LSP implements `sqletch lsp`: the language server of
// docs/design/10-lsp.md over stdio, backed by the OfflineChecker. A
// broken config does not kill the server (clients would restart it in
// a loop) — it serves degraded and reports the config diagnostics via
// window/showMessage.
func LSP(configPath string, in io.Reader, out, errW io.Writer) int {
	cfg, diags := config.Load(configPath)
	if len(diags) > 0 {
		src, _ := os.ReadFile(configPath)
		var b strings.Builder
		for _, d := range diags {
			b.WriteString(d.Render(src))
			b.WriteByte('\n')
		}
		return lsp.Serve(in, out, nil, strings.TrimSpace(b.String()), errW)
	}
	return lsp.Serve(in, out, &workspaceAdapter{checker: NewOfflineChecker(cfg)}, "", errW)
}

// workspaceAdapter narrows the OfflineChecker to the lsp.Workspace
// seam (identical shape, distinct types to keep lsp free of cli).
type workspaceAdapter struct {
	checker *OfflineChecker
}

func (a *workspaceAdapter) Check(overlay map[string][]byte) (lsp.WorkspaceResult, error) {
	res, err := a.checker.Check(overlay)
	if err != nil {
		return lsp.WorkspaceResult{}, err
	}
	return lsp.WorkspaceResult{Diags: res.Diags, Files: res.Files, Sources: res.Sources}, nil
}
