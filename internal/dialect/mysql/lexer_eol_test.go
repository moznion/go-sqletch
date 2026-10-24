package mysql

import (
	"errors"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// Security audit F11 (CWE-436): the MySQL server and the TiDB frontend
// end `--`/`#` line comments ONLY at '\n' (TiDB lexer.go startWithDash /
// startWithSharp: incAsLongAs(ch != '\n')). The profile used to end them
// at a lone '\r' too, so `-- note<CR> AND o.tenant_id = :tid` read as
// live SQL to sqletch (weave idempotence, Enforce, clause offsets) but
// as comment text to the engine — a silent cross-tenant leak.
func TestLexer_LineCommentEndsOnlyAtLF(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"dash lone CR", "-- open only\r AND o.tenant_id = :tid\nSELECT", "-- open only\r AND o.tenant_id = :tid"},
		{"hash lone CR", "# newest\rORDER BY id\nSELECT", "# newest\rORDER BY id"},
		{"dash CRLF keeps CR in comment", "-- c\r\nSELECT", "-- c\r"},
		{"dash to EOF", "-- c\rx", "-- c\rx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toks := lexAll(t, tc.src)
			if toks[0].Kind != dialect.KindLineComment || toks[0].Text != tc.want {
				t.Fatalf("first token = %v %q, want line comment %q", toks[0].Kind, toks[0].Text, tc.want)
			}
		})
	}
}

// `--` opens a comment when the third byte is one both the server
// (my_isspace || my_iscntrl) and TiDB (unicode.IsSpace on the byte)
// treat as a comment opener: '\t' '\n' '\v' '\f' '\r' ' ' or end of
// input. VT and FF used to lex as `-` `-` operators: live SQL to sqletch,
// a comment to the engine.
func TestLexer_DashDashOpenerAgreedBytes(t *testing.T) {
	for _, b := range []byte{'\t', '\n', '\v', '\f', '\r', ' '} {
		src := "--" + string([]byte{b}) + "x"
		toks := lexAll(t, src)
		if toks[0].Kind != dialect.KindLineComment {
			t.Errorf("%q: first token = %v %q, want a line comment", src, toks[0].Kind, toks[0].Text)
		}
	}
	if toks := lexAll(t, "--"); toks[0].Kind != dialect.KindLineComment {
		t.Errorf("bare `--` at EOF must be a comment, got %v", toks[0].Kind)
	}
	// Accepted neighbor: `--` before an ordinary byte is double negation.
	for _, tok := range lexAll(t, "1--2") {
		if tok.Kind == dialect.KindLineComment {
			t.Errorf("`1--2` must not open a comment")
		}
	}
}

// The server and TiDB DISAGREE on the remaining bytes: the server also
// opens a comment before any other control byte (0x00–0x08, 0x0E–0x1F,
// 0x7F); TiDB also before 0x85 and 0xA0 (unicode.IsSpace(rune(b))).
// Whichever reading sqletch picked, one of the two would differ from it,
// so the profile refuses the input (fail closed).
func TestLexer_DashDashOpenerDisputedBytesRefused(t *testing.T) {
	var disputed []byte
	for b := 0x00; b <= 0x08; b++ {
		disputed = append(disputed, byte(b))
	}
	for b := 0x0E; b <= 0x1F; b++ {
		disputed = append(disputed, byte(b))
	}
	disputed = append(disputed, 0x7F, 0x85, 0xA0)
	for _, b := range disputed {
		src := []byte("SELECT 1 --" + string([]byte{b}) + " x")
		pos := 0
		var err error
		for {
			var tok dialect.Token
			tok, err = Profile{}.NextToken(src, pos)
			if err != nil || tok.Kind == dialect.KindEOF {
				break
			}
			pos = tok.End
		}
		var le *dialect.LexError
		if !errors.As(err, &le) {
			t.Errorf("byte 0x%02X after `--`: want a LexError, got %v", b, err)
			continue
		}
		if le.Pos != 9 {
			t.Errorf("byte 0x%02X after `--`: LexError at %d, want 9 (the `--`)", b, le.Pos)
		}
	}
}
