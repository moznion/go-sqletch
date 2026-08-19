package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// noopObserver is a minimal Observer for install/race testing.
type noopObserver struct{}

func (noopObserver) ObserveCompose(string, ShapeKey, bool) {}
func (noopObserver) ObserveExec(context.Context, string, string, time.Duration, int64, error) {
}
func (noopObserver) ObserveReject(context.Context, string, error) {}

// TestSetObserver_RaceWithReads installs and clears an observer while
// concurrent hot-path reads run. Before obs became an atomic.Pointer,
// the two-word interface write raced the lock-free reads (`go test
// -race` flagged it); with the atomic install this is clean and a
// reader only ever sees a whole observer or none.
func TestSetObserver_RaceWithReads(t *testing.T) {
	c := NewComposedCache(64)
	frags := []Frag{{Kind: Skel, Text: "SELECT 1"}}
	key := ShapeKey{}
	c.Get("Q", frags, key) // prime the entry so reads take the hit path

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers flip the observer on and off.
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c.SetObserver(noopObserver{})
					c.SetObserver(nil)
				}
			}
		}()
	}
	// Readers hammer the lock-free hit path (which loads c.obs).
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20000; n++ {
				c.Get("Q", frags, key)
			}
		}()
	}
	// Bound the writers by the readers finishing.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()
}

// inListOverArity is a hand-built fragment table with a single @in
// fragment whose key arity exceeds MaxInArity — the one failure mode
// ComposeTreeStyle has even with an empty tree.
func inListOverArity() ([]Frag, ShapeKey) {
	return []Frag{{Kind: InList, ParamIdx: []int16{0}}},
		ShapeKey{Arities: []int32{MaxInArity + 1}}
}

// TestGetBindsStyle_InListOverArity_ReturnsError pins the error path:
// the arity limit is a returned error, never a panic, on the API that
// generated question-style @in code actually uses.
func TestGetBindsStyle_InListOverArity_ReturnsError(t *testing.T) {
	c := NewComposedCache(8)
	frags, key := inListOverArity()
	_, _, err := c.GetBindsStyle(StyleQuestion, "Q", frags, key)
	if !errors.Is(err, ErrShapeKeyLimit) {
		t.Fatalf("want ErrShapeKeyLimit, got %v", err)
	}
}

// TestComposeStyle_InListOverArity_Panics documents that the value-only
// APIs (Compose/ComposeStyle and the cache's Get/GetStyle) still panic
// on this input — they cannot return an error — but with an actionable,
// wrapped message rather than the old "no failure modes without a tree"
// falsehood. Generated code never reaches this path.
func TestComposeStyle_InListOverArity_Panics(t *testing.T) {
	frags, key := inListOverArity()

	assertPanicsWithLimit := func(name string, fn func()) {
		t.Helper()
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("%s: expected panic, got none", name)
			}
			err, ok := r.(error)
			if !ok || !errors.Is(err, ErrShapeKeyLimit) {
				t.Fatalf("%s: panic value %#v is not an ErrShapeKeyLimit", name, r)
			}
		}()
		fn()
	}

	assertPanicsWithLimit("ComposeStyle", func() {
		ComposeStyle(StyleQuestion, frags, key)
	})
	c := NewComposedCache(8)
	assertPanicsWithLimit("GetStyle", func() {
		c.GetStyle(StyleQuestion, "Q", frags, key)
	})
}
