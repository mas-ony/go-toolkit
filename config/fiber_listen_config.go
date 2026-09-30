package config

// The fiber.listen.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.listen.cert_client_file
//		FIBER_LISTEN_CERT_CLIENT_FILE
//	fiber.listen.cert_file
//		FIBER_LISTEN_CERT_FILE
//	fiber.listen.cert_key_file
//		FIBER_LISTEN_CERT_KEY_FILE
//	fiber.listen.disable_startup_message
//		FIBER_LISTEN_DISABLE_STARTUP_MESSAGE
//	fiber.listen.enable_prefork
//		FIBER_LISTEN_ENABLE_PREFORK
//	fiber.listen.enable_print_routes
//		FIBER_LISTEN_ENABLE_PRINT_ROUTES
//	fiber.listen.listener_network
//		FIBER_LISTEN_LISTENER_NETWORK
//	fiber.listen.prefork_recover_interval
//		FIBER_LISTEN_PREFORK_RECOVER_INTERVAL
//	fiber.listen.prefork_recover_threshold
//		FIBER_LISTEN_PREFORK_RECOVER_THRESHOLD
//	fiber.listen.prefork_shutdown_grace_period
//		FIBER_LISTEN_PREFORK_SHUTDOWN_GRACE_PERIOD
//	fiber.listen.shutdown_timeout
//		FIBER_LISTEN_SHUTDOWN_TIMEOUT
//	fiber.listen.tls_min_version
//		FIBER_LISTEN_TLS_MIN_VERSION
//	fiber.listen.unix_socket_file_mode
//		FIBER_LISTEN_UNIX_SOCKET_FILE_MODE
//
// Thirteen keys, and that is the whole section — NewListenConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/spf13/viper"
)

// ListenConfig wraps fiber.ListenConfig so it participates in the standard
// Validate/String lifecycle used by all other sub-configs.
type ListenConfig struct {
	// Embedded as a POINTER, so both the wrapper and the embedded struct can
	// be nil independently — hence the two-part nil check in Validate.
	// Embedding also promotes the fields (c.ShutdownTimeout, not
	// c.ListenConfig.ShutdownTimeout), which is why a nil embedded pointer
	// panics on field access rather than at the method call.
	*fiber.ListenConfig
}

// TLS version constants for fiber.listen.tls_min_version.
//
// Spelled as untyped decimal rather than tls.VersionTLS12 so this package does
// not import crypto/tls for two numbers, and so the constant names sit next to
// the spelling config.yaml actually uses.
//
// Decimal is a CONVENTION here, not a constraint. Both readers parse at base
// 0, so hex arrives intact from either source: go-yaml resolves 0x0303 to 771
// before Viper sees a string, and spf13/cast's ParseUint(s, 0, 0) accepts
// FIBER_LISTEN_TLS_MIN_VERSION=0x0303 for the same reason it accepts a leading
// zero as octal in unix_socket_file_mode below. The convention is worth
// keeping anyway: the validation error prints %d, so a deployment that writes
// hex reads one number in config.yaml and a different one in the message
// telling it what went wrong.
const (
	tlsVersion12 = 771 // 0x0303
	tlsVersion13 = 772 // 0x0304
)

// maxUnixSocketFileMode is the largest value
// fiber.listen.unix_socket_file_mode may take: the nine permission bits and
// nothing above them.
//
// The ceiling exists to catch one specific typo. A mode is conventionally
// written in octal, both here and in the YAML, and both the file parser and
// the environment parser read a leading zero as an octal prefix. Drop that
// zero and 770 is DECIMAL — 0o1402 — which is not "rwxrwx---" at all.
//
// What 0o1402 actually does is worth spelling out, because it is not the
// sticky bit it looks like. Go keeps setuid, setgid, and sticky in
// os.FileMode's HIGH bits — ModeSticky is 1<<20, not 0o1000 — and os.Chmod
// converts through syscallMode, which forwards i.Perm() (the low nine bits)
// plus those three named flags and nothing else. The 0o1000 here matches no
// flag and is dropped without a word, so the socket is chmodded to 0o402:
// read-only for its owner, nothing at all for its group, write-only for
// everyone else. Connecting to a unix socket needs WRITE permission, so the
// accounts that can reach it are exactly the ones outside both the owner and
// the group — the ones that were never meant to — while the service's own user
// cannot. One missing zero.
//
// Anything above 0o777 is that mistake, and nothing else worth having. A
// deliberate setuid/setgid/sticky request would have to be spelled with
// os.FileMode's high-bit constants rather than an octal literal, and none of
// the three means anything on a socket file to begin with.
const maxUnixSocketFileMode = 0o777

