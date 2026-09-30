package email

// Tests for message.go.
//
// Every assertion reads the composed bytes back through net/mail,
// mime/multipart and mime's word decoder, which stand in for a
// recipient's software, rather than comparing against the bytes this
// package meant to write. A test of that second kind passes for a message
// no client can read.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// entity is one decoded MIME entity: a leaf with its decoded body, or a
// multipart with its children.
type entity struct {
	mediaType  string
	params     map[string]string
	disp       string
	dispParams map[string]string
	body       []byte
	children   []entity
}

// testID is the Message-ID every composed test message carries, so its
// header is predictable.
const testID = "<id@example.go.id>"

// Fixed inputs for compose, so every test message is reproducible.
var (
	testFrom = &mail.Address{
		Name:    "Notifikasi",
		Address: "noreply@example.go.id",
	}
	testDate = time.Date(2026, 9, 25, 14, 30, 0, 0,
		time.FixedZone("WIB", 7*60*60))
)

// shape renders an entity's structure, "multipart/mixed[text/plain,
// application/pdf]" say.
func shape(e entity) string {
	if len(e.children) == 0 {
		return e.mediaType
	}
	kids := make([]string, len(e.children))
	for i, c := range e.children {
		kids[i] = shape(c)
	}
	return e.mediaType + "[" + strings.Join(kids, ",") + "]"
}

// build composes m the way Send does, with the date and Message-ID
// pinned.
func build(t *testing.T, m Message) []byte {
	t.Helper()
	env, err := parseEnvelope(m)
	if err != nil {
		t.Fatalf("parseEnvelope: %v", err)
	}
	raw, err := compose(testFrom, env, m, testDate, testID)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	return raw
}

// read parses raw the way a recipient's software would.
func read(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v\n%s", err, raw)
	}
	return msg
}

// decode reads the entity whose header get looks up and whose raw body
// is r, decoding the transfer encoding itself so the test sees exactly
// what the message declared.
func decode(t *testing.T, get func(string) string, r io.Reader) entity {
	t.Helper()
	mt, params, err := mime.ParseMediaType(get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type %q: %v", get("Content-Type"), err)
	}
	e := entity{mediaType: mt, params: params}
	if d := get("Content-Disposition"); d != "" {
		if e.disp, e.dispParams, err = mime.ParseMediaType(d); err != nil {
			t.Fatalf("Content-Disposition %q: %v", d, err)
		}
	}

	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				return e
			}
			if err != nil {
				t.Fatalf("NextRawPart: %v", err)
			}
			e.children = append(e.children, decode(t, p.Header.Get, p))
		}
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	switch cte := get("Content-Transfer-Encoding"); cte {
	case "quoted-printable":
		e.body, err = io.ReadAll(quotedprintable.NewReader(
			bytes.NewReader(raw)))
	case "base64":
		// The decoder skips the CRLFs between lines.
		e.body, err = base64.StdEncoding.DecodeString(string(raw))
	default:
		t.Fatalf("unexpected Content-Transfer-Encoding %q", cte)
	}
	if err != nil {
		t.Fatalf("decoding %s body: %v", mt, err)
	}
	return e
}

// root decodes the top-level entity of msg.
func root(t *testing.T, msg *mail.Message) entity {
	t.Helper()
	return decode(t, msg.Header.Get, msg.Body)
}

