// Package logfield holds the fields more than one package of the toolkit
// writes on the same kind of log line: the name of the handler that
// wrote it, which rest gives every handler's logger and server its error
// and panic handlers'; the method and the path on a line about a
// request, which rest writes for a handler's failures and server for an
// error or a panic that escapes a route; the value and the stack on a
// line about a recovered panic, which server writes for a route and
// notify for a notification; the entity on a line about the rows of one
// table, which crud writes for each row a service writes and notify for
// the panic of a notification about one; and the recipient, the file and
// the message id on a line about a delivered notification, which email,
// telegram and whatsapp write for each message they send.
//
// Each field is written in one place so the lines read alike wherever
// they come from: a search for the lines of one path, for every
// recovered panic, for the rows of one table, or for every message sent
// to one recipient, finds all of them under one key. Two copies of a key
// agree only until one of them is edited, and nothing reports the moment
// they stop. The keys of a delivery are declared here rather than
// written, because each channel writes them with a type of its own.
//
// The package imports zerolog alone. A line about a request is handed
// the method and the path rather than the fiber.Ctx they are read from,
// so notify, which serves no request, links no web framework to log a
// panic.
//
// The package is internal because none of it is a contract with a
// service. It is the toolkit agreeing with itself, and its API moves
// whenever a caller needs it to.
package logfield
