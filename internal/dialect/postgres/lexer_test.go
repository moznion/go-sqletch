package postgres

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

func TestLexer_TokenBoundaries(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []struct {
			kind dialect.TokenKind
			text string
		}
	}{
		{
			name: "basic select with param",
			src:  "SELECT u.id FROM users WHERE u.status = :status",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
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
			name: "param with cast",
			src:  ":email_prefix::text",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindParamRef, ":email_prefix"},
				{dialect.KindCast, "::"},
				{dialect.KindIdent, "text"},
			},
		},
		{
			name: "string with doubled quote escape",
			src:  "'a''b' x",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindString, "'a''b'"},
				{dialect.KindIdent, "x"},
			},
		},
		{
			name: "e-string with backslash escape",
			src:  `E'\'quoted\'' y`,
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindString, `E'\'quoted\''`},
				{dialect.KindIdent, "y"},
			},
		},
		{
			name: "dollar quoted anonymous",
			src:  "$$ body with ' and :param $$ z",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindString, "$$ body with ' and :param $$"},
				{dialect.KindIdent, "z"},
			},
		},
		{
			name: "dollar quoted tagged",
			src:  "$fn$ SELECT 1; $fn$",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindString, "$fn$ SELECT 1; $fn$"},
			},
		},
		{
			name: "positional param",
			src:  "$1 $23",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindPositionalParam, "$1"},
				{dialect.KindPositionalParam, "$23"},
			},
		},
		{
			name: "at operators are not constructs",
			src:  "a @> b @@ c <@ d",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindIdent, "a"},
				{dialect.KindOperator, "@>"},
				{dialect.KindIdent, "b"},
				{dialect.KindOperator, "@@"},
				{dialect.KindIdent, "c"},
				{dialect.KindOperator, "<@"},
				{dialect.KindIdent, "d"},
			},
		},
		{
			name: "line and nested block comments",
			src:  "-- hello\n/* a /* b */ c */ id",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindLineComment, "-- hello"},
				{dialect.KindBlockComment, "/* a /* b */ c */"},
				{dialect.KindIdent, "id"},
			},
		},
		{
			name: "quoted identifier with escape",
			src:  `"we""ird" col`,
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindQuotedIdent, `"we""ird"`},
				{dialect.KindIdent, "col"},
			},
		},
		{
			name: "numbers",
			src:  "1 2.5 1e10 3.14e-2",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindNumber, "1"},
				{dialect.KindNumber, "2.5"},
				{dialect.KindNumber, "1e10"},
				{dialect.KindNumber, "3.14e-2"},
			},
		},
		{
			name: "punctuation",
			src:  "( ) , ;",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindLParen, "("},
				{dialect.KindRParen, ")"},
				{dialect.KindComma, ","},
				{dialect.KindSemicolon, ";"},
			},
		},
		{
			name: "operator does not absorb comment start",
			src:  "1+-- c\n2",
			want: []struct {
				kind dialect.TokenKind
				text string
			}{
				{dialect.KindNumber, "1"},
				{dialect.KindOperator, "+"},
				{dialect.KindLineComment, "-- c"},
				{dialect.KindNumber, "2"},
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
	for _, src := range []string{"'abc", `"abc`, "/* abc", "$tag$ abc", "E'abc"} {
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
	src := "SELECT 'a', $$b$$, :p::int -- t\nFROM x /* y */ WHERE a @> b AND c = $1;"
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
