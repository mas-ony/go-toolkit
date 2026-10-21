// Package strlist holds the word and list rules more than one package of
// the toolkit applies: what a comma-separated value splits into, the form
// a word is normalised to before it is looked up, and how a value is
// looked up in a list whose spellings are matched without regard to case.
//
// A query parameter in request and a list key in config are the same
// shape, "a, b,,c", and both are read the same way, so one function says
// what a list is for both. A word matched against a fixed vocabulary is
// trimmed and lowercased before the lookup, wherever it is read: a
// channel or a TLS mode in config, a driver name or an encrypt mode for
// the database, a boolean or a sort direction in a query, a file
// extension in fileutil. The case-folded lookup serves config's SameSite
// allowlist and the SMTP AUTH mechanisms email picks from, the two places
// a list keeps its own spellings and is compared under case folding rather
// than normalised and looked up in a map.
//
// The package is internal because all three are conveniences for the
// toolkit's own packages, not an API a service is meant to depend on.
package strlist
