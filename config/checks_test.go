package config

// Tests for checks.go.
//
// The sections' own rule tables already hold each key to these rules
// through Validate. What is pinned here is the part they share, once: a
// section never built printed by its type, an absent value reported as
// required and never also as too short, a valid value let through, a
// message that names the key and ends on the advice about a missing unit,
// a negative value printed as its type prints it, and a list of allowed
// values named in the order it was given. The wording of a key left unset
// and of a section never built is upstream's, and pinned there.

import (
	"strings"
	"testing"
	"time"
)

// A section never built is printed by its type in angle brackets, so the
// line holds no value.
func TestNeverBuiltLabels(t *testing.T) {
	t.Parallel()
	for want, got := range map[string]string{
		"<nil XConfig>":           nilLabel("XConfig"),
		"<uninitialised XConfig>": uninitialisedLabel("XConfig"),
	} {
		if got != want {
			t.Errorf("got %q\nwant %q", got, want)
		}
	}
}

// The reason, when there is one, leads into the advice about a missing
// unit, and the message reads whole without one.
func TestUnderASecond(t *testing.T) {
	t.Parallel()
	got := underASecond("x.timeout", 30, "a reason").Error()
	want := "x.timeout must be at least 1s (got 30ns): a reason, and if " +
		"this was meant as seconds, write the unit — a bare number is " +
		"nanoseconds"
	if got != want {
		t.Errorf("with a reason:\n got %q\nwant %q", got, want)
	}

	got = underASecond("x.timeout", -time.Second, "").Error()
	want = "x.timeout must be at least 1s (got -1s): if this was meant as " +
		"seconds, write the unit — a bare number is nanoseconds"
	if got != want {
		t.Errorf("without one:\n got %q\nwant %q", got, want)
	}
}

// A duration is required, at least a second, and reported once whatever
// is wrong with it: an absent one carries the reason it is required and
// nothing about its length, and a short or negative one the reason it is
// too short.
func TestCheckDuration(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "x.ttl is required: a reason"},
		{30, "x.ttl must be at least 1s (got 30ns): too short, and if " +
			"this was meant as seconds, write the unit — a bare number " +
			"is nanoseconds"},
		{-time.Minute, "x.ttl must be at least 1s (got -1m0s): too " +
			"short, and if this was meant as seconds, write the unit — a " +
			"bare number is nanoseconds"},
		{time.Second, ""},
	} {
		err := checkDuration("x.ttl", c.d, ": a reason", "too short")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("checkDuration(%s) = %v, want nil", c.d, err)
		case c.want != "" && (err == nil || err.Error() != c.want):
			t.Errorf("checkDuration(%s) = %v, want %q", c.d, err, c.want)
		}
	}
	if err := checkDuration("x.ttl", 0, "", ""); err == nil ||
		err.Error() != "x.ttl is required" {
		t.Errorf("without a reason: %v", err)
	}
}

// The value is printed as its type prints it, a count or a duration, and
// the reason, when there is one, follows it.
func TestBelowZero(t *testing.T) {
	t.Parallel()
	for want, err := range map[string]error{
		"x.count cannot be negative (got -1)": belowZero(
			"x.count", -1, ""),
		"x.wait cannot be negative (got -2s): use 0 for none": belowZero(
			"x.wait", -2*time.Second, "use 0 for none"),
	} {
		if got := err.Error(); got != want {
			t.Errorf("got %q\nwant %q", got, want)
		}
	}
}

// The allowed values are named in the order given, and the value is
// quoted as it was written.
func TestNotOneOf(t *testing.T) {
	t.Parallel()
	got := notOneOf("x.mode", " Fast", []string{"slow", "fast"}).Error()
	want := `x.mode must be one of: slow, fast (got " Fast")`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A list in words names its values in the order given, the last two
// joined by "or".
func TestAlternatives(t *testing.T) {
	t.Parallel()
	for want, allowed := range map[string][]string{
		"":          nil,
		"a":         {"a"},
		"a or b":    {"a", "b"},
		"a, b or c": {"a", "b", "c"},
	} {
		if got := alternatives(allowed); got != want {
			t.Errorf("alternatives(%q) = %q, want %q", allowed, got, want)
		}
	}
}

// A send timeout is required, at least a second, and the two refusals name
// the upstream and the exchange the caller passed.
func TestCheckSendTimeout(t *testing.T) {
	t.Parallel()
	err := checkSendTimeout("x.timeout", 0, "a relay", "SMTP conversation")
	if err == nil || !strings.HasPrefix(err.Error(),
		"x.timeout is required") ||
		!strings.Contains(err.Error(), "a relay that stops answering") {
		t.Errorf("absent: %v", err)
	}

	err = checkSendTimeout("x.timeout", 30, "a relay", "SMTP conversation")
	if err == nil || !strings.Contains(err.Error(), "at least 1s") ||
		!strings.Contains(err.Error(), "no SMTP conversation completes") {
		t.Errorf("a bare number: %v", err)
	}

	for _, d := range []time.Duration{time.Second, time.Minute} {
		if err := checkSendTimeout("x.timeout", d, "a relay",
			"SMTP conversation"); err != nil {
			t.Errorf("%s: %v, want nil", d, err)
		}
	}
}

// A secret is shown as set or not, and never as itself.
func TestSetOrNone(t *testing.T) {
	t.Parallel()
	if got := setOrNone(true); got != "(set)" {
		t.Errorf("setOrNone(true) = %q", got)
	}
	if got := setOrNone(false); got != "(none)" {
		t.Errorf("setOrNone(false) = %q", got)
	}
}
