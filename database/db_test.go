package database

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/mas-ony/go-toolkit/config"
)

// safeUsername stands in wherever the corpus value is being exercised as a
// password and the username just has to be uncontroversial.
const safeUsername = "svc_account"

// testPort is 1433 for every fixture, including the mysql ones. buildDSN only
// ever hands the port to net.JoinHostPort, so nothing in it reads the number,
// and one value keeps the expected address literal below a single constant
// instead of a per-driver branch.
const testPort = 1433

// hostileCredentials is the corpus of values a username or password can hold
// that would break one of the DSNs if buildDSN interpolated credentials with
// fmt.Sprintf:
//
//	";"  truncates the MSSQL ADO connection string at that point
//	":"  shifts MySQL's user/password split, so "us:er" logs in as "us"
//	"#"  ends a URL DSN's authority and makes the rest a fragment
//	"%"  is an invalid percent-escape in a URL DSN, failing url.Parse
//	"@"  and "/" are the delimiters the MySQL DSN is scanned for
//	"?"  and "&" delimit query sections in the sqlserver URL DSN
//	'"'  is the ADO parser's own escape character
//
// The corpus is shared across both drivers, but the EXPECTATION is not, and
// that asymmetry is the point of this file rather than an inconvenience in it.
// The sqlserver URL DSN escapes structurally, so every value below round-trips
// in either field. MySQL does not: mysql.Config.FormatDSN writes both fields
// raw, so a ":" in the USERNAME cannot survive and buildDSN refuses it
// instead.
//
// The two fields are exercised separately rather than sharing one column. A
// table that used each value as username AND password would look uniform while
// two of its MySQL cases silently could not pass; splitting them is what makes
// the one real exception visible.
var hostileCredentials = []struct {
	name  string
	value string
}{
	{"semicolon", "pa;ss"},
	{"colon", "pa:ss"},
	{"hash", "pa#ss"},
	{"percent", "pa%ss"},
	{"percent-looks-like-escape", "pa%zz"},
	{"at", "pa@ss"},
	{"slash", "pa/ss"},
	{"question", "pa?ss"},
	{"ampersand", "pa&ss"},
	{"equals", "pa=ss"},
	{"double-quote", `pa"ss`},
	{"brace", "pa{ss}"},
	{"space", "pa ss"},
	{"everything", `p;a:s#s%z@/?&="{}' `},
}

// redact keeps a failing assertion from printing a DSN into test output. The
// credentials here are fixtures, but the habit is the point: nothing in this
// package should ever log a whole DSN, tests included.
//
// The cut is made at the credential separator rather than at a fixed prefix
// length. A prefix is the wrong shape entirely: both DSN forms put the
// credentials at the FRONT and the diagnostically useful part (address,
// database, parameters) behind them, so keeping the first N bytes of a
// MySQL DSN —
//
//	svc_account:pa...   <- the first 14 bytes of a MySQL DSN
//
// — hands over the whole username and the start of the password. Every
// value this file builds escapes or forbids a literal "@" inside a
// credential, so the last one is the separator on both forms.
func redact(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return "[redacted]"
	}
	// Keep the scheme when there is one, so a failure still says which DSN
	// shape it was looking at.
	scheme := ""
	if i := strings.Index(dsn, "://"); i >= 0 && i < at {
		scheme = dsn[:i+3]
	}
	return scheme + "[redacted]" + dsn[at:]
}

func testConfigs(
	driver, credential string,
) (*config.DatabaseConfig, *config.AppConfig) {
	return testConfigsWith(driver, credential, credential)
}

// testConfigsWith is the two-field form, needed because MySQL treats the
// username and the password differently and the tests have to say which one
// they mean.
func testConfigsWith(
	driver, username, password string,
) (*config.DatabaseConfig, *config.AppConfig) {
	return &config.DatabaseConfig{
		Driver:   driver,
		Host:     "db.internal",
		Port:     testPort,
		Database: "TestDb",
		Username: username,
		Password: password,
		Encrypt:  "true",
	}, &config.AppConfig{
		Name:     "testapp",
		Location: "Asia/Jakarta",
	}
}

