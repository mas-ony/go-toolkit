package email

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// part is one MIME entity: its header and its encoded body.
type part struct {
	header textproto.MIMEHeader
	body   []byte
}

// envelope is what the addresses of a Message resolve to: the lists the
// headers show, and the recipients the relay is actually given.
type envelope struct {
	to, cc  []*mail.Address
	replyTo *mail.Address

	// rcpts holds every recipient once, To first, then Cc, then Bcc, in
	// the spelling of its first appearance.
	rcpts []string
}

// Attachment is one file carried by a Message.
type Attachment struct {
	// Filename is the name the recipient sees, extension included. A
	// directory part is dropped, a name longer than any file system
	// allows is shortened with its extension kept, and an empty name
	// becomes "attachment".
	Filename string

	// ContentType is the media type, "application/pdf" say. When it is
	// empty or does not parse, the type registered for Filename's
	// extension is used, and application/octet-stream when there is
	// none.
	//
	// That lookup reads the system's MIME tables on top of a short
	// built-in list, so outside the list — .xlsx and .docx among them —
	// a minimal container answers differently from a desktop. Set it for
	// anything the recipient's client should recognise by type.
	ContentType string

	// Data is the file itself. It travels as base64, which makes it about
	// a third larger on the wire.
	Data []byte
}

// Message is one email. Its zero value holds nothing, which Send treats
// as nothing to send rather than as a mistake.
type Message struct {
	// To, Cc and Bcc hold ONE address per entry, bare
	// ("budi@example.go.id") or with a display name
	// ("Budi Santoso <budi@example.go.id>"). An entry that is empty or
	// all spaces is skipped. A name containing a comma has to be quoted;
	// see the package documentation.
	To  []string
	Cc  []string
	Bcc []string

	// ReplyTo is the address replies go to instead of From. For a noreply
	// sender it is usually the only address a reply can reach. Optional.
	ReplyTo string

	// Subject may hold any text. Non-ASCII is encoded, and so is a line
	// break, which is carried as text rather than obeyed: a subject built
	// from user input cannot open a header of its own.
	Subject string

	// Text and HTML are the body, and either may be empty. With both,
	// the recipient's client picks one. Supplying Text alongside HTML is
	// also what keeps an HTML message from scoring as spam for lacking a
	// plain alternative.
	Text string
	HTML string

	// Attachments follow the body, in order.
	Attachments []Attachment
}

// foldAt is the length a header line is folded at when its words allow
// it: the 78 RFC 5322 asks for. The 998 it requires is never at risk from
// a folded field. A single word longer than 78 is left whole, since a
// header can only be folded where it already has white space.
const foldAt = 78

// base64Line is the length of each line of an encoded attachment, the
// maximum RFC 2045 allows.
const base64Line = 76

// maxFilename is the longest attachment name kept, in bytes.
//
// It is ext4's limit, so the name of a file from such a disk never reaches
// it. NTFS allows 255 UTF-16 units rather than bytes, so a long non-ASCII
// name from Windows can pass it, and is then shortened. What the bound
// protects is the header carrying the name, which stays under SMTP's
// 998-byte line limit even when every byte is percent-encoded. A parameter
// value is one word and cannot be folded, and a line past the limit is
// refused by the relay, not trimmed.
const maxFilename = 255

// blank reports whether s holds nothing but white space.
func blank(s string) bool { return strings.TrimSpace(s) == "" }

// empty reports whether m has nothing to deliver: no subject, no body and
// no attachment. White space alone counts as nothing.
func (m Message) empty() bool {
	return blank(m.Subject) && blank(m.Text) && blank(m.HTML) &&
		len(m.Attachments) == 0
}

// base64Lines encodes data as base64 in lines of base64Line characters
// separated by CRLF.
func base64Lines(data []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(data)
	out := make([]byte, 0, len(enc)+len(enc)/base64Line*2)
	for len(enc) > base64Line {
		out = append(out, enc[:base64Line]...)
		out = append(out, '\r', '\n')
		enc = enc[base64Line:]
	}
	return append(out, enc...)
}

