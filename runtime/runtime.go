// Package runtime is the small public package imported by
// sqletch-generated code. It composes verified constant fragments into
// SQL deterministically — the byte-for-byte mirror of the compiler's
// verification renderer (premise P2); a shared conformance test in the
// compiler pins the equality.
//
// Nothing here parses SQL or touches user data: composition is
// table-driven selection and concatenation, and user values travel
// exclusively through bind parameters.
package runtime

import (
	"container/list"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

type Kind uint8

const (
	Skel Kind = iota
	Guarded
	Choose
)

type Sep uint8

const (
	SepNone Sep = iota
	SepAnd
	SepComma
)

// Span marks a :name parameter token inside a fragment's text.
type Span struct{ Start, End int32 }

// Case is one selectable @choose body.
type Case struct {
	Text       string
	ParamSpans []Span
	ParamIdx   []int16
}

// Frag is one compile-time-constant fragment. Emitted by sqletch
// generate; never constructed by hand.
type Frag struct {
	Kind       Kind
	Text       string
	ParamSpans []Span  // :name token positions within Text
	ParamIdx   []int16 // flattened params-struct index per span
	GuardMask  uint64  // Guarded: all these bits must be set
	Sep        Sep
	Cases      []Case // Choose: named cases, then the default (if any)
}

// ShapeKey identifies one concrete query shape.
type ShapeKey struct {
	Guards  uint64
	Choices []uint8
}

// String is the canonical encoding (matches the compiler's shape.Key).
func (k ShapeKey) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "g=%x", k.Guards)
	if len(k.Choices) > 0 {
		b.WriteString(";c=")
		for i, c := range k.Choices {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(int(c)))
		}
	}
	return b.String()
}

// Compose walks the fragment table in source order and emits the SQL
// of the shape plus the bind order: argIdx[n] is the params-struct
// value index bound to placeholder $(n+1). Placeholders are numbered
// in first-occurrence order per shape.
func Compose(frags []Frag, key ShapeKey) (string, []int16) {
	var b strings.Builder
	assigned := map[int16]int{}
	var argIdx []int16

	emit := func(text string, spans []Span, idx []int16) {
		last := int32(0)
		for i, sp := range spans {
			b.WriteString(text[last:sp.Start])
			p := idx[i]
			n, ok := assigned[p]
			if !ok {
				n = len(argIdx) + 1
				assigned[p] = n
				argIdx = append(argIdx, p)
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			last = sp.End
		}
		b.WriteString(text[last:])
	}

	chooseSeen := 0
	for _, f := range frags {
		switch f.Kind {
		case Skel:
			emit(f.Text, f.ParamSpans, f.ParamIdx)
		case Guarded:
			if key.Guards&f.GuardMask != f.GuardMask {
				continue
			}
			b.WriteByte('\n')
			switch f.Sep {
			case SepAnd:
				b.WriteString("AND (")
			case SepComma:
				b.WriteString(", ")
			}
			emit(f.Text, f.ParamSpans, f.ParamIdx)
			if f.Sep == SepAnd {
				b.WriteByte(')')
			}
		case Choose:
			ord := 0
			if chooseSeen < len(key.Choices) {
				ord = int(key.Choices[chooseSeen])
			}
			chooseSeen++
			b.WriteByte('\n')
			if ord >= 0 && ord < len(f.Cases) {
				c := f.Cases[ord]
				emit(c.Text, c.ParamSpans, c.ParamIdx)
			}
		}
	}
	return b.String(), argIdx
}

// ErrChooseRequired is returned before any SQL is sent when a required
// @choose parameter carries its zero value.
var ErrChooseRequired = errors.New("sqletch: required @choose parameter has its zero value")

// ChooseOrdinal maps a generated enum value to the composer's case
// ordinal. With a @default, enum 0 selects the default (ordinal
// numNamed); named cases are 1..numNamed. Without a default, 0 is an
// error.
func ChooseOrdinal(v, numNamed int, hasDefault bool) (uint8, error) {
	switch {
	case v == 0 && hasDefault:
		return uint8(numNamed), nil
	case v >= 1 && v <= numNamed:
		return uint8(v - 1), nil
	case v == 0:
		return 0, ErrChooseRequired
	default:
		return 0, fmt.Errorf("sqletch: @choose enum value %d out of range", v)
	}
}

// BuildArgs selects the bind values for a shape from the flattened
// params-struct values. Pointer values pass through as-is (active
// guards guarantee non-nil; the driver encodes pointers natively).
func BuildArgs(argIdx []int16, vals []any) []any {
	if len(argIdx) == 0 {
		return nil
	}
	out := make([]any, len(argIdx))
	for i, idx := range argIdx {
		out[i] = vals[idx]
	}
	return out
}

// ComposedCache memoizes composed SQL per (query, shape), LRU-bounded.
// Hits compare the full key, never just its string form.
type ComposedCache struct {
	mu    sync.Mutex
	cap   int
	m     map[string]*list.Element
	order *list.List // front = most recent
}

type cacheEntry struct {
	mapKey string
	key    ShapeKey
	sql    string
	argIdx []int16
}

func NewComposedCache(capacity int) *ComposedCache {
	if capacity <= 0 {
		capacity = 256
	}
	return &ComposedCache{cap: capacity, m: map[string]*list.Element{}, order: list.New()}
}

func (c *ComposedCache) Get(queryName string, frags []Frag, key ShapeKey) (string, []int16) {
	mapKey := queryName + "|" + key.String()
	c.mu.Lock()
	if el, ok := c.m[mapKey]; ok {
		e := el.Value.(*cacheEntry)
		if e.key.Guards == key.Guards && choicesEqual(e.key.Choices, key.Choices) {
			c.order.MoveToFront(el)
			sql, argIdx := e.sql, e.argIdx
			c.mu.Unlock()
			return sql, argIdx
		}
		// Full-key mismatch (canonical-encoding collision would be a
		// bug, but never trust the string form): drop and recompute.
		c.order.Remove(el)
		delete(c.m, mapKey)
	}
	c.mu.Unlock()

	sql, argIdx := Compose(frags, key)

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[mapKey]; !ok {
		e := &cacheEntry{mapKey: mapKey, key: cloneKey(key), sql: sql, argIdx: argIdx}
		c.m[mapKey] = c.order.PushFront(e)
		for len(c.m) > c.cap {
			oldest := c.order.Back()
			c.order.Remove(oldest)
			delete(c.m, oldest.Value.(*cacheEntry).mapKey)
		}
	}
	return sql, argIdx
}

func cloneKey(k ShapeKey) ShapeKey {
	return ShapeKey{Guards: k.Guards, Choices: append([]uint8(nil), k.Choices...)}
}

func choicesEqual(a, b []uint8) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
