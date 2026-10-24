package cli

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Security audit F11/F12 (CWE-436): the leak shapes. On MySQL and SQLite
// the engine reads everything after `--` up to the next LF as comment,
// so in
//
//	WHERE o.status = :status -- open only<CR> AND o.tenant_id = :tenant_id
//
// the hand-written tenant conjunct is comment text (idempotence would
// credit it and skip the weave, Enforce would see it), and in
//
//	SELECT … FROM orders -- newest<CR>ORDER BY id
//
// the woven `WHERE (tenant)` lands inside the engine's comment. Both now
// stop at SQLETCH024 on every dialect, in the shared scan the pipeline
// and the LSP both use, before any weaving — nothing is woven or
// rendered for the file.
func TestLoneCR_PolicyLeakShapesRejected(t *testing.T) {
	shapes := []struct{ name, query string }{
		{"hand-written conjunct after comment", "-- name: ListOrders :many\n-- @param status: text\nSELECT o.id FROM orders AS o\nWHERE o.status = :status -- open only\r AND o.tenant_id = :tenant_id;\n"},
		{"weave point after comment", "-- name: ListOrders :many\nSELECT id FROM orders -- newest\rORDER BY id;\n"},
	}
	for _, d := range []struct{ dialect, version, typ string }{
		{"postgres", "16", "bigint"},
		{"mysql", "8.4", "bigint"},
		{"sqlite", "3", "integer"},
	} {
		for _, s := range shapes {
			t.Run(d.dialect+"/"+s.name, func(t *testing.T) {
				yaml := strings.Replace(policyProjectYAML, "dialect: postgres", "dialect: "+d.dialect, 1)
				yaml = strings.Replace(yaml, `server_version: "16"`, `server_version: "`+d.version+`"`, 1)
				yaml = strings.Replace(yaml, "type: bigint", "type: "+d.typ, 1)
				if d.dialect == "mysql" {
					yaml = strings.Replace(yaml, "schema:", "database:\n  oracle: native\nschema:", 1)
				}
				cfg := writeOfflineProject(t, map[string]string{
					"sqletch.yaml":       yaml,
					"db/schema.sql":      "CREATE TABLE orders (id bigint NOT NULL, tenant_id bigint NOT NULL, status text);",
					"queries/orders.sql": s.query,
				})
				c := NewOfflineChecker(cfg)
				res, err := c.Check(nil)
				if err != nil {
					t.Fatal(err)
				}
				path := cfg.Abs("queries/orders.sql")
				cr := strings.IndexByte(s.query, '\r')
				found := false
				for _, dg := range res.Diags[path] {
					if dg.Code == diagnostics.CodeLoneCR {
						if dg.Span.Start != cr || dg.Span.End != cr+1 {
							t.Fatalf("SQLETCH024 span = [%d,%d), want [%d,%d)", dg.Span.Start, dg.Span.End, cr, cr+1)
						}
						found = true
					}
				}
				if !found {
					t.Fatalf("no SQLETCH024; diagnostics: %+v", res.Diags[path])
				}
				if m := c.memo[path]; m != nil && len(m.wovenq) != 0 {
					t.Fatalf("a file with a lone CR must not be woven; woven: %v", m.wovenq)
				}
			})
		}
	}
}

// A lone CR inside a `//sqletch:query` raw string is reported at its
// .go file offset. (The Go compiler discards CRs from raw literals, so
// the const and the scanned bytes would not even agree with each other.)
func TestLoneCR_GoSourceRejected(t *testing.T) {
	const src = "package repo\n\n//sqletch:query\nconst q = ~\n-- name: FindT :many\nSELECT t.id FROM t -- c\rWHERE t.id = 1\n;\n~\n"
	cfg := writeOfflineProject(t, goProject(t, src))
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("repo/users.go")
	cr := strings.IndexByte(src, '\r')
	for _, dg := range res.Diags[path] {
		if dg.Code == diagnostics.CodeLoneCR && dg.Span.Start == cr && dg.Span.End == cr+1 {
			return
		}
	}
	t.Fatalf("want SQLETCH024 at [%d,%d); diagnostics: %+v", cr, cr+1, res.Diags[path])
}
