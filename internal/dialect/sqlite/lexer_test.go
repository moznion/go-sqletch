package sqlite

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
			name: "quoted identifier forms",
			src:  `"we""ird" [brack et] ` + "`tick``ed`" + ` col`,
			want: []wantTok{
				{dialect.KindQuotedIdent, `"we""ird"`},
				{dialect.KindQuotedIdent, "[brack et]"},
				{dialect.KindQuotedIdent, "`tick``ed`"},
				{dialect.KindIdent, "col"},
			},
		},
		{
			name: "strings double quotes only, no backslash escape",
			src:  `'a''b' '\' x`,
			want: []wantTok{
				{dialect.KindString, "'a''b'"},
				{dialect.KindString, `'\'`},
				{dialect.KindIdent, "x"},
			},
		},
		{
			name: "blob literal",
			src:  "x'DEAD' X'BEEF' y",
			want: []wantTok{
				{dialect.KindString, "x'DEAD'"},
				{dialect.KindString, "X'BEEF'"},
				{dialect.KindIdent, "y"},
			},
		},
		{
			name: "question placeholders plain and numbered",
			src:  "a = ? AND b = ?12",
			want: []wantTok{
				{dialect.KindIdent, "a"},
				{dialect.KindOperator, "="},
				{dialect.KindPositionalParam, "?"},
				{dialect.KindIdent, "AND"},
				{dialect.KindIdent, "b"},
				{dialect.KindOperator, "="},
				{dialect.KindPositionalParam, "?12"},
			},
		},
		{
			name: "dash-dash comments need no whitespace",
			src:  "1--2\n3",
			want: []wantTok{
				{dialect.KindNumber, "1"},
				{dialect.KindLineComment, "--2"},
				{dialect.KindNumber, "3"},
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
			name: "json and concat operators",
			src:  "a || b -> c ->> d",
			want: []wantTok{
				{dialect.KindIdent, "a"},
				{dialect.KindOperator, "||"},
				{dialect.KindIdent, "b"},
				{dialect.KindOperator, "->"},
				{dialect.KindIdent, "c"},
				{dialect.KindOperator, "->>"},
				{dialect.KindIdent, "d"},
			},
		},
		{
			name: "numbers incl hex and leading dot",
			src:  "1 2.5 1e10 0x1A .5",
			want: []wantTok{
				{dialect.KindNumber, "1"},
				{dialect.KindNumber, "2.5"},
				{dialect.KindNumber, "1e10"},
				{dialect.KindNumber, "0x1A"},
				{dialect.KindNumber, ".5"},
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
			name: "native at and dollar params lex as boundaries",
			src:  "@x $y",
			want: []wantTok{
				{dialect.KindOperator, "@"},
				{dialect.KindIdent, "x"},
				{dialect.KindOther, "$"},
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
	for _, src := range []string{"'abc", `"abc`, "[abc", "`abc", "/* abc", "x'AB"} {
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
	src := "SELECT 'a', [b c], `d`, :p --t\nFROM x /* y */ WHERE a ->> b AND c = ?;"
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
