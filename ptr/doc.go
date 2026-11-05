// Package ptr reads the optional values the toolkit models as pointers,
// where nil is "absent" and a zero is a value somebody chose: the fields
// of a model, a decoded request body, and the parameters package request
// returns.
//
// Value is the read that needs no decision of its own: what the pointer
// points to, or the zero value of its type when it is nil. A read with a
// fallback of its own is cmp.Or over Value, which takes the fallback for
// a zero value as well as for nil:
//
//	nama := cmp.Or(ptr.Value(u.Nama), "-")
//
// Taking the address of a value needs no helper: new(v) does that since
// Go 1.26.
//
// # What the tests hold in place
//
// Value reads through a pointer, and reads nil as the zero value, for a
// number, a string and a struct alike.
package ptr
