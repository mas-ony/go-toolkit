// Package basename holds the rule two packages of the toolkit apply to
// the last element of a path: which results of filepath.Base name no file
// at all.
//
// fileutil folds such a result onto its fallback name before anything is
// stored under it, and xlsx refuses a hyperlink whose target ends in one.
// Both ask the same question of the same function's output, so it is
// answered in one place. Two copies of the list agree only until one of
// them is edited, and nothing reports the moment they stop.
//
// The package is internal because none of it is a contract with a
// service. It is the toolkit agreeing with itself, and its API moves
// whenever a caller needs it to.
package basename