// defaultPreforkShutdownGracePeriod mirrors fasthttp's own substitution for an
// unset fiber.listen.prefork_shutdown_grace_period, so Validate can compare
// the EFFECTIVE master-side deadline against fiber.listen.shutdown_timeout
// rather than the literal 0.
//
// It is a COPY of a number this package does not own — fasthttp's
// defaultShutdownGracePeriod, applied inside shutdownChildren on `grace <= 0`,
// and unexported there, so there is nothing to reference. A bump on their side
// makes this stale in one direction only: the check would demand a shorter
// shutdown_timeout than the master would actually allow, so the cost of the
// drift is a startup error that could have been a working config, not a drain
// that silently loses requests. That is the right way round for a guess.
//
// Pinning the dependency is what keeps it honest; go.mod holds fasthttp at
// v1.74.0, where this is 5s. Re-read shutdownChildren on every bump.
const defaultPreforkShutdownGracePeriod = 5 * time.Second

// validListenerNetworks is the allowlist checked by ListenConfig.Validate.
//
// Matched exactly, no case folding, because both consumers compare exactly.
// An application branches on `== "unix"` to decide whether app.Host is a
// socket path or a bind address, and Fiber hands the string to net.Listen
// unchanged. "UNIX" would therefore take the tcp branch, get a ":port"
// appended, and die inside net.Listen as an unknown network — a message
// naming neither key. The application's cross-section validation leaves
// this case to Validate here.
//
// The empty string is absent deliberately: Fiber normalises "" to NetworkTCP4
// before use, so an unset key is valid and never reaches this map (see the
// guard in Validate).
var validListenerNetworks = map[string]struct{}{
	"tcp":  {},
	"tcp4": {},
	"tcp6": {},
	"unix": {},
}

// octalIntent recovers the mode an operator meant when they wrote one without
// its leading zero, by reading the value's DECIMAL DIGITS back as octal. For
// 770 that yields 0o770, which is what they typed and what they meant.
//
// The second return is false when the guess cannot be trusted, and both ways
// it fails are worth keeping out of the message. "778" has no octal reading at
// all — 8 is not an octal digit — so nothing can be recovered. "2000" reads as
// 0o2000, which is still above 0o777, so the digits are not a mode either and
// the mistake is something else. A confident wrong suggestion in either case
// would send someone down a path that ends in a second failed startup.
//
// Both sources accept the leading-zero spelling the suggestion uses, so it is
// literally what to type in either place: go-yaml resolves plain integers with
// strconv base 0, and spf13/cast parses an environment string the same way.
func octalIntent(got uint32) (uint32, bool) {
	meant, err := strconv.ParseUint(strconv.FormatUint(uint64(got), 10), 8, 32)
	if err != nil || meant > maxUnixSocketFileMode {
		return 0, false
	}
	return uint32(meant), true
}

