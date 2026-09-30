package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Every test in this file drives a Service whose client field is a NIL
// *whatsmeow.Client. That is not a shortcut around a missing fake — it is the
// only seam this package has, and it is worth setting out why before reading
// any assertion below.
//
// Service holds a CONCRETE *whatsmeow.Client, not an interface, so nothing can
// be substituted for it. New is untestable for a second reason: it opens a
// SQLite file, dials WhatsApp's servers, and on an unpaired store blocks on
// the QR channel. So the reachable surface is SendText, SendDocument and
// Disconnect, and reaching them means reaching them with a nil client.
//
// That works because whatsmeow guards its own entry points. Verified against
// the whatsmeow revision pinned in go.mod — no version is named here on
// purpose, since a hash in a comment goes stale the first time anyone runs
// `go get -u` and nothing fails when it does:
//
//	Client.SendMessage   first statement is `if cli == nil { … }`
//	Client.Upload        → rawUpload → refreshMediaConn, which guards nil
//	Client.Disconnect    first statement is `if cli == nil { return }`
//
// So each of the three does something observable and returns instead of
// panicking. TestNilClientIsTheTestingSeam pins that assumption directly, so a
// whatsmeow bump that drops a guard fails there by name rather than turning
// every other test in the file into an unattributed panic.
//
// What this CANNOT cover, stated so nobody assumes otherwise:
//
//   - Anything past the first client call. SendDocument fails at Upload, so
//     its second error message ("send document to …") is unreachable without a
//     live client. See TestErrorMessagesIdentifyWhichStepFailed.
//   - The QR pairing flow, session persistence, and reconnect behaviour in
//     New — including the unpaired-window check, which is the one branch of
//     New with a decision in it.
//   - Whether WhatsApp actually accepts a JID this package builds. The tests
//     assert the string handed to whatsmeow, not the server's opinion of it.

// safeBuffer is a zerolog sink that survives -race.
//
// Only Disconnect and the success paths log, and no test here reaches
// either concurrently: against a nil client every send fails before its
// success line. The mutex is here so that TestSendIsSafeForConcurrentUse
// stays a test of the Service rather than of whether bytes.Buffer tolerates
// concurrent writes, should a send path ever log on failure.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// capturePanic runs fn and returns whatever it panicked with, or nil.
func capturePanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// packageSourceFiles lists the non-test .go files in the package directory.
//
// go test runs with the package directory as the working directory, so "." is
// the source under test. Listing rather than naming whatsapp.go means
// splitting the file does not quietly reduce what the meta-test above covers.
func packageSourceFiles(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() ||
			!strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		t.Fatal("no source files found; the meta-test would pass vacuously")
	}
	return files
}

// exprName renders the expressions that can legally appear as a JID server:
// a qualified constant, a bare identifier, or a string literal. Anything else
// returns a placeholder, which fails the comparison above — correctly, since a
// computed server is exactly the case this test exists to notice.
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
		return "<qualified>." + v.Sel.Name
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	default:
		return "<computed expression>"
	}
}

// assertPhoneInError checks that a failed send reports the NORMALISED number
// behind the given step prefix.
//
// Asserting the prefix and the number together, rather than a bare
// strings.Contains, is what stops "628123456789" matching some longer number
// elsewhere in a wrapped message.
func assertPhoneInError(t *testing.T, err error, prefix, wantPhone string) {
	t.Helper()

	if err == nil {
		t.Fatalf(
			"got nil, want an error naming the normalised number %q",
			wantPhone)
	}
	want := prefix + wantPhone + ": "
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf(
			"error %q does not start with %q — the number was not normalised "+
				"as documented", err, want)
	}
}

// sameError compares two errors by text, which is the right granularity
// here: the two forms should produce the SAME message, not merely two
// errors of the same kind.
func sameError(a, b error) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return a.Error() == b.Error()
}

