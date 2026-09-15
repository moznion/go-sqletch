package sqletch_test

import (
	"testing"

	"github.com/moznion/go-optional"

	"github.com/moznion/go-sqletch"
)

// The zero value is the omitted state: a params struct literal that
// leaves a guarded field out must omit the guarded fragment(s).
func TestOmittable_ZeroValueIsOmitted(t *testing.T) {
	var o sqletch.Omittable[string]
	if o.IsPresent() {
		t.Fatal("zero value must be omitted")
	}
	if v, ok := o.Get(); ok || v != "" {
		t.Fatalf("Get on omitted = (%q, %v), want (\"\", false)", v, ok)
	}
	if got := o.OrZero(); got != "" {
		t.Fatalf("OrZero on omitted = %q, want zero", got)
	}
	if p := o.Ptr(); p != nil {
		t.Fatalf("Ptr on omitted = %v, want nil", p)
	}
}

func TestOmittable_Present(t *testing.T) {
	o := sqletch.Present("neo")
	if !o.IsPresent() {
		t.Fatal("Present must be present")
	}
	if v, ok := o.Get(); !ok || v != "neo" {
		t.Fatalf("Get = (%q, %v), want (\"neo\", true)", v, ok)
	}
	if got := o.OrZero(); got != "neo" {
		t.Fatalf("OrZero = %q, want neo", got)
	}
	p := o.Ptr()
	if p == nil || *p != "neo" {
		t.Fatalf("Ptr = %v, want pointer to neo", p)
	}
}

// Present of a zero value is still present: presence is not inferred
// from the value (binding "" or 0 is a legitimate request).
func TestOmittable_PresentZeroValueIsPresent(t *testing.T) {
	o := sqletch.Present(0)
	if !o.IsPresent() {
		t.Fatal("Present(0) must be present")
	}
	if p := o.Ptr(); p == nil || *p != 0 {
		t.Fatalf("Ptr = %v, want pointer to 0", p)
	}
}

// Ptr hands out a copy: mutating through it must not change the
// Omittable (generated code binds the pointer; the caller's value must
// not be aliased by the driver).
func TestOmittable_PtrDoesNotAlias(t *testing.T) {
	o := sqletch.Present(1)
	*o.Ptr() = 2
	if got := o.OrZero(); got != 1 {
		t.Fatalf("Omittable mutated through Ptr: got %d, want 1", got)
	}
}

// The nested form is design 20's PATCH tri-state: omitted, present
// NULL, present value. Generated code binds OrZero().UnwrapAsPtr().
func TestOmittable_NestedOptionTriState(t *testing.T) {
	var omitted sqletch.Omittable[optional.Option[string]]
	if omitted.IsPresent() {
		t.Fatal("zero nested value must be omitted")
	}
	if p := omitted.OrZero().UnwrapAsPtr(); p != nil {
		t.Fatalf("omitted bind = %v, want nil", p)
	}

	null := sqletch.Present(optional.None[string]())
	if !null.IsPresent() {
		t.Fatal("Present(None) must be present")
	}
	if p := null.OrZero().UnwrapAsPtr(); p != nil {
		t.Fatalf("Present(None) bind = %v, want nil (SQL NULL)", p)
	}

	val := sqletch.Present(optional.Some("neo"))
	if p := val.OrZero().UnwrapAsPtr(); p == nil || *p != "neo" {
		t.Fatalf("Present(Some) bind = %v, want pointer to neo", p)
	}
}