// NewListenConfig reads ListenConfig fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.listen.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// # The seven fields NOT read here
//
// fiber.ListenConfig has twenty exported fields and this constructor populates
// thirteen. The remaining seven are absent for two reasons:
//
//   - Five cannot be expressed in YAML at any spelling. TLSConfigFunc,
//     ListenerAddrFunc, and BeforeServeFunc are funcs; PreforkLogger is an
//     interface (one Printf method); GracefulContext is a context.Context. No
//     scalar, sequence, or mapping could carry any of them, and Fiber runs
//     with none of them set. Two are worth naming individually, because
//     "unset" does not mean the same thing for both. GracefulContext is the
//     one with teeth, and it IS a true no-hook nil: leave it out and Fiber
//     never starts its shutdown goroutine, so ShutdownTimeout below is read by
//     nothing at all. The application supplies it. PreforkLogger is NOT a
//     no-hook nil, despite sitting in the same list. Fiber's prefork path
//     substitutes its own preforkLogger{}, which forwards to Fiber's
//     log.Infof — so the master's "child exited, restarting" lines go out
//     through Fiber's unstructured logger rather than the application's
//     zerolog. That is the same split-output shape as the recover
//     middleware's stderr fallback, on the one path whose whole job is
//     reporting a crash loop. Inert while enable_prefork is false, which is
//     why it is not wired; wiring it means Go code at the Listen call site,
//     since an interface cannot come from a key here.
//   - Two are Go-only for a softer reason. TLSConfig (*tls.Config) and
//     AutoCertManager (*autocert.Manager) are structs with constructors rather
//     than literals — TLSConfig's useful part is its GetCertificate hook,
//     which is a func again, and AutoCertManager is the ACME path. Both are
//     alternative certificate sources. This package reads the certificate
//     from cert_file and cert_key_file, which suits a service whose
//     certificate something else renews — a gateway, cert-manager — so
//     neither is wired. Adding either means Go code at the Listen call site,
//     not a key here.
//
// The zero values are what the section has to explain. Fiber passes both
// straight through to fasthttp's prefork package, which substitutes a default
// for one and takes the other literally:
//
//	PreforkRecoverInterval
//		0 is the SETTING, not a request for a default: a crashed child
//		is respawned immediately, with no backoff, so a child that dies
//		on startup is respawned as fast as it can fail until
//		prefork_recover_threshold is exceeded
//	PreforkShutdownGracePeriod
//		0 IS replaced — fasthttp tests grace <= 0 and substitutes 5s.
//		That is how long the master waits after SIGTERMing its children
//		before SIGKILLing the survivors; on Windows there is no SIGTERM
//		step at all and children are killed outright
//
// The second is the one to hold against shutdown_timeout below, because the
// two numbers look like the same deadline and are not. shutdown_timeout is how
// long a WORKER drains its own in-flight requests; this is how long the MASTER
// tolerates a worker that has not finished. Leave this key unset against the
// template's 10s drain and the master's substituted 5s is the smaller of the
// two, so a request still in flight at the five second mark is SIGKILLed with
// its worker while that worker believes it has five seconds left. Nothing
// reports it, so Validate does: see the pairing rule at the end of that
// method, which fires only when enable_prefork is true, because that is the
// only configuration where the master exists to enforce anything.
//
// Stating a grace period above the worker's shutdown_timeout avoids that
// pairing: the worker's deadline stays the binding one, with slack for
// the master's own teardown. The unset case is documented because it is
// what a deployment gets by touching neither key.
//
// TLSMinVersion is written as a plain decimal integer in config.yaml by
// convention — see the tlsVersion12 constants for why, and for why hex would
// in fact parse. Fiber v3 accepts ONLY two values:
//
//	771 = 0x0303 = tls.VersionTLS12  (Fiber's default)
//	772 = 0x0304 = tls.VersionTLS13
//
// Any other non-zero value — including 769 (TLS 1.0) and 770 (TLS 1.1) —
// makes Fiber panic at Listen time with "unsupported TLS version, please use
// tls.VersionTLS12 or tls.VersionTLS13". A value of 0 is replaced by Fiber's
// default (TLS 1.2) before that check runs, so leaving the key unset is safe.
//
// Validate rejects every other value at startup, so that panic is UNREACHABLE
// from this key. It is documented anyway for two reasons: it is what the field
// does without the check, and Fiber only runs it on the branch where
// ListenConfig.TLSConfig is nil. A *tls.Config assigned at the Listen call
// site takes the other branch, where this value is not validated and not used
// — it is reported as superseded and the tls.Config's own MinVersion decides.
// This package never sets that field (see the seven above), so that branch
// is reachable only from Go code at the Listen call site.
func NewListenConfig(v *viper.Viper) *ListenConfig {
	return &ListenConfig{
		ListenConfig: &fiber.ListenConfig{
			// CertClientFile is the path to the CA certificate used for mutual
			// TLS (mTLS) client certificate verification. Leave empty to
			// disable mTLS and accept connections without a client
			// certificate.
			//
			// Only meaningful alongside CertFile/CertKeyFile — mTLS is client
			// verification layered on top of server TLS, not an alternative to
			// it. Setting this alone accomplishes nothing.
			CertClientFile: v.GetString("fiber.listen.cert_client_file"),

			// CertFile is the path to the server TLS certificate (PEM format).
			// Leave empty to serve plain HTTP. Must be set together with
			// CertKeyFile: with only one of the two, Fiber serves plain HTTP
			// without saying so, which Validate refuses.
			CertFile: v.GetString("fiber.listen.cert_file"),

			// CertKeyFile is the path to the server TLS private key
			// (PEM format). Must match CertFile. Never include this path in
			// response bodies or public log aggregators.
			CertKeyFile: v.GetString("fiber.listen.cert_key_file"),

			// DisableStartupMessage suppresses Fiber's ASCII banner and the
			// "listening on ..." line it prints from the parent process.
			//
			// What the banner costs in production is not noise alone: it goes
			// to stdout unstructured, which in a JSON log pipeline is a line
			// nothing can parse, and it prints app.name + app.version, the
			// PID, and the bind address at a point in startup before any
			// redaction the log stack applies.
			DisableStartupMessage: v.GetBool(
				"fiber.listen.disable_startup_message"),

			// EnablePrefork forks one worker process per CPU core, all
			// accepting on the same port through SO_REUSEPORT, for higher
			// multi-core throughput.
			//
			// Every worker is a whole process, so everything a process keeps
			// in memory is multiplied or split:
			//
			//   - Sessions. Each worker has its own in-memory session store,
			//     so under fiber.auth.mode: session a request that lands on
			//     another worker arrives logged out. The application's
			//     cross-section validation refuses that pairing.
			//   - The database pool. Each worker opens its own and applies
			//     database.max_open_conns on its own, so the cap is per
			//     process: 25 on an 8-core host becomes up to 200
			//     connections, and the pool tuning in database_config.go
			//     stops describing what the database sees. Divide
			//     max_open_conns by the core count before enabling this.
			//   - The rate limiter. Each worker counts on its own, so the
			//     per-client quota becomes fiber.limiter.max × workers.
			//
			// Graceful shutdown also needs every child to receive SIGTERM.
			// Fiber handles the fork side, but systemd or Docker must signal
			// the process GROUP rather than only the parent, or the children
			// are killed outright while the parent drains.
			EnablePrefork: v.GetBool("fiber.listen.enable_prefork"),

			// EnablePrintRoutes prints all registered routes to stdout at
			// startup. Helpful during development; disable in production to
			// reduce log noise.
			EnablePrintRoutes: v.GetBool("fiber.listen.enable_print_routes"),

			// ListenerNetwork is the network protocol for the listener socket.
			//
			// An empty value falls back to Fiber v3's default, which is "tcp4"
			// (IPv4 only) — NOT dual-stack. If the service must also be
			// reachable over IPv6, set "tcp" explicitly. The committed
			// config.yaml states tcp4, which matches the default.
			// "unix" requires a socket path instead of host:port in
			// AppConfig.Host.
			//
			// Allowed values: tcp | tcp4 | tcp6 | unix
			// tcp4 = IPv4 only; tcp6 = IPv6 only; tcp = dual-stack.
			ListenerNetwork: v.GetString("fiber.listen.listener_network"),

			// PreforkRecoverInterval is the backoff the master waits before
			// respawning a child that crashed. Read only when EnablePrefork is
			// true; inert otherwise.
			//
			// 0 is the SETTING, not a request for a default — the one prefork
			// key where zero is not "ask Fiber". fasthttp tests
			// `RecoverInterval > 0`, so 0 means respawn immediately, and a
			// child that dies during startup is then respawned as fast as it
			// can fail until prefork_recover_threshold is exhausted. A crash
			// loop with no backoff burns the whole restart budget in
			// milliseconds and takes the master down with it, which is a fair
			// outcome and an unreadable one: the log is a burst of identical
			// lines with no time between them.
			//
			// It is PER CHILD, not a global rate. fasthttp applies the delay
			// inside each child's own Wait goroutine before reporting that
			// exit, so N children dying together are each respawned roughly
			// one interval after their own exit rather than one interval
			// apart. Set it as backoff-per-crash, not as a fleet-wide tick.
			//
			// It does not extend shutdown. The wait is interruptible — the
			// master cancels those goroutines before it starts tearing
			// children down — so a pending backoff is dropped rather than
			// added to PreforkShutdownGracePeriod below.
			//
			// A NEGATIVE value is rejected by Validate. fasthttp's `> 0` test
			// treats it as no backoff at all, so it would read here as a
			// setting and act as though the key were absent.
			PreforkRecoverInterval: v.GetDuration(
				"fiber.listen.prefork_recover_interval"),

			// PreforkRecoverThreshold is how many times the master may restart
			// a crashed child before it gives up and exits with an error.
			// Read only when EnablePrefork is true; inert otherwise, which is
			// why Validate does not pair the two.
			//
			// It is a LIFETIME budget, not a rate. fasthttp's prefork master
			// increments one counter per child exit and never resets it, so a
			// process that has been up for a month and lost one child a week
			// is at four, not at zero.
			//
			// 0 means "use Fiber's default", which is computed rather than
			// constant: max(1, runtime.GOMAXPROCS(0)/2), so half the cores on
			// the machine that ends up running the binary. That is the right
			// value for almost every deployment and the reason this key is not
			// required — unlike database.max_idle_conns, whose
			// absent-reads-as-0 case silently disables a pool, an absent key
			// here lands on a sensible number.
			//
			// Set it explicitly only to change the failure mode, and MIND THE
			// OFF-BY-ONE: the master exits when the crash count EXCEEDS this
			// number, so 1 restarts once and gives up on the second crash. The
			// smallest configurable tolerance is therefore one restart, since
			// 0 is claimed by the default above — exiting on the very first
			// child crash is not reachable through this key at all. A large
			// number keeps a flapping service technically up and hides the
			// crash loop from anything watching the process.
			PreforkRecoverThreshold: v.GetInt(
				"fiber.listen.prefork_recover_threshold"),

			// PreforkShutdownGracePeriod is how long the prefork MASTER waits
			// after SIGTERMing its children before SIGKILLing the survivors.
			// Read only when EnablePrefork is true; inert otherwise.
			//
			// The master tears its children down this way when its own
			// supervision loop ends: when child exits, clean ones included,
			// exceed prefork_recover_threshold, or when a respawn fails.
			//
			// NOT the same deadline as ShutdownTimeout below, despite reading
			// like one. That key is how long a WORKER drains its own in-flight
			// requests; this is how long the master tolerates a worker that
			// has not finished. Whichever is smaller is the one that decides,
			// and when this one is smaller the difference is paid in requests
			// killed mid-flight while the worker still believes it has time.
			// Validate rejects that pairing rather than leaving it to be
			// discovered in a drain.
			//
			// 0 IS replaced here, unlike PreforkRecoverInterval above:
			// fasthttp tests `grace <= 0` and substitutes 5s. So an unset key
			// is a 5s master-side deadline, which is SHORTER than any
			// shutdown_timeout above 5s — the pairing above, reachable by
			// leaving two keys alone. Validate rejects it if a deployment
			// removes this key and turns prefork on.
			//
			// ON WINDOWS it does nothing. There is no SIGTERM step at all;
			// children are killed outright, so the graceful half of shutdown
			// is unavailable to a preforked deployment there regardless of
			// what this says.
			//
			// A NEGATIVE value is rejected by Validate for the same reason as
			// the interval above, with the sign of the mistake reversed: it
			// reads as "kill immediately" and acts as the 5s default.
			PreforkShutdownGracePeriod: v.GetDuration(
				"fiber.listen.prefork_shutdown_grace_period"),

			// ShutdownTimeout is how long Fiber lets in-flight requests finish
			// once a graceful shutdown BEGINS, before it stops waiting. Under
			// prefork that holds in every worker: Listen starts the shutdown
			// goroutine before it takes the prefork branch, so each worker
			// drains on its own context.
			//
			// Fiber installs no signal handler. It starts its shutdown
			// goroutine only when ListenConfig.GracefulContext is non-nil, and
			// that goroutine blocks on <-ctx.Done(); turning SIGTERM/SIGINT
			// into that cancellation is the application's job. This key is
			// therefore inert on any path where the application stops
			// supplying the context, with nothing reporting it — the same
			// shape as fiber.trust_proxy without an allowlist.
			//
			// 0 is NOT "use Fiber's default". Fiber's documented 10s default
			// is only applied when Listen is called with no ListenConfig at
			// all, which never happens here — a config IS passed, so 0
			// survives, and the shutdown goroutine reads it as "no deadline"
			// and calls app.Shutdown(), which waits indefinitely for the last
			// connection. A hung request would then hold the process past the
			// orchestrator's grace period and take a SIGKILL instead. That is
			// why Validate requires this key rather than letting it default:
			// zero here is the one zero in this section that means something
			// other than "ask Fiber".
			//
			// Size it above the slowest in-flight request — an upload that
			// writes to disk is usually the one — and below the orchestrator's
			// kill grace period, or the container is SIGKILLed mid-drain
			// regardless. Kubernetes defaults terminationGracePeriodSeconds to
			// 30s, so 10s sits comfortably inside it.
			//
			// Required, and at least one second. A bare number parses as
			// NANOSECONDS, and a negative value gives the drain a deadline
			// that has already passed; either would make shutdown
			// effectively instant, dropping every request still in flight.
			// Always write a unit.
			ShutdownTimeout: v.GetDuration("fiber.listen.shutdown_timeout"),

			// TLSMinVersion is the minimum TLS version the server accepts.
			// Only 771 (TLS 1.2) and 772 (TLS 1.3) are valid — any other
			// non-zero value would panic inside Listen, which is why Validate
			// rejects it at startup and that panic is unreachable from this
			// key (see the constructor comment). 0 means "use Fiber's
			// default", which is TLS 1.2 (applied by Fiber's ListenConfig
			// normalisation, not by crypto/tls). Explicitly setting 771 is
			// still preferred over relying on the default so the intended
			// floor is documented in config.yaml.
			TLSMinVersion: v.GetUint16("fiber.listen.tls_min_version"),

			// UnixSocketFileMode is the permission mode Fiber chmods the
			// socket file to, and it is read ONLY when ListenerNetwork is
			// "unix" — createListener guards both the stale-socket removal
			// before the bind and the chmod after it on that same test. On
			// every tcp network the value is carried and never applied.
			//
			// The conversion is required, not stylistic: the Fiber field is
			// os.FileMode, which is a DEFINED type over uint32 rather than an
			// alias, so the uint32 that GetUint32 returns does not assign to
			// it without one.
			//
			// 0 means "use Fiber's default", which is 0o770. Validate
			// therefore does not require this key; it only rejects values
			// above the permission bits (see maxUnixSocketFileMode).
			//
			// Octal survives both sources, which is worth knowing rather than
			// assuming. From config.yaml, go-yaml parses a leading-zero scalar
			// with strconv.ParseInt at base 0, so 0770 is 504. From the
			// environment, spf13/cast parses it the same way — its parseUint
			// calls strconv.ParseUint(s, 0, 0) — so
			// FIBER_LISTEN_UNIX_SOCKET_FILE_MODE=0770 agrees with the file.
			//
			// A digit outside the octal range is where the two sources
			// DIVERGE, and only one of the two failures is loud. 0778 and 0789
			// have no valid octal reading, and neither source falls back to
			// decimal at the parse step:
			//
			//   - From the ENVIRONMENT it is silent. The cast fails,
			//     cast.ToUint32 discards the error, GetUint32 hands back 0,
			//     and Fiber replaces 0 with its 0o770 default. Validate never
			//     sees a value to reject, so a typo in the last digit is
			//     quietly the default and nothing but this comment says so.
			//   - From config.yaml it is caught. go-yaml tries ParseInt then
			//     ParseUint at base 0, fails both, and falls through to its
			//     float branch, whose yamlStyleFloat pattern matches a bare
			//     digit run — so 0778 resolves as the float 778, casts to 778,
			//     and the range check below rejects it for exceeding 0777.
			//
			// The range check therefore covers the file and CANNOT cover the
			// environment. That asymmetry is a reason to set this key in
			// config.yaml and override it from the environment only when a
			// deployment genuinely has to.
			UnixSocketFileMode: os.FileMode(
				v.GetUint32("fiber.listen.unix_socket_file_mode")),
		},
	}
}