// newTestService builds the Service under test and returns the buffer its
// logger writes to.
//
// DebugLevel rather than the package default so nothing is filtered out before
// a test can see it; the levels this package emits are all Info, but a future
// Debug line should be visible here without anyone having to work out why it
// is not.
func newTestService(t *testing.T) (*Service, *safeBuffer) {
	t.Helper()
	out := &safeBuffer{}
	return &Service{
		client: nil,
		log:    zerolog.New(out).Level(zerolog.DebugLevel),
	}, out
}

// Write appends p under the lock.
func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far, under the lock.
func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestNilClientIsTheTestingSeam pins the upstream nil guards this whole file
// rests on.
//
// It asserts two things per method: that the call returns rather than
// panicking, and — for the two that return an error — that the error is
// whatsmeow's own ErrClientIsNil rather than something this package invented.
// The second half matters because it proves the call actually REACHED
// whatsmeow. An error produced by a guard in this package would satisfy
// "no panic" just as well while proving nothing about the client call at all.
//
// If whatsmeow ever drops one of these guards, this test panics first and
// names the method, which is a far cheaper diagnosis than a stack trace
// attributed to whichever normalisation test happened to run first.
func TestNilClientIsTheTestingSeam(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t)

	tests := []struct {
		name     string
		call     func() error
		wantErr  bool
		guardRef string
	}{
		{
			name: "SendText",
			call: func() error {
				return svc.SendText(
					"628123456789",
					"hi")
			},
			wantErr:  true,
			guardRef: "whatsmeow.Client.SendMessage",
		},
		{
			name: "SendDocument",
			call: func() error {
				return svc.SendDocument(
					"628123456789", []byte("x"),
					"a.pdf", "application/pdf", "")
			},
			wantErr:  true,
			guardRef: "whatsmeow.Client.refreshMediaConn (via Upload)",
		},
		{
			name:     "Disconnect",
			call:     func() error { svc.Disconnect(); return nil },
			wantErr:  false,
			guardRef: "whatsmeow.Client.Disconnect",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var err error
			if p := capturePanic(func() { err = tc.call() }); p != nil {
				t.Fatalf(
					"panicked with %#v — the nil guard in %s is gone, and "+
						"every test in this file depends on it",
					p,
					tc.guardRef)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal(
						"got nil, want an error: a nil client cannot have " +
							"sent anything")
				}
				if !errors.Is(err, whatsmeow.ErrClientIsNil) {
					t.Errorf("got %v, want an error wrapping "+
						"whatsmeow.ErrClientIsNil — the call did not reach "+
						"whatsmeow", err)
				}
			} else if err != nil {
				t.Errorf("got %v, want nil", err)
			}
		})
	}
}

// TestSendTextNoOpsOnEmptyInput covers the guard the doc comment justifies: a
// user record with no phone number filled in is an expected operational state,
// not an error the caller should have to check for.
//
// The assertion is `err == nil`, and that is airtight rather than weak. With a
// nil client EVERY path that reaches whatsmeow necessarily fails, so a nil
// error can only mean the function returned before touching the client — which
// is precisely what the guard is for. The same reasoning is what makes
// TestSendDocumentDoesNotGuardFilenameOrMimetype meaningful in the other
// direction.
func TestSendTextNoOpsOnEmptyInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		phone   string
		message string
	}{
		{"no phone number on the user record", "", "new message"},
		{"nothing to say", "628123456789", ""},
		{"neither", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, out := newTestService(t)
			if err := svc.SendText(tc.phone, tc.message); err != nil {
				t.Errorf(
					"got %v, want nil — an empty input must be a silent "+
						"no-op",
					err)
			}
			if got := out.String(); got != "" {
				t.Errorf("a no-op wrote a log line:\n%s", got)
			}
		})
	}
}

