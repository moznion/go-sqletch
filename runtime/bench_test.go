package runtime

import (
	goruntime "runtime"
	"testing"
	"time"
)

// benchFrags mirrors examples/postgres/gen/search_users.sql.gen.go — the
// realistic shape of a generated fragment table: a skeleton, four
// presence-guarded conjuncts, a @choose ORDER BY, and a trailing LIMIT.
func benchFrags() []Frag {
	return []Frag{
		{Kind: Skel, Text: "\nSELECT\n    u.id,\n    u.email,\n    u.status,\n    u.created_at\nFROM users AS u\n\n"},
		{Kind: Guarded, GuardMask: 0x1, Sep: SepNone,
			Text:       "JOIN organization_users AS ou\n  ON ou.user_id = u.id\n AND ou.organization_id = :organization_id",
			ParamSpans: []Span{{Start: 79, End: 95}}, ParamIdx: []int16{0}},
		{Kind: Skel, Text: "\n\nWHERE TRUE\n\n"},
		{Kind: Guarded, GuardMask: 0x2, Sep: SepAnd,
			Text: "u.status = :status", ParamSpans: []Span{{Start: 11, End: 18}}, ParamIdx: []int16{1}},
		{Kind: Skel, Text: "\n\n"},
		{Kind: Guarded, GuardMask: 0x4, Sep: SepAnd,
			Text: "u.email LIKE :email_prefix || '%'", ParamSpans: []Span{{Start: 13, End: 26}}, ParamIdx: []int16{2}},
		{Kind: Skel, Text: "\n\n"},
		{Kind: Guarded, GuardMask: 0x8, Sep: SepAnd,
			Text: "u.created_at >= :created_after", ParamSpans: []Span{{Start: 16, End: 30}}, ParamIdx: []int16{3}},
		{Kind: Skel, Text: "\n\n"},
		{Kind: Choose, Cases: []Case{
			{Text: "ORDER BY u.created_at DESC"},
			{Text: "ORDER BY u.created_at ASC"},
			{Text: "ORDER BY u.email ASC, u.id ASC"},
			{Text: "ORDER BY u.id ASC"},
		}},
		{Kind: Skel, Text: "\n\nLIMIT :limit;\n\n", ParamSpans: []Span{{Start: 8, End: 14}}, ParamIdx: []int16{4}},
	}
}

// benchTreeFrags mirrors a @filter-tree query: skeleton + a predicate
// vocabulary spliced as one WHERE conjunct.
func benchTreeFrags() []Frag {
	return []Frag{
		{Kind: Skel, Text: "SELECT u.id, u.email FROM users AS u WHERE TRUE AND "},
		{Kind: FilterTree, Cases: []Case{
			{Text: "u.status = :status", ParamSpans: []Span{{Start: 11, End: 18}}, ParamIdx: []int16{0}},
			{Text: "u.org_id = :org_id", ParamSpans: []Span{{Start: 11, End: 18}}, ParamIdx: []int16{0}},
			{Text: "u.created_at >= :since", ParamSpans: []Span{{Start: 16, End: 22}}, ParamIdx: []int16{0}},
		}},
		{Kind: Skel, Text: "\nLIMIT :limit", ParamSpans: []Span{{Start: 7, End: 13}}, ParamIdx: []int16{0}},
	}
}

func benchTree() Tree {
	return And(
		NewLeaf(0, "active"),
		Or(NewLeaf(1, int64(7)), NewLeaf(1, int64(9))),
		NewLeaf(2, time.Unix(0, 0)),
	)
}

// benchKeys covers the range from a trivial key to one exercising every
// segment of the canonical encoding.
var benchKeys = []struct {
	name string
	key  ShapeKey
}{
	{"minimal", ShapeKey{Guards: 0x5, Choices: []uint8{3}}},
	{"orders", ShapeKey{Guards: 0xf, Choices: []uint8{1}, Orders: [][]uint8{{0, 3, 4}}}},
	{"full", ShapeKey{
		Guards:  0xdeadbeef,
		Choices: []uint8{1, 2},
		Orders:  [][]uint8{{0, 3}, nil, {}},
		Trees:   []string{"&(p0,|(p1,p1),p2)"},
		Arities: []int32{3, 0},
	}},
}

