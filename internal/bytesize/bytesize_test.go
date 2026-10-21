package bytesize

import (
	"math"
	"testing"
)

// The label only uses a larger unit when the number divides into it
// exactly. Rounding would print a figure the check does not enforce,
// which is the drift these labels exist to stop.
//
// The counts past what 32 bits hold are xlsx's: MaxUnzippedBytes is an
// int64, and a cap of several gibibytes has to be named as one.
func TestLabelStaysTruthful(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		n    int64
		want string
	}{
		{20 << 20, "20 MiB"},
		{512 << 10, "512 KiB"},
		{1 << 30, "1 GiB"},
		{1024, "1 KiB"},
		{1023, "1023 bytes"},
		{(20 << 20) + 1, "20971521 bytes"},
		{1536 << 20, "1536 MiB"},
		{16 << 30, "16 GiB"},
		{math.MaxInt64, "9223372036854775807 bytes"},
		{0, "0 bytes"},
		{-1, "-1 bytes"},
	} {
		if got := Label(c.n); got != c.want {
			t.Errorf("Label(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
