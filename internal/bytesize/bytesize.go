package bytesize

import "fmt"

// Label renders a byte count in binary units, choosing the largest unit
// that divides it exactly so the label stays truthful. A cap of 20 MiB
// prints "20 MiB" and one of 512 KiB prints "512 KiB"; a count no unit
// divides prints as bytes rather than rounding into a figure the check
// does not enforce.
//
// Zero and a negative count print as bytes as well, "0 bytes" say, which
// reads as nothing allowed. A caller whose zero or negative limit means
// no limit at all says so itself rather than printing it.
func Label(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
