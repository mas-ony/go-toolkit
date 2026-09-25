package xlsx

import (
	"strings"
	"testing"
	"time"
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
		{"23-09-2005", "2005-09-23", false},
		{"23.09.2005", "2005-09-23", false},
		{"3.4.2005", "2005-04-03", false}, // the unpadded dotted form
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

// Each of these reaches excelize as a float and comes back, without the guard,
// as a nil error and a date no column can hold. "NaN" and "Inf" get there
// because ParseFloat accepts both, case-insensitively, and because a NaN
// compares false against everything — including the `serial <= 0` check that
// looks like it covers them. "1e308" gets there because the day arithmetic
// overflows and wraps into a negative year. The years are -5005 and -4713.
//
// The check is on the RESULT rather than the input, which is what makes one
// guard cover all three and stay correct under either epoch.
func TestDateRejectsNonFiniteAndOutOfRangeSerials(t *testing.T) {
	for _, in := range []string{
		"NaN", "nan", "Inf", "inf", "+Inf", "-Inf", "1e308", "2958466",
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

// The upper bound is a real date, not a round number: serial 2958465 is
// 9999-12-31, the last day a DATE column holds, and it must still parse.
func TestDateAcceptsTheLastRepresentableDay(t *testing.T) {
	got, err := Date("2958465", false)
	if err != nil || got == nil {
		t.Fatalf("Date(2958465): %v", err)
	}
	if want := "9999-12-31"; got.Time.Format("2006-01-02") != want {
		t.Errorf(
			"Date(\"2958465\") = %s, want %s",
			got.Time.Format("2006-01-02"),
			want)
	}
}

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
// exactly 2^63, the round-trip compares equal, and "9223372036854775808" is
// read as 9223372036854775807. That is the case a developer on Apple silicon
// meets and CI on amd64 does not, so the assertion is written to pin the
// answer rather than the platform.
//
// The last two entries are the boundary. 2^63 is the first float64 no int64
// holds; 2^63-1024 is the largest one that fits, and it must still parse —
// a guard written with the wrong comparison rejects both.
func TestIntRejectsNonFiniteAndOutOfRangeValues(t *testing.T) {
	for _, in := range []string{
		"NaN", "nan", "Inf", "inf", "+Inf", "-Inf",
		"1e308", "-1e308", "9223372036854775808",
	} {
		if got, err := Int(in); err == nil {
			t.Errorf("Int(%q) = %v, want an error", in, *got)
		}
	}

	const largestThatFits = "9223372036854774784" // 2^63 - 1024
	got, err := Int(largestThatFits)
	if err != nil || got == nil {
		t.Fatalf("Int(%q): %v", largestThatFits, err)
	}
	if *got != 9223372036854774784 {
		t.Errorf("Int(%q) = %d, want 9223372036854774784",
			largestThatFits, *got)
	}
}

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
func TestTrimNormalisesNonBreakingSpace(t *testing.T) {
	if got := Trim("\u00a0"); got != "" {
		t.Errorf("Trim(NBSP) = %q, want empty", got)
	}
	if got := Trim("\u00a035.002\u00a0"); got != "35.002" {
		t.Errorf("Trim = %q, want 35.002", got)
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

	gap := right.Time.Sub(wrong.Time)
	if want := 1462 * 24 * time.Hour; gap != want {
		t.Errorf("the epochs differ by %v, want %v", gap, want)
	}
}