// Validate returns a joined error for every invalid or missing ListenConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether CertFile and CertKeyFile actually exist, are readable, parse as
//     PEM, or match each other. Only the PAIRING is checked. Reading the files
//     here would duplicate what tls.LoadX509KeyPair does at Listen time and
//     would race with a cert-manager rotation between startup and bind.
//   - Whether the application supplies a GracefulContext at all. Without one
//     Fiber never starts its shutdown goroutine and ShutdownTimeout is read
//     by nothing, but that field is a context.Context set at the Listen call
//     site, which this package cannot see.
//   - EnablePrintRoutes. A bool has no invalid value: both settings are
//     meaningful and an absent key reads as false, which is the documented
//     default. There is nothing a check could reject.
//   - EnablePrefork. Same as EnablePrintRoutes. Also, EnablePrefork against
//     database.max_open_conns. The interaction is real and documented on the
//     field, but the safe divisor is the host's core count, which this process
//     cannot know at config time on the machine that will run it.
//   - DisableStartupMessage. A bool has no invalid value, the same as
//     EnablePrintRoutes above. What could go wrong with it is not a value but
//     an overwrite: an application that dereferences cfg.Listen.ListenConfig
//     into a local before calling Listen, and assigns a field on that copy,
//     silently outranks the key this package read. That is a line of Go in
//     another package, which nothing here can see.
//   - CertClientFile on its own. mTLS without server TLS is a useless
//     combination rather than an invalid one: Fiber ignores it when CertFile
//     is empty, so nothing breaks and no behaviour is silently wrong.
//   - ShutdownTimeout's UPPER bound. A value above the orchestrator's kill
//     grace period gets the container SIGKILLed mid-drain, and this package
//     has no way to see that number.
//   - UnixSocketFileMode on a tcp listener, and the three prefork_* keys with
//     EnablePrefork false. All are inert rather than wrong — Fiber reads none
//     of them on that path — and all are the same shape as CertClientFile
//     above: a useless combination, not an invalid one. Their VALUES are still
//     checked below, unconditionally and on purpose, so a deployment that
//     switches to a unix socket or turns prefork on later does not inherit a
//     number nobody ever validated.
//
// Two checks below are the exception to all of that, both gated on
// EnablePrefork. One is the listener network, which prefork can bind only as
// tcp4 or tcp6. The other is not a value check at all: ShutdownTimeout
// against PreforkShutdownGracePeriod, where both numbers are individually
// fine and the pairing is what is wrong, and only under prefork, where a
// master exists to enforce the shorter of the two.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.listen.* problem at once.
func (c *ListenConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil || c.ListenConfig == nil {
		return errors.New("fiber.listen config was not initialised")
	}

	// Required rather than defaulted, because 0 is not "ask Fiber" here — it
	// disables the drain deadline and waits for the last connection forever.
	// Anything else under a second is refused too: a negative deadline has
	// already passed when the drain starts, and a bare number is
	// nanoseconds, so both drop every in-flight request. See the field
	// comment in NewListenConfig.
	switch {
	case c.ShutdownTimeout == 0:
		errs = append(errs, errors.New("fiber.listen.shutdown_timeout is "+
			"required: 0 does not select Fiber's 10s default, it removes the "+
			"drain deadline entirely and waits indefinitely for in-flight "+
			"requests"))
	case c.ShutdownTimeout < time.Second:
		errs = append(errs, fmt.Errorf("fiber.listen.shutdown_timeout must "+
			"be at least 1s (got %s): a shorter drain drops every request "+
			"still in flight, and if this was meant as seconds, write the "+
			"unit — a bare number is nanoseconds",
			c.ShutdownTimeout))
	}

	// CertFile and CertKeyFile are a pair, and Fiber does not check that
	// they are. Listen builds a TLS config only when BOTH are set; with one
	// of them it falls through to the no-TLS branch and serves plain HTTP,
	// without an error or a log line. So a certificate whose key was left
	// out of the environment is a silent downgrade rather than a startup
	// failure, and this check is what turns it into one.
	if (c.CertFile == "") != (c.CertKeyFile == "") {
		errs = append(errs, errors.New("fiber.listen.cert_file and "+
			"fiber.listen.cert_key_file must both be set (to serve TLS) or "+
			"both be empty (to serve plain HTTP): with only one of them, "+
			"Fiber serves plain HTTP without saying so"))
	}

	// Empty is valid because Fiber normalises it to tcp4. So only a non-empty
	// value is checked against the allowlist.
	if c.ListenerNetwork != "" {
		if _, ok := validListenerNetworks[c.ListenerNetwork]; !ok {
			errs = append(errs, fmt.Errorf("fiber.listen.listener_network "+
				"must be one of: tcp, tcp4, tcp6, unix (got %q)",
				c.ListenerNetwork))
		}
	}

	// 0 is valid because Fiber normalises it to TLS 1.2. So only a non-zero
	// value is checked against either 771 (TLS 1.2) or 772 (TLS 1.3).
	if c.TLSMinVersion != 0 &&
		c.TLSMinVersion != tlsVersion12 && c.TLSMinVersion != tlsVersion13 {
		errs = append(errs, fmt.Errorf("fiber.listen.tls_min_version must be "+
			"771 (TLS 1.2), 772 (TLS 1.3), or 0 to use Fiber's default of "+
			"TLS 1.2 (got %d)",
			c.TLSMinVersion))
	}

	// Empty is valid because Fiber normalises it to tcp4. Prefork binds
	// through SO_REUSEPORT, which fasthttp's reuseport package implements
	// for tcp4 and tcp6 only.
	if c.EnablePrefork && c.ListenerNetwork != "" &&
		c.ListenerNetwork != "tcp4" && c.ListenerNetwork != "tcp6" {
		errs = append(errs, fmt.Errorf("fiber.listen.listener_network must "+
			"be tcp4 or tcp6 when fiber.listen.enable_prefork is true "+
			"(got %q): prefork binds through SO_REUSEPORT, which fasthttp "+
			"implements for those two networks only",
			c.ListenerNetwork))
	}

	// A value above 0o777 is not a permission mode, and it is nearly always
	// one specific mistake: a mode written without its leading zero. `770` is
	// DECIMAL 770, which is 0o1402 — so the number in the error bears no
	// resemblance to the number that was typed, and an operator reading
	// "must be between 0 and 0777 (got 01402)" has to work out where 01402
	// came from before they can act on it. Naming the decimal reading and the
	// octal one in the same breath removes that step.
	//
	// Why the excess is worth rejecting at all — os.Chmod forwards only the
	// low nine bits, so anything above them vanishes without a word — is on
	// maxUnixSocketFileMode, not here: it explains the check rather than the
	// message, and it is not what someone staring at a failed startup needs in
	// the next sentence.
	if c.UnixSocketFileMode > maxUnixSocketFileMode {
		got := uint32(c.UnixSocketFileMode)
		msg := fmt.Sprintf("fiber.listen.unix_socket_file_mode must be "+
			"between 0 and 0777: %d evaluates to %#o",
			got,
			got)
		if meant, ok := octalIntent(got); ok {
			msg += fmt.Sprintf("; did you mean %#o?", meant)
		}
		errs = append(errs, errors.New(msg))
	}

	// 0 is valid, because Fiber replaces it with max(1, GOMAXPROCS/2), and so
	// is any positive count. Only a negative value is checked — and it has to
	// be, because Fiber's substitution tests for == 0, not <= 0, so a
	// negative value reaches fasthttp as a restart budget the crash count
	// exceeds on the first crash.
	if c.PreforkRecoverThreshold < 0 {
		errs = append(errs, fmt.Errorf(
			"fiber.listen.prefork_recover_threshold cannot be negative "+
				"(got %d): use 0 for Fiber's default of "+
				"max(1, GOMAXPROCS/2), or a positive restart count",
			c.PreforkRecoverThreshold))
	}

	// Both prefork durations reject a NEGATIVE value and accept 0, and the two
	// zeros mean opposite things — which is the whole reason each check names
	// what its own zero does rather than sharing a message.
	//
	// Unconditional, like the socket mode's range check above and unlike the
	// pairing rule below: a deployment that enables prefork later should not
	// inherit a number nobody ever validated.
	if c.PreforkRecoverInterval < 0 {
		errs = append(errs, fmt.Errorf(
			"fiber.listen.prefork_recover_interval cannot be negative "+
				"(got %s): fasthttp applies the backoff only when it is "+
				"above zero, so a negative value reads as a setting and acts "+
				"as none — use 0 to respawn a crashed child immediately, or "+
				"a positive backoff",
			c.PreforkRecoverInterval))
	}

	if c.PreforkShutdownGracePeriod < 0 {
		errs = append(errs, fmt.Errorf(
			"fiber.listen.prefork_shutdown_grace_period cannot be negative "+
				"(got %s): fasthttp replaces anything at or below zero "+
				"with %s, so a negative value reads as \"kill children "+
				"immediately\" and acts as that default — use 0 to take it "+
				"deliberately, or a positive deadline",
			c.PreforkShutdownGracePeriod,
			defaultPreforkShutdownGracePeriod))
	}

	// The pairing rule. shutdown_timeout is the WORKER's drain deadline;
	// prefork_shutdown_grace_period is how long the MASTER waits before
	// SIGKILLing that worker. When the master's number is the smaller one, the
	// difference is not a slower shutdown — it is requests killed in flight
	// while the worker still believes it has time, with nothing logged on
	// either side.
	//
	// Compared against the EFFECTIVE grace, not the literal value, since 0
	// here is fasthttp's 5s rather than "no deadline" — see the constant. And
	// gated on EnablePrefork, like the network check above, because without a
	// master there is nobody to enforce the shorter number: an unset grace
	// against any drain deadline is inert on a single-process deployment,
	// where fasthttp's substituted 5s is a number nothing ever reads.
	// Rejecting it there would fail configurations in which neither value is
	// consulted, and would make a key that is documented as prefork-only
	// mandatory for everyone.
	//
	// A grace period stated above the drain deadline passes this rule with or
	// without the gate. The gate is what keeps a deployment that never turns
	// prefork on from having to carry that key at all.
	if c.EnablePrefork {
		// graceDesc says where the number came from, because the two cases
		// need different edits. A literal 12s is a key to raise; a 5s that
		// came from the default is a key missing from config.yaml, and "raise
		// it" is unactionable until the message says there is nothing there to
		// raise. The <= 0 branch covers a negative too, which the check above
		// also reports — appending both is the point of this method.
		grace := c.PreforkShutdownGracePeriod
		graceDesc := c.PreforkShutdownGracePeriod.String()
		if grace <= 0 {
			grace = defaultPreforkShutdownGracePeriod
			graceDesc = fmt.Sprintf("%s, so fasthttp's default of %s",
				c.PreforkShutdownGracePeriod,
				grace)
		}
		if c.ShutdownTimeout > grace {
			errs = append(errs, fmt.Errorf("fiber.listen.shutdown_timeout "+
				"(%s) exceeds fiber.listen.prefork_shutdown_grace_period "+
				"(%s) while fiber.listen.enable_prefork is true: the prefork "+
				"master SIGKILLs a worker after the grace period, so "+
				"requests still draining at %s are dropped mid-flight while "+
				"that worker believes it has time left — raise the grace "+
				"period to at least the drain deadline, or lower the drain "+
				"deadline to fit inside it",
				c.ShutdownTimeout,
				graceDesc,
				grace))
		}
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of ListenConfig.
//
// The pointer receiver means fmt only picks this up for a *ListenConfig.
// Printing a value copy (%v on ListenConfig, not &ListenConfig) bypasses it
// and dumps the struct fields directly.
//
// The nil check is TWO-PART, mirroring Validate's. A logger's own guard, such
// as zerolog's `if val == nil` before it calls a Stringer, compares an
// INTERFACE with nil, which neither a typed nil pointer nor a wrapper around
// a nil embedded pointer satisfies. Without the second half, the half-built
// one would panic inside the startup log line instead of rendering a
// placeholder.
//
// UnixSocketFileMode prints in octal (%#o), the notation it is written in.
func (c *ListenConfig) String() string {
	if c == nil {
		return "<nil ListenConfig>"
	}
	if c.ListenConfig == nil {
		return "<uninitialised ListenConfig>"
	}
	return fmt.Sprintf("CertClientFile=%s "+
		"CertFile=%s "+
		"CertKeyFile=%s "+
		"DisableStartupMessage=%t "+
		"EnablePrefork=%t "+
		"EnablePrintRoutes=%t "+
		"ListenerNetwork=%s "+
		"PreforkRecoverInterval=%s "+
		"PreforkRecoverThreshold=%d "+
		"PreforkShutdownGracePeriod=%s "+
		"ShutdownTimeout=%s "+
		"TLSMinVersion=%d "+
		"UnixSocketFileMode=%#o",
		c.CertClientFile,
		c.CertFile,
		c.CertKeyFile,
		c.DisableStartupMessage,
		c.EnablePrefork,
		c.EnablePrintRoutes,
		c.ListenerNetwork,
		c.PreforkRecoverInterval,
		c.PreforkRecoverThreshold,
		c.PreforkShutdownGracePeriod,
		c.ShutdownTimeout,
		c.TLSMinVersion,
		uint32(c.UnixSocketFileMode),
	)
}
