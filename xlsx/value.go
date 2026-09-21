package xlsx

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"

	"github.com/mas-ony/go-toolkit/datetime"
)

// dateLayouts are the text date formats accepted when a cell is not a serial
// number, ordered from least to most ambiguous.
//
// The day-first entries come before the month-first ones deliberately: in a
// day-first locale 03/04/2005 means 3 April, and a parser that tried
// "01/02/2006" first would silently read it as 4 March. Both parse
// successfully, so order is the only thing that decides — which is exactly
// why a wrong order here would be invisible rather than an error.
//
// Each numeric separator is listed twice, padded and unpadded. Go's parser
// reads the "2" verb as one digit or two, so the unpadded spelling subsumes
// the padded one; both are kept because a separator with only one of the two
// forms reads as a deliberate exclusion rather than an omission, and the
// omission is what actually happens.
//
// The two spelled-out layouts are ENGLISH-ONLY, which is worth stating in a
// package whose numeric conventions are not. time.Parse matches month names
// against its own table, so a localised month name does not parse however
// close its spelling is to the English one — and a few languages share
// enough spellings to hide how narrow that is. The rest fail LOUDLY — "is
// not a recognised date" naming the cell — which is why this is documented
// rather than patched. Adding them means a month-name table and a decision
// about mixed-language workbooks, not another layout string.
var dateLayouts = []string{
	"2006-01-02",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	time.RFC3339,
	"02/01/2006", // dd/mm/yyyy — day-first
	"2/1/2006",
	"02-01-2006",
	"2-1-2006",
	"02.01.2006",
	"2.1.2006",
	"2 January 2006",
	"2 Jan 2006",
}

// isGrouping reports whether sep appears in s in the pattern of a thousands
// separator: more than once, with every group after the first exactly three
// digits long. A single separator is never treated as grouping — see the
// ambiguity note on Decimal.
func isGrouping(s string, sep rune) bool {
	parts := strings.Split(s, string(sep))
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts[1:] {
		if len(p) != 3 {
			return false
		}
	}
	return true
}

// Trim returns the cell with surrounding whitespace removed and any
// non-breaking spaces normalised.
//
// U+00A0 arrives via copy-paste from web pages and PDFs and is invisible in
// Excel, so a cell that looks empty is not, and a value that reads as "35.002"
// does not compare equal to it. Normalising here means every downstream
// emptiness check and lookup sees the value a human believes is there.
func Trim(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return strings.TrimSpace(s)
}

// Str returns a pointer to the trimmed cell, or nil when it is empty.
// nil becomes SQL NULL, which is what an empty spreadsheet cell means.
func Str(s string) *string {
	s = Trim(s)
	if s == "" {
		return nil
	}
	return &s
}

// StrMax is Str with a length ceiling, returning an error when the value is
// too long for its column rather than letting the driver truncate it or reject
// the whole INSERT with a message that does not name the field.
//
// field names the column for that message and is the reason this is not simply
// two arguments: "notes is 812 bytes, the column holds 500" is actionable, and
// "String or binary data would be truncated" is not.
//
// limit is in BYTES, and the unit is load-bearing rather than incidental.
// SQL Server sizes VARCHAR(n) in storage bytes under the column's collation
// codepage, not in characters, so a rune count is the LOOSER of the two: a
// value of fifty multi-byte runes is a hundred bytes and overflows a column
// that accepted the count, and the overflow surfaces as a driver truncation
// error naming no field — the outcome this function exists to prevent. Go's
// len() on a UTF-8 string is never smaller than the codepage encoding of the
// same text, so a byte check is exact under a UTF-8 collation and merely
// conservative under a single-byte one.
//
// MySQL sizes VARCHAR(n) in CHARACTERS, so the same declaration there holds
// n characters — up to four times that in bytes under utf8mb4. The byte
// check stays correct on both: exact against SQL Server, strict against
// MySQL. Any other cap measured against a column width should be counted the
// same way, so that one convention covers all of them.
//
// The parameter is named limit rather than max because max is a builtin as
// of Go 1.21, and shadowing it inside a function this small is a trap for
// whoever next needs the builtin here.
func StrMax(s string, limit int, field string) (*string, error) {
	p := Str(s)
	if p == nil {
		return nil, nil
	}
	if n := len(*p); n > limit {
		return nil, fmt.Errorf(
			"%s is %d bytes, the column holds %d",
			field,
			n,
			limit)
	}
	return p, nil
}