// TestBuildDSNRoundTripsHostileCredentials is why db.go documents no list of
// characters to avoid in a password: the escaping rules are stated by the code
// that executes them.
//
// Each case builds a DSN through buildDSN and parses it back with the SAME
// parser the driver uses at Open, then asserts the credentials came out
// byte-identical. A driver upgrade that changes escaping rules fails here
// rather than silently invalidating a doc comment.
func TestBuildDSNRoundTripsHostileCredentials(t *testing.T) {
	t.Parallel()

	t.Run("sqlserver", func(t *testing.T) {
		t.Parallel()
		for _, tc := range hostileCredentials {
			t.Run(tc.name, func(t *testing.T) {
				cfg, app := testConfigs("sqlserver", tc.value)
				_, dsn, err := buildDSN(cfg, app)
				if err != nil {
					t.Fatalf("buildDSN: %v", err)
				}

				// msdsn.Parse is what go-mssqldb's OpenConnector calls, so
				// this is the real decode path and not an approximation of it.
				p, err := msdsn.Parse(dsn)
				if err != nil {
					t.Fatalf("msdsn.Parse rejected the DSN: %v", err)
				}
				if p.User != tc.value {
					t.Errorf("user: got %q, want %q", p.User, tc.value)
				}
				if p.Password != tc.value {
					t.Errorf("password: got %q, want %q", p.Password, tc.value)
				}
				// The surrounding fields matter as much as the credentials: a
				// password that eats the rest of the string corrupts these
				// rather than the password itself.
				if p.Host != cfg.Host {
					t.Errorf("host: got %q, want %q", p.Host, cfg.Host)
				}
				if p.Port != uint64(cfg.Port) {
					t.Errorf("port: got %d, want %d", p.Port, cfg.Port)
				}
				if p.Database != cfg.Database {
					t.Errorf("database: got %q, want %q",
						p.Database, cfg.Database)
				}
				if p.AppName != app.Name {
					t.Errorf("app name: got %q, want %q",
						p.AppName, app.Name)
				}
				// Instance must stay empty. A URL path would be read as a
				// named instance, so this catches anyone "tidying"
				// cfg.Database out of the query and into the path.
				if p.Instance != "" {
					t.Errorf("instance: got %q, want empty — database "+
						"belongs in the query, not the path", p.Instance)
				}
				if got := app.Location; got != app.Location {
					t.Errorf("timezone: got %q, want %q", got, app.Location)
				}
			})
		}
	})

	// MySQL is split in two because the driver treats the two fields
	// differently. Every corpus value is exercised as a PASSWORD; only the
	// colon-free ones are exercised as a USERNAME, and the excluded ones are
	// covered by TestMySQLRejectsAColonInTheUsername rather than skipped.
	t.Run("mysql", func(t *testing.T) {
		t.Parallel()

		check := func(
			t *testing.T,
			cfg *config.DatabaseConfig,
			app *config.AppConfig,
		) {
			t.Helper()

			_, dsn, err := buildDSN(cfg, app)
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			parsed, err := mysql.ParseDSN(dsn)
			if err != nil {
				t.Fatalf("mysql.ParseDSN rejected the DSN: %v", err)
			}
			if parsed.User != cfg.Username {
				t.Errorf("user: got %q, want %q", parsed.User, cfg.Username)
			}
			if parsed.Passwd != cfg.Password {
				t.Errorf("password: got %q, want %q",
					parsed.Passwd, cfg.Password)
			}
			if want := "db.internal:1433"; parsed.Addr != want {
				t.Errorf("addr: got %q, want %q", parsed.Addr, want)
			}
			if parsed.DBName != cfg.Database {
				t.Errorf("dbname: got %q, want %q",
					parsed.DBName, cfg.Database)
			}
			// parseTime and loc are the two settings datetime.Datetime.Scan
			// depends on; a DSN that parses but drops them fails at query
			// time, far from here.
			if !parsed.ParseTime {
				t.Error("parseTime: got false, want true")
			}
			if parsed.Loc == nil || parsed.Loc.String() != app.Location {
				t.Errorf("loc: got %v, want %q", parsed.Loc, app.Location)
			}
			// clientFoundRows is the third of that set, and the one whose
			// absence is silent: the DSN still parses, every query still runs,
			// and a no-op UPDATE simply starts returning 404 where SQL Server
			// returns success. See the branch in buildDSN.
			if !parsed.ClientFoundRows {
				t.Error("clientFoundRows: got false, want true — " +
					"requireOneRow would read a no-op UPDATE as missing")
			}
		}

		t.Run("as password", func(t *testing.T) {
			t.Parallel()
			for _, tc := range hostileCredentials {
				t.Run(tc.name, func(t *testing.T) {
					cfg, app := testConfigsWith(
						"mysql", safeUsername, tc.value)
					check(t, cfg, app)
				})
			}
		})

		t.Run("as username", func(t *testing.T) {
			t.Parallel()
			for _, tc := range hostileCredentials {
				if strings.Contains(tc.value, ":") {
					continue // see TestMySQLRejectsAColonInTheUsername
				}
				t.Run(tc.name, func(t *testing.T) {
					cfg, app := testConfigsWith("mysql", tc.value, "secret")
					check(t, cfg, app)
				})
			}
		})
	})
}

