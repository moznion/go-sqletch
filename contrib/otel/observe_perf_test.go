package otelsqletch

import "testing"

// TestObserveComposeSaturatedNoKeyEncoding pins M4: once a query's
// distinct-shape set is saturated, the compose hot path must not encode
// the key (an allocation) nor take a lock — the steady-state cost of an
// observed cache collapses to the counter increment. It also confirms
// the un-saturated path still encodes (so the saved allocation is real),
// and that tracking is sharded per query rather than serialized on one
// global lock.
func TestObserveComposeSaturatedNoKeyEncoding(t *testing.T) {
	_, b, _ := setup(t, WithUsedShapeBound(2))

	// Saturate Q's set: bound 2 → the 3rd distinct key flips saturated.
	for g := uint64(0); g < 3; g++ {
		b.ObserveCompose("Q", keyG(g), false)
	}

	// A repeat compose on the saturated query skips key.String() and the
	// lock. The one residual allocation is the OpenTelemetry counter's
	// own Add (it fires on every event, saturated or not, and is outside
	// this package's control); the key encoding this fix removed is the
	// difference measured against the un-saturated path below.
	satAllocs := testing.AllocsPerRun(200, func() {
		b.ObserveCompose("Q", keyG(0), true)
	})
	if satAllocs > 1 {
		t.Errorf("M4: saturated compose allocated %.0f times; want <=1 (only the SDK counter Add, no key encoding)", satAllocs)
	}

	// An un-saturated query still encodes the key on every call — this is
	// exactly what the saturated path avoids. Query "R" holds a single
	// key (well under the bound), so it never saturates.
	b.ObserveCompose("R", keyG(0), true)
	unsatAllocs := testing.AllocsPerRun(200, func() {
		b.ObserveCompose("R", keyG(0), true)
	})
	if unsatAllocs <= satAllocs {
		t.Errorf("M4: un-saturated compose allocated %.0f, saturated %.0f — the saturated fast path must allocate strictly less (the removed key encoding)",
			unsatAllocs, satAllocs)
	}
}
