package strlist

// Tests for strlist.go: what a comma-separated value splits into, including
// the nil that tells an absent list from an empty one, the form a word is
// normalised to, and the lookup that ignores case.

import (
	"reflect"
	"testing"
)

// Split trims each piece and drops the empty ones, and answers nil only for
// a value with nothing in it at all. DeepEqual tells nil from empty, so the
// last two cases hold the second half of that.
func TestSplit(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"":          nil,
		"   ":       nil,
		"a":         {"a"},
		" a , b ":   {"a", "b"},
		",a,,b,":    {"a", "b"},
		"a:desc,b":  {"a:desc", "b"},
		"GET, POST": {"GET", "POST"},
		",":         {},
		" , , ":     {},
	}
	for raw, want := range cases {
		if got := Split(raw); !reflect.DeepEqual(got, want) {
			t.Errorf("Split(%q) = %#v, want %#v", raw, got, want)
		}
	}
}

// Normalise trims before it lowercases, so space of any kind around a word
// is gone and the word inside it is left whole.
func TestNormalise(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":             "",
		"strict":       "strict",
		" STRICT\t":    "strict",
		"\nStartTLS ":  "starttls",
		"two words":    "two words",
		" Two  Words ": "two  words",
	}
	for raw, want := range cases {
		if got := Normalise(raw); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", raw, got, want)
		}
	}
}

// ContainsFold matches under case folding and finds nothing that is not in
// the list, least of all in an empty one. A misspelling is absent however
// it is capitalised.
func TestContainsFold(t *testing.T) {
	t.Parallel()
	list := []string{"Strict", "Lax"}
	if !ContainsFold(list, "strict") || !ContainsFold(list, "LAX") {
		t.Error("ContainsFold should ignore case")
	}
	if ContainsFold(list, "Strcit") || ContainsFold(nil, "Lax") {
		t.Error("ContainsFold found something absent")
	}
}
