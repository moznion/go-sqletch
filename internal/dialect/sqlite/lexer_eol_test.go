package sqlite

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// Security audit F12 (CWE-436): SQLite (tokenize.c: `c!='\n'`) and the
// rqlite frontend (scanner.go: `case -1, '\n'`) end `--` comments ONLY
// at '\n'. The profile used to end them at a lone '\r' too, so
// `-- newest<CR>ORDER BY id` hid the ORDER BY from the engine while
// sqletch wove a WHERE in front of it — inside the engine's comment.
func TestLexer_LineCommentEndsOnlyAtLF(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"lone CR", "-- open only\r AND o.tenant_id = :tid\nSELECT", "-- open only\r AND o.tenant_id = :tid"},
		{"lone CR before ORDER BY", "-- newest\rORDER BY id\nSELECT", "-- newest\rORDER BY id"},
		{"CRLF keeps CR in comment", "-- c\r\nSELECT", "-- c\r"},
		{"to EOF", "--c\rx", "--c\rx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toks := lexAll(t, tc.src)
			if toks[0].Kind != dialect.KindLineComment || toks[0].Text != tc.want {
				t.Fatalf("first token = %v %q, want line comment %q", toks[0].Kind, toks[0].Text, tc.want)
			}
		})
	}
}