func BenchmarkShapeKeyString(b *testing.B) {
	for _, tc := range benchKeys {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sink = tc.key.String()
			}
		})
	}
}

var sink string

// BenchmarkComposeCold measures composition itself (no cache).
func BenchmarkComposeCold(b *testing.B) {
	frags := benchFrags()
	for _, tc := range []struct {
		name string
		key  ShapeKey
	}{
		{"no_guards", ShapeKey{Choices: []uint8{3}}},
		{"all_guards", ShapeKey{Guards: 0xf, Choices: []uint8{0}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sql, _ := Compose(frags, tc.key)
				sink = sql
			}
		})
	}
}

func BenchmarkComposeCold_Question(b *testing.B) {
	frags := benchFrags()
	key := ShapeKey{Guards: 0xf, Choices: []uint8{0}}
	b.ReportAllocs()
	for b.Loop() {
		sql, _ := ComposeStyle(StyleQuestion, frags, key)
		sink = sql
	}
}

func BenchmarkComposeTreeCold(b *testing.B) {
	frags := benchTreeFrags()
	tree := benchTree()
	key := ShapeKey{Trees: []string{tree.Encode()}}
	b.ReportAllocs()
	for b.Loop() {
		sql, _, err := ComposeTree(frags, key, tree, DefaultTreeCaps)
		if err != nil {
			b.Fatal(err)
		}
		sink = sql
	}
}

// BenchmarkGeneratedCall is the shipped hot path: exactly what a
// generated query method does per call before handing SQL to the driver
// — build the key, hit the composed cache, materialize args, and feed
// the OnQuery hook. This is the number that matters to consumers.
func BenchmarkGeneratedCall(b *testing.B) {
	frags := benchFrags()
	cache := NewComposedCache(256)
	orgID := int64(42)
	status := "active"
	limit := int64(100)

	// Generated code passes the key to the hook unencoded, so the
	// canonical encoding is only spelled out when a hook is installed.
	// Both cases are measured: no_hook is what an application without
	// observability pays, with_hook adds the encoding.
	for _, hooked := range []bool{false, true} {
		name := "no_hook"
		if hooked {
			name = "with_hook"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var key ShapeKey
				key.Guards |= 1 << 0
				key.Guards |= 1 << 1
				ord, err := ChooseOrdinal(1, 3, true)
				if err != nil {
					b.Fatal(err)
				}
				key.Choices = []uint8{ord}
				sqlText, argIdx := cache.Get("SearchUsers", frags, key)
				args := BuildArgs(argIdx, []any{&orgID, &status, nil, nil, limit})
				sink = sqlText
				sinkArgs = args
				if hooked {
					sinkKey = key.String()
				}
			}
		})
	}
}

var (
	sinkArgs []any
	sinkKey  string
)

// BenchmarkGeneratedCallTree is the @filter-tree hot path.
func BenchmarkGeneratedCallTree(b *testing.B) {
	frags := benchTreeFrags()
	cache := NewComposedCache(256)
	tree := benchTree()
	limit := int64(100)

	b.ReportAllocs()
	for b.Loop() {
		var key ShapeKey
		key.Trees = []string{tree.Encode()}
		sqlText, binds, err := cache.GetTree("FilterUsers", frags, key, tree, DefaultTreeCaps)
		if err != nil {
			b.Fatal(err)
		}
		args := ResolveArgs(binds, []any{limit}, TreeArgs(tree))
		sink = sqlText
		sinkArgs = args
		sinkKey = key.String()
	}
}

