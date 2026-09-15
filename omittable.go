// Package sqletch is the user-facing companion of sqletch-generated
// code: types application code writes at call sites. The composition
// machinery generated code runs on lives in the runtime subpackage.
package sqletch

// Omittable is the value of an @if-present (presence guard) parameter.
// The zero value is omitted: every fragment the parameter guards is left
// out of the composed SQL, so an INSERT column falls back to its
// DEFAULT and an UPDATE SET item leaves the column untouched. Present
// provides a value.
//
// Omission is a query-shape concept, distinct from SQL NULL, which
// generated code spells optional.Option[T] (design 20). A guarded
// parameter that writes a nullable column is therefore
// Omittable[optional.Option[T]]: omitted, Present(None) for NULL, or
// Present(Some(v)).
type Omittable[T any] struct {
	v  T
	ok bool
}

// Present returns a provided Omittable holding v. Presence is never
// inferred from the value: Present of a zero value is still present.
func Present[T any](v T) Omittable[T] { return Omittable[T]{v: v, ok: true} }

// IsPresent reports whether a value was provided.
func (o Omittable[T]) IsPresent() bool { return o.ok }

// Get returns the value and whether it was provided.
func (o Omittable[T]) Get() (T, bool) { return o.v, o.ok }

// OrZero returns the value, or T's zero value when omitted.
func (o Omittable[T]) OrZero() T { return o.v }

// Ptr returns a pointer to a copy of the value, or nil when omitted.
func (o Omittable[T]) Ptr() *T {
	if !o.ok {
		return nil
	}
	v := o.v
	return &v
}
