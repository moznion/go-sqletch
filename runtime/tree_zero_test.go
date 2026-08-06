package runtime

import "testing"

// Tree is a value type so that `nil` — the shape a forgotten scope
// takes — is not a Tree at all. This is a compile-time property, so the
// test that matters is the one that does not compile:
//
//	var _ Tree = nil
//	→ cannot use nil as Tree value in variable declaration
//
// What is left to check at runtime is that the one zero which does
// compile, Tree{}, is distinguishable from every constructed tree, and
// that it is not mistaken for Unscoped(): "the caller did not decide"
// and "the caller decided not to scope" must not collapse into each
// other, or a forgotten scope would silently read everything.
func TestZeroTreeIsDistinctFromUnscoped(t *testing.T) {
	if !(Tree{}).IsZero() {
		t.Error("the zero Tree does not report IsZero")
	}
	if Unscoped().IsZero() {
		t.Error("Unscoped() reports IsZero; a deliberate opt-out would be refused")
	}
	if NewLeaf(0, int64(1)).IsZero() {
		t.Error("a constructed leaf reports IsZero")
	}
	if And(NewLeaf(0, int64(1)), NewLeaf(1, "x")).IsZero() {
		t.Error("a combined tree reports IsZero")
	}

	// Both render TRUE, which is why they cannot be told apart after
	// composition — the distinction has to survive in the value.
	if got, want := (Tree{}).Encode(), Unscoped().Encode(); got != want {
		t.Errorf("encodings differ: zero %q, Unscoped %q", got, want)
	}
}

// And/Or drop zero children rather than propagating them: composing a
// scope with something the caller left unset must not quietly discard
// the scope.
func TestCombineDropsZeroChildren(t *testing.T) {
	leaf := NewLeaf(0, int64(1))

	if got := And(leaf, Tree{}).Encode(); got != leaf.Encode() {
		t.Errorf("And(leaf, zero) = %q, want %q", got, leaf.Encode())
	}
	if got := And(Tree{}, Tree{}).Encode(); got != Unscoped().Encode() {
		t.Errorf("And(zero, zero) = %q, want TRUE", got)
	}
	if And(Tree{}, Tree{}).IsZero() {
		t.Error("And of zeros produced a zero Tree; it must be an explicit TRUE")
	}
}
