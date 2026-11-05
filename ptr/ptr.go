package ptr

// Value returns the value p points to, or the zero value of T when p is
// nil.
func Value[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
