// Package upstream holds what the packages that reach another server share
// with the configuration sections that describe one: how a section left
// out is told from one filled in wrong, the checks a base URL has to pass
// and the form it is trimmed to, the range a port has to fall in, the
// masking that keeps a credential out of a message about a URL and the
// mark it leaves in the credential's place, the rule for when a credential
// may cross in clear text, the form an HTTP method is compared in, the
// statuses an answer succeeds with and how much of one is kept, and the
// wording of an error that names the step of an exchange it ended.
//
// They live in one place because the callers have to agree, not merely
// resemble each other. A base URL config.ClientConfig.Validate accepts has
// to be one httpclient.New accepts, since both are told the same value; a
// relay config.EmailConfig.Validate lets a credential reach in clear text
// has to be one the email package will log in to, or a configuration that
// validates fails on its first send. Two copies of a rule agree only until
// one of them is edited, and nothing reports the moment they stop.
//
// Two of the rules config also applies to the service's own settings: the
// port range to app.port, and the method form to fiber.request_methods,
// the methods the service's router serves. Each is a rule about a value
// rather than about a peer, and lives here because a package that reaches
// a peer applies it too: database refuses database.port by the first, and
// httpclient matches the method of every request by the second.
//
// The package is internal because none of it is a contract with a
// service. It is the toolkit agreeing with itself, and its API moves
// whenever a caller needs it to.
package upstream