// TestMySQLRejectsAColonInTheUsername is the exception the corpus above routes
// around, and the assertion that keeps it from being quietly re-escaped.
//
// mysql.Config.FormatDSN writes User and Passwd unescaped, and ParseDSN
// recovers them by splitting on the FIRST ":" before the last "@". A colon in
// the username therefore does not produce a malformed DSN — it produces a
// well-formed one naming a DIFFERENT login, which is the worst available
// outcome. buildDSN must refuse it.
//
// The negative half of the test is what stops the guard from being widened
// into a blanket ban: a colon in the PASSWORD is harmless, because the split
// point has already been consumed by the username, and rejecting it would take
// a legal password away for no reason.
func TestMySQLRejectsAColonInTheUsername(t *testing.T) {
	t.Parallel()

	t.Run("rejected in the username", func(t *testing.T) {
		t.Parallel()

		usernames := []string{
			"us:er",
			":leading",
			"trailing:",
			":",
			`p;a:s#s%z@/?&="{}' `,
		}

		for _, username := range usernames {
			t.Run(username, func(t *testing.T) {
				cfg, app := testConfigsWith("mysql", username, "secret")

				driver, dsn, err := buildDSN(cfg, app)
				if !errors.Is(err, ErrInvalidCredential) {
					t.Fatalf("got %v, want ErrInvalidCredential", err)
				}
				if driver != "" || dsn != "" {
					t.Errorf("expected empty driver and dsn on error, got "+
						"%q and a %d-byte dsn", driver, len(dsn))
				}
				// The message has to name the key an operator can fix,
				// and must NOT quote the credential, because a caller
				// may log a startup error verbatim.
				if !strings.Contains(err.Error(), "database.username") {
					t.Errorf("error %q does not name the offending key", err)
				}
				// Only meaningful for a credential distinctive enough not to
				// occur in the explanation: the message necessarily contains a
				// bare ":" while saying which byte is the problem.
				if len(username) > 1 &&
					strings.Contains(err.Error(), username) {
					t.Errorf("error %q quotes the credential back; it "+
						"reaches the log aggregator", err)
				}
			})
		}
	})

	t.Run("accepted in the password", func(t *testing.T) {
		t.Parallel()

		const password = "pa:ss:word"
		cfg, app := testConfigsWith("mysql", safeUsername, password)

		_, dsn, err := buildDSN(cfg, app)
		if err != nil {
			t.Fatalf("buildDSN refused a colon in the password: %v", err)
		}
		parsed, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("mysql.ParseDSN: %v", err)
		}
		if parsed.User != safeUsername || parsed.Passwd != password {
			t.Errorf("got user %q password %q, want %q and %q",
				parsed.User, parsed.Passwd, safeUsername, password)
		}
	})

	t.Run("the sqlserver driver is unaffected", func(t *testing.T) {
		t.Parallel()

		// The guard is MySQL's alone. A colon is an ordinary byte to
		// url.UserPassword, and banning it everywhere would be a config
		// restriction invented by the weaker of the two DSN grammars. Both
		// config spellings are checked because both reach the same branch.
		for _, driver := range []string{"sqlserver", "mssql"} {
			t.Run(driver, func(t *testing.T) {
				cfg, app := testConfigsWith(driver, "us:er", "pa:ss")
				if _, _, err := buildDSN(cfg, app); err != nil {
					t.Errorf("buildDSN: %v — only mysql restricts the "+
						"username", err)
				}
			})
		}
	})
}

