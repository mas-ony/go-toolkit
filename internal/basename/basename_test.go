package basename

// Tests for basename.go: every answer filepath.Base gives for a path that
// names no file is refused, and an ordinary name is not, however many
// dots it carries.

import (
	"path/filepath"
	"testing"
)

// What Base makes of an empty path, a climb, and a run of separators, and
// the "" a trim can leave, all name no file.
func TestNamesNoFileRefusesWhatBaseLeavesOfNoFile(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", "..", "a/..", "///",
		string(filepath.Separator)} {
		if base := filepath.Base(path); !NamesNoFile(base) {
			t.Errorf("Base(%q) = %q, which NamesNoFile accepts",
				path, base)
		}
	}
	if !NamesNoFile("") {
		t.Error(`NamesNoFile("") = false, want true`)
	}
}

// A name is a name, dotfiles and dotted stems included.
func TestNamesNoFileAcceptsAName(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"scan.pdf", ".hidden", "...", "a..b",
		"35.002"} {
		if NamesNoFile(base) {
			t.Errorf("NamesNoFile(%q) = true, want false", base)
		}
	}
}
