package datetime

import (
	"testing"
	"time"
)

// MonthName and DayName name every month and day in Indonesian, and
// nothing outside them.
func TestNames(t *testing.T) {
	d := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	if got := DayName(d.Weekday()) + ", 2 " + MonthName(d.Month()) +
		" 2026"; got != "Jumat, 2 Januari 2026" {
		t.Errorf("got %q", got)
	}
	months := map[time.Month]string{
		time.February: "Februari", time.May: "Mei",
		time.August: "Agustus", time.December: "Desember",
	}
	for m, want := range months {
		if got := MonthName(m); got != want {
			t.Errorf("MonthName(%v) = %q, want %q", m, got, want)
		}
	}
	if DayName(time.Sunday) != "Minggu" || DayName(time.Saturday) != "Sabtu" {
		t.Error("the week does not run Minggu to Sabtu")
	}
	if MonthName(0) != "" || MonthName(13) != "" || DayName(-1) != "" ||
		DayName(7) != "" {
		t.Error("a value out of range was named")
	}
}