// addressList renders addresses for a To or Cc field. mail.Address
// quotes a display name that needs it and encodes one that is not ASCII.
func addressList(list []*mail.Address) string {
	s := make([]string, len(list))
	for i, a := range list {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// parseEnvelope resolves m's addresses, or returns ErrInvalidAddress
// naming the first entry that does not parse and the field it is in.
//
// A recipient listed twice, in one field or across two, is given to the
// relay once, compared without regard to case: "Budi@x" and "budi@x"
// reach the same mailbox at every provider this is likely to meet, and a
// second copy is the kind of duplicate a recipient reports as a bug. The
// headers still show each field as it was given.
func parseEnvelope(m Message) (envelope, error) {
	var env envelope
	seen := make(map[string]bool)
	add := func(field string, entries []string) ([]*mail.Address, error) {
		var list []*mail.Address
		for _, raw := range entries {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			a, err := mail.ParseAddress(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: %s %q: %v",
					ErrInvalidAddress, field, raw, err)
			}
			list = append(list, a)
			if key := strings.ToLower(a.Address); !seen[key] {
				seen[key] = true
				env.rcpts = append(env.rcpts, a.Address)
			}
		}
		return list, nil
	}

	var err error
	if env.to, err = add("To", m.To); err != nil {
		return envelope{}, err
	}
	if env.cc, err = add("Cc", m.Cc); err != nil {
		return envelope{}, err
	}
	if _, err = add("Bcc", m.Bcc); err != nil {
		return envelope{}, err
	}
	if raw := strings.TrimSpace(m.ReplyTo); raw != "" {
		if env.replyTo, err = mail.ParseAddress(raw); err != nil {
			return envelope{}, fmt.Errorf("%w: Reply-To %q: %v",
				ErrInvalidAddress, raw, err)
		}
	}
	return env, nil
}

// textPart encodes s as quoted-printable UTF-8. Every line ending in s —
// LF, CR or CRLF — goes out as CRLF, long lines are soft-wrapped under 76
// characters, and invalid UTF-8 becomes U+FFFD rather than bytes the
// declared charset cannot hold.
//
// Quoted-printable rather than base64 because a text body stays readable
// in raw form, which is how an operator sees it in a bounce or a relay's
// queue. It also escapes "=" and trailing spaces, so a relay that strips
// trailing white space cannot change what arrives.
func textPart(subtype, s string) part {
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	// quotedprintable reports only its writer's errors, and a
	// bytes.Buffer has none to report.
	_, _ = w.Write([]byte(strings.ToValidUTF8(s, "\uFFFD")))
	_ = w.Close()

	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", "text/"+subtype+"; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	return part{header: h, body: b.Bytes()}
}

// attachmentType is the media type an attachment is sent as, with its
// parameters: the given type when it parses as one, else the type
// registered for the name's extension, else application/octet-stream.
// The map is always a new one, so the caller may add to it.
func attachmentType(given, name string) (string, map[string]string) {
	registered := mime.TypeByExtension(filepath.Ext(name))
	for _, t := range []string{given, registered} {
		mt, params, err := mime.ParseMediaType(t)
		// A bare token parses too, and names no type a client can act
		// on, so a subtype is required.
		if err == nil && strings.Contains(mt, "/") {
			return mt, params
		}
	}
	return "application/octet-stream", map[string]string{}
}

// attachmentName is the name an attachment is sent under.
//
// Only the last path element is kept: a recipient's client drops the
// rest anyway, and the sender's directory layout is nobody's business.
// Invalid UTF-8 becomes U+FFFD, a name longer than maxFilename is cut at
// a character boundary with its extension kept, and an empty name becomes
// "attachment", which clients show better than a part with no name.
func attachmentName(name string) string {
	name = strings.ToValidUTF8(name, "\uFFFD")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "attachment"
	}
	if len(name) <= maxFilename {
		return name
	}

	ext := filepath.Ext(name)
	if len(ext) > maxFilename/2 {
		ext = ""
	}
	stem := name[:len(name)-len(ext)]
	keep := maxFilename - len(ext)
	for keep > 0 && !utf8.RuneStart(stem[keep]) {
		keep--
	}
	return stem[:keep] + ext
}

// attachmentPart encodes a as a base64 file part.
//
// The name travels twice, as Content-Type's name and as
// Content-Disposition's filename, because clients disagree about which
// one they read. mime formats both, which percent-encodes (RFC 2231) any
// name that is not plain ASCII, a line break in one included, so a name
// cannot add a header either.
func attachmentPart(a Attachment) part {
	name := attachmentName(a.Filename)
	mediaType, params := attachmentType(a.ContentType, name)
	params["name"] = name

	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment",
		map[string]string{"filename": name}))
	h.Set("Content-Transfer-Encoding", "base64")
	return part{header: h, body: base64Lines(a.Data)}
}

