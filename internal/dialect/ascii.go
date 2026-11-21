package dialect

import "strings"

// asciiSpace is the whitespace every engine's scanner agrees on (PG
// scan.l `space`, MySQL my_isspace over ASCII, SQLite sqlite3Isspace
// plus \v). unicode.IsSpace also accepts U+0085, U+00A0, U+2000…, which
// MySQL/SQLite read as identifier bytes and PostgreSQL rejects.
func asciiSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r'
}

// ASCIITrimSpace is strings.TrimSpace restricted to ASCII whitespace.
func ASCIITrimSpace(s string) string { return strings.TrimFunc(s, asciiSpace) }

// ASCIIFields is strings.Fields restricted to ASCII whitespace.
func ASCIIFields(s string) []string { return strings.FieldsFunc(s, asciiSpace) }

// ASCIIEqualFold is strings.EqualFold restricted to ASCII letters: the
// long s `ſ` and the Kelvin sign are NOT `s` and `k`. Use it wherever
// the other side compares case-insensitively over ASCII (engines, DNS
// host names, protocol header names).
func ASCIIEqualFold(a, b string) bool {
	return len(a) == len(b) && ASCIILower(a) == ASCIILower(b)
}
