package xlsx

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mas-ony/go-toolkit/datetime"
	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"
)

// intBound is 2^(w-1) for the width w of int: the magnitude of math.MinInt,
// and one more than math.MaxInt. As a power of two it is exact in a
// float64, which math.MaxInt is not on a 64-bit target; see Int.
const intBound = -float64(math.MinInt)

// maxExponent bounds the power of ten a number written in scientific
// notation may carry, in either direction. It is the widest DECIMAL
// either engine declares, 65 digits, so a number past it could not be
// stored whatever its column.
//
// The decimal package has a bound of its own, MaxDecodeExponent, 10000
// unless a program sets another, and it is that one which keeps a cell
// such as 1E+999999999 from being written out as a gigabyte of digits.
// This one is the tighter of the two, and does not move with that
// setting.
const maxExponent = 65

// maxSerial is the first serial number past 9999-12-31 under the 1900
// epoch, the later of the two epochs to reach that day. No serial at or
// beyond it is a date a DATE column holds, whichever epoch a workbook
// counts from. See Date.
const maxSerial = 2958466

// MaxCodeDigits is the most digits Code takes: what a BIGINT column holds
// whatever the digits are.
const MaxCodeDigits = 18

// dateLayouts are the text date formats accepted when a cell is not a serial
// number, ordered from least to most ambiguous.
//
// Every numeric layout is day-first, and no month-first one is listed at
// all. In a day-first locale 03/04/2005 means 3 April, and a month-first
// layout would read the same text as 4 March; both readings parse, so
// adding one would leave the answer to list order, invisibly. As it stands,
// a month-first date is misread when its day is 12 or less and refused when
// it is not, since 12/25/2005 has no month 25.
//
// Each numeric separator is listed twice, padded and unpadded. Go's parser
// reads the "2" verb as one digit or two, so the unpadded spelling subsumes
// the padded one; both are kept because a separator with only one of the two
// forms reads as a deliberate exclusion rather than an omission, and the
// omission is what actually happens.
//
// The two spelled-out layouts read a month name in ENGLISH or in
// INDONESIAN and in no other language. time.Parse matches month names
// against its own English table, so Date first rewrites an Indonesian name
// as the English one; see bulan. Any other language's month name does not
// parse however close its spelling is to one of those two — and a few
// languages share enough spellings to hide how narrow that is. The rest
// fail LOUDLY — "is not a recognised date" naming the cell — which is why
// they are documented rather than guessed at. Adding a language means its
// names in the table, and a check that none of them names a different
// month in a language already there.
var dateLayouts = []string{
	time.DateOnly,
	time.DateTime,
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

// bulan maps the Indonesian month names a written date may carry, in
// lower case, to the months they name: the full names, their usual
// abbreviations, and the older spellings Pebruari and Nopember. The
// three months the two languages name alike, April, September and
// November, are left to time.Parse with their abbreviations, and are
// not listed.
//
// The two languages can share one table because they never disagree: no
// spelling here names one month in Indonesian and another in English.
// That is what lets Date read either without being told which language
// a workbook is in, and read a workbook that mixes them.
var bulan = map[string]time.Month{
	"januari":  time.January,
	"jan":      time.January,
	"februari": time.February,
	"feb":      time.February,
	"pebruari": time.February,
	"peb":      time.February,
	"maret":    time.March,
	"mar":      time.March,
	"mei":      time.May,
	"juni":     time.June,
	"jun":      time.June,
	"juli":     time.July,
	"jul":      time.July,
	"agustus":  time.August,
	"agu":      time.August,
	"agt":      time.August,
	"ags":      time.August,
	"oktober":  time.October,
	"okt":      time.October,
	"nopember": time.November,
	"nop":      time.November,
	"desember": time.December,
	"des":      time.December,
}

// scientific matches a number written in scientific notation, 1.5E7,
// and nothing around it.
var scientific = regexp.MustCompile(
	`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)[eE][+-]?[0-9]+$`)

// dotGrouped matches a number written with full stops grouping its
// thousands and a comma before its decimals: an optional minus, the
// whole part in groups of three or as bare digits, and the decimals, if
// any.
var dotGrouped = regexp.MustCompile(
	`^-?([0-9]{1,3}(\.[0-9]{3})+|[0-9]+)(,[0-9]+)?$`)

// ooxmlEscape matches the form a character takes in a workbook's XML
// when it cannot appear there as itself: _x000D_ for a carriage return.
var ooxmlEscape = regexp.MustCompile(`_x[0-9A-Fa-f]{4}_`)

// inThrees reports whether sep appears in s where a thousands separator
// would: at least once, with every group after the first exactly three
// digits long.
func inThrees(s string, sep rune) bool {
	parts := strings.Split(s, string(sep))
	for _, p := range parts[1:] {
		if len(p) != 3 {
			return false
		}
	}
	return len(parts) > 1
}

// isGrouping reports whether sep appears in s in the pattern of a thousands
// separator: more than once, with every group after the first exactly three
// digits long. A single separator is not treated as grouping on its own
// account — see the ambiguity note on Decimal, and what it says of a
// separator the number ends in.
func isGrouping(s string, sep rune) bool {
	return strings.Count(s, string(sep)) > 1 && inThrees(s, sep)
}

// notANumber is the error for a cell that holds no number the reader can
// take. cell is the text as it was typed, since the message is there to
// send somebody to it; see Decimal.
func notANumber(cell string) error {
	return fmt.Errorf("%q is not a number", cell)
}

// outOfRange is the error for a cell whose number is too large or too
// small for what it is read into. cell is quoted as notANumber quotes it.
func outOfRange(cell string) error {
	return fmt.Errorf("%q is out of range", cell)
}

// englishMonth returns a date written out with an Indonesian month
// name, "2 Januari 2026", as the same date under the English name
// time.Parse knows. Text of any other shape is returned as it is.
func englishMonth(s string) string {
	parts := strings.Fields(s)
	if len(parts) != 3 {
		return s
	}
	month, known := bulan[strings.ToLower(parts[1])]
	if !known {
		return s
	}
	return parts[0] + " " + month.String() + " " + parts[2]
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

// Unescape replaces every _xHHHH_ escape in a cell's text with the
// character it stands for. A workbook's XML cannot carry every character
// as itself: most control characters are not allowed in it at all, and a
// carriage return would be read back as a line feed. So text holding a
// carriage return and a line feed, the Windows line break, is stored as
// "_x000D_" followed by a line feed.
//
// Excelize decodes these in a shared or an inline string, and leaves
// them in a cell stored as a plain string value, the form a program
// that writes its cells directly tends to produce. A cell does not say
// which kind it was, so the choice is the caller's: wrap the text
// columns of a sheet that shows the escapes, and leave the rest alone.
// The one case that reads wrong for it is a shared string whose text
// really is the seven characters of an escape, which arrives with them
// intact and is decoded all the same.
//
// _x005F_ is the escape for the underscore that would otherwise start
// one, and each match is replaced once, left to right, so
// "_x005F_x000D_" becomes the text "_x000D_". Half of a surrogate pair
// is left as it is.
func Unescape(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	return ooxmlEscape.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(m[2:6], 16, 16)
		if err != nil || utf16.IsSurrogate(rune(n)) {
			return m
		}
		return string(rune(n))
	})
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
	n, err := strconv.Atoi(s)
	if err == nil {
		return &n, nil
	}
	// Digits that run past int are out of range as they stand, and are
	// kept from the float below: one just under the minimum rounds onto
	// it there, so "-9223372036854775809" would be read as the number
	// one above it.
	if errors.Is(err, strconv.ErrRange) {
		return nil, outOfRange(s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, notANumber(s)
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
	// and "9223372036854775808.0" is read as 9223372036854775807 — a number
	// nobody typed, on the architecture half the developers are running.
	//
	// Date's serial branch guards the same hazard in a conversion that is
	// excelize's, and for the same reason does it on the way in: an answer
	// that depends on the processor cannot be checked once it is back.
	//
	// The bounds are intBound, a power of two, rather than math.MaxInt.
	// On a 64-bit target math.MaxInt rounds UP to 2^63 as a float64, so a
	// `> math.MaxInt` check would admit 2^63 itself; intBound is exactly
	// that value, so `>=` is right, and it is right on a 32-bit target
	// too, where intBound is 2^31 and int is that narrow.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, notANumber(s)
	}
	if f < -intBound || f >= intBound {
		return nil, outOfRange(s)
	}

	n = int(f)
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
// invariant ones, where 1.234,56 means 1234.56. Read raw, the numeric cells
// carry no separators at all — but a cell somebody typed as text does, and
// the two conventions have to be told apart.
//
// The rule used is positional rather than locale-configured, because a single
// workbook can contain both: whichever of "." and "," appears LAST is the
// decimal separator, and the other is grouping. That is unambiguous whenever
// both are present. When only one appears, it is treated as grouping if it
// appears more than once with exactly three digits after each occurrence, and
// as a decimal point otherwise — so "1.234.567" is 1234567, while "1.234" is
// 1.234.
//
// The remaining ambiguity is a lone separator followed by exactly three
// digits: "1.234" could be either. It is read as a decimal, which is the
// conservative reading for a measured quantity — an area of 1.234 m² is
// obviously wrong and gets noticed, whereas 1234 m² is plausible and would
// be believed.
//
// A separator at either end of the number says something too. One the
// number opens with is its decimal point, so ".5" and ",5" are both a half.
// One it ends in has no decimals after it and is dropped, but names the
// decimal separator all the same: "5.000,-", the way a whole amount is
// written in Indonesian, is 5000, because its comma is the decimal
// separator and the full stop is therefore grouping. Where that reading
// does not fit, as in "1285.5,", the closing separator is a typo and the
// rest is read without it.
//
// A cell that is a number in scientific notation and nothing else, 1.5E7,
// is read in full. That is how a numeric cell arrives when the program
// that wrote the workbook put it so, and the rule below, which stops at the
// first character a plain number cannot hold, would return the 1.5 in
// front of the E. An exponent past maxExponent is refused.
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
	if scientific.MatchString(s) {
		d, err := decimal.NewFromString(s)
		if err != nil || d.Exponent() < -maxExponent ||
			d.Exponent() > maxExponent {
			return nil, outOfRange(orig)
		}
		return &d, nil
	}
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
		return nil, notANumber(orig)
	}
	// A separator directly in front of the first digit opens the number:
	// ".5" and ",5" are a half, the way some programs write a number below
	// one, and with the separator skipped as a prefix they would be read
	// as 5. After a letter the same character closes an abbreviation
	// instead, as in "Rp.5", and after another separator it is one of a
	// row of dots. Either way it stays outside.
	if c := s[start]; start > 0 && c >= '0' && c <= '9' &&
		(s[start-1] == '.' || s[start-1] == ',') {
		before, _ := utf8.DecodeLastRuneInString(s[:start-1])
		if !unicode.IsLetter(before) && before != '.' && before != ',' {
			start--
		}
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
	// A separator the run ends in has no decimals after it, and comes off
	// before the rest are read. Left on, it would be the last of them and
	// so the decimal point whatever came before: "1285.5," would lose its
	// real one as grouping and come back as 12855.
	//
	// Which separator it was is kept, in closing, because it can settle
	// the reading the rules below leave open. In "5.000,-" the comma is
	// the decimal separator, with nothing behind it, so the full stop is
	// grouping although it appears once. closing is a digit when the run
	// ended in one.
	run := s[start:end]
	s = strings.TrimRight(run, ".,")
	if s == "" || s == "-" || s == "+" {
		return nil, notANumber(orig)
	}
	closing := run[len(run)-1]

	lastDot := strings.LastIndex(s, ".")
	lastComma := strings.LastIndex(s, ",")

	switch {
	case s[0] == '.' || s[0] == ',':
		// The number opened with its separator, which is the decimal
		// point whichever of the two it is. A second separator after it
		// is left in place, for NewFromString to refuse.
		s = "." + s[1:]
	case lastDot >= 0 && lastComma >= 0:
		// Both present: the later one is the decimal separator.
		if lastComma > lastDot {
			s = strings.ReplaceAll(s, ".", "")
			s = strings.Replace(s, ",", ".", 1)
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case lastComma >= 0:
		if isGrouping(s, ',') || closing == '.' && inThrees(s, ',') {
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
		if isGrouping(s, '.') || closing == ',' && inThrees(s, '.') {
			s = strings.ReplaceAll(s, ".", "")
		}
	}

	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil, notANumber(orig)
	}
	return &d, nil
}

// DecimalDotGrouped parses a cell holding a number written out as text
// with full stops grouping its thousands and a comma before its
// decimals, 1.234.567,89, returning nil for an empty cell. It is the
// notation Indonesian and most continental European sheets write an
// amount of money in.
//
// It is strict where Decimal is not, and has to be. Decimal takes a lone
// separator with three digits after it for a decimal point, which is the
// conservative reading of a measurement and makes 300.000 rupiah three
// hundred. Here a full stop only ever groups thousands and a comma only
// ever starts the decimals, and a cell in any other form is refused, not
// guessed at: a wrong guess is wrong by a factor of a thousand.
//
// So it is for a column known to be in this notation, which is the
// caller's to know. A column of numeric cells arrives without separators
// and is Decimal's to read.
func DecimalDotGrouped(s string) (*decimal.Decimal, error) {
	s = Trim(s)
	if s == "" {
		return nil, nil
	}
	if !dotGrouped.MatchString(s) {
		return nil, fmt.Errorf(
			"%q is not a number written as 1.234.567,89", s)
	}
	plain := strings.ReplaceAll(s, ".", "")
	d, err := decimal.NewFromString(strings.Replace(plain, ",", ".", 1))
	if err != nil {
		return nil, notANumber(s)
	}
	return &d, nil
}

// Code parses a cell that holds a dotted code, 5.1.02.01.01.0024, as the
// number its digits spell once the dots are taken out: 510201010024. It
// returns nil for an empty cell, and refuses one that holds anything but
// digits and dots, or more than MaxCodeDigits digits.
//
// The leading zeros of a code are lost in the number, which is the
// point: the column it is stored in is a number, and two codes that
// differ only in their dots are the same code.
func Code(s string) (*int, error) {
	digits := strings.ReplaceAll(Trim(s), ".", "")
	if digits == "" {
		return nil, nil
	}
	if strings.Trim(digits, "0123456789") != "" ||
		len(digits) > MaxCodeDigits {
		return nil, fmt.Errorf("%q is not a dotted code of at most "+
			"%d digits", s, MaxCodeDigits)
	}
	code, err := strconv.Atoi(digits)
	if err != nil {
		return nil, outOfRange(s)
	}
	return &code, nil
}

// Day returns the calendar date t names, read in t's own location, as
// midnight in time.Local: the form the datetime package gives every
// date-only value, and the one that keeps its day through Value on either
// side of Greenwich. See Date, which also says where a day has no midnight
// to give.
//
// Exported because a caller that recognises a shape this package does not — an
// acquisition month, a bare year — still has to produce a value in the same
// form, and doing it by hand is how one column ends up an hour off from the
// rest.
func Day(t time.Time) *datetime.Datetime {
	d := datetime.Datetime{
		Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0,
			time.Local),
	}
	return &d
}

// Date parses a cell as a date, returning nil for an empty cell.
//
// Two representations are handled:
//
//  1. An Excel serial number ("36573"), which is what a real date cell holds.
//     excelize.ExcelDateToTime performs the conversion, including the
//     1900-leap-year fiction that makes naive arithmetic wrong by a day.
//  2. A text date, for cells typed as text or pasted from elsewhere, or
//     written as text by the program that produced the sheet. A date
//     spelled out may name its month in English or in Indonesian:
//     "2 January 2026", "2 Januari 2026".
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
// The result is the calendar date at midnight in time.Local, built by Day.
// Both parse paths produce the date in UTC — excelize.ExcelDateToTime
// returns it, and time.Parse resolves a layout carrying no zone in UTC — and
// Day keeps its year, month and day while moving it to local midnight. The
// target columns are DATE, so the time part is discarded by the engine.
//
// Local rather than UTC because of what happens on the way to the driver.
// datetime.Datetime.Value converts into time.Local, which leaves a local
// midnight exactly where it is, so the column records the intended date on
// either side of Greenwich. Midnight UTC survives that only east of it: west
// of it, Value converts it to the previous evening and the column records
// the previous day. Local midnight is also the form the datetime package
// gives a date-only value everywhere else, so a date read from a workbook
// and one typed into a form marshal alike.
//
// One date a year can escape that where the clocks go forward at midnight,
// as they do in Havana and Santiago. That day has no midnight, time.Date
// may answer with the last hour of the day before, and a DATE column then
// records the previous day. The datetime package builds its date-only
// values the same way, so the two agree there as well.
func Date(s string, date1904 bool) (*datetime.Datetime, error) {
	s = Trim(s)
	if s == "" {
		return nil, nil
	}

	if serial, err := strconv.ParseFloat(s, 64); err == nil {
		// The serial is bounded BEFORE it is converted. ParseFloat hands
		// over more than a guard on zero covers: "NaN" and "Inf", in any
		// letter case, and a finite value however large. The conversion is
		// unchecked arithmetic on the float, and what it makes of those is
		// not one answer:
		//
		//   - "NaN" compares false against everything, a check on zero
		//     included, which is why it is asked for by name.
		//   - A serial past the range of int, "Inf" and "1e308" among them,
		//     becomes whichever int the processor chooses. On amd64 that is
		//     the minimum, and the date comes back in a year before 1. On
		//     arm64 it is the maximum, the day arithmetic wraps, and the
		//     date comes back in the 2190s.
		//   - A serial that fits an int can still wrap the day arithmetic,
		//     on any processor, and lands on an ordinary date when it wraps
		//     far enough: "144115188075900872", a mistyped run of digits,
		//     is 7 March 2023.
		//
		// A check on the RESULT therefore cannot be trusted with them, and
		// maxSerial keeps them from producing one. Left to the driver, a
		// date no column holds is rejected with a message naming neither
		// the column nor the cell.
		if math.IsNaN(serial) || serial <= 0 || serial >= maxSerial {
			return nil, fmt.Errorf("%q is not a valid Excel date", s)
		}
		t, err := excelize.ExcelDateToTime(serial, date1904)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid Excel date: %w", s, err)
		}
		// Below maxSerial nothing overflows, and the year is all that is
		// left to settle: the 1904 epoch reaches the end of 9999 1462
		// serials sooner, and a time of day within half a second of
		// midnight is rounded up into the day after.
		if t.Year() > 9999 {
			return nil, fmt.Errorf("%q is not a valid Excel date", s)
		}
		return Day(t), nil
	}

	text := englishMonth(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, text); err == nil {
			return Day(t), nil
		}
	}
	return nil, fmt.Errorf("%q is not a recognised date", s)
}