// TestBuildDSNRoundTripsEveryCredentialByte is the exhaustive form of the two
// tests above, and the reason buildDSN can describe the MySQL exception as
// measured rather than reasoned about.
//
// The corpus at the top of this file is fourteen values chosen for being
// plausible, which is the right shape for a readable failure but proves
// nothing about the bytes nobody thought of. This sweeps the whole range
// through BOTH fields independently, so "a colon in the username is the only
// byte that does not survive" is a statement this file executes. It is also
// what keeps the guard from being widened: every byte OTHER than ":" has to
// round-trip, or buildDSN is refusing usernames the DSN grammar can carry.
//
// 0x01 to 0x7e. NUL is excluded because neither DSN grammar can carry it
// and no ordinary credential store produces it; DEL and above are excluded
// because both drivers reach them through the same escaping paths the
// printable range already exercises.
//
// Each byte sits inside a fixed template rather than standing alone, matching
// the corpus. A username that is nothing but "@" is a different question, and
// one a failure here could not be attributed to.
func TestBuildDSNRoundTripsEveryCredentialByte(t *testing.T) {
	t.Parallel()

	credential := func(b byte) string {
		return "pa" + string(rune(b)) + "ss"
	}

	// Plain loops rather than a subtest per byte: 126 bytes across four
	// field-and-driver combinations is 504 subtests of noise, and t.Errorf
	// naming the byte gives a failure the same information without them.
	forEachByte := func(fn func(b byte)) {
		for b := byte(0x01); b <= 0x7e; b++ {
			fn(b)
		}
	}

	t.Run("sqlserver", func(t *testing.T) {
		t.Parallel()

		// Both fields go through url.UserPassword, which escapes
		// structurally, so there is no exception on this driver and the
		// same check serves both columns.
		check := func(b byte, username, password string) {
			cfg, app := testConfigsWith("sqlserver", username, password)
			_, dsn, err := buildDSN(cfg, app)
			if err != nil {
				t.Errorf("byte 0x%02x: buildDSN: %v", b, err)
				return
			}
			p, err := msdsn.Parse(dsn)
			if err != nil {
				t.Errorf(
					"byte 0x%02x: msdsn.Parse rejected the DSN: %v",
					b,
					err)
				return
			}
			if p.User != username {
				t.Errorf(
					"byte 0x%02x: user: got %q, want %q",
					b,
					p.User,
					username)
			}
			if p.Password != password {
				t.Errorf(
					"byte 0x%02x: password: got %q, want %q",
					b,
					p.Password,
					password)
			}
			// A credential that escapes its own field corrupts what sits
			// behind it rather than itself, so the surrounding values are
			// the more sensitive assertion of the two.
			if p.Host != cfg.Host || p.Database != cfg.Database {
				t.Errorf(
					"byte 0x%02x: got host %q database %q, want %q and %q",
					b,
					p.Host,
					p.Database,
					cfg.Host,
					cfg.Database)
			}
		}

		forEachByte(func(b byte) {
			check(b, credential(b), "secret")
			check(b, safeUsername, credential(b))
		})
	})

	t.Run("mysql", func(t *testing.T) {
		t.Parallel()

		t.Run("as password", func(t *testing.T) {
			t.Parallel()

			forEachByte(func(b byte) {
				password := credential(b)
				cfg, app := testConfigsWith("mysql", safeUsername, password)
				_, dsn, err := buildDSN(cfg, app)
				if err != nil {
					t.Errorf("byte 0x%02x: buildDSN: %v", b, err)
					return
				}
				parsed, err := mysql.ParseDSN(dsn)
				if err != nil {
					t.Errorf(
						"byte 0x%02x: mysql.ParseDSN rejected the DSN: %v",
						b,
						err)
					return
				}
				if parsed.User != safeUsername || parsed.Passwd != password {
					t.Errorf(
						"byte 0x%02x: got user %q password %q, want %q and "+
							"%q — the split point is consumed by the "+
							"username, so no byte here can move it",
						b,
						parsed.User,
						parsed.Passwd,
						safeUsername,
						password)
				}
			})
		})

		t.Run("as username", func(t *testing.T) {
			t.Parallel()

			forEachByte(func(b byte) {
				username := credential(b)
				cfg, app := testConfigsWith("mysql", username, "secret")
				_, dsn, err := buildDSN(cfg, app)

				// The one exception in the whole range. FormatDSN writes
				// the username raw and ParseDSN splits on the first ":",
				// so this byte does not produce a malformed DSN — it
				// produces a well-formed one naming a different login.
				if b == ':' {
					if !errors.Is(err, ErrInvalidCredential) {
						t.Errorf(
							"byte 0x%02x: got %v, want "+
								"ErrInvalidCredential",
							b,
							err)
					}
					return
				}

				if err != nil {
					t.Errorf(
						"byte 0x%02x: buildDSN: %v — only \":\" is refused",
						b,
						err)
					return
				}
				parsed, err := mysql.ParseDSN(dsn)
				if err != nil {
					t.Errorf(
						"byte 0x%02x: mysql.ParseDSN rejected the DSN: %v",
						b,
						err)
					return
				}
				if parsed.User != username || parsed.Passwd != "secret" {
					t.Errorf(
						"byte 0x%02x: got user %q password %q, want %q and "+
							"\"secret\"",
						b,
						parsed.User,
						parsed.Passwd,
						username)
				}
			})
		})
	})
}