// Int parses a cell as an integer, returning nil for an empty cell.
//
// A value stored as a float ("6" arriving as "6.0", which happens when a
// column has been through a formula) is accepted as long as it has no
// fractional part; a genuine fraction is an error rather than a silent
// truncation, because silently turning 6.5 into 6 in a location or map
// reference produces a wrong answer that looks right.
//
// Decimal's separator rules are deliberately NOT applied here, which is
// worth stating because a reader arriving from that function will assume
// they are. Int is for small counters, so a value carrying a grouping
// separator is not a large number written in a locale's convention, it is a
// cell holding something other than what the column is for. Under Decimal's
// rules "1.234" would be read as a fraction and rejected as non-whole, and
// "1.234.567" would be quietly accepted as a million; refusing both, loudly,
// is the outcome that gets the cell looked at.
func Int(s string) (*int, error) {
	s = Trim(s)
	if s == "" {
		return nil, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return &n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", s)
	}

	// ParseFloat also accepts "NaN" and "Inf", case-insensitively, and any
	// finite value however large. Converting one of those to int is
	// implementation-dependent by the spec — not a panic, but not a defined
	// answer either — so the range has to be checked before the conversion
	// rather than after it.
	//
	// The round-trip below LOOKS like it covers this, and on amd64 it does:
	// the conversion wraps to the minimum int, whose float64 differs from
	// the input, so every one of these comes back as "not a whole number".
	// arm64 saturates instead. There int(2^63) is the MAXIMUM int, whose
	// float64 rounds back to exactly 2^63, the round-trip compares equal,
	// and "9223372036854775808" is read as 9223372036854775807 — a number
	// nobody typed, on the architecture half the developers are running.
	//
	// This is the same hazard Date's serial branch documents at more length,
	// reached from the other direction: Date range-checks its RESULT because
	// its conversion is excelize's, and this checks its INPUT because the
	// conversion is the one on the next line.
	//
	// The bounds are int64's, while the conversion below produces an int.
	// The two are the same width on a 64-bit target, and the check is
	// written to that: MinInt64 converts to float64 exactly, and MaxInt64
	// rounds UP to 2^63, which is why the upper comparison has to be >=
	// rather than >. On a 32-bit target the guard would pass values int
	// cannot hold, so it would need rewriting around math.MinInt and
	// math.MaxInt rather than merely re-reading.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%q is not a number", s)
	}
	if f < math.MinInt64 || f >= math.MaxInt64 {
		return nil, fmt.Errorf("%q is out of range", s)
	}

	n := int(f)
	if float64(n) != f {
		return nil, fmt.Errorf("%q is not a whole number", s)
	}
	return &n, nil
}

// Decimal parses a cell as a fixed-point decimal, returning nil for an empty
// cell.
//
// Grouping and decimal separators are a real hazard whenever the source
// workbooks come from a locale whose conventions are the reverse of the
// invariant ones, where 1.234,56 means one thousand two hundred thirty-four
// and a half. Read raw, the numeric cells carry no separators at all — but a
// cell somebody typed as text does, and the two conventions have to be told
// apart.
//
// The rule used is positional rather than locale-configured, because a single
// workbook can contain both: whichever of "." and "," appears LAST is the
// decimal separator, and the other is grouping. That is unambiguous whenever
// both are present. When only one appears, it is treated as grouping if it
// separates a group of exactly three digits from the end and appears more than
// once, and as a decimal point otherwise — so "1.234.567" is a million, while
// "1.234" is one-point-two-three-four.
//
// The remaining ambiguity is a lone separator followed by exactly three
// digits: "1.234" could be either. It is read as a decimal, which is the
// conservative reading for a measured quantity — an area of 1.234 m² is
// obviously wrong and gets noticed, whereas 1234 m² is plausible and would
// be believed.
//
// Every error quotes the cell AS THE OPERATOR TYPED IT, not the partially
// rewritten form the function is working on at the point of failure. The
// separator rules rewrite the string in place, so a message built from the
// working copy would name a value that appears nowhere in the workbook — and
// the whole purpose of the message is to send somebody to a cell.
func Decimal(s string) (*decimal.Decimal, error) {
	s = Trim(s)
	if s == "" {
		return nil, nil
	}
	orig := s
	// Extract the numeric run. Naively deleting every non-numeric character is
	// wrong in a way that is easy to miss: "1285 m2" would keep the 2 from the
	// unit and yield 12852 — a plausible-looking area that is silently ten
	// times too large. Instead, skip any leading prefix ("Rp", "±"), then
	// collect from the first digit or sign until the first character that
	// cannot be part of the number, discarding the rest.
	//
	// Whitespace is removed first so a space used as a thousands separator
	// ("1 285") does not terminate the run.
	s = strings.Join(strings.Fields(s), "")
	start := strings.IndexFunc(s, func(r rune) bool {
		return (r >= '0' && r <= '9') || r == '-' || r == '+'
	})
	if start < 0 {
		return nil, fmt.Errorf("%q is not a number", orig)
	}
	// Every character the run can contain is ASCII, so the walk is over
	// BYTES. The alternative — converting each byte to a rune — reads as
	// though it handled multi-byte input and does not: it would hand the
	// loop a continuation byte rather than the character it belongs to.
	// Nothing breaks either way, because no continuation byte matches a
	// digit or a separator and the run ends there in both readings, but only
	// one of the two spellings says which property is being relied on.
	end := start
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == ',' {
			end++
			continue
		}
		if (c == '-' || c == '+') && end == start {
			end++
			continue
		}
		break
	}
	s = s[start:end]
	if s == "" || s == "-" || s == "+" {
		return nil, fmt.Errorf("%q is not a number", orig)
	}

	lastDot := strings.LastIndex(s, ".")
	lastComma := strings.LastIndex(s, ",")

	switch {
	case lastDot >= 0 && lastComma >= 0:
		// Both present: the later one is the decimal separator.
		if lastComma > lastDot {
			s = strings.ReplaceAll(s, ".", "")
			s = strings.Replace(s, ",", ".", 1)
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case lastComma >= 0:
		if isGrouping(s, ',') {
			s = strings.ReplaceAll(s, ",", "")
		} else {
			s = strings.Replace(s, ",", ".", 1)
		}
	case lastDot >= 0:
		// No else, unlike the comma branch above, and the asymmetry is the
		// point rather than an omission: a lone dot that is not grouping is
		// ALREADY the decimal separator NewFromString wants, so there is
		// nothing to rewrite. A dot pattern that is neither — "1.2345.678",
		// with a group of four — falls through untouched and NewFromString
		// refuses the whole cell, which is the loud outcome a malformed
		// number should get.
		if isGrouping(s, '.') {
			s = strings.ReplaceAll(s, ".", "")
		}
	}

	// A value ending in a separator ("1285,") is a typo, not a convention;
	// trimming it here keeps NewFromString from rejecting the whole cell.
	s = strings.TrimRight(s, ".,")
	if s == "" || s == "-" || s == "+" {
		return nil, fmt.Errorf("%q is not a number", orig)
	}

	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", orig)
	}
	return &d, nil
}

