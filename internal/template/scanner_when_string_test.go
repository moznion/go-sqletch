package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// whenStringSrc wraps a bare @when string literal in a minimal query.
func whenStringSrc(literal string) string {
	return "-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n" +
		"@when(kind = " + literal + ")\n  AND t.flag\n@end\n;\n"
}

// scanProfile scans src with an explicit dialect lexer profile, so the
// non-plain string forms (which only some dialects lex as KindString)
// can be exercised directly.
func scanProfile(t *testing.T, prof dialect.LexerProfile, src string) []diagnostics.Diagnostic {
	t.Helper()
	_, diags := NewScanner(prof).ScanFile("test.sql", []byte(src))
	return diags
}

// TestScan_When_StringLiteral_Rejected asserts that every non-plain
// KindString @when literal is refused with SQLETCH015 plus a
// compliant-rewrite hint. Without the fix, the delimiters/escapes
// survive into the stored guard value and the guarded fragment is
// permanently dead with no diagnostic.
func TestScan_When_StringLiteral_Rejected(t *testing.T) {
	cases := []struct {
		name    string
		prof    dialect.LexerProfile
		literal string
	}{
		{"pg_estring", postgres.Profile{}, "E'abc'"},
		{"pg_dollar", postgres.Profile{}, "$$abc$$"},
		{"pg_dollar_tag", postgres.Profile{}, "$t$abc$t$"},
		{"mysql_double_quoted", mysql.Profile{}, `"abc"`},
		{"mysql_backslash_escape", mysql.Profile{}, `'a\'b'`},
		{"sqlite_blob", sqlite.Profile{}, "x'6162'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := scanProfile(t, tc.prof, whenStringSrc(tc.literal))
			if !hasCode(diags, diagnostics.CodeWhenStringLiteral) {
				t.Fatalf("want %s for @when literal %s, got %+v",
					diagnostics.CodeWhenStringLiteral, tc.literal, diags)
			}
			var withHint bool
			for _, d := range diags {
				if d.Code == diagnostics.CodeWhenStringLiteral && strings.TrimSpace(d.Hint) != "" {
					withHint = true
				}
			}
			if !withHint {
				t.Errorf("%s diagnostic must carry a compliant-rewrite hint, got %+v",
					diagnostics.CodeWhenStringLiteral, diags)
			}
		})
	}
}

// TestScan_When_StringLiteral_Accepted pins that a plain single-quoted
// literal still decodes to its Go value (delimiters stripped, a doubled
// single-quote collapsed) and produces no diagnostic.
func TestScan_When_StringLiteral_Accepted(t *testing.T) {
	cases := []struct {
		name    string
		literal string
		want    string
	}{
		{"plain", "'abc'", "abc"},
		{"empty", "''", ""},
		{"doubled_quote", "'it''s'", "it's"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := scanClean(t, whenStringSrc(tc.literal))
			g := f.Queries[0].GuardAtoms[0]
			if g.Kind != ValueString || g.Value != tc.want || g.RawValue != tc.literal {
				t.Errorf("atom = %+v, want ValueString Value=%q RawValue=%q",
					g, tc.want, tc.literal)
			}
		})
	}
}

// TestScan_When_NonString_Unaffected guards against the string-literal
// rejection leaking into the integer/boolean @when paths.
func TestScan_When_NonString_Unaffected(t *testing.T) {
	for _, src := range []string{
		"-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n@when(a = 42)\n  AND t.a\n@end\n;\n",
		"-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n@when(a = true)\n  AND t.a\n@end\n;\n",
		"-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n@when(a != false)\n  AND t.a\n@end\n;\n",
	} {
		diags := scanProfile(t, postgres.Profile{}, src)
		if hasCode(diags, diagnostics.CodeWhenStringLiteral) {
			t.Errorf("%s must not fire on a non-string @when: %s\ndiags=%+v",
				diagnostics.CodeWhenStringLiteral, src, diags)
		}
	}
}

// TestIsPlainSQLString unit-tests the plain-string predicate directly.
func TestIsPlainSQLString(t *testing.T) {
	plain := []string{"''", "'a'", "'abc'", "'it''s'", "'a''b''c'"}
	for _, s := range plain {
		if !isPlainSQLString(s) {
			t.Errorf("isPlainSQLString(%q) = false, want true", s)
		}
	}
	// Backslash is dialect-dependent (MySQL escape vs. standard-SQL
	// literal) and unquoteSQLString does not honor it, so any backslash
	// is non-plain — even a standard-SQL literal one like 'a\b'.
	nonPlain := []string{
		"", "'", "abc", "E'abc'", "$$abc$$", "$t$abc$t$",
		`"abc"`, `'a\'b'`, `'a\b'`, "x'6162'", "'unterminated", "trailing'",
	}
	for _, s := range nonPlain {
		if isPlainSQLString(s) {
			t.Errorf("isPlainSQLString(%q) = true, want false", s)
		}
	}
}