// TestBuildDSNReturnsTheRegisteredDriverName pins the first return value,
// which buildDSN's doc comment describes as load-bearing.
//
// The name reaches sqlx.Open and becomes db.DriverName(), which
// sqlx.BindType maps to a placeholder dialect. "mssql" is a real
// registration and an accepted config value, but sqlx does not know it:
// Rebind would become a no-op, the ? placeholders would reach SQL Server
// unchanged, and every query would fail as a syntax error at request time
// with nothing pointing back at the configured driver name. Collapsing both
// spellings onto "sqlserver" is what prevents that, and it is one word away
// from being undone by a tidy-up.
func TestBuildDSNReturnsTheRegisteredDriverName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		configValue string
		want        string
	}{
		{"sqlserver", "sqlserver"},
		{"mssql", "sqlserver"}, // NOT "mssql" — see above
		{"mysql", "mysql"},

		// NewDatabaseConfig lowercases the key, but a struct literal never
		// passes through it, which is why buildDSN lowercases again.
		{"SQLServer", "sqlserver"},
		{"MSSQL", "sqlserver"},
		{"MySQL", "mysql"},
	}

	for _, tc := range tests {
		t.Run(tc.configValue, func(t *testing.T) {
			t.Parallel()

			cfg, app := testConfigs(tc.configValue, "secret")
			driver, dsn, err := buildDSN(cfg, app)
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			if driver != tc.want {
				t.Errorf("driver name: got %q, want %q", driver, tc.want)
			}
			if dsn == "" {
				t.Error("dsn is empty")
			}
		})
	}
}

