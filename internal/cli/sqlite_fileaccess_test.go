package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Security audit F15 (CWE-22): a SQLite schema file that ATTACHes (or
// VACUUMs INTO) a database file outside the project must be refused as
// SQLETCH322 with a span on the offending statement, before any file is
// created — the in-process engine would otherwise write wherever the
// repo-controlled path points, bypassing SQLETCH306.
func TestRun_SQLiteSchemaFileAccessRefused(t *testing.T) {
	for _, tc := range []struct {
		name, stmt string
	}{
		{"attach", "ATTACH DATABASE %s AS x;\nCREATE TABLE x.t (id INTEGER);\n"},
		{"vacuum into", "VACUUM INTO %s;\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")
			outside := t.TempDir()
			target := filepath.Join(outside, "escaped.db")
			prefix := "CREATE TABLE users (id INTEGER PRIMARY KEY, status TEXT NOT NULL);\n"
			schemaPath := filepath.Join(dir, "db", "schema.sql")
			stmt := strings.Replace(tc.stmt, "%s", "'"+target+"'", 1)
			if err := os.WriteFile(schemaPath, []byte(prefix+stmt), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, diags := config.Load(cfgPath)
			if diagnostics.HasErrors(diags) {
				t.Fatalf("config: %v", diags)
			}
			for _, mode := range []Mode{ModeCheck, ModeCheckExhaustive} {
				res, err := Run(context.Background(), cfg, mode, RunOptions{AllowDestructive: true})
				if err != nil {
					t.Fatalf("a refused schema statement must be a diagnostic, not an environment error: %v", err)
				}
				d := findCode(res.Diags, diagnostics.CodeSQLiteFileAccess)
				if d == nil {
					t.Fatalf("want %s, got %v", diagnostics.CodeSQLiteFileAccess, res.Diags)
				}
				if filepath.Base(d.Span.File) != "schema.sql" {
					t.Errorf("span file = %q, want the schema file", d.Span.File)
				}
				if d.Span.Start != len(prefix) || d.Span.End != len(prefix)+1 {
					t.Errorf("span = [%d,%d), want [%d,%d)", d.Span.Start, d.Span.End, len(prefix), len(prefix)+1)
				}
				if d.Hint == "" {
					t.Error("diagnostic must carry a hint")
				}
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("schema application created files outside the project: %d entries", len(entries))
			}

			var out, errW bytes.Buffer
			if code := explainAnalyze(context.Background(), cfg, nil, 1, true, &out, &errW); code != ExitDiagnostics {
				t.Errorf("explain --analyze exit = %d, want %d (stderr %q)", code, ExitDiagnostics, errW.String())
			}
			if !strings.Contains(errW.String(), string(diagnostics.CodeSQLiteFileAccess)) {
				t.Errorf("explain --analyze must report %s: %q", diagnostics.CodeSQLiteFileAccess, errW.String())
			}
		})
	}
}

// Must-stay: the ordinary SQLite project still checks clean.
func TestRun_SQLiteOrdinarySchemaNotRefused(t *testing.T) {
	dir := t.TempDir()
	cfg, diags := config.Load(writeSQLiteProject(t, dir, "3", "dev.sqlite3"))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %v", diags)
	}
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: %v", res.Diags)
	}
}