// The headers read back through a recipient's parser match what was sent,
// a non-ASCII subject and display name included.
func TestComposeHeadersReadBack(t *testing.T) {
	t.Parallel()
	subject := "Laporan realisasi — Triwulan III ✓"
	msg := read(t, build(t, Message{
		To:      []string{"Budi Santoso <budi@example.go.id>"},
		Cc:      []string{"Évelyne <eve@example.go.id>"},
		ReplyTo: "Helpdesk <helpdesk@example.go.id>",
		Subject: subject,
		Text:    "isi",
	}))

	var dec mime.WordDecoder
	got, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || got != subject {
		t.Errorf("Subject decodes to %q (%v), want %q", got, err, subject)
	}

	for _, c := range []struct{ field, name, addr string }{
		{"From", "Notifikasi", "noreply@example.go.id"},
		{"To", "Budi Santoso", "budi@example.go.id"},
		{"Cc", "Évelyne", "eve@example.go.id"},
		{"Reply-To", "Helpdesk", "helpdesk@example.go.id"},
	} {
		list, err := msg.Header.AddressList(c.field)
		if err != nil || len(list) != 1 ||
			list[0].Name != c.name || list[0].Address != c.addr {
			t.Errorf("%s = %v (%v), want %s <%s>",
				c.field, list, err, c.name, c.addr)
		}
	}

	if date, err := msg.Header.Date(); err != nil || !date.Equal(testDate) {
		t.Errorf("Date = %v (%v), want %v", date, err, testDate)
	}
	for field, want := range map[string]string{
		"Message-ID":     testID,
		"Auto-Submitted": "auto-generated",
		"MIME-Version":   "1.0",
	} {
		if got := msg.Header.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

// RFC 5322 requires CRLF and at most 998 characters on every line, and
// asks for 78 in a header. A long subject and a long recipient list are
// what push a header past that, so both are folded, and both still read
// back whole.
func TestComposeKeepsEveryLineLegal(t *testing.T) {
	t.Parallel()
	subject := strings.Repeat("Laporan realisasi — Triwulan III ✓ ", 6)
	var to []string
	for i := range 12 {
		to = append(to, "Penerima Nomor "+string(rune('A'+i))+
			" <penerima"+string(rune('a'+i))+"@example.go.id>")
	}
	raw := build(t, Message{
		To:      to,
		Subject: subject,
		Text:    strings.Repeat("baris panjang tanpa jeda ", 40),
		Attachments: []Attachment{{
			Filename: "data.bin",
			Data:     bytes.Repeat([]byte{0, 1, 2, 250}, 500),
		}},
	})

	if bytes.Count(raw, []byte("\n")) !=
		bytes.Count(raw, []byte("\r\n")) ||
		bytes.Count(raw, []byte("\r")) !=
			bytes.Count(raw, []byte("\r\n")) {
		t.Fatal("a line ends in a bare LF or CR")
	}
	head, _, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatal("no blank line between header and body")
	}
	for _, line := range strings.Split(string(head), "\r\n") {
		if len(line) > foldAt {
			t.Errorf("header line of %d bytes: %q", len(line), line)
		}
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Errorf("line of %d bytes", len(line))
		}
	}

	msg := read(t, raw)
	var dec mime.WordDecoder
	if got, err := dec.DecodeHeader(msg.Header.Get("Subject")); err != nil ||
		got != subject {
		t.Errorf("folded Subject decodes to %q (%v)", got, err)
	}
	if list, err := msg.Header.AddressList("To"); err != nil ||
		len(list) != len(to) {
		t.Errorf("folded To reads back as %d addresses (%v), want %d",
			len(list), err, len(to))
	}
}

// A line break in the subject is carried as text. If it were written
// raw, whatever followed it would be read as a header of its own — a Bcc
// the sender never chose, or a blank line that ends the header early.
func TestASubjectCannotAddAHeader(t *testing.T) {
	t.Parallel()
	subject := "Halo\r\nBcc: attacker@evil.example\r\n\r\nisi palsu"
	msg := read(t, build(t, Message{
		To:      []string{"budi@example.go.id"},
		Subject: subject,
		Text:    "isi asli",
	}))

	if got := msg.Header.Get("Bcc"); got != "" {
		t.Fatalf("the subject added a header: Bcc: %s", got)
	}
	var dec mime.WordDecoder
	if got, _ := dec.DecodeHeader(msg.Header.Get("Subject")); got !=
		subject {
		t.Errorf("Subject decodes to %q, want %q", got, subject)
	}
	if got := string(root(t, msg).body); got != "isi asli" {
		t.Errorf("body = %q, want the real one", got)
	}
}

