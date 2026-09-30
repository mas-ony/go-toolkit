//go:build integration

package config

// Integration tests for splitlist.go.
//
// splitList exists because a list key reaches Viper as a bare string when
// it comes from the environment, and the default reader splits that on
// whitespace — so the comma spelling an operator reaches for yields one
// element holding the whole string. The unit suite reproduces that with
// Set; this reproduces it with a real variable, which is the case the
// helper was written for.
//
//	go test -tags integration -run Integration ./config

import (
	"slices"
	"testing"
)

// splitList's whole reason to exist, end to end: the comma spelling an
// operator reaches for in a real environment variable yields the list
// they meant rather than one element holding the whole string.
func TestIntegrationListKeyFromARealVariable(t *testing.T) {
	t.Setenv("ZQLIST_ITEMS", "status,method,latency")

	v := NewViper()
	got := splitList(v, "zqlist.items")
	want := []string{"status", "method", "latency"}
	if !slices.Equal(got, want) {
		t.Errorf("splitList = %#v, want %#v", got, want)
	}

	// The failure it prevents, stated beside it: the default reader
	// returns ONE element. This is the premise splitList's documentation
	// rests on, so a version of spf13/cast that splits on commas fails the
	// test, as NewViper's premise does in section_integration_test.go,
	// rather than leaving that documentation describing a hazard that is
	// not there.
	if raw := v.GetStringSlice("zqlist.items"); len(raw) != 1 {
		t.Errorf("GetStringSlice read %d elements (%#v), not one: the "+
			"whitespace-only split splitList's documentation describes "+
			"does not hold for this version of spf13/cast, so revisit "+
			"that comment",
			len(raw), raw)
	}
}
