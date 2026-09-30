package config

// Tests for splitlist.go.
//
// splitList exists because the same key arrives in two shapes depending on
// where it came from, and the obvious environment spelling breaks the
// default reader SILENTLY: cast.ToStringSlice splits a bare string on
// whitespace, so "a,b,c" becomes the single element "a,b,c". For
// fiber.request_methods that means a router whose only method is a string
// no client sends: the first route registered panics at startup, and any
// request that reaches the router is answered 501.
//
// The file shape and the comma shape are both reachable in memory — Set
// puts a bare string where an environment variable would put one — so
// none of this needs a real variable.

import (
	"slices"
	"testing"

	"github.com/spf13/viper"
)

// Every shape a list key arrives in reads as the same elements: a YAML
// sequence, a comma-separated string with or without stray spaces and
// empty elements, and a whitespace-separated one.
func TestSplitList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		want  []string
	}{
		{
			// What config.yaml produces, and what GetStringSlice already
			// handles; splitList must pass it through untouched.
			name:  "a YAML sequence",
			value: []any{"status", "method", "latency"},
			want:  []string{"status", "method", "latency"},
		},
		{
			// The spelling an operator reaches for in an environment
			// variable, and the reason this helper exists.
			name:  "a comma-separated string",
			value: "status,method,latency",
			want:  []string{"status", "method", "latency"},
		},
		{
			name:  "commas with surrounding space",
			value: " status , method ,latency ",
			want:  []string{"status", "method", "latency"},
		},
		{
			// Empty elements are dropped rather than kept as "", which a
			// method list or a field list would otherwise carry through.
			name:  "empty elements and trailing commas",
			value: "status,,method,",
			want:  []string{"status", "method"},
		},
		{
			// No comma: the whitespace-splitting fallback, which is the
			// other spelling an environment variable can take.
			name:  "a whitespace-separated string",
			value: "status method latency",
			want:  []string{"status", "method", "latency"},
		},
		{
			name:  "a single element",
			value: "status",
			want:  []string{"status"},
		},
		{
			// A sequence element that itself contains a comma is NOT
			// split: GetString on a sequence fails its cast and yields
			// "", so the comma branch is never taken for a file value.
			name:  "a sequence element containing a comma",
			value: []any{"a,b", "c"},
			want:  []string{"a,b", "c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := viper.New()
			v.Set("zqunit.list", tc.value)

			got := splitList(v, "zqunit.list")
			if !slices.Equal(got, tc.want) {
				t.Errorf("splitList = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// Nothing set, and only commas: both are "no list", and neither may
// produce a list holding an empty string — which a caller checking
// len() > 0 would read as one requested element.
func TestSplitListHasNoPhantomElements(t *testing.T) {
	t.Parallel()

	v := viper.New()
	if got := splitList(v, "zqunit.absent"); len(got) != 0 {
		t.Errorf("an absent key = %#v, want no elements", got)
	}

	v.Set("zqunit.commas", ",,,")
	if got := splitList(v, "zqunit.commas"); len(got) != 0 {
		t.Errorf("a value of only commas = %#v, want no elements", got)
	}
}

// An absent key and a stated empty list both have length zero and mean
// different things: nil lets a middleware substitute its own default,
// while a made-but-empty list is taken as written. The zerolog field and
// level lists and fiber.client.no_retry_methods all turn on that
// difference, so it is pinned here rather than assumed of spf13/cast.
func TestSplitListKeepsAbsentAndEmptyApart(t *testing.T) {
	t.Parallel()

	if got := splitList(viper.New(), "zqunit.absent"); got != nil {
		t.Errorf("an absent key = %#v, want nil", got)
	}

	commas := viper.New()
	commas.Set("zqunit.list", ",")
	for name, v := range map[string]*viper.Viper{
		"a YAML []":   yamlViper(t, "zqunit:\n  list: []\n"),
		"only commas": commas,
	} {
		if got := splitList(v, "zqunit.list"); got == nil || len(got) != 0 {
			t.Errorf("%s = %#v, want a non-nil empty list", name, got)
		}
	}
}