// Date parses a cell as a date, returning nil for an empty cell.
//
// Two representations are handled:
//
//  1. An Excel serial number ("36573"), which is what a real date cell holds.
//     excelize.ExcelDateToTime performs the conversion, including the
//     1900-leap-year fiction that makes naive arithmetic wrong by a day.
//  2. A text date, for cells typed as text or pasted from elsewhere.
//
// date1904 selects the workbook's epoch. It is false for essentially every
// file produced on Windows; a workbook authored in classic Mac Excel uses
// 1904, and reading it with the wrong epoch shifts every date by four years
// and a day. Ask the file rather than the operator where you can — excelize
// exposes it through GetWorkbookProps.
//
// A caller passing a value that might be a bare year should check for that
// FIRST. "2019" parses cleanly as a serial and lands on 11 July 1905, which is
// wrong by a century and looks like an ordinary date in the report.
//
// The result is normalised to midnight UTC by Day. Both parse paths already
// produce UTC — excelize.ExcelDateToTime returns it, and time.Parse resolves
// a layout carrying no zone in UTC — so this pins what is already true rather
// than converting anything. The target columns are DATE, so the time part is
// discarded by the engine either way.
//
// The anchor is only correct while the process's local zone is AHEAD of UTC.
// datetime.Datetime.Value converts into time.Local on the way to the driver,
// so midnight UTC becomes the same day's morning and the column records the
// intended date. Under a zone BEHIND UTC the same value converts to the
// previous evening and the column records the previous day — so this
// anchoring and the local zone move together rather than being independently
// correct.
func Date(s string, date1904 bool) (*datetime.Datetime, error) {
	s = Trim(s)
	if s == "" {
		return nil, nil
	}

	if serial, err := strconv.ParseFloat(s, 64); err == nil {
		if serial <= 0 {
			return nil, fmt.Errorf("%q is not a valid Excel date", s)
		}
		t, err := excelize.ExcelDateToTime(serial, date1904)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid Excel date: %w", s, err)
		}
		// The conversion is unchecked arithmetic on the float, so the guard
		// above does not cover every value ParseFloat will hand over. Three
		// get past it and none is caught downstream:
		//
		//   - "NaN". Every comparison against NaN is false, so `serial <= 0`
		//     lets it through, and the conversion yields the year -5005.
		//   - "Inf", and "inf" — ParseFloat is case-insensitive for both this
		//     and NaN — which is positive and so passes the guard too.
		//   - A finite but enormous serial ("1e308", or simply a mistyped run
		//     of digits), which overflows the day arithmetic and wraps into a
		//     negative year.
		//
		// Range-checking the RESULT catches all three at once, and does it
		// without a second epoch-dependent constant: the answer either lands
		// in the range a DATE column can hold or it does not. Left unchecked
		// these reach the driver, which rejects the parameter with a message
		// naming neither the column nor the cell.
		if y := t.Year(); y < 1 || y > 9999 {
			return nil, fmt.Errorf("%q is not a valid Excel date", s)
		}
		return Day(t), nil
	}

	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return Day(t), nil
		}
	}
	return nil, fmt.Errorf("%q is not a recognised date", s)
}

// Day truncates a time to its calendar date and pins it to UTC.
//
// Exported because a caller that recognises a shape this package does not — an
// acquisition month, a bare year — still has to produce a value in the same
// form, and doing it by hand is how one column ends up an hour off from the
// rest.
func Day(t time.Time) *datetime.Datetime {
	d := datetime.Datetime{
		Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC),
	}
	return &d
}
