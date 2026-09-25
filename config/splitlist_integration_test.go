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
	"reflect"
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitList = %#v, want %#v", got, want)
	}

	// The failure it prevents, stated beside it: the default reader
	// returns ONE element.
	if raw := v.GetStringSlice("zqlist.items"); len(raw) != 1 {
		t.Logf("GetStringSlice now splits on commas (%#v); splitList's "+
			"comment describing the hazard should be revisited", raw)
	}
}
