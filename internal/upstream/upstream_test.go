package upstream

// Tests for upstream.go.
//
// What the helpers promise: a URL quoted in an error or a log line shows no
// userinfo, whatever characters the credential holds; the cause url.Parse
// reports keeps its meaning while repeating no part of the input; a base
// URL is trimmed and refused the same way whoever asks; a key left unset
// and a section never built are each refused in one wording; a port is
// held to one range; a section left out is told from one filled in wrong,
// and only the second is validated; the loopback rule is net/smtp's,
// spelling for spelling; a success is a 2xx and nothing else; a method is
// compared upper-cased; and an error naming a step still answers
// errors.Is for the context that ended it.

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// section is an Optional that answers as the test sets it, and records
// whether it was validated. A nil one reports itself absent, as a nil
// pointer to one of config's sections does.
type section struct {
	supplied bool
	invalid  error
	asked    bool
}

// Configured reports what the test set, and false for a nil section.
func (s *section) Configured() bool { return s != nil && s.supplied }

// Validate records that it was asked and returns what the test set.
func (s *section) Validate() error {
	s.asked = true
	return s.invalid
}

// Userinfo is masked in every shape a credential can take, including the
// ones that make url.Parse fail, and a URL without one is left alone.
func TestRedactUserinfo(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://reports.example":        "https://reports.example",
		"https://reports.example/api":    "https://reports.example/api",
		"https://u:p@reports.example":    "https://***@reports.example",
		"https://u:p@api.example/api/v1": "https://***@api.example/api/v1",
		"https://u@api.example":          "https://***@api.example",
		// A password may itself contain "@", "/" or a space.
		"https://u:p@ss@api.example": "https://***@api.example",
		"https://u:p/ss@api.example": "https://***@api.example",
		"https://u:p ss@api.example": "https://***@api.example",
		// An "@" in the query takes the host with it: the conservative
		// direction, as RedactUserinfo documents.
		"https://api.example/x?to=a@b": "https://***@b",
		// With no scheme there is nothing to keep in front of it.
		"u:p@api.example": "***@api.example",
		"":                "",
	} {
		if got := RedactUserinfo(raw); got != want {
			t.Errorf("RedactUserinfo(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The cause url.Parse reports survives, and no part of the input does:
// neither the copy url.Error carries nor a fragment the cause quotes. The
// port cases are the ones worth having: a "/", "?" or "#" inside the
// password ends the authority early, and the port error that follows
// quotes the password itself.
func TestParseCauseDropsTheInput(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		raw    string // a URL url.Parse refuses
		secret string // the part of it that must not be repeated
		cause  string // what the message still has to say
	}{
		{"https://admin:hunter2 x@reports.example", "hunter2",
			"invalid userinfo"},
		{"https://admin:hunter2/x@reports.example", "hunter2",
			"invalid port"},
		{"https://admin:hunter2?x@reports.example", "hunter2",
			"invalid port"},
		{"https://admin:hunter2#x@reports.example", "hunter2",
			"invalid port"},
		{"https://admin:hu%zzter2@reports.example", "zz",
			"invalid URL escape"},
	} {
		_, err := url.Parse(c.raw)
		if err == nil {
			t.Errorf("url.Parse(%q) succeeded", c.raw)
			continue
		}
		got := ParseCause(err).Error()
		if strings.Contains(got, c.secret) {
			t.Errorf("ParseCause(%q) kept %q: %q", c.raw, c.secret, got)
		}
		if !strings.Contains(got, c.cause) {
			t.Errorf("ParseCause(%q) lost the cause: %q", c.raw, got)
		}
	}
	if ParseCause(nil) != nil {
		t.Error("ParseCause(nil) is not nil")
	}
}

// The reason a key is required follows the key as written: a condition
// opens with a space, a reason with a colon, and none leaves the bare
// requirement.
func TestRequired(t *testing.T) {
	t.Parallel()
	for why, want := range map[string]string{
		"":              "x.key is required",
		": a reason":    "x.key is required: a reason",
		" when x is on": "x.key is required when x is on",
	} {
		if got := Required("x.key", why).Error(); got != want {
			t.Errorf("Required(%q) = %q, want %q", why, got, want)
		}
	}
}

// A section never built is named as the caller names it, a prefix or a
// plain word, and the message says nothing about any value in it.
func TestNotInitialised(t *testing.T) {
	t.Parallel()
	for section, want := range map[string]string{
		"x.section": "x.section config was not initialised",
		"client":    "client config was not initialised",
	} {
		if got := NotInitialised(section).Error(); got != want {
			t.Errorf("NotInitialised(%q) = %q, want %q",
				section, got, want)
		}
	}
}

// A port is required, held to 1..65535, and reported once whatever is
// wrong with it.
func TestCheckPort(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		port    int
		mention string
	}{
		{0, "x.port is required"},
		{-1, "x.port must be between 1 and 65535 (got -1)"},
		{65536, "x.port must be between 1 and 65535 (got 65536)"},
		{1, ""},
		{65535, ""},
	} {
		err := CheckPort("x.port", c.port)
		switch {
		case c.mention == "" && err != nil:
			t.Errorf("CheckPort(%d) = %v, want nil", c.port, err)
		case c.mention != "" && (err == nil || err.Error() != c.mention):
			t.Errorf("CheckPort(%d) = %v, want %q", c.port, err, c.mention)
		}
	}
}

// An absent section is answered with the caller's sentinel and is not
// validated, since nothing in it can be wrong. A supplied one is answered
// by Validate, whatever that says, so a broken section never reads as an
// absent one.
func TestCheckSection(t *testing.T) {
	t.Parallel()
	absent := errors.New("absent")
	broken := errors.New("broken")

	var none *section
	if err := CheckSection(none, absent); !errors.Is(err, absent) {
		t.Errorf("a nil section: got %v, want the absent sentinel", err)
	}

	unsupplied := &section{invalid: broken}
	if err := CheckSection(unsupplied, absent); !errors.Is(err, absent) {
		t.Errorf("an unsupplied section: got %v, want the absent "+
			"sentinel", err)
	}
	if unsupplied.asked {
		t.Error("an unsupplied section was validated")
	}

	half := &section{supplied: true, invalid: broken}
	if err := CheckSection(half, absent); !errors.Is(err, broken) {
		t.Errorf("a broken section: got %v, want Validate's error", err)
	}

	whole := &section{supplied: true}
	if err := CheckSection(whole, absent); err != nil {
		t.Errorf("a valid section: got %v, want nil", err)
	}
	if !whole.asked {
		t.Error("a supplied section was not validated")
	}
}

// Exactly the three spellings net/smtp's PlainAuth accepts, matched as
// written. The others all reach the loopback interface or look as if they
// should, and are refused, because a caller that accepted one of them
// would draw the line somewhere net/smtp does not.
func TestIsLoopback(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"localhost":          true,
		"127.0.0.1":          true,
		"::1":                true,
		"LOCALHOST":          false,
		"127.0.0.2":          false,
		"[::1]":              false,
		"0.0.0.0":            false,
		"smtp.example.go.id": false,
		"":                   false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %t, want %t", host, got, want)
		}
	}
}