// The body takes the shape content documents for each combination of
// text, HTML and attachments.
func TestComposeStructure(t *testing.T) {
	t.Parallel()
	pdf := Attachment{Filename: "a.pdf", Data: []byte("%PDF-1.7")}
	for _, c := range []struct {
		name string
		m    Message
		want string
	}{
		{"text", Message{Text: "t"}, "text/plain"},
		{"html", Message{HTML: "<p>h</p>"}, "text/html"},
		{"text and html", Message{Text: "t", HTML: "<p>h</p>"},
			"multipart/alternative[text/plain,text/html]"},
		{"file only", Message{Attachments: []Attachment{pdf}},
			"multipart/mixed[application/pdf]"},
		{"text and file", Message{Text: "t",
			Attachments: []Attachment{pdf}},
			"multipart/mixed[text/plain,application/pdf]"},
		{"everything", Message{Text: "t", HTML: "<p>h</p>",
			Attachments: []Attachment{pdf, pdf}},
			"multipart/mixed[multipart/alternative[text/plain," +
				"text/html],application/pdf,application/pdf]"},
		{"subject only", Message{Subject: "s"}, "text/plain"},
		{"blank text is no text", Message{Text: " \n ", HTML: "<p/>"},
			"text/html"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.m.To = []string{"budi@example.go.id"}
			if got := shape(root(t, read(t, build(t, c.m)))); got !=
				c.want {
				t.Errorf("structure = %s, want %s", got, c.want)
			}
		})
	}
}

// Every line ending leaves as CRLF, and everything else in the text
// arrives as it was: a line longer than quoted-printable's 76, trailing
// spaces, "=", a leading dot, and characters outside ASCII.
func TestTextRoundTrips(t *testing.T) {
	t.Parallel()
	in := "Yth. Bapak/Ibu,\n" +
		strings.Repeat("realisasi anggaran ", 12) + "\n" +
		"spasi di akhir   \n" +
		".titik di awal\r\n" +
		"a=b\r" +
		"é ✓ selesai"
	want := strings.NewReplacer("\r\n", "\r\n", "\r", "\r\n",
		"\n", "\r\n").Replace(in)

	for _, m := range []Message{{Text: in}, {HTML: in}} {
		m.To = []string{"budi@example.go.id"}
		e := root(t, read(t, build(t, m)))
		if got := string(e.body); got != want {
			t.Errorf("%s body =\n%q\nwant\n%q", e.mediaType, got, want)
		}
		if e.params["charset"] != "utf-8" {
			t.Errorf("%s charset = %q", e.mediaType, e.params["charset"])
		}
	}
}

// The declared charset is UTF-8, so bytes that are not UTF-8 cannot go
// out as they are.
func TestInvalidUTF8BecomesTheReplacementCharacter(t *testing.T) {
	t.Parallel()
	msg := read(t, build(t, Message{
		To:      []string{"budi@example.go.id"},
		Subject: "caf\xe9",
		Text:    "caf\xe9",
	}))
	var dec mime.WordDecoder
	subject, _ := dec.DecodeHeader(msg.Header.Get("Subject"))
	for what, got := range map[string]string{
		"Subject": subject,
		"body":    string(root(t, msg).body),
	} {
		if got != "caf\uFFFD" {
			t.Errorf("%s = %q, want %q", what, got, "caf\uFFFD")
		}
	}
}

