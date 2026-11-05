package ptr

// Tests for Value.

import "testing"

// pair is a struct field type, which Value reads as it reads a number.
type pair struct{ A, B int }

// Value reads through a pointer and reads nil as the zero value, for a
// number, a string and a struct alike.
func TestValue(t *testing.T) {
	if got := Value(new(7)); got != 7 {
		t.Errorf("Value(new(7)) = %d, want 7", got)
	}
	if got := Value[int](nil); got != 0 {
		t.Errorf("Value of a nil *int = %d, want 0", got)
	}
	if got := Value(new("a")); got != "a" {
		t.Errorf(`Value(new("a")) = %q, want "a"`, got)
	}
	if got := Value[string](nil); got != "" {
		t.Errorf(`Value of a nil *string = %q, want ""`, got)
	}
	if got := Value(&pair{1, 2}); got != (pair{1, 2}) {
		t.Errorf("Value(&pair{1, 2}) = %+v", got)
	}
	if got := Value[pair](nil); got != (pair{}) {
		t.Errorf("Value of a nil *pair = %+v, want the zero pair", got)
	}
}
