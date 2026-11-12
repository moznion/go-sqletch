package mysql

import (
	"strings"
	"testing"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/format"

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

// TestLexer_RefusesExecutableComment pins the fail-closed handling of
// MySQL's /*! ... */ (and /*!NNNNN ... */) executable comments. The
// server STRIPS the markers and executes the content, so a `?`/`:param`
// inside such a comment is counted by the server but was invisible to
// sqletch when it lexed as an ordinary block comment (0 vs 1
// placeholders → bind-slot mismatch and native-oracle byte-identity
// divergence). It must be REFUSED with a clear diagnostic.
func TestLexer_RefusesExecutableComment(t *testing.T) {
	for _, src := range []string{
		"SELECT /*! ? */ 1 FROM t1 WHERE id = ?",
		"SELECT /*!40000 ? */ 1 FROM t1",
		"/*!*/",
		"/*!50000 SQL_NO_CACHE */",
	} {
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
		le, ok := err.(*dialect.LexError)
		if !ok {
			t.Errorf("expected *dialect.LexError for %q, got %T: %v", src, err, err)
			continue
		}
		if !strings.Contains(le.Msg, "executable comment") {
			t.Errorf("refusal for %q should mention the executable comment: %q", src, le.Msg)
		}
	}

	// A plain block comment (no '!') still lexes fine.
	toks := lexAll(t, "SELECT /* plain */ 1")
	if len(toks) == 0 {
		t.Fatal("plain block comment must not be refused")
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

// Numeric literals and digit-led identifiers follow the TiDB frontend
// (whose readings are pinned below via ParseOneStmt + Restore, and which
// mirrors the server's MY_LEX_NUMBER_IDENT): a digit run followed by an
// identifier character is ONE identifier (`1abc`, `1_000`, `1$`, and
// `1e`/`1ex` when no exponent digit follows), `0x`/`0b` literals are
// numbers only when the digit run is not continued by an identifier
// character (`0x1Fg`, `0b012`, `0x` are identifiers), an exponent ends
// the number (`1e1e1` is `1e1` + `e1`, `1e5abc` is `1e5` + `abc`), and a
// number takes at most ONE `.` (`1.2.3` is `1.2` then `.3`).
func TestLexer_NumericLiteralsMatchTiDB(t *testing.T) {
	tests := []struct {
		src  string
		want []wantTok
	}{
		{"1abc 1_000 1$", []wantTok{{dialect.KindIdent, "1abc"}, {dialect.KindIdent, "1_000"}, {dialect.KindIdent, "1$"}}},
		{"1e 1ex", []wantTok{{dialect.KindIdent, "1e"}, {dialect.KindIdent, "1ex"}}},
		{"1e+x", []wantTok{{dialect.KindIdent, "1e"}, {dialect.KindOperator, "+"}, {dialect.KindIdent, "x"}}},
		{"1e5abc 1e1e1", []wantTok{{dialect.KindNumber, "1e5"}, {dialect.KindIdent, "abc"}, {dialect.KindNumber, "1e1"}, {dialect.KindIdent, "e1"}}},
		{"0x1F 0x1e 0b01", []wantTok{{dialect.KindNumber, "0x1F"}, {dialect.KindNumber, "0x1e"}, {dialect.KindNumber, "0b01"}}},
		{"0x1Fg 0x1F$ 0b012 0x 0b", []wantTok{{dialect.KindIdent, "0x1Fg"}, {dialect.KindIdent, "0x1F$"}, {dialect.KindIdent, "0b012"}, {dialect.KindIdent, "0x"}, {dialect.KindIdent, "0b"}}},
		{"1.2.3", []wantTok{{dialect.KindNumber, "1.2"}, {dialect.KindNumber, ".3"}}},
		{"1.e5 .5e1 1.5abc", []wantTok{{dialect.KindNumber, "1.e5"}, {dialect.KindNumber, ".5e1"}, {dialect.KindNumber, "1.5"}, {dialect.KindIdent, "abc"}}},
		{"1x'a' 0x1F'b'", []wantTok{{dialect.KindIdent, "1x"}, {dialect.KindString, "'a'"}, {dialect.KindNumber, "0x1F"}, {dialect.KindString, "'b'"}}},
		{"1--2 1#c", []wantTok{{dialect.KindNumber, "1"}, {dialect.KindOperator, "--"}, {dialect.KindNumber, "2"}, {dialect.KindNumber, "1"}, {dialect.KindLineComment, "#c"}}},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got := kindsOf(lexAll(t, tt.src))
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i, w := range tt.want {
				if got[i].Kind != w.kind || got[i].Text != w.text {
					t.Errorf("token %d = (%v, %q), want (%v, %q)", i, got[i].Kind, got[i].Text, w.kind, w.text)
				}
			}
		})
	}
}

// The TiDB readings the table above encodes, so a parser upgrade that
// changes them is noticed (the frontend is the profile's ground truth).
func TestLexer_NumericLiteralsTiDBReadings(t *testing.T) {
	for src, want := range map[string]string{
		"1abc":   "SELECT `1abc`",
		"1_000":  "SELECT `1_000`",
		"1e+x":   "SELECT `1e`+`x`",
		"1e5abc": "SELECT 1e+05 AS `abc`",
		"1e1e1":  "SELECT 1e+01 AS `e1`",
		"0x1F":   "SELECT x'1f'",
		"0x1Fg":  "SELECT `0x1Fg`",
		"0b012":  "SELECT `0b012`",
		"1.5abc": "SELECT 1.5 AS `abc`",
		"1x'a'":  "SELECT `1x` AS `a`",
	} {
		stmt, err := parser.New().ParseOneStmt("SELECT "+src, "", "")
		if err != nil {
			t.Errorf("%q: %v", src, err)
			continue
		}
		var sb strings.Builder
		if err := stmt.Restore(format.NewRestoreCtx(format.DefaultRestoreFlags, &sb)); err != nil {
			t.Fatal(err)
		}
		if sb.String() != want {
			t.Errorf("%q restores as %q, want %q", src, sb.String(), want)
		}
	}
}
