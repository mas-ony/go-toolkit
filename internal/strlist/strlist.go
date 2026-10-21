package strlist

import "strings"

// Split cuts a comma-separated value into trimmed, non-empty pieces. It
// returns nil for a value that is empty or all space, and an empty, non-nil
// slice for one that holds only commas and space, so a caller can tell a
// list nobody wrote from a list written empty.
//
// The pieces share memory with s. A caller holding s in a buffer that is
// reused, as a request's query is, clones each piece it keeps.
func Split(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Normalise returns the form a word is looked up in: surrounding space
// trimmed, then lowercased. A word matched against a fixed vocabulary is
// normalised this way wherever it is read, so a value written " STRICT"
// in one place and compared in another is the same word in both.
func Normalise(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ContainsFold reports whether s equals any element of list under Unicode
// case folding.
func ContainsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