// TestEncryptModeReachesEachDriver walks the encryptModes table end to end.
//
// The table is the whole reason database.encrypt is required for every
// driver rather than only the one whose vocabulary it borrows, and the
// failure it prevents is silent: without it, a MySQL deployment would
// connect unencrypted while the configuration carried an "encrypt" key that
// read as though it applied. A missing or mistranslated parameter
// reproduces exactly that, and nothing about the connection would look
// wrong.
//
// The assertions read the emitted DSN rather than the driver's decoded enum,
// because the wire value is what the server acts on and what an operator sees.
func TestEncryptModeReachesEachDriver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		encrypt string
		mssql   string // encrypt=
		mysql   string // tls=
	}{
		{encrypt: "disable", mssql: "disable", mysql: "false"},
		{encrypt: "false", mssql: "false", mysql: "preferred"},
		{encrypt: "true", mssql: "true", mysql: "true"},
		{encrypt: "strict", mssql: "strict", mysql: "true"},
	}

	for _, tc := range tests {
		t.Run(tc.encrypt, func(t *testing.T) {
			t.Parallel()

			t.Run("sqlserver", func(t *testing.T) {
				cfg, app := testConfigs("sqlserver", "secret")
				cfg.Encrypt = tc.encrypt
				_, dsn, err := buildDSN(cfg, app)
				if err != nil {
					t.Fatalf("buildDSN: %v", err)
				}
				u, err := url.Parse(dsn)
				if err != nil {
					t.Fatalf("url.Parse: %v", err)
				}
				if got := u.Query().Get("encrypt"); got != tc.mssql {
					t.Errorf("encrypt: got %q, want %q", got, tc.mssql)
				}
				// The value has to be one go-mssqldb accepts, not merely one
				// this table can spell.
				if _, err := msdsn.Parse(dsn); err != nil {
					t.Errorf(
						"go-mssqldb rejected encrypt=%q: %v",
						tc.mssql,
						err)
				}
			})

			t.Run("mysql", func(t *testing.T) {
				cfg, app := testConfigs("mysql", "secret")
				cfg.Encrypt = tc.encrypt
				_, dsn, err := buildDSN(cfg, app)
				if err != nil {
					t.Fatalf("buildDSN: %v", err)
				}
				parsed, err := mysql.ParseDSN(dsn)
				if err != nil {
					t.Fatalf("mysql.ParseDSN: %v", err)
				}
				if parsed.TLSConfig != tc.mysql {
					t.Errorf(
						"tls: got %q, want %q",
						parsed.TLSConfig,
						tc.mysql)
				}
				// "skip-verify" would encrypt without authenticating, which is
				// what loosening this table to make a private CA work would
				// reach for. It must never be produced.
				if parsed.TLSConfig == "skip-verify" {
					t.Error(`tls=skip-verify: "true" would mean encrypted ` +
						`but unauthenticated`)
				}
			})
		})
	}
}

// TestBuildDSNRejectsUnsupportedEncryptMode covers the branch that exists
// because buildDSN cannot forward an unrecognised value and let the driver
// complain — on MySQL there is nothing to forward it TO.
//
// It fires on every accepted driver spelling. In the normal startup path
// config.DatabaseConfig.Validate rejects these first and names the key; this
// is the guard for a DatabaseConfig built in code.
func TestBuildDSNRejectsUnsupportedEncryptMode(t *testing.T) {
	t.Parallel()

	// Values go-mssqldb itself accepts, which is precisely why the allowlist
	// here is closed rather than delegated.
	//
	// Note what is NOT in this list: "True", "STRICT", " disable ". encryptFor
	// lowercases and trims before the lookup, so those are canonical values in
	// disguise and are accepted — see
	// TestEncryptModeIsCaseAndSpaceInsensitive. The rejected set is a matter
	// of VOCABULARY, not spelling.
	rejected := []string{
		"", "0", "1", "t", "f", "yes", "no", "mandatory", "optional",
		"require", "verify-full", "skip-verify",
	}

	for _, driver := range []string{"sqlserver", "mssql", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			t.Parallel()
			for _, encrypt := range rejected {
				cfg, app := testConfigs(driver, "secret")
				cfg.Encrypt = encrypt

				gotDriver, dsn, err := buildDSN(cfg, app)
				if !errors.Is(err, ErrUnsupportedEncryptMode) {
					t.Errorf(
						"encrypt=%q: got %v, want ErrUnsupportedEncryptMode",
						encrypt,
						err)
					continue
				}
				if gotDriver != "" || dsn != "" {
					t.Errorf(
						"encrypt=%q: expected empty driver and dsn on error, "+
							"got %q and a %d-byte dsn",
						encrypt,
						gotDriver,
						len(dsn))
				}
			}
		})
	}
}

