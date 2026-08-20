package codegen

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Common initialisms rendered in caps, sqlc-compatible.
var initialisms = map[string]string{
	"id": "ID", "url": "URL", "uri": "URI", "api": "API", "uuid": "UUID",
	"sql": "SQL", "http": "HTTP", "json": "JSON", "html": "HTML", "ip": "IP",
}

// GoName maps a snake_case (or arbitrary SQL) identifier to an
// exported Go name. Collisions after mapping are the caller's job to
// detect (SQLETCH310) — this function never disambiguates silently.
func GoName(name string) string {
	var b strings.Builder
	for part := range strings.SplitSeq(name, "_") {
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		if up, ok := initialisms[lower]; ok {
			b.WriteString(up)
			continue
		}
		r := []rune(lower)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	if b.Len() == 0 {
		return "X"
	}
	out := b.String()
	// A Go identifier must begin with a letter (or '_'). Decode the first
	// rune properly: out[0] is a UTF-8 lead byte, and testing it as a rune
	// misclassifies every multibyte first letter (e.g. Hebrew, which then
	// wrongly acquired an "X" prefix).
	if r0, _ := utf8.DecodeRuneInString(out); !unicode.IsLetter(r0) {
		return "X" + out
	}
	return out
}

// lowerCamel lowers the leading initialism-or-rune of an exported name
// (SearchUsers -> searchUsers, IDList -> idList).
func lowerCamel(name string) string {
	if name == "" {
		return name
	}
	r := []rune(name)
	n := 0
	for n < len(r) && unicode.IsUpper(r[n]) {
		n++
	}
	// Keep the last upper rune with the following word (IDList -> idList).
	if n > 1 && n < len(r) {
		n--
	}
	for i := 0; i < n; i++ {
		r[i] = unicode.ToLower(r[i])
	}
	return string(r)
}