// multipartOf wraps parts in a multipart entity of the given subtype,
// under multipart's own random boundary, which no body here can contain
// by chance.
func multipartOf(subtype string, parts ...part) (part, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range parts {
		pw, err := w.CreatePart(p.header)
		if err != nil {
			return part{}, err
		}
		if _, err := pw.Write(p.body); err != nil {
			return part{}, err
		}
	}
	if err := w.Close(); err != nil {
		return part{}, err
	}

	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", mime.FormatMediaType("multipart/"+subtype,
		map[string]string{"boundary": w.Boundary()}))
	return part{header: h, body: b.Bytes()}, nil
}

// content builds the body of m as one entity, a single part when that is
// all it needs and a multipart tree otherwise:
//
//	text only              text/plain
//	HTML only              text/html
//	text and HTML          multipart/alternative, plain first
//	with attachments       multipart/mixed: the above, then each file
//	subject only           an empty text/plain
//
// Plain comes first in an alternative because a client shows the LAST
// part it can render, so the richer one has to be last.
func content(m Message) (part, error) {
	hasText, hasHTML := !blank(m.Text), !blank(m.HTML)

	var parts []part
	switch {
	case hasText && hasHTML:
		alt, err := multipartOf("alternative",
			textPart("plain", m.Text), textPart("html", m.HTML))
		if err != nil {
			return part{}, err
		}
		parts = append(parts, alt)
	case hasHTML:
		parts = append(parts, textPart("html", m.HTML))
	case hasText:
		parts = append(parts, textPart("plain", m.Text))
	case len(m.Attachments) == 0:
		// A message still needs a body, and an empty one is the honest
		// rendering of a notification whose subject says everything.
		parts = append(parts, textPart("plain", ""))
	}
	if len(m.Attachments) == 0 {
		return parts[0], nil
	}
	for _, a := range m.Attachments {
		parts = append(parts, attachmentPart(a))
	}
	return multipartOf("mixed", parts...)
}

// writeHeader writes one header field, folded at spaces so that no line
// passes foldAt where the value's words allow it. Folding turns a space
// into CRLF followed by that same space, so unfolding gives back the
// value exactly.
//
// That includes the space after the colon. An encoded subject opens with
// a word of up to 75 characters, which does not fit after "Subject: ",
// and RFC 5322 allows the fold there as anywhere else.
func writeHeader(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteByte(':')
	n := len(name) + 1
	for _, word := range strings.Split(value, " ") {
		if word != "" && n+1+len(word) > foldAt {
			b.WriteString("\r\n")
			n = 0
		}
		b.WriteByte(' ')
		b.WriteString(word)
		n += 1 + len(word)
	}
	b.WriteString("\r\n")
}

// compose renders the message DATA uploads: headers, a blank line and the
// body, with every line ending in CRLF. date and id are parameters so a
// test can pin them.
func compose(
	from *mail.Address,
	env envelope,
	m Message,
	date time.Time,
	id string,
) ([]byte, error) {
	root, err := content(m)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	writeHeader(&b, "From", from.String())
	// RFC 5322 allows a message without To, and some filters score one
	// as spam, so a message addressed only by Cc or Bcc names the
	// conventional empty group instead of leaving the field out.
	to := "undisclosed-recipients:;"
	if len(env.to) > 0 {
		to = addressList(env.to)
	}
	writeHeader(&b, "To", to)
	if len(env.cc) > 0 {
		writeHeader(&b, "Cc", addressList(env.cc))
	}
	if env.replyTo != nil {
		writeHeader(&b, "Reply-To", env.replyTo.String())
	}
	writeHeader(&b, "Subject", mime.QEncoding.Encode("utf-8",
		strings.ToValidUTF8(m.Subject, "\uFFFD")))
	writeHeader(&b, "Date", date.Format(time.RFC1123Z))
	writeHeader(&b, "Message-ID", id)
	// RFC 3834. A vacation responder or ticket system that honours it
	// does not answer, which keeps a notification from starting a loop
	// with an autoresponder.
	writeHeader(&b, "Auto-Submitted", "auto-generated")
	writeHeader(&b, "MIME-Version", "1.0")
	for _, k := range []string{
		"Content-Type", "Content-Transfer-Encoding",
	} {
		if v := root.header.Get(k); v != "" {
			writeHeader(&b, k, v)
		}
	}
	b.WriteString("\r\n")
	b.Write(root.body)
	return b.Bytes(), nil
}