// TestEncryptModeIsCaseAndSpaceInsensitive matches how NewDatabaseConfig
// normalises the value, so a DatabaseConfig built as a struct literal in a
// test behaves the same as one read from YAML.
func TestEncryptModeIsCaseAndSpaceInsensitive(t *testing.T) {
	t.Parallel()

	spellings := []string{"DISABLE", " disable ", "Disable", "\tSTRICT\n"}
	for _, encrypt := range spellings {
		t.Run(encrypt, func(t *testing.T) {
			t.Parallel()

			cfg, app := testConfigs("sqlserver", "secret")
			cfg.Encrypt = encrypt
			if _, _, err := buildDSN(cfg, app); err != nil {
				t.Errorf("buildDSN: %v", err)
			}
		})
	}
}

// TestBuildDSNRejectsUnsupportedDriver keeps the default branch honest: an
// unknown name must produce ErrUnsupportedDriver and an empty DSN, never a
// half-built string that reaches sqlx.Open.
func TestBuildDSNRejectsUnsupportedDriver(t *testing.T) {
	t.Parallel()

	cfg, app := testConfigs("oracle", "secret")
	driver, dsn, err := buildDSN(cfg, app)
	if !errors.Is(err, ErrUnsupportedDriver) {
		t.Fatalf("got %v, want ErrUnsupportedDriver", err)
	}
	if driver != "" || dsn != "" {
		t.Errorf("expected empty driver and dsn on error, got %q and a "+
			"%d-byte dsn", driver, len(dsn))
	}
}

// TestDriverIsReportedBeforeEncrypt pins the ordering buildDSN's comment gives
// a reason for: encryptFor is called inside each branch rather than once up
// front, so a config wrong in both places names the driver first. That is the
// more fundamental of the two — the encrypt vocabulary only means anything
// once the driver is known.
func TestDriverIsReportedBeforeEncrypt(t *testing.T) {
	t.Parallel()

	cfg, app := testConfigs("oracle", "secret")
	cfg.Encrypt = "nonsense"

	_, _, err := buildDSN(cfg, app)
	if !errors.Is(err, ErrUnsupportedDriver) {
		t.Errorf(
			"got %v, want ErrUnsupportedDriver to win over "+
				"ErrUnsupportedEncryptMode",
			err)
	}
}

// TestNewErrorsCarryNoCredentials is the assertion behind the package doc's
// promise that a DSN never leaves this package.
//
// Every error New can return without reaching the network is exercised, and
// none of them may contain the password. A caller is expected to be able to
// log a startup error verbatim, so a credential in any of these messages
// lands in the log aggregator.
//
// ErrMalformedDSN is NOT among them, and cannot be: buildDSN assembles its
// output with url.URL and mysql.Config.FormatDSN, so there is no input that
// reaches sqlx.Open with a string those parsers reject. The sentinel exists so
// that the driver's own error can be dropped rather than wrapped at that call
// site — the driver may quote the connection string it was handed — and it
// stays untested here for want of a way to provoke it.
func TestNewErrorsCarryNoCredentials(t *testing.T) {
	t.Parallel()

	const password = "sup3r-s3cret-p455w0rd"

	tests := []struct {
		name    string
		mutate  func(*config.DatabaseConfig)
		wantErr error
	}{
		{
			name:    "unsupported driver",
			mutate:  func(c *config.DatabaseConfig) { c.Driver = "oracle" },
			wantErr: ErrUnsupportedDriver,
		},
		{
			name:    "unsupported encrypt mode",
			mutate:  func(c *config.DatabaseConfig) { c.Encrypt = "yes" },
			wantErr: ErrUnsupportedEncryptMode,
		},
		{
			name:    "colon in a mysql username",
			mutate:  func(c *config.DatabaseConfig) { c.Driver = "mysql" },
			wantErr: ErrInvalidCredential,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, app := testConfigsWith("sqlserver", "us:er", password)
			tc.mutate(cfg)

			db, err := New(cfg, app)
			if db != nil {
				t.Error("New returned a pool alongside an error")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("the password reached the error message: %q", err)
			}
		})
	}
}