// TestSendDocumentNoOpsOnEmptyInput is the same guard on the other method.
//
// Both empty-slice shapes are exercised because they arrive from different
// places: a nil slice is what a failed read returns, an empty non-nil slice is
// what reading a zero-byte file returns. len() treats them alike and the test
// records that it is len() being relied on, not a nil check.
func TestSendDocumentNoOpsOnEmptyInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		phone string
		data  []byte
	}{
		{"no phone number on the user record", "", []byte("%PDF-1.4")},
		{"nil data", "628123456789", nil},
		{"empty but non-nil data", "628123456789", []byte{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, out := newTestService(t)
			err := svc.SendDocument(
				tc.phone,
				tc.data,
				"Incoming Letter 042.pdf",
				"application/pdf",
				"caption")
			if err != nil {
				t.Errorf(
					"got %v, want nil — an empty input must be a silent "+
						"no-op",
					err)
			}
			if got := out.String(); got != "" {
				t.Errorf("a no-op wrote a log line:\n%s", got)
			}
		})
	}
}

// TestSendDocumentDoesNotGuardFilenameOrMimetype pins the asymmetry between
// the two methods.
//
// SendText refuses to send an empty MESSAGE. The nearest equivalents here —
// filename, mimetype, caption — are unguarded, so an empty filename produces a
// DocumentMessage carrying FileName:"" and the recipient gets a nameless
// attachment. SendDocument's doc comment states that deliberately; this is the
// assertion that keeps the statement true.
//
// An error back from a nil client is proof the upload was attempted, i.e. that
// no guard fired. If a filename guard is ever added, this fails — which is
// correct, because a caller passing a stored filename straight through relies
// on an empty one still being sent.
func TestSendDocumentDoesNotGuardFilenameOrMimetype(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		mimetype string
		caption  string
	}{
		{"empty filename", "", "application/pdf", "caption"},
		{"empty mimetype", "Incoming Letter 042.pdf", "", "caption"},
		{"empty caption", "Incoming Letter 042.pdf", "application/pdf", ""},
		{"all three empty", "", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, _ := newTestService(t)
			err := svc.SendDocument(
				"628123456789",
				[]byte("%PDF-1.4"),
				tc.filename,
				tc.mimetype,
				tc.caption)
			if err == nil {
				t.Fatal(
					"got nil — a guard fired that the callers are " +
						"not told about; see SendText's message check " +
						"for the shape a real guard has")
			}
			if !errors.Is(err, whatsmeow.ErrClientIsNil) {
				t.Errorf(
					"got %v, want the upload to have been attempted",
					err)
			}
		})
	}
}

// TestPhoneNormalisationRunsBeforeTheEmptyGuard covers a number that is not
// empty but normalises to nothing.
//
// A guard comparing the RAW value against "" would let a phone column holding
// a single space through: it normalises to "" and produces
// types.NewJID("", DefaultUserServer), which renders as the bare
// "s.whatsapp.net", WhatsApp's own address rather than any user's. The send
// then fails somewhere remote from here with nothing pointing back at the
// field.
//
// The two halves are both needed. `err == nil` proves nothing was sent, since
// a nil client makes every real path error. The JID assertion states what is
// being avoided, so the test still explains itself once the reasoning is a
// year old.
func TestPhoneNormalisationRunsBeforeTheEmptyGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		phone string
	}{
		{"a single space", " "},
		{"several spaces", "   "},
		{"a lone plus", "+"},
		{"punctuation only", "+ - "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, out := newTestService(t)

			if err := svc.SendText(tc.phone, "new message"); err != nil {
				t.Errorf(
					"SendText: got %v, want nil — %q normalises to nothing "+
						"and is as empty as \"\"",
					err,
					tc.phone)
			}
			err := svc.SendDocument(
				tc.phone,
				[]byte("%PDF-1.4"),
				"a.pdf",
				"application/pdf",
				"")
			if err != nil {
				t.Errorf("SendDocument: got %v, want nil", err)
			}
			if got := out.String(); got != "" {
				t.Errorf("a no-op wrote a log line:\n%s", got)
			}
		})
	}

	// What an unnormalised guard would produce, stated directly rather than
	// left to be inferred from an absent error.
	got := types.NewJID("", types.DefaultUserServer).String()
	if got != types.DefaultUserServer {
		t.Errorf(
			"empty-user JID renders as %q, want %q",
			got,
			types.DefaultUserServer)
	}
}