// A base or a path prefix loses its surrounding space and every trailing
// slash, and nothing inside it: a slash that is not trailing stays put.
func TestTrimBase(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://api.example.go.id":    "https://api.example.go.id",
		" https://api.example.go.id/ ": "https://api.example.go.id",
		"https://api.example.go.id//":  "https://api.example.go.id",
		"/api/v1/":                     "/api/v1",
		" /api//v1 ":                   "/api//v1",
		"https://":                     "https:",
		" / ":                          "",
		"":                             "",
	} {
		if got := TrimBase(raw); got != want {
			t.Errorf("TrimBase(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Every 2xx is a success and nothing either side of the range is,
// redirects included.
func TestIsSuccess(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]bool{
		199: false,
		200: true,
		204: true,
		299: true,
		300: false,
		302: false,
		404: false,
		500: false,
		0:   false,
	} {
		if got := IsSuccess(code); got != want {
			t.Errorf("IsSuccess(%d) = %t, want %t", code, got, want)
		}
	}
}

// A method is compared upper-cased, whatever case it was written in, and
// nothing else about it changes: space around one is left for the check
// that refuses it.
func TestNormaliseMethod(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"POST":  "POST",
		"post":  "POST",
		"Patch": "PATCH",
		" get ": " GET ",
		"":      "",
	} {
		if got := NormaliseMethod(raw); got != want {
			t.Errorf("NormaliseMethod(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A base is accepted in the forms somebody pastes from a browser, with the
// surrounding space and trailing slashes gone from what is returned.
func TestParseBaseURLAcceptsAPastedBase(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ raw, want string }{
		{"https://api.example.go.id", "https://api.example.go.id"},
		{" https://api.example.go.id/ ", "https://api.example.go.id"},
		{"http://127.0.0.1:8080/api/v1//", "http://127.0.0.1:8080/api/v1"},
	} {
		u, err := ParseBaseURL("base", c.raw, "advice")
		if err != nil {
			t.Errorf("ParseBaseURL(%q): %v", c.raw, err)
			continue
		}
		if got := u.String(); got != c.want {
			t.Errorf("ParseBaseURL(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// Each refusal names the URL as the caller called it, says what is wrong,
// and repeats no credential the URL held. The advice closes the userinfo
// refusal and no other.
func TestParseBaseURLRefusals(t *testing.T) {
	t.Parallel()
	const advice = "put it in the token instead"
	for _, c := range []struct {
		raw     string
		problem string
		advice  bool
	}{
		{"", "must start with http:// or https://", false},
		{"api.example.go.id", "must start with http:// or https://", false},
		{"ftp://api.example.go.id", "must start with http://", false},
		{"https://", "names no host", false},
		{"https:///path", "names no host", false},
		{"https://svc:hunter2@api.example", "must not carry userinfo", true},
		{"https://svc@api.example", "must not carry userinfo", true},
		{"https://svc:hunter2 x@api.example", "is not a valid URL", false},
		{"https://svc:hunter2/x@api.example", "is not a valid URL", false},
		{"https://svc:hunter2?x@api.example", "is not a valid URL", false},
		{"https://svc:hunter2#x@api.example", "is not a valid URL", false},
	} {
		u, err := ParseBaseURL("app.base_url", c.raw, advice)
		if err == nil {
			t.Errorf("ParseBaseURL(%q) = %v, want an error", c.raw, u)
			continue
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "app.base_url "+c.problem) {
			t.Errorf("ParseBaseURL(%q): %q, want it to open with %q",
				c.raw, msg, "app.base_url "+c.problem)
		}
		if strings.Contains(msg, "hunter2") {
			t.Errorf("ParseBaseURL(%q) quotes the password: %q", c.raw, msg)
		}
		if got := strings.HasSuffix(msg, advice); got != c.advice {
			t.Errorf("ParseBaseURL(%q): ends with the advice = %t, "+
				"want %t: %q", c.raw, got, c.advice, msg)
		}
	}
}

// The step leads the message, and an error that ends the step stays
// reachable.
func TestStepErrNamesTheStep(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection reset by peer")
	err := StepErr(context.Background(), "DATA", cause)
	want := "DATA: connection reset by peer"
	if got := err.Error(); got != want {
		t.Errorf("StepErr = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Errorf("StepErr = %v, want it to wrap the cause", err)
	}
}

// A deadline that surfaced as something else is still reachable through
// errors.Is, and one the cause already reports is not repeated.
func TestStepErrCarriesAnEndedContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	timeout := errors.New("i/o timeout")
	err := StepErr(ctx, "connect", timeout)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, timeout) {
		t.Errorf("StepErr = %v, want both context.Canceled and the cause",
			err)
	}

	said := StepErr(ctx, "connect", context.Canceled)
	named := strings.Count(said.Error(), context.Canceled.Error())
	if named != 1 {
		t.Errorf("StepErr = %q, names the context %d times, want once",
			said, named)
	}
	if !errors.Is(said, context.Canceled) {
		t.Errorf("StepErr = %v, want context.Canceled", said)
	}
}

// The *url.Error net/http wraps a failed request in quotes the URL, which
// can carry a token, so it is dropped and what it wrapped kept.
func TestStepErrDropsTheURL(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection refused")
	wrapped := &url.Error{
		Op:  "Post",
		URL: "https://api.example/bot123:secret/sendMessage",
		Err: cause,
	}
	err := StepErr(context.Background(), "request", wrapped)
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("StepErr = %q, want it without the URL", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("StepErr = %v, want it to wrap the cause", err)
	}
}
