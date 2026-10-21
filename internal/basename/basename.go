package basename

import "path/filepath"

// NamesNoFile reports whether base, a result of filepath.Base, names
// something other than a file. Base answers "." for an empty path, ".."
// for one that only climbs, and a lone separator for one made of
// separators alone. "" is refused as well: Base never returns it, but a
// caller that trims what Base returned can be left holding it.
//
// The separator is the platform's, because that is the one Base returns:
// "/" here, `\` on Windows. Writing both as literals instead would be a
// duplicate case on Windows, where string(filepath.Separator) is `\`, and
// a compile error there. A backslash on any other platform is an ordinary
// character, and a name made of one is a name.
func NamesNoFile(base string) bool {
	switch base {
	case "", ".", "..", string(filepath.Separator):
		return true
	}
	return false
}