// TestPhoneNormalisation walks the formats the doc comment promises to accept.
//
// The observation point is the ERROR MESSAGE, which embeds the normalised
// number — the only place it becomes visible without a live client. That is
// indirect, and it is also the reason TestErrorMessagesIdentifyWhichStepFailed
// exists: the prefixes asserted here are load-bearing for this test, not just
// for an operator reading a log.
//
// Both methods are exercised with the same table on purpose. They share one
// normalisePhone helper, and running the table twice is what proves the
// sharing is real rather than asserted in a comment. It is also what would
// catch one method growing its own pre-processing step before the call.
func TestPhoneNormalisation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"already bare digits", "628123456789", "628123456789"},
		{"leading plus", "+628123456789", "628123456789"},
		{"spaces", "62 812 3456789", "628123456789"},
		{"hyphens", "62-812-3456789", "628123456789"},
		{"plus, spaces and hyphens", "+62 812-3456789", "628123456789"},
		{"the doc comment's own example", "62 812-3456789", "628123456789"},
		{"leading and trailing space", " 628123456789 ", "628123456789"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			t.Run("SendText", func(t *testing.T) {
				svc, _ := newTestService(t)
				err := svc.SendText(tc.input, "new message")
				assertPhoneInError(t, err, "whatsapp: send to ", tc.want)
			})

			t.Run("SendDocument", func(t *testing.T) {
				svc, _ := newTestService(t)
				err := svc.SendDocument(
					tc.input,
					[]byte("%PDF-1.4"),
					"a.pdf",
					"application/pdf", "")
				assertPhoneInError(
					t, err, "whatsapp: upload document for ", tc.want)
			})
		})
	}
}

// TestNormalisationLeavesOtherPunctuationIntact pins the limitation the doc
// comment states and the consequence a shorter statement of it would miss.
//
// Only "+", " " and "-" are stripped. A number like "(+62) 812.3456789" is not
// merely malformed: types.ParseJID reads a "." in the user part as the agent
// separator of an AD-JID, so the string round-trips into a DIFFERENT kind of
// JID rather than being rejected.
//
// This matters operationally rather than theoretically. The number is free
// text typed into a user record and typically reaches SendText unvalidated.
func TestNormalisationLeavesOtherPunctuationIntact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"parentheses survive", "(+62) 8123456789", "(62)8123456789"},
		{"dots survive", "62.812.3456789", "62.812.3456789"},
		{"parentheses and a dot", "(+62) 812.3456789", "(62)812.3456789"},
	}

	// Two assertions per case, and the split is the point. normalisePhone
	// LEAVES the punctuation — that is what this test is named for — and
	// the caller then REFUSES the result rather than filtering further,
	// which is the decision normalisePhone's comment argues for. Asserting
	// only the second would not show that the punctuation survived; only
	// the first would not show that surviving it is refused.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := normalisePhone(tc.input); got != tc.want {
				t.Errorf("normalisePhone(%q) = %q, want %q",
					tc.input, got, tc.want)
			}

			svc, _ := newTestService(t)
			err := svc.SendText(tc.input, "new message")
			if !errors.Is(err, ErrInvalidPhone) {
				t.Fatalf("SendText(%q) = %v, want ErrInvalidPhone",
					tc.input, err)
			}
			// The error quotes the number AS GIVEN, not as normalised:
			// the value to go and fix is the one in the stored record.
			if !strings.Contains(err.Error(), tc.input) {
				t.Errorf("error %q does not quote the original %q",
					err, tc.input)
			}
		})
	}

	// The dot is worse than "malformed". ParseJID splits the user part on "."
	// and reads the second half as an agent, so a number with one dot too few
	// is a parse error and a number with exactly one is a valid AD-JID naming
	// somebody else's device.
	t.Run("a dot is an AD-JID separator, not a rejection", func(t *testing.T) {
		t.Parallel()

		jid := types.NewJID("628.1", types.DefaultUserServer)
		parsed, err := types.ParseJID(jid.String())
		if err != nil {
			t.Fatalf("ParseJID(%q): %v", jid.String(), err)
		}
		if parsed.User != "628" || parsed.RawAgent != 1 {
			t.Errorf(
				"got user %q agent %d, want the dot to have been read as an "+
					"agent separator",
				parsed.User,
				parsed.RawAgent)
		}
	})
}

