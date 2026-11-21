package dialect_test

import (
	"slices"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

func TestASCIITrimSpaceAndFields(t *testing.T) {
	for in, want := range map[string]string{
		" \t\n\v\f\rint\r\n": "int", "\u00a0int\u0085": "\u00a0int\u0085", "": "", "a b": "a b",
	} {
		if got := dialect.ASCIITrimSpace(in); got != want {
			t.Errorf("ASCIITrimSpace(%q) = %q, want %q", in, got, want)
		}
	}
	if got := dialect.ASCIIFields(" a\tb\u00a0c\n"); !slices.Equal(got, []string{"a", "b\u00a0c"}) {
		t.Errorf("ASCIIFields = %q", got)
	}
}

func TestASCIIEqualFold(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"localhost", "LOCALHOST", true}, {"localhoſt", "localhost", false},
		{"Key", "key", false}, {"Content-Length", "content-length", true}, {"a", "ab", false},
	} {
		if got := dialect.ASCIIEqualFold(tc.a, tc.b); got != tc.want {
			t.Errorf("ASCIIEqualFold(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