// An attachment holding every byte value decodes back unchanged from the
// composed message.
func TestAttachmentRoundTrips(t *testing.T) {
	t.Parallel()
	data := make([]byte, 256*300)
	for i := range data {
		data[i] = byte(i)
	}
	name := "Laporan Realisasi – Triwulan III ✓.pdf"
	raw := build(t, Message{
		To: []string{"budi@example.go.id"},
		Attachments: []Attachment{{
			Filename:    name,
			ContentType: "application/pdf",
			Data:        data,
		}},
	})

	file := root(t, read(t, raw)).children[0]
	if !bytes.Equal(file.body, data) {
		t.Error("the attachment's bytes changed on the way")
	}
	if file.mediaType != "application/pdf" {
		t.Errorf("type = %s, want application/pdf", file.mediaType)
	}
	if file.disp != "attachment" {
		t.Errorf("disposition = %q, want attachment", file.disp)
	}
	if file.params["name"] != name || file.dispParams["filename"] != name {
		t.Errorf("name = %q and filename = %q, want %q both",
			file.params["name"], file.dispParams["filename"], name)
	}

	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz0123456789+/="
	for _, line := range strings.Split(string(raw), "\r\n") {
		isBase64 := line != "" && strings.Trim(line, alphabet) == ""
		if isBase64 && len(line) > base64Line {
			t.Fatalf("base64 line of %d characters", len(line))
		}
	}
}

// attachmentType uses the given media type when it parses, the type
// registered for the extension otherwise, and application/octet-stream
// last.
func TestAttachmentType(t *testing.T) {
	t.Parallel()
	xlsx := "application/vnd.openxmlformats-officedocument." +
		"spreadsheetml.sheet"
	for _, c := range []struct {
		given, name, want string
	}{
		{"", "laporan.pdf", "application/pdf"},
		{"", "foto.PNG", "image/png"},
		{"not a type", "foto.png", "image/png"},
		{"text", "laporan.pdf", "application/pdf"},
		{xlsx, "realisasi.xlsx", xlsx},
		{"", "arsip.nosuchextension", "application/octet-stream"},
		{"", "tanpa-ekstensi", "application/octet-stream"},
	} {
		if got, _ := attachmentType(c.given, c.name); got != c.want {
			t.Errorf("attachmentType(%q, %q) = %s, want %s",
				c.given, c.name, got, c.want)
		}
	}

	// A parameter the caller gave is kept next to the name.
	_, params := attachmentType("text/csv; charset=utf-8", "data.csv")
	if params["charset"] != "utf-8" {
		t.Errorf("params = %v, want the charset kept", params)
	}
}