// TestJIDRenderingContract pins the string a normalised number turns into.
//
// This is a test of whatsmeow, deliberately: the doc comment on SendText makes
// a promise about the JID shape ("<phone>@s.whatsapp.net") that this package
// does not implement and cannot check, because the JID is built and consumed
// inside one expression. A whatsmeow change to JID.String would otherwise
// surface as undelivered notifications with nothing here to point at.
func TestJIDRenderingContract(t *testing.T) {
	t.Parallel()

	jid := types.NewJID("628123456789", types.DefaultUserServer)

	if got, want := jid.String(), "628123456789@s.whatsapp.net"; got != want {
		t.Errorf("JID: got %q, want %q", got, want)
	}
	if jid.Server != types.DefaultUserServer {
		t.Errorf(
			"server: got %q, want %q",
			jid.Server,
			types.DefaultUserServer)
	}
}

// TestSourceBuildsUserJIDsOnly reads this package's own source, because the
// behaviour it protects is invisible from outside.
//
// The JID is constructed and consumed within a single statement — built on one
// line, passed to a client call on the next — so it never becomes an argument
// any test can inspect. Swapping types.DefaultUserServer for types.GroupServer
// in either method passes every other test in this file.
//
// Parsing the source is the right tool because the property is structural, so
// a structural test is the one that can see it. The alternative is an
// interface seam over *whatsmeow.Client, which is the better long-term answer
// and a bigger change than a test file should assume.
//
// Two things are asserted. Every types.NewJID call uses the user server, and
// there are exactly two of them — so a third send path added later has to come
// here and add itself to the tables above rather than inheriting silence.
func TestSourceBuildsUserJIDsOnly(t *testing.T) {
	t.Parallel()

	files := packageSourceFiles(t)

	type site struct {
		pos    string
		server string
	}
	var sites []site

	for _, name := range files {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewJID" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "types" {
				return true
			}
			if len(call.Args) != 2 {
				t.Errorf(
					"%s: types.NewJID takes 2 arguments, got %d",
					fset.Position(call.Pos()),
					len(call.Args))
				return true
			}
			sites = append(sites, site{
				pos:    fset.Position(call.Args[1].Pos()).String(),
				server: exprName(call.Args[1]),
			})
			return true
		})
	}

	if len(sites) != 2 {
		t.Fatalf(
			"found %d types.NewJID calls, want 2 (SendText and "+
				"SendDocument): %+v",
			len(sites),
			sites)
	}
	for _, s := range sites {
		if s.server != "types.DefaultUserServer" {
			t.Errorf(
				"%s: JID built with server %s, want "+
					"types.DefaultUserServer — this package sends "+
					"individual notifications only, and a group JID "+
					"would deliver one to everyone in a group",
				s.pos,
				s.server)
		}
	}
}

// TestErrorMessagesIdentifyWhichStepFailed pins the message prefixes.
//
// SendDocument is a two-step operation whose failures read almost identically
// ("upload document for X" versus "send document to X"). Callers log these at
// Warn and do nothing else with them, so the prefix is the entire diagnosis:
// it says whether WhatsApp's media servers were unreachable or whether the
// message itself was refused.
//
// The send-side prefix is NOT asserted here, and cannot be: a nil client fails
// at Upload, so the second message is unreachable without a live connection.
// Listed anyway so its absence is a known gap rather than an assumed pass.
func TestErrorMessagesIdentifyWhichStepFailed(t *testing.T) {
	t.Parallel()

	const phone = "628123456789"

	t.Run("SendText names the recipient and the step", func(t *testing.T) {
		t.Parallel()

		svc, _ := newTestService(t)
		err := svc.SendText(phone, "new message")
		if err == nil {
			t.Fatal("got nil, want an error")
		}
		want := "whatsapp: send to " + phone + ": "
		if !strings.HasPrefix(err.Error(), want) {
			t.Errorf("error %q does not start with %q", err, want)
		}
	})

	t.Run("SendDocument distinguishes upload from send", func(t *testing.T) {
		t.Parallel()

		svc, _ := newTestService(t)
		err := svc.SendDocument(
			phone, []byte("%PDF-1.4"), "a.pdf", "application/pdf", "")
		if err == nil {
			t.Fatal("got nil, want an error")
		}
		want := "whatsapp: upload document for " + phone + ": "
		if !strings.HasPrefix(err.Error(), want) {
			t.Errorf("error %q does not start with %q", err, want)
		}
	})
}