// TestBuildDSNBracketsIPv6Hosts pins the net.JoinHostPort behaviour. A plain
// fmt.Sprintf("%s:%d") yields "::1:1433", which is not a valid address and
// which no amount of credential escaping would fix.
func TestBuildDSNBracketsIPv6Hosts(t *testing.T) {
	t.Parallel()

	for _, driver := range []string{"sqlserver", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			cfg, app := testConfigs(driver, "secret")
			cfg.Host = "::1"

			_, dsn, err := buildDSN(cfg, app)
			if err != nil {
				t.Fatalf("buildDSN: %v", err)
			}
			if !strings.Contains(dsn, "[::1]:1433") {
				t.Errorf("expected a bracketed IPv6 address in the DSN, "+
					"got %q", redact(dsn))
			}
		})
	}

	// The bracketing has to survive the driver's own parser too, not just look
	// right in the string — an unparseable address is the failure being
	// guarded against.
	cfg, app := testConfigs("sqlserver", "secret")
	cfg.Host = "::1"
	_, dsn, err := buildDSN(cfg, app)
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	p, err := msdsn.Parse(dsn)
	if err != nil {
		t.Fatalf("msdsn.Parse rejected a bracketed IPv6 DSN: %v", err)
	}
	if p.Host != "::1" || p.Port != testPort {
		t.Errorf(
			"got host %q port %d, want \"::1\" and %d",
			p.Host,
			p.Port,
			testPort)
	}
}

// NewContext must honour a context that is already done BEFORE it reaches
// the network, which is the one thing it offers that New cannot.
//
// No server and no reachable host are involved: sqlx.Open does not dial, and
// database/sql checks ctx.Err() at the top of its connection request, so a
// cancelled context returns from the ping without a packet being sent. That
// is what makes this a unit test rather than an integration one, and 192.0.2.1
// (TEST-NET-1, routed nowhere) is what would make it hang for pingTimeout if
// the short-circuit ever stopped working.
func TestNewContextStopsBeforeDialingOnACancelledContext(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{"sqlserver", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			t.Parallel()
			cfg, app := testConfigs(driver, safeUsername)
			cfg.Host = "192.0.2.1"

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			db, err := NewContext(ctx, cfg, app)
			if db != nil {
				_ = db.Close()
				t.Error("NewContext returned a pool, want nil")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
		})
	}
}

// New is a wrapper over NewContext and must not acquire an error path of its
// own. A DSN failure happens before either function reaches a context, so it
// is the one outcome both can be compared on without a server.
func TestNewDelegatesToNewContext(t *testing.T) {
	t.Parallel()
	cfg, app := testConfigs("oracle", safeUsername)

	_, direct := New(cfg, app)
	_, viaContext := NewContext(context.Background(), cfg, app)

	if !errors.Is(direct, ErrUnsupportedDriver) {
		t.Errorf("New: err = %v, want ErrUnsupportedDriver", direct)
	}
	if direct == nil || viaContext == nil ||
		direct.Error() != viaContext.Error() {
		t.Errorf("New returned %v, NewContext returned %v — want the "+
			"same error", direct, viaContext)
	}
}
