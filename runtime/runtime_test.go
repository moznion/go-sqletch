package runtime

import (
	"errors"
	"strings"
	"testing"
)

// A hand-built fragment table mirroring a small template:
//
//	SELECT id FROM t
//	[guard 0] AND (t.a = :a)
//	[guard 1] AND (t.b = :b AND t.c = :a)   -- :a shared across frags
//	[choose]  ORDER BY ... / default
//	LIMIT :limit
func testFrags() []Frag {
	return []Frag{
		{Kind: Skel, Text: "SELECT id FROM t WHERE TRUE"},
		{Kind: Guarded, GuardMask: 1 << 0, Sep: SepAnd,
			Text: "t.a = :a", ParamSpans: []Span{{6, 8}}, ParamIdx: []int16{0}},
		{Kind: Guarded, GuardMask: 1 << 1, Sep: SepAnd,
			Text: "t.b = :b AND t.c = :a", ParamSpans: []Span{{6, 8}, {19, 21}}, ParamIdx: []int16{1, 0}},
		{Kind: Choose, Cases: []Case{
			{Text: "ORDER BY t.x"},
			{Text: "ORDER BY t.y"},
			{Text: "ORDER BY t.id"}, // default
		}},
		{Kind: Skel, Text: "\nLIMIT :limit", ParamSpans: []Span{{7, 13}}, ParamIdx: []int16{2}},
	}
}

func TestCompose_Shapes(t *testing.T) {
	frags := testFrags()

	sql, argIdx := Compose(frags, ShapeKey{Guards: 0, Choices: []uint8{2}})
	if strings.Contains(sql, "t.a") || strings.Contains(sql, "t.b") {
		t.Errorf("inactive fragments leaked:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY t.id") || !strings.Contains(sql, "LIMIT $1") {
		t.Errorf("minimal shape:\n%s", sql)
	}
	if len(argIdx) != 1 || argIdx[0] != 2 {
		t.Errorf("argIdx = %v, want [2]", argIdx)
	}

	sql, argIdx = Compose(frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	for _, want := range []string{"AND (t.a = $1)", "AND (t.b = $2 AND t.c = $1)", "ORDER BY t.x", "LIMIT $3"} {
		if !strings.Contains(sql, want) {
			t.Errorf("maximal shape missing %q:\n%s", want, sql)
		}
	}
	if len(argIdx) != 3 || argIdx[0] != 0 || argIdx[1] != 1 || argIdx[2] != 2 {
		t.Errorf("argIdx = %v, want [0 1 2]", argIdx)
	}

	// Guard 1 only: :b gets $1 (first occurrence), shared :a gets $2.
	sql, argIdx = Compose(frags, ShapeKey{Guards: 0b10, Choices: []uint8{1}})
	if !strings.Contains(sql, "AND (t.b = $1 AND t.c = $2)") || !strings.Contains(sql, "LIMIT $3") {
		t.Errorf("renumbering per shape:\n%s", sql)
	}
	if len(argIdx) != 3 || argIdx[0] != 1 || argIdx[1] != 0 {
		t.Errorf("argIdx = %v, want [1 0 2]", argIdx)
	}
}

func TestComposeStyle_Question(t *testing.T) {
	frags := testFrags()

	// Question style: one '?' per occurrence; the shared :a binds twice.
	sql, argIdx := ComposeStyle(StyleQuestion, frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	if strings.Contains(sql, "$") {
		t.Errorf("dollar placeholder leaked into question style:\n%s", sql)
	}
	for _, want := range []string{"AND (t.a = ?)", "AND (t.b = ? AND t.c = ?)", "ORDER BY t.x", "LIMIT ?"} {
		if !strings.Contains(sql, want) {
			t.Errorf("question-style shape missing %q:\n%s", want, sql)
		}
	}
	want := []int16{0, 1, 0, 2}
	if len(argIdx) != len(want) {
		t.Fatalf("argIdx = %v, want %v", argIdx, want)
	}
	for i := range want {
		if argIdx[i] != want[i] {
			t.Fatalf("argIdx = %v, want %v", argIdx, want)
		}
	}

	// The cache path composes with the same style.
	c := NewComposedCache(8)
	cSQL, cIdx := c.GetStyle(StyleQuestion, "Q", frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	if cSQL != sql || len(cIdx) != len(argIdx) {
		t.Errorf("cache composed differently:\n%s\nvs\n%s", cSQL, sql)
	}
	// Warm hit returns the identical plan.
	cSQL2, _ := c.GetStyle(StyleQuestion, "Q", frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	if cSQL2 != cSQL {
		t.Error("cache hit differs from miss")
	}
}

func TestCompose_Deterministic(t *testing.T) {
	frags := testFrags()
	k := ShapeKey{Guards: 1, Choices: []uint8{1}}
	a, _ := Compose(frags, k)
	b, _ := Compose(frags, k)
	if a != b {
		t.Fatal("composition must be byte-deterministic")
	}
}

func TestChooseOrdinal(t *testing.T) {
	tests := []struct {
		v, numNamed int
		hasDefault  bool
		want        uint8
		err         bool
	}{
		{0, 3, true, 3, false},  // zero selects the default (last ordinal)
		{1, 3, true, 0, false},  // first named case
		{3, 3, true, 2, false},  // last named case
		{4, 3, true, 0, true},   // out of range
		{0, 3, false, 0, true},  // required: zero value is an error
		{2, 3, false, 1, false}, // required: valid
	}
	for _, tt := range tests {
		got, err := ChooseOrdinal(tt.v, tt.numNamed, tt.hasDefault)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("ChooseOrdinal(%d,%d,%v) = (%d,%v), want (%d,err=%v)",
				tt.v, tt.numNamed, tt.hasDefault, got, err, tt.want, tt.err)
		}
	}
	if _, err := ChooseOrdinal(0, 2, false); !errors.Is(err, ErrChooseRequired) {
		t.Errorf("zero without default must be ErrChooseRequired, got %v", err)
	}
}

func TestBuildArgs(t *testing.T) {
	v := "x"
	vals := []any{&v, int64(7), nil}
	args := BuildArgs([]int16{1, 0}, vals)
	if len(args) != 2 || args[0] != int64(7) || args[1] != &v {
		t.Errorf("args = %+v", args)
	}
	if BuildArgs(nil, vals) != nil {
		t.Error("empty argIdx must yield nil args")
	}
}

func TestComposedCache_HitAndEvict(t *testing.T) {
	frags := testFrags()
	c := NewComposedCache(2)

	k1 := ShapeKey{Guards: 1, Choices: []uint8{0}}
	s1, _ := c.Get("Q", frags, k1)
	s1b, _ := c.Get("Q", frags, k1)
	if s1 != s1b {
		t.Fatal("cache hit must return identical SQL")
	}

	// Fill beyond capacity; the oldest entry is evicted but results
	// stay correct.
	c.Get("Q", frags, ShapeKey{Guards: 2, Choices: []uint8{0}})
	c.Get("Q", frags, ShapeKey{Guards: 3, Choices: []uint8{0}})
	s1c, _ := c.Get("Q", frags, k1) // recomputed after eviction
	if s1c != s1 {
		t.Fatal("recomputed SQL must equal the original")
	}
}

func TestShapeKeyString_Canonical(t *testing.T) {
	k := ShapeKey{Guards: 0x2a, Choices: []uint8{1, 0}}
	if got := k.String(); got != "g=2a;c=1,0" {
		t.Errorf("String() = %q", got)
	}
	if got := (ShapeKey{Guards: 5}).String(); got != "g=5" {
		t.Errorf("String() = %q", got)
	}
}