// TestErrorsWrapRatherThanReplace checks that the cause survives fmt.Errorf.
//
// %w rather than %v is what lets a caller distinguish "not paired yet" from
// "network is down" with errors.Is. A caller that only logs and moves on needs
// none of that, but the alternative is a decision that cannot be reversed
// later without touching every message.
func TestErrorsWrapRatherThanReplace(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t)

	textErr := svc.SendText("628123456789", "new message")
	if !errors.Is(textErr, whatsmeow.ErrClientIsNil) {
		t.Errorf("SendText: %v does not wrap the cause", textErr)
	}

	docErr := svc.SendDocument(
		"628123456789", []byte("%PDF-1.4"), "a.pdf", "application/pdf", "")
	if !errors.Is(docErr, whatsmeow.ErrClientIsNil) {
		t.Errorf("SendDocument: %v does not wrap the cause", docErr)
	}
}

// TestErrorsEmbedTheRecipientsPhoneNumber records a privacy consequence rather
// than a defect, so that changing it is a decision and leaving it is one too.
//
// Both error messages interpolate the number, so a caller whose only handling
// is to log the error puts a mobile number wherever its logs go, once per
// failed notification.
//
// If the number is ever redacted here, this test fails and points at the
// callers that would need re-reading.
func TestErrorsEmbedTheRecipientsPhoneNumber(t *testing.T) {
	t.Parallel()

	const phone = "628123456789"
	svc, _ := newTestService(t)

	err := svc.SendText(phone, "new message")
	if err == nil {
		t.Fatal("got nil, want an error")
	}
	if !strings.Contains(err.Error(), phone) {
		t.Errorf("error %q no longer carries the number. If that was "+
			"deliberate, delete this test and re-read the callers that "+
			"log it; if it was not, the message just lost the field that "+
			"identifies which notification failed", err)
	}
}

// TestFailedSendsAreSilent checks that this package logs nothing on the error
// path, leaving the decision to the caller.
//
// The division is deliberate and worth protecting: SendText logs the SUCCESS
// (Info, with the number), and the caller logs the FAILURE alongside whatever
// record it was notifying about. Logging here as well would double every
// failed notification, and the duplicate is the one carrying no field that
// ties it back to that record.
func TestFailedSendsAreSilent(t *testing.T) {
	t.Parallel()

	svc, out := newTestService(t)

	_ = svc.SendText("628123456789", "new message")
	_ = svc.SendDocument(
		"628123456789", []byte("%PDF-1.4"), "a.pdf", "application/pdf", "")

	if got := out.String(); got != "" {
		t.Errorf(
			"a failed send logged from inside the package; the caller "+
				"already logs it:\n%s",
			got)
	}
}

// TestDisconnectIsSafeAndAnnouncesItself covers the shutdown path.
//
// Nothing in this package registers Disconnect into a shutdown sequence, so
// this is the only place its behaviour is exercised at all. The log line is
// asserted because it is the only evidence a shutdown hook would have that
// the call landed.
func TestDisconnectIsSafeAndAnnouncesItself(t *testing.T) {
	t.Parallel()

	svc, out := newTestService(t)

	if p := capturePanic(svc.Disconnect); p != nil {
		t.Fatalf("Disconnect panicked with %#v", p)
	}

	got := out.String()
	if !strings.Contains(got, "whatsapp: disconnected") {
		t.Errorf("Disconnect logged nothing recognisable:\n%s", got)
	}
	if !strings.Contains(got, `"level":"info"`) {
		t.Errorf("expected an info-level entry:\n%s", got)
	}
}

