package dbdriver

import "github.com/mas-ony/go-toolkit/internal/strlist"

// The engines a database.driver name selects, each spelled as the
// database/sql driver name the database package opens it under.
const (
	SQLServer = "sqlserver"
	MySQL     = "mysql"
)

// Encryption is what one database.encrypt value asks of each engine's
// driver.
type Encryption struct {
	// SQLServer is the value of go-mssqldb's encrypt= parameter.
	SQLServer string

	// MySQL is the value of go-sql-driver/mysql's tls= parameter.
	MySQL string
}

// vocabulary is the words one key accepts, each paired with what it
// selects, in the order an error lists them.
type vocabulary[T any] []struct {
	word  string
	value T
}

// lookup returns what word selects, and false for a word outside the
// vocabulary. Case and surrounding space are ignored.
func (v vocabulary[T]) lookup(word string) (T, bool) {
	w := strlist.Normalise(word)
	for _, e := range v {
		if e.word == w {
			return e.value, true
		}
	}
	var zero T
	return zero, false
}

// words returns the vocabulary's words in order, in a slice the caller may
// keep.
func (v vocabulary[T]) words() []string {
	out := make([]string, len(v))
	for i, e := range v {
		out[i] = e.word
	}
	return out
}

// drivers pairs each name database.driver accepts with the engine it
// selects. "mssql" is the legacy alias go-mssqldb registers for the same
// driver, so it selects the same engine.
var drivers = vocabulary[string]{
	{"sqlserver", SQLServer},
	{"mssql", SQLServer},
	{"mysql", MySQL},
}

// encryptions pairs each value database.encrypt accepts with what it asks
// of each driver. EncryptionFor says what the table means and what it does
// not.
var encryptions = vocabulary[Encryption]{
	{"disable", Encryption{SQLServer: "disable", MySQL: "false"}},
	{"false", Encryption{SQLServer: "false", MySQL: "preferred"}},
	{"true", Encryption{SQLServer: "true", MySQL: "true"}},
	{"strict", Encryption{SQLServer: "strict", MySQL: "true"}},
}

// Engine returns the engine a database.driver value selects, SQLServer or
// MySQL, and "" for a name the database package cannot open. Case and
// surrounding space are ignored.
func Engine(driver string) string {
	engine, _ := drivers.lookup(driver)
	return engine
}

// Names returns the names database.driver accepts, in the order an error
// lists them. The slice is the caller's to keep.
func Names() []string { return drivers.words() }

// EncryptionFor returns what a database.encrypt value asks of each driver,
// and false for a value that is none of the four. Case and surrounding
// space are ignored, so "True" and " STRICT " resolve.
//
//	database.encrypt   sqlserver encrypt=   mysql tls=
//	disable            disable              false
//	false              false                preferred
//	true               true                 true
//	strict             strict               true
//
// The vocabulary is SQL Server's because one of the two drivers already
// spells it that way. Without the translation the mysql driver would be
// handed no TLS parameter at all and connect on whatever it defaulted to,
// unencrypted, while the configuration carried an "encrypt" setting that
// read as if it applied. That is the gap the table closes, and it is why
// the key is required for both drivers rather than only the one whose
// vocabulary it uses.
//
// The vocabulary is closed rather than forwarded to the driver, because
// the value is TRANSLATED: an unrecognised one has no defined meaning on
// mysql, and there is no driver left to hand it to. One practical
// consequence: go-mssqldb accepts a wider vocabulary on its own —
// "mandatory", "yes", "1" and "t" all mean "true" to it, and "optional",
// "no", "0" and "f" mean "false" — so a configuration carrying
// "encrypt: 0" or "encrypt: yes" fails at startup and needs the canonical
// spelling. That failure names the key.
//
// Three things the table does not say, all of which matter before trusting
// a value:
//
//   - "false" means genuinely different things. On SQL Server it still
//     encrypts the LOGIN PACKET, so credentials never cross the network in
//     clear text; only the session after it does. On MySQL the equivalent
//     is opportunistic and unverified: a server that offers no TLS gets a
//     plaintext connection, and one that does gets an unauthenticated
//     tunnel. Do not read "false" as "at least the password is safe"
//     outside SQL Server.
//
//   - "true" and "strict" collapse into one setting on MySQL. It has no
//     analogue of TDS 8.0, where TLS precedes the protocol handshake
//     rather than being negotiated inside it, and it already verifies the
//     certificate chain and the hostname at the "true" row. Keeping the two
//     distinct in config means a deployment can change engines without
//     editing this key, which is the entire point of the indirection.
//
//   - The verifying rows need a certificate the CLIENT trusts, and the
//     trust store is not configurable. MySQL verifies against the system
//     pool unless a tls.Config is registered with mysql.RegisterTLSConfig,
//     and the database package does not expose that — so a private CA or a
//     self-signed server certificate fails at connect on "true" and
//     "strict", the same wall as SQL Server without
//     trustservercertificate, and the usual reason a deployment slides
//     back to "disable". Adding a database.tls_root_cert key is the fix if
//     that comes up; loosening the mapping to skip-verify is not, because
//     it would make "true" mean "encrypted but unauthenticated" on one of
//     the two engines and nothing in the config would say so.
func EncryptionFor(mode string) (Encryption, bool) {
	return encryptions.lookup(mode)
}

// EncryptModes returns the values database.encrypt accepts, in the order an
// error lists them. The slice is the caller's to keep.
func EncryptModes() []string { return encryptions.words() }