// BenchmarkGeneratedCallParallel is the hot path under concurrent load —
// a server serving the same query from many goroutines. It exposes
// contention on the composed cache, which every call goes through.
func BenchmarkGeneratedCallParallel(b *testing.B) {
	frags := benchFrags()
	cache := NewComposedCache(256)
	orgID := int64(42)
	status := "active"
	limit := int64(100)
	for g := uint64(0); g < 4; g++ {
		cache.Get("SearchUsers", frags, ShapeKey{Guards: g, Choices: []uint8{1}})
	}

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		// Sinks are per-goroutine: writing package-level ones from every
		// core would ping-pong their cache line and measure the
		// benchmark's own contention rather than the cache's.
		var localSQL string
		var localArgs []any
		var g uint64
		for pb.Next() {
			key := ShapeKey{Guards: g % 4, Choices: []uint8{1}}
			sqlText, argIdx := cache.Get("SearchUsers", frags, key)
			args := BuildArgs(argIdx, []any{&orgID, &status, nil, nil, limit})
			localSQL = sqlText
			localArgs = args
			g++
		}
		goruntime.KeepAlive(localSQL)
		goruntime.KeepAlive(localArgs)
	})
}

// BenchmarkCacheGet isolates the cache lookup from key construction and
// arg materialization.
func BenchmarkCacheGet(b *testing.B) {
	frags := benchFrags()
	cache := NewComposedCache(256)
	key := ShapeKey{Guards: 0x3, Choices: []uint8{1}}
	cache.Get("SearchUsers", frags, key) // warm

	b.ReportAllocs()
	for b.Loop() {
		sqlText, _ := cache.Get("SearchUsers", frags, key)
		sink = sqlText
	}
}

// BenchmarkCacheGetShapes rotates over distinct shapes so the LRU list
// is genuinely exercised (MoveToFront on every hit).
func BenchmarkCacheGetShapes(b *testing.B) {
	frags := benchFrags()
	cache := NewComposedCache(256)
	var keys []ShapeKey
	for g := uint64(0); g < 16; g++ {
		for c := uint8(0); c < 4; c++ {
			k := ShapeKey{Guards: g, Choices: []uint8{c}}
			cache.Get("SearchUsers", frags, k)
			keys = append(keys, k)
		}
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		sqlText, _ := cache.Get("SearchUsers", frags, keys[i%len(keys)])
		sink = sqlText
		i++
	}
}

func BenchmarkBuildArgs(b *testing.B) {
	argIdx := []int16{0, 1, 4}
	orgID := int64(42)
	status := "active"
	vals := []any{&orgID, &status, nil, nil, int64(100)}
	b.ReportAllocs()
	for b.Loop() {
		sinkArgs = BuildArgs(argIdx, vals)
	}
}

func BenchmarkResolveArgs(b *testing.B) {
	binds := []Bind{{Idx: 0}, {FromTree: true, Idx: 0}, {FromTree: true, Idx: 1}}
	vals := []any{int64(100)}
	treeArgs := []any{"active", int64(7)}
	b.ReportAllocs()
	for b.Loop() {
		sinkArgs = ResolveArgs(binds, vals, treeArgs)
	}
}

func BenchmarkResolveArgsElem(b *testing.B) {
	binds := []Bind{{Idx: 0}, {Idx: 1, Elem: 1}, {Idx: 1, Elem: 2}, {Idx: 1, Elem: 3}}
	vals := []any{int64(7), []string{"a", "b", "c"}}
	b.ReportAllocs()
	for b.Loop() {
		sinkArgs = ResolveArgs(binds, vals, nil)
	}
}

func BenchmarkTreeEncode(b *testing.B) {
	tree := benchTree()
	b.ReportAllocs()
	for b.Loop() {
		sink = tree.Encode()
	}
}

func BenchmarkTreeArgs(b *testing.B) {
	tree := benchTree()
	b.ReportAllocs()
	for b.Loop() {
		sinkArgs = TreeArgs(tree)
	}
}

func BenchmarkTreeValidate(b *testing.B) {
	tree := benchTree()
	b.ReportAllocs()
	for b.Loop() {
		if err := tree.validate(3, DefaultTreeCaps); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderSeq(b *testing.B) {
	vals := []int{0, 3, 4}
	b.ReportAllocs()
	for b.Loop() {
		seq, err := OrderSeq(vals, 4)
		if err != nil {
			b.Fatal(err)
		}
		sinkSeq = seq
	}
}

var sinkSeq []uint8