// TestSendIsSafeForConcurrentUse backs the "safe for concurrent use" claim as
// far as this package can.
//
// The claim has two halves and only one is testable here. Whether whatsmeow
// serialises writes to the WebSocket is whatsmeow's business (it does —
// NoiseSocket.SendFrame takes a write lock). What this package owns is that
// Service adds no mutable state of its own on top, and that is exactly what
// -race checks below: both fields are read-only after construction, and both
// methods keep their working values in locals.
//
// Run with -race or this test proves very little.
func TestSendIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	svc, _ := newTestService(t)

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = svc.SendText("+62 812-3456789", "new message")
				return
			}
			_ = svc.SendDocument(
				"+62 812-3456789",
				[]byte("%PDF-1.4"),
				"a.pdf",
				"application/pdf",
				"")
		}(i)
	}
	wg.Wait()
}

// TestANilServiceIsNotSafe documents the asymmetry between a nil CLIENT and a
// nil SERVICE, which is easy to conflate after reading the rest of this file.
//
// whatsmeow guards its nil receivers; this package does not. That is fine and
// probably correct — but it is load-bearing for a caller that holds a
// *Service which is nil when WhatsApp is not configured. Its nil check is not
// defensive tidiness; it is the only thing standing between an unconfigured
// deployment and a panic, which a call dispatched into a detached goroutine
// has nobody to catch.
//
// The empty-input case is the trap: it returns cleanly on a nil Service
// because the guard runs before any field access, so a smoke test with a blank
// phone number would suggest the whole thing is nil-safe.
func TestANilServiceIsNotSafe(t *testing.T) {
	t.Parallel()

	var svc *Service

	t.Run("the empty-input guard survives a nil receiver", func(t *testing.T) {
		t.Parallel()

		if p := capturePanic(func() { _ = svc.SendText("", "") }); p != nil {
			t.Errorf(
				"panicked with %#v; the guard reads no fields and should be "+
					"reachable",
				p)
		}
	})

	t.Run("a real send does not", func(t *testing.T) {
		t.Parallel()

		p := capturePanic(func() {
			_ = svc.SendText("628123456789", "new message")
		})
		if p == nil {
			t.Error(
				"got no panic — if this package became nil-receiver safe, " +
					"the nil checks at the call sites can be revisited")
		}
	})
}

// storeDSN is the one place a caller-supplied string is spliced into
// something a parser reads, so it gets the same treatment the database
// package gives a DSN: the escaping is asserted rather than described.
func TestStoreDSN(t *testing.T) {
	t.Parallel()

	const pragmas = "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"

	tests := []struct {
		name, path, want string
	}{
		{
			"empty falls back to the default",
			"",
			"file:" + DefaultStorePath + pragmas,
		},
		{
			"a relative path is used as given",
			"var/whatsapp.db",
			"file:var/whatsapp.db" + pragmas,
		},
		{
			// One slash after "file:" is an absolute path to SQLite,
			// so nothing has to tell the two shapes apart.
			"an absolute path needs no special case",
			"/var/lib/app/whatsapp.db",
			"file:/var/lib/app/whatsapp.db" + pragmas,
		},
		{
			// Without the escape this truncates at the "?" and opens
			// a different, empty database in which the device is
			// unpaired — a working process that has silently
			// forgotten its pairing.
			"a question mark cannot open the query",
			"odd?dir/whatsapp.db",
			"file:odd%3Fdir/whatsapp.db" + pragmas,
		},
		{
			"a hash cannot open a fragment",
			"reports#2/whatsapp.db",
			"file:reports%232/whatsapp.db" + pragmas,
		},
		{
			// SQLite decodes percent-escapes, so a literal "%" has to
			// survive as one.
			"a percent is escaped, and the escapes are not rescanned",
			"100%/whatsapp.db",
			"file:100%25/whatsapp.db" + pragmas,
		},
		{
			"every special character at once",
			"a%b?c#d.db",
			"file:a%25b%3Fc%23d.db" + pragmas,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := storeDSN(tc.path); got != tc.want {
				t.Errorf("storeDSN(%q) =\n%q\nwant\n%q",
					tc.path, got, tc.want)
			}
		})
	}
}

