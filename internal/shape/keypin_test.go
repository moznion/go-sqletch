package shape

import (
	"testing"

	"github.com/moznion/go-sqletch/runtime"
)

// The compiler-side Key and the runtime ShapeKey must agree byte-for-
// byte on every dimension the compiler enumerates (guards, choices,
// order sequences, @in arities). The runtime key additionally carries a
// `;t=` segment for @filter-tree encodings — a runtime-only dimension:
// verification quotients the tree space to the maximal and empty
// renderings and never enumerates it, so the compiler Key has no tree
// field. This pin keeps the two encoders from drifting on the shared
// dimensions.
func TestKeyString_MatchesRuntimeShapeKey(t *testing.T) {
	cases := []struct {
		name    string
		compile Key
		run     runtime.ShapeKey
	}{
		{
			name:    "zero",
			compile: Key{},
			run:     runtime.ShapeKey{},
		},
		{
			name:    "guards only",
			compile: Key{Guards: 0xbeef},
			run:     runtime.ShapeKey{Guards: 0xbeef},
		},
		{
			name:    "all shared dimensions",
			compile: Key{Guards: 5, Choices: []uint8{2, 0}, Orders: [][]uint8{{3, 0}, {}}, Ins: []uint8{1, 0}},
			run: runtime.ShapeKey{Guards: 5, Choices: []uint8{2, 0},
				Orders: [][]uint8{{3, 0}, {}}, Arities: []int32{1, 0}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := tc.compile.String(), tc.run.String(); got != want {
				t.Fatalf("compiler key %q != runtime key %q", got, want)
			}
		})
	}
}