// attachmentName keeps only the last path element, shortens an over-long
// name with its extension kept, and names an empty one attachment.
func TestAttachmentName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{"laporan.pdf", "laporan.pdf"},
		{"reports/2026/q3.pdf", "q3.pdf"},
		{`C:\Laporan\q3.pdf`, "q3.pdf"},
		{"  spasi.pdf  ", "spasi.pdf"},
		{"", "attachment"},
		{"reports/", "attachment"},
		{"rusak\xffnama.pdf", "rusak\uFFFDnama.pdf"},
	} {
		if got := attachmentName(c.in); got != c.want {
			t.Errorf("attachmentName(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	long := strings.Repeat("é", 200) + ".xlsx"
	got := attachmentName(long)
	if len(got) > maxFilename || !strings.HasSuffix(got, ".xlsx") ||
		!utf8.ValidString(got) {
		t.Errorf("a %d-byte name became %d bytes, %q", len(long),
			len(got), got)
	}
}

// Bcc is the one field whose whole point is not appearing, so the test
// looks for the address anywhere in the message rather than only in a
// header named Bcc.
func TestBccAppearsNowhereInTheMessage(t *testing.T) {
	t.Parallel()
	m := Message{
		To:   []string{"budi@example.go.id"},
		Bcc:  []string{"arsip.rahasia@example.go.id"},
		Text: "isi",
	}
	env, err := parseEnvelope(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.rcpts) != 2 {
		t.Fatalf("rcpts = %v, want the Bcc among them", env.rcpts)
	}
	if bytes.Contains(build(t, m), []byte("arsip.rahasia")) {
		t.Error("the Bcc address is in the message")
	}

	m.To = nil
	msg := read(t, build(t, m))
	if got := msg.Header.Get("To"); got != "undisclosed-recipients:;" {
		t.Errorf("To = %q for a Bcc-only message", got)
	}
}

// parseEnvelope skips blank entries, keeps each field as it was given, and
// hands the relay every recipient once, whatever its case.
func TestParseEnvelope(t *testing.T) {
	t.Parallel()

	env, err := parseEnvelope(Message{
		To:  []string{"", "  ", "Budi <Budi@example.go.id>"},
		Cc:  []string{"budi@EXAMPLE.go.id", "sari@example.go.id"},
		Bcc: []string{"arsip@example.go.id", "SARI@example.go.id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Budi@example.go.id", "sari@example.go.id", "arsip@example.go.id",
	}
	if strings.Join(env.rcpts, " ") != strings.Join(want, " ") {
		t.Errorf("rcpts = %v, want %v", env.rcpts, want)
	}
	// The headers show each field as given, duplicates and all.
	if len(env.to) != 1 || len(env.cc) != 2 {
		t.Errorf("header lists = %d To and %d Cc, want 1 and 2",
			len(env.to), len(env.cc))
	}

	for _, c := range []struct {
		name  string
		m     Message
		field string
	}{
		{"a list in one entry",
			Message{Cc: []string{"a@example.go.id, b@example.go.id"}},
			"Cc"},
		{"no domain", Message{To: []string{"budi"}}, "To"},
		{"an unquoted comma in a name",
			Message{Bcc: []string{"Budi, S.Kom <budi@example.go.id>"}},
			"Bcc"},
		{"a bad Reply-To",
			Message{To: []string{"budi@example.go.id"},
				ReplyTo: "helpdesk"}, "Reply-To"},
	} {
		_, err := parseEnvelope(c.m)
		if !errors.Is(err, ErrInvalidAddress) ||
			!strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: err = %v, want ErrInvalidAddress naming %s",
				c.name, err, c.field)
		}
	}

	if _, err := parseEnvelope(Message{
		To: []string{`"Budi, S.Kom" <budi@example.go.id>`},
	}); err != nil {
		t.Errorf("a quoted comma was refused: %v", err)
	}
}

// Folding only ever turns a space into CRLF and a space, so removing
// every CRLF gives back the value exactly, whatever its spacing.
func TestWriteHeaderUnfoldsToTheValue(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"",
		"pendek",
		strings.Repeat("kata ", 40),
		"dua  spasi   di  tengah " + strings.Repeat("x ", 50),
		strings.Repeat("y", 120),
		strings.Repeat("y", 120) + " " + strings.Repeat("z", 90),
	} {
		var b bytes.Buffer
		writeHeader(&b, "Subject", value)
		out := b.String()
		if !strings.HasSuffix(out, "\r\n") {
			t.Fatalf("header does not end in CRLF: %q", out)
		}
		unfolded := strings.ReplaceAll(strings.TrimSuffix(out, "\r\n"),
			"\r\n", "")
		if unfolded != "Subject: "+value {
			t.Errorf("unfolds to %q, want %q", unfolded, "Subject: "+value)
		}
		for _, line := range strings.Split(out, "\r\n") {
			if len(line) > foldAt && strings.Contains(
				strings.TrimSpace(line), " ") {
				t.Errorf("line of %d bytes was foldable: %q",
					len(line), line)
			}
		}
	}
}

// A message is empty only when it has no subject, no body and no
// attachment, white space counting as nothing.
func TestMessageEmpty(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		m    Message
		want bool
	}{
		{Message{}, true},
		{Message{To: []string{"budi@example.go.id"}}, true},
		{Message{Subject: " ", Text: "\n", HTML: "\t"}, true},
		{Message{Subject: "s"}, false},
		{Message{Text: "t"}, false},
		{Message{HTML: "<p/>"}, false},
		{Message{Attachments: []Attachment{{}}}, false},
	} {
		if got := c.m.empty(); got != c.want {
			t.Errorf("%+v empty = %t, want %t", c.m, got, c.want)
		}
	}
}