// The pragma order is load-bearing rather than cosmetic: the driver
// documents busy timeout as one of the pragmas that must be set first.
func TestStoreDSNSetsBusyTimeoutBeforeForeignKeys(t *testing.T) {
	t.Parallel()

	dsn := storeDSN("")
	busy := strings.Index(dsn, "busy_timeout")
	fk := strings.Index(dsn, "foreign_keys")
	if busy < 0 || fk < 0 {
		t.Fatalf("both pragmas must be present: %s", dsn)
	}
	if busy > fk {
		t.Errorf("busy_timeout comes after foreign_keys in %s", dsn)
	}
}

// Documented: the no-op cases are decided before the context is consulted,
// so "nothing to send" is one answer rather than one that depends on
// timing. A context that is already dead must not turn a silent no-op into
// an error.
func TestNoOpsAreDecidedBeforeTheContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc, out := newTestService(t)

	if err := svc.SendTextContext(ctx, "", "a message"); err != nil {
		t.Errorf("SendTextContext with no recipient: %v, want nil", err)
	}
	if err := svc.SendTextContext(ctx, "628123456789", ""); err != nil {
		t.Errorf("SendTextContext with no message: %v, want nil", err)
	}
	if err := svc.SendDocumentContext(
		ctx, "628123456789", nil, "a.pdf", "application/pdf", ""); err != nil {
		t.Errorf("SendDocumentContext with no data: %v, want nil", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("a no-op wrote a log line:\n%s", got)
	}
}

// A malformed recipient is refused before the context too, and by the same
// reasoning: it is a bad stored value, not a delivery failure, so it must
// not change shape depending on whether the caller's deadline has passed.
func TestInvalidPhoneIsRefusedRegardlessOfTheContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc, _ := newTestService(t)
	const bad = "(62)8123456789"

	if err := svc.SendTextContext(ctx, bad, "m"); !errors.Is(
		err, ErrInvalidPhone) {
		t.Errorf("SendTextContext = %v, want ErrInvalidPhone", err)
	}
	err := svc.SendDocumentContext(
		ctx, bad, []byte("%PDF-1.4"), "a.pdf", "application/pdf", "")
	if !errors.Is(err, ErrInvalidPhone) {
		t.Errorf("SendDocumentContext = %v, want ErrInvalidPhone", err)
	}
}

// The shortcuts add a context and nothing else, so every outcome a test can
// observe has to be identical between the two forms. Asserting it here is
// what stops the pair drifting into two implementations.
func TestShortcutsMatchTheirContextForms(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const bad = "62.812.3456789"

	t.Run("SendText", func(t *testing.T) {
		t.Parallel()
		svc, _ := newTestService(t)
		for _, c := range []struct{ phone, message string }{
			{"", "m"},
			{" + - ", "m"},
			{"628123456789", ""},
			{bad, "m"},
			{"628123456789", "m"},
		} {
			short := svc.SendText(c.phone, c.message)
			long := svc.SendTextContext(ctx, c.phone, c.message)
			if !sameError(short, long) {
				t.Errorf("SendText(%q, %q) = %v, context form = %v",
					c.phone, c.message, short, long)
			}
		}
	})

	t.Run("SendDocument", func(t *testing.T) {
		t.Parallel()
		svc, _ := newTestService(t)
		pdf := []byte("%PDF-1.4")
		for _, c := range []struct {
			phone string
			data  []byte
		}{
			{"", pdf},
			{"628123456789", nil},
			{bad, pdf},
			{"628123456789", pdf},
		} {
			short := svc.SendDocument(
				c.phone, c.data, "a.pdf", "application/pdf", "")
			long := svc.SendDocumentContext(
				ctx, c.phone, c.data, "a.pdf", "application/pdf", "")
			if !sameError(short, long) {
				t.Errorf("SendDocument(%q, %d bytes) = %v, "+
					"context form = %v",
					c.phone, len(c.data), short, long)
			}
		}
	})
}
