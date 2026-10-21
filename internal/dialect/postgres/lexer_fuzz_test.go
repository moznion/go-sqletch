package postgres

import (
	"fmt"
	"strings"
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// byteClass is what a lexer says about one source byte, at the
// granularity the lexical policy/rule scanners depend on: is it
// separator whitespace, inside an opaque token (string literal, quoted
// identifier, comment — text no scanner may read as SQL), or ordinary
// SQL text. Exact token boundaries of ordinary text are deliberately
// NOT compared: the profile is a boundary lexer, not PostgreSQL's
// (e.g. it keeps `<-` as one operator where scan.l splits it), and
// such differences cannot hide or expose SQL text.
type byteClass uint8

const (
	classSpace byteClass = iota
	classOpaque
	classPlain
	classDontCare // a literal's type prefix (E, B, X, N, U&): PG folds it into the token
)

func (c byteClass) String() string {
	return [...]string{"space", "opaque", "plain", "prefix"}[c]
}

// oursClasses classifies src with Profile.NextToken; ok=false on a lex
// error. paramRef reports a `:name` token (template syntax).
func oursClasses(src []byte) (cls []byteClass, paramRef, ok bool) {
	cls = make([]byteClass, len(src))
	pos := 0
	for {
		tok, err := Profile{}.NextToken(src, pos)
		if err != nil {
			return nil, paramRef, false
		}
		if tok.Kind == dialect.KindEOF {
			return cls, paramRef, true
		}
		paramRef = paramRef || tok.Kind == dialect.KindParamRef
		c := classPlain
		switch tok.Kind {
		case dialect.KindWhitespace:
			c = classSpace
		case dialect.KindString, dialect.KindQuotedIdent, dialect.KindLineComment, dialect.KindBlockComment:
			c = classOpaque
		}
		for i := tok.Start; i < tok.End; i++ {
			cls[i] = c
		}
		pos = tok.End
	}
}

// pgClasses classifies src with PostgreSQL's own scanner (pg_query.Scan
// = scan.l of the vendored server version) — the independent oracle.
// Bytes no token covers are whitespace to PostgreSQL.
func pgClasses(src string) ([]byteClass, bool) {
	res, err := pgquery.Scan(src)
	if err != nil {
		return nil, false
	}
	cls := make([]byteClass, len(src)) // zero value = classSpace
	for _, tk := range res.GetTokens() {
		start, end := int(tk.GetStart()), int(tk.GetEnd())
		if start < 0 || end > len(src) || start > end {
			return nil, false
		}
		text := src[start:end]
		opaque := false
		switch tk.GetToken() {
		case pgquery.Token_SCONST, pgquery.Token_USCONST, pgquery.Token_BCONST, pgquery.Token_XCONST,
			pgquery.Token_SQL_COMMENT, pgquery.Token_C_COMMENT:
			opaque = true
		case pgquery.Token_IDENT, pgquery.Token_UIDENT:
			opaque = strings.HasPrefix(text, `"`) || strings.HasPrefix(strings.ToUpper(text), `U&"`)
		}
		if !opaque {
			for i := start; i < end; i++ {
				cls[i] = classPlain
			}
			continue
		}
		body := start
		if !strings.HasPrefix(text, "--") && !strings.HasPrefix(text, "/*") {
			if k := strings.IndexAny(text, `'"$`); k > 0 {
				for i := start; i < start+k; i++ {
					cls[i] = classDontCare
				}
				body = start + k
			}
		}
		for i := body; i < end; i++ {
			cls[i] = classOpaque
		}
	}
	return cls, true
}

// renderParamRefs replaces each `:name` template parameter with a
// placeholder the way rendering does before PostgreSQL ever sees the
// text (`:E'…'` is a param ref followed by a plain string to sqletch,
// never an E-string). The placeholder is blank-padded so it cannot fuse
// with a neighbor into a new token, which would test this harness, not
// the lexer. From a lex error on, the text is kept verbatim so the
// comparison still sees the profile reject it.
func renderParamRefs(src []byte) []byte {
	var out []byte
	pos := 0
	for {
		tok, err := Profile{}.NextToken(src, pos)
		if err != nil {
			return append(out, src[pos:]...)
		}
		if tok.Kind == dialect.KindEOF {
			return out
		}
		if tok.Kind == dialect.KindParamRef {
			out = append(out, " $1 "...)
		} else {
			out = append(out, src[tok.Start:tok.End]...)
		}
		pos = tok.End
	}
}

// lexerDiffSeeds are the historic divergences (F16) plus one sample of
// every literal/comment form; each must reach the byte comparison
// (TestLexerVsPgScan_SeedsLive).
var lexerDiffSeeds = []string{
	"SELECT o.id FROM orders o WHERE o.note <> $é$ AND o.deleted_at IS NULL AND $é$",
	"SELECT $\x80_9$ a $\x80_9$, $$x$$, $a1$ $ $a1$",
	"SELECT $é$ $É$ $é$",
	"SELECT a\fFROM\vt\r\nWHERE b",
	"SELECT 'a''b', E'c\\'d', B'01', X'ff', N'n', U&'u', U&\"v\" FROM \"q\"\"i\"",
	"SELECT 'a'\n'b' -- c\n/* d /* e */ f */ g",
	"SELECT :p, :p::int, $1 FROM t WHERE a @> b AND c <-1",
	"SELECT é1, _x$y, a$$b$$ FROM t",
	"$1$ x",
	"SELECT o.id FROM orders o WHERE o.note <> E'a'\n'\\' AND o.deleted_at IS NULL AND \\''",
	"SELECT E'a'\v\n\f'\\'' , e'b' \r'c' , 'd'\n'\\'",
}

// lexerVsPg runs the comparison on one template text. reached=false when
// the input never gets to the comparison; failure is non-empty on a
// divergence.
func lexerVsPg(s string) (reached bool, failure string) {
	if strings.IndexByte(s, 0) >= 0 {
		return false, "" // NUL truncates the C scanner's input
	}
	src := renderParamRefs([]byte(s))
	pg, pgOK := pgClasses(string(src))
	if !pgOK {
		return false, "" // PostgreSQL rejects it: nothing reaches the server
	}
	ours, paramRef, ok := oursClasses(src)
	if !ok {
		return true, fmt.Sprintf("profile lex error on text PostgreSQL scans:\n%q", src)
	}
	if paramRef {
		// Rendering exposed new `:name` text (it was inside a literal the
		// placeholder re-delimited); the server never sees that text.
		return false, ""
	}
	for i := range src {
		p, o := pg[i], ours[i]
		agree := p == classDontCare || p == o ||
			// a plain quote continuation ('a'\n'b' is one PG literal)
			// keeps the separator whitespace as whitespace to the profile
			(p == classOpaque && o == classSpace && isSpace(src[i]))
		if !agree {
			lo, hi := max(0, i-16), min(len(src), i+16)
			return true, fmt.Sprintf("byte %d (%q) is %v to PostgreSQL but %v to the profile\ncontext: %q\nfull: %q",
				i, src[i], p, o, src[lo:hi], src)
		}
	}
	return true, ""
}

// FuzzLexerVsPgScan is the differential oracle for the lexer profile
// (F16): every byte PostgreSQL's scanner treats as inside a string
// literal, quoted identifier, or comment must be opaque to the profile,
// every byte it treats as SQL text must be SQL text to the profile, and
// every byte it skips as whitespace must be whitespace to the profile.
// A disagreement is how a predicate-shaped string body gets credited as
// a scoping conjunct by the lexical policy scanners (a non-ASCII
// dollar-quote tag, an E-string quote continuation), or a separator
// gets mistaken for SQL (\f, \v). A profile lex error on text
// PostgreSQL scans is a failure too (fail-closed, but a divergence).
// Inputs PostgreSQL rejects are skipped: the server refuses them anyway.
func FuzzLexerVsPgScan(f *testing.F) {
	for _, seed := range lexerDiffSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if _, failure := lexerVsPg(s); failure != "" {
			t.Fatal(failure)
		}
	})
}

// Liveness: a seed that stops reaching the comparison (pg_query starts
// rejecting it, or the harness skips it) would pass forever.
func TestLexerVsPgScan_SeedsLive(t *testing.T) {
	for _, s := range lexerDiffSeeds {
		if reached, _ := lexerVsPg(s); !reached {
			t.Errorf("seed never reaches the byte comparison: %q", s)
		}
	}
}
