// Package bytesize holds the rule two packages of the toolkit apply when
// a limit counted in bytes is named in a message: the largest binary unit
// that divides it exactly, and plain bytes when none does.
//
// fileutil names its upload cap this way, through MaxFileLabel, and xlsx
// names MaxUnzippedBytes in the error a workbook that would expand past
// it is refused with. Both tell a reader the number a check enforces, so
// both render it by one rule. Two copies of a rule agree only until one
// of them is edited, and rounding to a fixed unit instead would name a
// cap under one mebibyte as 0 MiB.
//
// The package is internal because none of it is a contract with a
// service. It is the toolkit agreeing with itself, and its API moves
// whenever a caller needs it to.
package bytesize
