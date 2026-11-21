package mysql

import (
	"strings"
	"unicode/utf8"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// MySQL compares identifiers two different ways (verified against a
// mysql:8.4 server, TestMySQLIdentFold* in internal/e2e):
//
//   - Column names (and output aliases) are case-insensitive under
//     my_strcasecmp(system_charset_info = utf8mb3_general_ci), which maps
//     each character through that collation's OWN lower-case table and
//     compares code points. The table is older than Go's Unicode data:
//     Go's strings.ToLower merges strictly MORE (486 BMP pairs, e.g.
//     `ẞ`/`ß`, `Ƀ`/`ƀ`), never less. identLower applies the server's
//     table, extracted from the server itself into identfold_table.go.
//   - Table names and aliases depend on lower_case_table_names: 0 (the
//     Linux default and the devdb container) compares them EXACTLY
//     (`a.x` does not reach alias `A`; table `T` is not `t`), 1/2
//     compare them case-insensitively. sqletch cannot see the setting
//     offline, so relations get a "may" key (identLower: equal under
//     some configuration) and a "must" key (exact: equal under all).
var _ dialect.IdentFolds = Profile{}

// FoldColumn is the server's column-name comparison key.
func (Profile) FoldColumn(name string) string { return identLower(name) }

// FoldRelationMay keys relation names that MAY be the same relation
// (they are under lower_case_table_names=1/2).
func (Profile) FoldRelationMay(name string) string { return identLower(name) }

// FoldRelationMust keys relation names that are the same relation under
// every configuration: only byte-identical ones.
func (Profile) FoldRelationMust(name string) string { return name }

var identLowerMap = func() map[rune]rune {
	m := make(map[rune]rune, len(identLowerPairs))
	for _, p := range identLowerPairs {
		m[p[0]] = p[1]
	}
	return m
}()

// identLower lowers every character through utf8mb3_general_ci's case
// table: ASCII letters directly, other BMP characters through
// identLowerPairs. Anything else — characters without a mapping,
// supplementary characters (MySQL identifiers are utf8mb3), invalid
// UTF-8 bytes — is kept as is.
func identLower(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteByte(byte(r) + 'a' - 'A')
		case r == utf8.RuneError && size <= 1:
			b.WriteByte(s[i])
		default:
			if l, ok := identLowerMap[r]; ok {
				b.WriteRune(l)
			} else {
				b.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	return b.String()
}

// IdentLowerPairs returns the committed (from, to) lower-case mappings
// of the server's identifier collation, for the devdb test that
// re-derives them from a live server.
func IdentLowerPairs() [][2]rune { return append([][2]rune(nil), identLowerPairs[:]...) }
