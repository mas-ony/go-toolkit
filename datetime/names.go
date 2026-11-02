package datetime

import "time"

// monthNames are the Indonesian month names, January first.
var monthNames = [...]string{
	"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli",
	"Agustus", "September", "Oktober", "November", "Desember",
}

// dayNames are the Indonesian day names, Sunday first, as time.Weekday
// counts them.
var dayNames = [...]string{
	"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu",
}

// MonthName returns the Indonesian name of m, "Januari" to "Desember":
// the name a date carries when an Indonesian document writes it out, as
// in "2 Januari 2026". It returns "" for a value outside January to
// December.
func MonthName(m time.Month) string {
	if m < time.January || m > time.December {
		return ""
	}
	return monthNames[m-1]
}

// DayName returns the Indonesian name of d, "Minggu" to "Sabtu", as in
// "Senin, 2 Februari 2026". It returns "" for a value outside Sunday to
// Saturday.
func DayName(d time.Weekday) string {
	if d < time.Sunday || d > time.Saturday {
		return ""
	}
	return dayNames[d]
}
