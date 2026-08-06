package runtime

import (
	"fmt"
	"sync"
	"testing"
)

// TestCacheChurnConcurrent maximizes eviction and snapshot republish
// churn while many goroutines read the lock-free path. A published
// snapshot mutated after its atomic Store would surface here as a fatal
// "concurrent map read and map write" — which no recover can catch — so
// this runs the capacities where republishing is most frequent.
//
// It also mixes placeholder styles under one query name, which is how
// the style came to lead the cache key: without it the two styles'
// entries collided and a caller could be handed the other style's SQL.
func TestCacheChurnConcurrent(t *testing.T) {
	frags := benchFrags()
	treeFrags := benchTreeFrags()

	for _, capacity := range []int{1, 2, 8, 64} {
		t.Run(fmt.Sprintf("cap=%d", capacity), func(t *testing.T) {
			c := NewComposedCache(capacity)

			// Shape space far larger than the capacity, so essentially
			// every call evicts and the snapshot is constantly stale.
			const shapes = 400
			wantDollar := make([]string, shapes)
			wantQuestion := make([]string, shapes)
			for i := range shapes {
				k := ShapeKey{Guards: uint64(i % 16), Choices: []uint8{uint8(i % 4)}, Trees: []string{fmt.Sprintf("p%d", i)}}
				wantDollar[i], _ = ComposeStyle(StyleDollar, frags, k)
				wantQuestion[i], _ = ComposeStyle(StyleQuestion, frags, k)
			}
			tree := benchTree()
			wantTree, _, err := ComposeTree(treeFrags, ShapeKey{Trees: []string{tree.Encode()}}, tree, DefaultTreeCaps)
			if err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			for w := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for n := range 2000 {
						i := (n*7 + w*13) % shapes
						k := ShapeKey{Guards: uint64(i % 16), Choices: []uint8{uint8(i % 4)}, Trees: []string{fmt.Sprintf("p%d", i)}}
						switch w % 3 {
						case 0:
							sql, argIdx := c.Get("Q", frags, k)
							if sql != wantDollar[i] {
								t.Errorf("dollar shape %d mismatched", i)
								return
							}
							_ = BuildArgs(argIdx, []any{nil, nil, nil, nil, int64(1)})
						case 1:
							sql, binds, err := c.GetBindsStyle(StyleQuestion, "Q", frags, k)
							if err != nil {
								t.Error(err)
								return
							}
							if sql != wantQuestion[i] {
								t.Errorf("question shape %d mismatched", i)
								return
							}
							_ = ResolveArgs(binds, []any{nil, nil, nil, nil, int64(1)}, nil)
						default:
							// Tree path shares the same cache, so tree
							// and non-tree entries evict each other.
							sql, binds, err := c.GetTree("T", treeFrags, ShapeKey{}, tree, DefaultTreeCaps)
							if err != nil {
								t.Error(err)
								return
							}
							if sql != wantTree {
								t.Errorf("tree shape mismatched:\n%s", sql)
								return
							}
							_ = ResolveArgs(binds, []any{int64(1)}, TreeArgs(tree))
						}
					}
				}()
			}
			wg.Wait()

			c.mu.Lock()
			n, l := len(c.m), c.order.Len()
			c.mu.Unlock()
			if n > capacity || n != l {
				t.Fatalf("cap=%d: %d entries, list %d", capacity, n, l)
			}
		})
	}
}

// TestComposeParallel hammers the pooled render scratch from many
// goroutines, including the caps-violation path that also returns the
// buffer: a double-Put or a use-after-Put would corrupt output here.
func TestComposeParallel(t *testing.T) {
	frags := benchFrags()
	treeFrags := benchTreeFrags()
	tree := benchTree()

	key := ShapeKey{Guards: 0xf, Choices: []uint8{0}}
	want, _ := ComposeStyle(StyleDollar, frags, key)
	wantTree, _, err := ComposeTree(treeFrags, ShapeKey{}, tree, DefaultTreeCaps)
	if err != nil {
		t.Fatal(err)
	}
	// A tree that violates the caps, so the error return path — which
	// also hands the buffer back to the pool — is exercised under load.
	deep := NewLeaf(0, "x")
	for range 40 {
		deep = And(deep, NewLeaf(0, "x"))
	}

	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 3000 {
				switch (n + w) % 3 {
				case 0:
					if got, _ := ComposeStyle(StyleDollar, frags, key); got != want {
						t.Errorf("composed SQL corrupted:\n%s", got)
						return
					}
				case 1:
					got, _, err := ComposeTree(treeFrags, ShapeKey{}, tree, DefaultTreeCaps)
					if err != nil || got != wantTree {
						t.Errorf("tree SQL corrupted: %v\n%s", err, got)
						return
					}
				default:
					if _, _, err := ComposeTree(treeFrags, ShapeKey{}, deep, TreeCaps{MaxNodes: 4, MaxDepth: 2}); err == nil {
						t.Error("expected a caps violation")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
