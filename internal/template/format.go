package template

import (
	"strings"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Format canonicalizes a template file: skeleton SQL is preserved
// byte-for-byte (sqletch never reformats the user's SQL), construct
// markers are rewritten in canonical layout, and a missing `WHERE
// TRUE` anchor (R6) is inserted. Files that fail to scan are returned
// unchanged with their diagnostics. Format is a fixpoint:
// Format(Format(x)) == Format(x).
func Format(profile dialect.LexerProfile, path string, src []byte) ([]byte, []diagnostics.Diagnostic) {
	file, diags := NewScanner(profile).ScanFile(path, src)
	if diagnostics.HasErrors(diags) {
		return src, diags
	}

	var b strings.Builder
	pos := 0
	for _, q := range file.Queries {
		b.Write(src[pos:q.HeaderSpan.End])
		pos = q.HeaderSpan.End
		lastTok := ""
		lastTokEnd := -1 // end offset in b of the last significant token
		for _, it := range q.Items {
			switch v := it.(type) {
			case *Skeleton:
				start := b.Len()
				b.WriteString(v.Text)
				tok, end := lastSignificantToken(profile, v.Text)
				if tok != "" {
					lastTok = tok
					lastTokEnd = start + end
				}
			case *IfPresent:
				// R6 auto-fix: WHERE/HAVING directly followed by an
				// optional conjunct gets its TRUE anchor.
				needsAnchor := (v.Slot == SlotWhereConjunct && lastTok == "WHERE") ||
					(v.Slot == SlotHavingConjunct && lastTok == "HAVING")
				if needsAnchor && lastTokEnd >= 0 {
					out := b.String()
					b.Reset()
					b.WriteString(out[:lastTokEnd])
					b.WriteString(" TRUE")
					b.WriteString(out[lastTokEnd:])
				}
				writeIfPresent(&b, v)
				lastTok = "@construct"
			case *Choose:
				writeChoose(&b, v)
				lastTok = "@construct"
			case *OrderBy:
				writeOrderBy(&b, v)
				lastTok = "@construct"
			case *FilterTree:
				writeFilterTree(&b, v)
				lastTok = "@construct"
			case *InExpr:
				b.WriteString("@in(:")
				b.WriteString(v.Param)
				b.WriteString(")")
				lastTok = "@construct"
			}
			pos = it.Raw().End
		}
	}
	b.Write(src[pos:])
	return []byte(b.String()), diags
}

func writeIfPresent(b *strings.Builder, v *IfPresent) {
	isWhen := len(v.Guards) == 1 && v.Guards[0].IsValue()
	if isWhen {
		g := v.Guards[0]
		b.WriteString("@when(")
		b.WriteString(g.Param)
		b.WriteString(" ")
		b.WriteString(g.Op)
		b.WriteString(" ")
		b.WriteString(g.RawValue)
		b.WriteString(")\n")
	} else {
		b.WriteString("@if-present(")
		for i, g := range v.Guards {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(g.Param)
		}
		b.WriteString(")\n")
	}
	switch v.Sep {
	case SepAnd:
		b.WriteString("  AND ")
	case SepComma:
		b.WriteString("  , ")
	}
	b.WriteString(v.Body)
	if isWhen {
		b.WriteString("\n@end")
	} else {
		b.WriteString("\n@endif")
	}
}

func writeChoose(b *strings.Builder, v *Choose) {
	b.WriteString("@choose(")
	b.WriteString(v.Param)
	b.WriteString(")\n")
	for _, cs := range v.Cases {
		b.WriteString("@case(")
		b.WriteString(cs.Name)
		b.WriteString(")\n")
		b.WriteString(cs.Body)
		b.WriteString("\n")
	}
	if v.Default != nil {
		b.WriteString("@default\n")
		if v.Default.Body != "" {
			b.WriteString(v.Default.Body)
			b.WriteString("\n")
		}
	}
	b.WriteString("@end")
}

func writeOrderBy(b *strings.Builder, v *OrderBy) {
	b.WriteString("@order-by(")
	b.WriteString(v.Param)
	b.WriteString(")\n")
	for _, k := range v.Keys {
		b.WriteString("@key(")
		b.WriteString(k.Name)
		b.WriteString(")\n")
		b.WriteString(k.Body)
		b.WriteString("\n")
	}
	if v.Default != nil {
		b.WriteString("@default\n")
		if v.Default.Body != "" {
			b.WriteString(v.Default.Body)
			b.WriteString("\n")
		}
	}
	b.WriteString("@end")
}

func writeFilterTree(b *strings.Builder, v *FilterTree) {
	b.WriteString("@filter-tree")
	if v.Required {
		b.WriteString("!")
	}
	b.WriteString("(")
	b.WriteString(v.Param)
	b.WriteString(")\n")
	for _, pr := range v.Predicates {
		b.WriteString("@predicate(")
		b.WriteString(pr.Name)
		b.WriteString(")\n")
		b.WriteString(pr.Body)
		b.WriteString("\n")
	}
	b.WriteString("@end")
}

// lastSignificantToken returns the last non-trivia token (uppercased
// for idents) of text and its end offset, or ("", -1).
func lastSignificantToken(profile dialect.LexerProfile, text string) (string, int) {
	src := []byte(text)
	pos := 0
	tok, end := "", -1
	for {
		t, err := profile.NextToken(src, pos)
		if err != nil || t.Kind == dialect.KindEOF {
			return tok, end
		}
		switch t.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
		case dialect.KindIdent:
			tok, end = strings.ToUpper(t.Text), t.End
		default:
			tok, end = t.Text, t.End
		}
		pos = t.End
	}
}
