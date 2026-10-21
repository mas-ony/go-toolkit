package xlsx

// Tests for value.go: the separator rules and scientific notation in
// Decimal, the strict notation of DecimalDotGrouped, both epochs and every
// text layout in Date along with the serials no DATE column can hold and
// the month names of both languages, the range guards in Int, the
// byte-measured cap in StrMax, the emptiness rules in Str and Trim, the
// escapes Unescape decodes, and a date that keeps its day through
// datetime's Value.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mas-ony/go-toolkit/internal/testutil"
)

// The separator rules in Decimal are the subtlest logic here and the easiest
// to get silently wrong, because both readings of "1.234" parse successfully
// and only one is right. These cases pin the intended behaviour.
//
// The want column is decimal.Decimal.String(), which TRIMS trailing zeros: a
// cell reading "1285.50" parses at scale 2 and prints as "1285.5". Nothing
// here turns on the scale, so String stays the comparison; a case that needed
// the scale itself would have to use StringFixed.
func TestDecimal(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means nil
		fail bool
	}{
		{"", "", false},         // empty cell → NULL
		{"1285", "1285", false}, // raw numeric, the normal case
		{"303", "303", false},
		{"25656", "25656", false},
		{"1285.50", "1285.5", false},    // invariant decimal point
		{"1285,50", "1285.5", false},    // decimal comma
		{"1.234.567", "1234567", false}, // dot grouping
		{"1,234,567", "1234567", false}, // invariant grouping
		{"1.234,56", "1234.56", false},  // both: comma last → decimal
		{"1,234.56", "1234.56", false},  // both: dot last → decimal
		{"1285 m2", "1285", false},      // stray unit stripped
		{" 1 285 ", "1285", false},      // space grouping stripped
		{".5", "0.5", false},            // below one, written bare
		{",75", "0.75", false},          // the same, decimal comma
		{"-.5", "-0.5", false},
		{"$.50", "0.5", false},
		{"Rp.5", "5", false},     // the dot of an abbreviation
		{"... 5", "5", false},    // the last of a row of dots
		{".234.567", "", true},   // a decimal point, then another
		{"1285,", "1285", false}, // a closing separator: a typo
		{"1285.5,", "1285.5", false},
		{"12,5.", "12.5", false},
		{"1.234,56.", "1234.56", false},
		{"1.234.567.", "1234567", false},
		{"Rp 5.000,-", "5000", false}, // closed by its decimal comma
		{"5,000.", "5000", false},     // and by its decimal point
		{"abc", "", true},
	}
	for _, c := range cases {
		got, err := Decimal(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("Decimal(%q): expected an error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Decimal(%q): unexpected error %v", c.in, err)
			continue
		}
		if c.want == "" {
			if got != nil {
				t.Errorf("Decimal(%q): expected nil, got %v", c.in, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("Decimal(%q): expected %s, got nil", c.in, c.want)
			continue
		}
		if got.String() != c.want {
			t.Errorf("Decimal(%q) = %s, want %s", c.in, got.String(), c.want)
		}
	}
}

// The separator rules rewrite the string in place, so an error built from the
// working copy names a value that appears nowhere in the workbook — and the
// only purpose of the message is to send somebody to a cell. "1,2,3" is the
// case that shows it: the first comma becomes a decimal point before
// NewFromString refuses the rest, so the working copy at the point of failure
// reads "1.2,3", which is not what anyone typed or can search for.
func TestDecimalErrorQuotesTheCellAsTyped(t *testing.T) {
	for _, in := range []string{"1,2,3", "abc", "-", "Rp"} {
		_, err := Decimal(in)
		if err == nil {
			t.Errorf("Decimal(%q): expected an error", in)
			continue
		}
		if !strings.Contains(err.Error(), in) {
			t.Errorf(
				"Decimal(%q) error is %q; it must quote the cell as typed",
				in, err)
		}
	}
}

// A single separator followed by exactly three digits is genuinely
// ambiguous. It is read as a decimal — see the note on Decimal for why that
// is the conservative choice for a measured quantity.
func TestDecimalAmbiguousSingleSeparator(t *testing.T) {
	got, err := Decimal("1.234")
	if err != nil || got == nil {
		t.Fatalf("Decimal(\"1.234\"): %v", err)
	}
	if got.String() != "1.234" {
		t.Errorf(
			"Decimal(\"1.234\") = %s, want 1.234 (decimal, not 1234)",
			got.String())
	}
}

// Date reads an Excel serial under the 1900 epoch and every text layout,
// day-first; an empty cell is nil, and anything else is an error.
func TestDate(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means nil
		fail bool
	}{
		{"", "", false},
		{"36573", "2000-02-17", false}, // Excel serial, 1900 system
		{"41752", "2014-04-23", false},
		{"44589", "2022-01-28", false},
		{"2005-09-23", "2005-09-23", false},
		{"23/09/2005", "2005-09-23", false}, // dd/mm/yyyy — day-first
		{"3/4/2005", "2005-04-03", false},   // 3 April, NOT 4 March
		{"03/04/2005", "2005-04-03", false}, // the same day, padded
		{"23-09-2005", "2005-09-23", false},
		{"3-4-2005", "2005-04-03", false}, // the unpadded dashed form
		{"23.09.2005", "2005-09-23", false},
		{"3.4.2005", "2005-04-03", false}, // the unpadded dotted form
		{"2005-09-23 14:30:00", "2005-09-23", false},
		{"2005-09-23T14:30:00", "2005-09-23", false},
		{"2005-09-23T14:30:00Z", "2005-09-23", false},
		// A stated offset keeps the day it was written with, whatever
		// zone the test runs in.
		{"2005-09-23T23:30:00-05:00", "2005-09-23", false},
		{"0", "", true}, // a serial has to be positive
		{"-36573", "", true},
		{"not a date", "", true},
	}
	for _, c := range cases {
		got, err := Date(c.in, false)
		if c.fail {
			if err == nil {
				t.Errorf("Date(%q): expected an error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("Date(%q): unexpected error %v", c.in, err)
			continue
		}
		if c.want == "" {
			if got != nil {
				t.Errorf("Date(%q): expected nil", c.in)
			}
			continue
		}
		if got == nil || got.Time.Format("2006-01-02") != c.want {
			t.Errorf("Date(%q) = %v, want %s", c.in, got, c.want)
		}
	}
}

// The 1904 epoch shifts every date by four years and a day, and the result
// still looks like an ordinary date — which is why the two are pinned against
// each other rather than only the common one being tested.
func TestDateHonoursThe1904Epoch(t *testing.T) {
	got, err := Date("36573", true)
	if err != nil || got == nil {
		t.Fatalf("Date(1904): %v", err)
	}
	if want := "2004-02-18"; got.Time.Format("2006-01-02") != want {
		t.Errorf(
			"Date(\"36573\", true) = %s, want %s",
			got.Time.Format("2006-01-02"),
			want)
	}
}

// None of these is a date, and each would reach excelize as a float if the
// serial were not bounded first. ParseFloat accepts "NaN" and "Inf" in any
// letter case, and a NaN compares false against everything, a check on zero
// included. What the conversion makes of a serial past the range of int
// depends on the processor: a year before 1 on amd64, which a check on the
// result would still catch, and a day in the 2190s on arm64, which it would
// not.
//
// The two eighteen-digit serials hold the guard in place on every processor.
// Each fits an int, wraps the day arithmetic, and comes back under the 1900
// epoch as an ordinary date, 30 December 1899 and 7 March 2023, so a Date
// that judged only the result would accept both.
func TestDateRejectsNonFiniteAndOutOfRangeSerials(t *testing.T) {
	for _, in := range []string{
		"NaN", "nan", "Inf", "inf", "+Inf", "-Inf", "1e308", "1e19",
		"9223372036854775808", "144115188075855872",
		"144115188075900872", "2958466",
	} {
		for _, date1904 := range []bool{false, true} {
			got, err := Date(in, date1904)
			if err == nil {
				t.Errorf(
					"Date(%q, %v) = %v, want an error",
					in, date1904, got)
			}
		}
	}
}

// The upper bound is a real date, not a round number: 9999-12-31 is the last
// day a DATE column holds, and it must still parse. It is serial 2958465
// under the 1900 epoch and 2957003 under 1904, which reaches every day 1462
// serials sooner, and one serial on is past it under either.
//
// Under 1904 that next serial is still below the bound Date holds its input
// to, so it is the check on the year that refuses it.
func TestDateAcceptsTheLastRepresentableDay(t *testing.T) {
	for _, c := range []struct {
		date1904   bool
		last, past string
	}{
		{false, "2958465", "2958466"},
		{true, "2957003", "2957004"},
	} {
		got, err := Date(c.last, c.date1904)
		if err != nil || got == nil {
			t.Fatalf("Date(%q, %v): %v", c.last, c.date1904, err)
		}
		if day := got.Time.Format("2006-01-02"); day != "9999-12-31" {
			t.Errorf("Date(%q, %v) = %s, want 9999-12-31",
				c.last, c.date1904, day)
		}
		if got, err := Date(c.past, c.date1904); err == nil {
			t.Errorf("Date(%q, %v) = %v, want an error",
				c.past, c.date1904, got)
		}
	}
}

// Int accepts a whole number, and a float with no fractional part; a
// fraction is an error, never a truncation.
func TestInt(t *testing.T) {
	if p, err := Int(""); err != nil || p != nil {
		t.Errorf("Int(\"\") = %v, %v; want nil, nil", p, err)
	}
	if p, err := Int("6"); err != nil || p == nil || *p != 6 {
		t.Errorf("Int(\"6\") = %v, %v", p, err)
	}
	if p, err := Int("6.0"); err != nil || p == nil || *p != 6 {
		t.Errorf(
			"Int(\"6.0\") = %v, %v; a whole float should be accepted",
			p, err)
	}
	if _, err := Int("6.5"); err == nil {
		t.Error(
			"Int(\"6.5\"): a fraction must be an error, never a silent " +
				"truncation")
	}
}

// ParseFloat accepts all of these, and converting any of them to int is
// implementation-dependent — which means the round-trip check that looks
// like it covers them gives a DIFFERENT answer per architecture.
//
// On amd64 the conversion wraps to the minimum int and the round-trip catches
// every case, so this test passes with the guard removed. On arm64 it
// saturates: int(2^63) is the maximum int, float64 of that rounds back to
// exactly 2^63, the round-trip compares equal, and "9223372036854775808.0"
// is read as 9223372036854775807. That is the case a developer on Apple
// silicon meets and CI on amd64 does not, so the assertion is written to pin
// the answer rather than the platform.
//
// The boundary is written with a fraction of zero on both sides of it,
// because digits alone are read as an integer and never meet the guard.
// 2^63 is the first float64 no int64 holds; 2^63-1024 is the largest one
// that fits, and it must still parse — a guard written with the wrong
// comparison rejects both.
//
// Digits alone that run past int are refused on their own account, and the
// last entry is why they are kept from the float: it rounds onto the minimum
// there, which is in range, and would come back as a number nobody typed.
func TestIntRejectsNonFiniteAndOutOfRangeValues(t *testing.T) {
	for _, in := range []string{
		"NaN", "nan", "Inf", "inf", "+Inf", "-Inf",
		"1e308", "-1e308", "9223372036854775808.0",
		"9223372036854775808", "-9223372036854775809",
	} {
		if got, err := Int(in); err == nil {
			t.Errorf("Int(%q) = %v, want an error", in, *got)
		}
	}

	// The boundary is int64's, and means nothing where int is narrower.
	if strconv.IntSize < 64 {
		t.Skip("int is narrower than 64 bits on this target")
	}
	const largestThatFits = "9223372036854774784.0" // 2^63 - 1024
	got, err := Int(largestThatFits)
	if err != nil || got == nil {
		t.Fatalf("Int(%q): %v", largestThatFits, err)
	}
	if int64(*got) != 9223372036854774784 {
		t.Errorf("Int(%q) = %d, want 9223372036854774784",
			largestThatFits, *got)
	}
}

// StrMax refuses a value longer than its column, and accepts one exactly
// as long.
func TestStrMaxRejectsOverlongValue(t *testing.T) {
	if _, err := StrMax("12345678901", 10, "location"); err == nil {
		t.Error("expected an error for a value wider than its column")
	}
	p, err := StrMax("C-4", 10, "location")
	if err != nil || p == nil || *p != "C-4" {
		t.Errorf("StrMax = %v, %v", p, err)
	}
	if _, err := StrMax("1234567890", 10, "location"); err != nil {
		t.Errorf("a value of exactly the column width must fit: %v", err)
	}
}

// The width is in BYTES. Only a multi-byte value can tell the two units
// apart, and it has to be one whose rune count fits while its byte count
// does not — an ASCII case passes under either implementation and pins
// nothing.
//
// "Rue de la Paixè" is 15 runes and 16 bytes. A rune count accepts it into a
// VARCHAR(15); SQL Server, which sizes the column in storage bytes, does
// not, and the value comes back as a truncation error naming no field. That
// is the failure StrMax exists to turn into a message naming the column, so
// accepting the value here defeats the whole function.
func TestStrMaxMeasuresBytesNotRunes(t *testing.T) {
	const v = "Rue de la Paixè" // 15 runes, 16 bytes

	if _, err := StrMax(v, 15, "usage"); err == nil {
		t.Error(
			"a 16-byte value was accepted into a 15-byte column; the " +
				"check is counting runes and is looser than the column")
	}
	got, err := StrMax(v, 16, "usage")
	if err != nil || got == nil || *got != v {
		t.Errorf("StrMax(%q, 16) = %v, %v; 16 bytes must fit", v, got, err)
	}
}

// The message carries the unit, because "usage is 17, the column holds 16"
// invites the reader to count characters and find sixteen of them.
func TestStrMaxErrorNamesFieldAndUnit(t *testing.T) {
	_, err := StrMax("12345678901", 10, "location")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"location", "bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// An empty cell is SQL NULL, not an empty string: the distinction is the whole
// reason these return pointers.
func TestStrIsNilForAnEmptyCell(t *testing.T) {
	if p := Str("   "); p != nil {
		t.Errorf("Str(whitespace) = %q, want nil", *p)
	}
	if p := Str("C-4"); p == nil || *p != "C-4" {
		t.Errorf("Str(\"C-4\") = %v", p)
	}
}

// A non-breaking space arriving via copy-paste must not make a populated cell
// look empty, nor an otherwise identical value fail to compare equal.
//
// The last case is the one that needs Trim's own rewrite. TrimSpace takes a
// non-breaking space off either end by itself, and leaves one in the middle.
func TestTrimNormalisesNonBreakingSpace(t *testing.T) {
	if got := Trim("\u00a0"); got != "" {
		t.Errorf("Trim(NBSP) = %q, want empty", got)
	}
	if got := Trim("\u00a035.002\u00a0"); got != "35.002" {
		t.Errorf("Trim = %q, want 35.002", got)
	}
	if got := Trim("Jl.\u00a0Merdeka\u00a045"); got != "Jl. Merdeka 45" {
		t.Errorf("Trim = %q, want the spaces inside made ordinary", got)
	}
}

// The two date systems count from different days — 1899-12-30 and
// 1904-01-01 — which are 1462 days apart, so every serial lands exactly
// that far apart under the two. Pinning the gap rather than one date is
// what shows the flag is being honoured at all: a Date that ignored it
// would still produce a plausible date for either setting.
//
// This needs no workbook, which is why it is a unit test; the integration
// suite checks the other half, that Open reads the flag from the file.
func TestTheEpochsAreAFixedDistanceApart(t *testing.T) {
	const serial = "35000"

	right, err := Date(serial, true)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := Date(serial, false)
	if err != nil {
		t.Fatal(err)
	}
	if right.Time.Equal(wrong.Time) {
		t.Fatal("both epochs produced the same date; the flag is " +
			"being ignored")
	}

	// Counted in calendar days rather than as a Duration between the two
	// values, because both are local midnights, and a zone that changes its
	// offset between the two dates would put the Duration an hour out
	// without either date being wrong.
	day := func(tm time.Time) time.Time {
		y, m, d := tm.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	gap := day(right.Time).Sub(day(wrong.Time))
	if want := 1462 * 24 * time.Hour; gap != want {
		t.Errorf("the epochs differ by %v, want %v", gap, want)
	}
}

// A date keeps its day on the way to the driver in any zone.
// datetime.Datetime.Value converts into time.Local, so Date has to hand back
// local midnight: midnight UTC would convert to the previous evening west of
// Greenwich, and a DATE column would record the day before.
//
// The zone is set for this test alone, which is safe only because nothing in
// this package's tests runs in parallel. Both parse paths are checked, since
// each reaches Day from a UTC value of its own.
func TestDateKeepsItsDayThroughValueWestOfGreenwich(t *testing.T) {
	testutil.Set(t, &time.Local, time.FixedZone("UTC-5", -5*60*60))

	for _, in := range []string{"36573", "17/02/2000"} {
		got, err := Date(in, false)
		if err != nil || got == nil {
			t.Fatalf("Date(%q): %v", in, err)
		}
		v, err := got.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		tm, ok := v.(time.Time)
		if !ok {
			t.Fatalf("Value = %T, want time.Time", v)
		}
		if day := tm.Format("2006-01-02"); day != "2000-02-17" {
			t.Errorf("Date(%q) reaches the driver as %s, want 2000-02-17",
				in, day)
		}
	}
}

// A number in scientific notation is how a numeric cell arrives when the
// program that wrote it put it so, and the separator rules stop at the E:
// read by them, "1.5E7" is 1.5, wrong by ten million and with no error.
// So it is read whole, and a power of ten no DECIMAL column holds is
// refused rather than written out as a gigabyte of digits.
func TestDecimalReadsScientificNotation(t *testing.T) {
	for in, want := range map[string]string{
		"1.5E7":    "15000000",
		"1.5e+07":  "15000000",
		"-2.5E-1":  "-0.25",
		" 1E3 ":    "1000",
		".5E1":     "5",
		"1E65":     "1" + strings.Repeat("0", 65),
		"12345E-3": "12.345",
	} {
		got, err := Decimal(in)
		if err != nil || got == nil || got.String() != want {
			t.Errorf("Decimal(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	// The value is not printed when one comes back: written out, the
	// last of these is a gigabyte of digits.
	for _, in := range []string{"1E66", "1E-66", "1E+999999999"} {
		got, err := Decimal(in)
		if got != nil || err == nil || !strings.Contains(err.Error(), in) {
			t.Errorf("Decimal(%q) returned a value: %t, and %v; want "+
				"an error quoting it", in, got != nil, err)
		}
	}
}

// DecimalDotGrouped reads the notation it is named for and nothing else.
// A full stop only ever groups thousands, so 300.000 is three hundred
// thousand, which Decimal reads as three hundred; a comma only ever
// starts the decimals; and a cell in any other form is refused.
func TestDecimalDotGrouped(t *testing.T) {
	for in, want := range map[string]string{
		"":             "",
		"0":            "0",
		"500":          "500",
		"300.000":      "300000",
		"1.300.000":    "1300000",
		"1.234.567,89": "1234567.89",
		"-5.000":       "-5000",
		"1250000":      "1250000",
		" 75.000 ":     "75000",
		"12,5":         "12.5",
	} {
		got, err := DecimalDotGrouped(in)
		switch {
		case err != nil:
			t.Errorf("DecimalDotGrouped(%q): %v", in, err)
		case want == "" && got != nil:
			t.Errorf("DecimalDotGrouped(%q) = %s, want nil", in, got)
		case want != "" && (got == nil || got.String() != want):
			t.Errorf("DecimalDotGrouped(%q) = %v, want %s", in, got, want)
		}
	}
	for _, in := range []string{
		"1,234.56", "1.23", "12.3456", "1.000.00", "Rp 5.000",
		"5.000,", "lima ribu", "1e3", "1.5E7", "+5.000",
	} {
		got, err := DecimalDotGrouped(in)
		if err == nil {
			t.Errorf("DecimalDotGrouped(%q) = %v, want an error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), in) {
			t.Errorf("DecimalDotGrouped(%q): the error %q does not "+
				"quote the cell", in, err)
		}
	}
}

// A date spelled out may name its month in Indonesian, in full, by its
// usual abbreviation, or under the older spellings Pebruari and Nopember,
// in any letter case, and English names read beside them, in full or
// abbreviated. A day its month does not have is refused, in either
// language, and so is a date with anything written after its year.
func TestDateReadsIndonesianMonths(t *testing.T) {
	for in, want := range map[string]string{
		"2 Januari 2026":   "2026-01-02",
		"17 agustus 2025":  "2025-08-17",
		"28 Pebruari 2026": "2026-02-28",
		"30 Nopember 2026": "2026-11-30",
		"1 MEI 2026":       "2026-05-01",
		"05 Okt 2026":      "2026-10-05",
		"17 Agu 2025":      "2025-08-17",
		"24  Des  2026":    "2026-12-24",
		"9 April 2026":     "2026-04-09",
		"2 January 2026":   "2026-01-02",
		"5 Oct 2026":       "2026-10-05",
		"31 Desember 2026": "2026-12-31",
		"29 Februari 2024": "2024-02-29",
		"1 September 2026": "2026-09-01",
		"3 Juni 2026":      "2026-06-03",
		"14 Juli 2026":     "2026-07-14",
		"21 Maret 2026":    "2026-03-21",
	} {
		got, err := Date(in, false)
		if err != nil || got == nil || got.Format("2006-01-02") != want {
			t.Errorf("Date(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{
		"31 Februari 2026", "29 Pebruari 2026", "0 Januari 2026",
		"2 Januari 0", "32 Mei 2026", "2 Smarch 2026", "2 Januar 2026",
		"Januari 2026", "2 Januari", "2 Januari 2026 10:00",
	} {
		got, err := Date(in, false)
		if err == nil || !strings.Contains(err.Error(), in) {
			t.Errorf("Date(%q) = %v, %v; want an error quoting it",
				in, got, err)
		}
	}
}

// One table serves both languages only because they never disagree: no
// spelling in it names one month in Indonesian and another in English.
// Each entry is offered to time.Parse as English, and wherever English
// reads it at all it has to read the same month. "Jan", "Feb", "Mar",
// "Jun" and "Jul" are the names that make this worth holding; "Mei" and
// "Des" are among the ones English does not know.
func TestNoMonthNameMeansTwoMonths(t *testing.T) {
	for name, month := range bulan {
		for _, layout := range []string{"2 January 2006", "2 Jan 2006"} {
			got, err := time.Parse(layout, "1 "+name+" 2026")
			if err == nil && got.Month() != month {
				t.Errorf("%q is %v in Indonesian and %v in English",
					name, month, got.Month())
			}
		}
	}
}

// Unescape decodes the escapes a workbook writes a character as when its
// XML cannot hold it, leaves alone what only looks like one, and decodes
// the escaped underscore once, so the literal text of an escape survives.
func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		"baris 1_x000D_\nbaris 2": "baris 1\r\nbaris 2",
		"_x005F_x000D_":           "_x000D_",
		"tab_x0009_tab":           "tab\ttab",
		"kode_x":                  "kode_x",
		"_x00e9_":                 "\u00e9",
		"half _xD83D_ a pair":     "half _xD83D_ a pair",
		"_x12G4_":                 "_x12G4_",
		"no escape at all":        "no escape at all",
	} {
		if got := Unescape(in); got != want {
			t.Errorf("Unescape(%q) = %q, want %q", in, got, want)
		}
	}
}
