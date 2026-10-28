package config

// The rules more than one section applies to keys of its own: a duration
// that has to be set and at least a second, the timeout that bounds a
// notification send, a number that may be zero but not negative, a value
// that has to be one of a fixed few, whether a section was supplied at
// all, what String prints for a section that was never built, and what a
// String prints in place of a secret or of a value left to a default.
// Each section still names its keys and says why a value is wrong for it;
// what lives here is the part of the check, and of the message, that would
// otherwise be written out again in every section that holds such a key.
//
// A rule another package applies as well is internal/upstream's instead.
// The range a port has to fall in is upstream.CheckPort, the error for a
// key left unset is upstream.Required, and the one for a section never
// built is upstream.NotInitialised. Every Validate opens with the last,
// since every line after it dereferences the receiver, and one that
// collects findings declares its errs slice just above it, so all of them
// open with the same two statements in the same order. The form a
// configured word is compared in is strlist.Normalise.
//
// It is a file of its own for the reason splitlist.go is: a section file
// is the authoritative list of one section's keys and nothing else.

import (
	"fmt"
	"strings"
	"time"

	"github.com/mas-ony/go-toolkit/internal/upstream"
)

// loggedCredential is why a credential cannot sit in a URL key: every
// section is printed through String on every start.
const loggedCredential = "the configuration is logged on every start, " +
	"so a credential here reaches the log"

// The labels a String prints in place of a value. Each is in parentheses,
// so the startup line cannot be read as carrying a value somebody wrote.
const (
	// noneLabel stands for a setting left empty, or a secret not set.
	noneLabel = "(none)"

	// middlewareDefaultLabel marks a setting left for the middleware to
	// fill in, where nil is the documented way to select its default
	// rather than an absence.
	middlewareDefaultLabel = "(middleware default)"
)

// configured reports whether c holds any setting at all: it is not nil,
// and not the zero value every key reads as when no source supplied one.
// Each section's Configured method is this, so a section is judged absent
// by one rule whichever package asks.
func configured[T comparable](c *T) bool {
	var zero T
	return c != nil && *c != zero
}

// nilLabel is what a String prints for a nil receiver; typeName is the
// section's type. It is in angle brackets, as uninitialisedLabel is, so
// the startup line cannot be read as carrying a value somebody wrote.
func nilLabel(typeName string) string {
	return "<nil " + typeName + ">"
}

// uninitialisedLabel is what a String prints for a wrapper whose embedded
// middleware config is nil, the other case upstream.NotInitialised
// reports.
func uninitialisedLabel(typeName string) string {
	return "<uninitialised " + typeName + ">"
}

// underASecond is the error for a duration key set below one second.
//
// why says what such a value does to the thing the key configures, or is
// empty where the floor needs no reason of its own. Either way the message
// ends on the likelier explanation: Viper reads a bare number as
// nanoseconds, so "30" meant as seconds lands here.
//
// It formats and does not compare: checkDuration does the comparing, for
// every key the rule applies to, and calls this for a value under a
// second.
func underASecond(key string, got time.Duration, why string) error {
	if why != "" {
		why += ", and "
	}
	return fmt.Errorf("%s must be at least 1s (got %s): %sif this was "+
		"meant as seconds, write the unit — a bare number is nanoseconds",
		key,
		got,
		why)
}

// checkDuration reports a duration key that is unset or under a second,
// the rule for a duration with no default worth inheriting and no meaning
// below a second.
//
// unset is the why upstream.Required is given for an absent key, and short
// the why underASecond is given. A negative value is under a second like
// any other, which is the right answer for every key it reaches. A key
// whose middleware reads a negative value differently from a short one,
// as fiber.session's timeouts do, reports a negative value itself and
// leaves the rest to this.
func checkDuration(key string, d time.Duration, unset, short string) error {
	switch {
	case d == 0:
		return upstream.Required(key, unset)
	case d < time.Second:
		return underASecond(key, d, short)
	}
	return nil
}

// checkSendTimeout reports the timeout of a notification channel that is
// unset or under a second.
//
// It is required because a send that runs detached from a request has no
// other bound, so without it an upstream that stops answering holds the
// send open for good. peer names that upstream as the message calls it,
// "a relay" or "an API", and exchange is the one round of the protocol
// that cannot complete in under a second.
func checkSendTimeout(
	key string,
	timeout time.Duration,
	peer, exchange string,
) error {
	return checkDuration(key, timeout,
		": it is the only bound a send has once it runs detached from a "+
			"request, and without one "+peer+" that stops answering holds "+
			"that send open indefinitely",
		"no "+exchange+" completes in less")
}

// belowZero is the error for a key set below zero, where zero is a valid
// setting and no negative value is. why follows the value, saying what
// such a value does or what to write instead, and is empty where the floor
// needs no reason of its own.
//
// It formats and does not compare, as underASecond does: the section
// holds the comparison, beside the reason its own zero is valid.
func belowZero(key string, got any, why string) error {
	if why != "" {
		why = ": " + why
	}
	return fmt.Errorf("%s cannot be negative (got %v)%s", key, got, why)
}

// notOneOf is the error for a key whose value is none of allowed. It names
// every allowed value in the order given, and a section passes the list
// its check reads, so the message is written from the same declaration as
// the rule and cannot fall behind it.
//
// It formats and does not compare, as underASecond does: a section matches
// its key in whatever form the key is compared in, and the message quotes
// the value as it was written.
func notOneOf(key, got string, allowed []string) error {
	return fmt.Errorf("%s must be one of: %s (got %q)",
		key,
		strings.Join(allowed, ", "),
		got)
}

// alternatives names allowed in the order given, the way a sentence lists
// them: "a", "a or b", "a, b or c". It does for a message worded its own
// way what notOneOf does for its fixed one: the values come from the list
// the check reads, so the message cannot fall behind the rule.
func alternatives(allowed []string) string {
	n := len(allowed)
	if n < 2 {
		return strings.Join(allowed, "")
	}
	return strings.Join(allowed[:n-1], ", ") + " or " + allowed[n-1]
}

// setOrNone renders a secret for a String: whether it is set, and nothing
// of what it holds.
func setOrNone(set bool) string {
	if set {
		return "(set)"
	}
	return noneLabel
}
