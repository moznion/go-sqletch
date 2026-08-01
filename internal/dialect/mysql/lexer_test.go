package mysql

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// lexAll tokenizes src fully, failing the test on lex errors.
func lexAll(t *testing.T, src string) []dialect.Token {
	t.Helper()
	var toks []dialect.Token
	p := Profile{}
	pos := 0
	for {
		tok, err := p.NextToken([]byte(src), pos)
		if err != nil {
			t.Fatalf("lex error at %d in %q: %v", pos, src, err)
		}
		if tok.Kind == dialect.KindEOF {
			return toks
		}
		toks = append(toks, tok)
		pos = tok.End
	}
}

// kindsOf drops trivia and returns (kind, text) pairs.
func kindsOf(toks []dialect.Token) []dialect.Token {
	var out []dialect.Token
	for _, tok := range toks {
		if tok.Kind == dialect.KindWhitespace {
			continue
		}
		out = append(out, tok)
	}
	return out
}

type wantTok struct {
	kind dialect.TokenKind
	text string
}

func TestLexer_TokenBoundaries(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []wantTok
	}{
		{
			name: "basic select with param",
			src:  "SELECT u.id FROM users WHERE u.status = :status",
			want: []wantTok{
				{dialect.KindIdent, "SELECT"},
				{dialect.KindIdent, "u"},
				{dialect.KindOther, "."},
				{dialect.KindIdent, "id"},
				{dialect.KindIdent, "FROM"},
				{dialect.KindIdent, "users"},
				{dialect.KindIdent, "WHERE"},
				{dialect.KindIdent, "u"},
				{dialect.KindOther, "."},
				{dialect.KindIdent, "status"},
				{dialect.KindOperator, "="},
				{dialect.KindParamRef, ":status"},
			},
		},
		{
			name: "backtick quoted identifier with escape",
			src:  "`we``ird` col",
			want: []wantTok{
				{dialect.KindQuotedIdent, "`we``ird`"},
				{dialect.KindIdent, "col"},
			},
		},
		{
			name: "single-quoted string with backslash and doubled escapes",
			src:  `'a\'b' 'c''d' x`,
			want: []wantTok{
				{dialect.KindString, `'a\'b'`},
				{dialect.KindString, "'c''d'"},
				{dialect.KindIdent, "x"},
			},
		},
		{
			name: "double quotes are strings in default sql_mode",
			src:  `"not an ident" y`,
			want: []wantTok{
				{dialect.KindString, `"not an ident"`},
				{dialect.KindIdent, "y"},
			},
		},
		{
			name: "question mark positional param",
			src:  "a = ? AND b = ?",
			want: []wantTok{
				{dialect.KindIdent, "a"},
				{dialect.KindOperator, "="},
				{dialect.KindPositionalParam, "?"},
				{dialect.KindIdent, "AND"},
				{dialect.KindIdent, "b"},
				{dialect.KindOperator, "="},
				{dialect.KindPositionalParam, "?"},
			},
		},
		{
			name: "hash line comment",
			src:  "# hello\nid",
			want: []wantTok{
				{dialect.KindLineComment, "# hello"},
				{dialect.KindIdent, "id"},
			},
		},
		{
			name: "dash-dash comment requires whitespace",
			src:  "-- ok\n1--2",
			want: []wantTok{
				{dialect.KindLineComment, "-- ok"},
				{dialect.KindNumber, "1"},
				{dialect.KindOperator, "--"},
				{dialect.KindNumber, "2"},
			},
		},
		{
			name: "block comments do not nest",
			src:  "/* a /* b */ c",
			want: []wantTok{
				{dialect.KindBlockComment, "/* a /* b */"},
				{dialect.KindIdent, "c"},
			},
		},
		{
			name: "dollar is a legal identifier character",
			src:  "$var a$b",
			want: []wantTok{
				{dialect.KindIdent, "$var"},
				{dialect.KindIdent, "a$b"},
			},
		},
		{
			name: "null-safe equality and assignment operators",
			src:  "a <=> b := c",
			want: []wantTok{
				{dialect.KindIdent, "a"},
				{dialect.KindOperator, "<=>"},
				{dialect.KindIdent, "b"},
				{dialect.KindOperator, ":="},
				{dialect.KindIdent, "c"},
			},
		},
		{
			name: "numbers",
			src:  "1 2.5 1e10 3.14e-2",
			want: []wantTok{
				{dialect.KindNumber, "1"},
				{dialect.KindNumber, "2.5"},
				{dialect.KindNumber, "1e10"},
				{dialect.KindNumber, "3.14e-2"},
			},
		},
		{
			name: "punctuation",
			src:  "( ) , ;",
			want: []wantTok{
				{dialect.KindLParen, "("},
				{dialect.KindRParen, ")"},
				{dialect.KindComma, ","},
				{dialect.KindSemicolon, ";"},
			},
		},
		{
			name: "user variable at-sign lexes as operator plus ident",
			src:  "@x @@global.y",
			want: []wantTok{
				{dialect.KindOperator, "@"},
				{dialect.KindIdent, "x"},
				{dialect.KindOperator, "@@"},
				{dialect.KindIdent, "global"},
				{dialect.KindOther, "."},
				{dialect.KindIdent, "y"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kindsOf(lexAll(t, tt.src))
			if len(got) != len(tt.want) {
				t.Fatalf("token count = %d, want %d\ngot: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				if got[i].Kind != w.kind || got[i].Text != w.text {
					t.Errorf("token %d = (%v, %q), want (%v, %q)", i, got[i].Kind, got[i].Text, w.kind, w.text)
				}
			}
		})
	}
}

func TestLexer_Unterminated(t *testing.T) {
	for _, src := range []string{"'abc", `"abc`, "/* abc", "`abc", `'abc\'`} {
		p := Profile{}
		pos := 0
		var err error
		var tok dialect.Token
		for {
			tok, err = p.NextToken([]byte(src), pos)
			if err != nil || tok.Kind == dialect.KindEOF {
				break
			}
			pos = tok.End
		}
		if err == nil {
			t.Errorf("expected lex error for %q, got none", src)
		}
	}
}

func TestLexer_CoversAllBytes(t *testing.T) {
	src := "SELECT 'a', `b`, :p # t\nFROM x /* y */ WHERE a <=> b AND c = ?;"
	toks := lexAll(t, src)
	pos := 0
	for _, tok := range toks {
		if tok.Start != pos {
			t.Fatalf("gap or overlap at %d (token starts at %d)", pos, tok.Start)
		}
		pos = tok.End
	}
	if pos != len(src) {
		t.Fatalf("tokens end at %d, want %d", pos, len(src))
	}
}
