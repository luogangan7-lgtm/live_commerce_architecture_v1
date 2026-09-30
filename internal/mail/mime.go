// mime.go builds the RFC 5322 / MIME body the adapter writes after DATA. It touches no network and
// no other package; smtp.go calls buildMIME exactly once per Send. Contract:
// contracts/merchant-password-auth-v1.md §3 (headers, Auto-Submitted, no tracking or links added).
//
// Wire constants (docs retrieved 2026-09-30):
//   - header names and Date/Message-ID formats: RFC 5322 §3.3, §3.6.4 https://www.rfc-editor.org/rfc/rfc5322
//   - encoded-word Subject for non-ASCII: RFC 2047 §4.2 https://www.rfc-editor.org/rfc/rfc2047
//   - Auto-Submitted: auto-generated: RFC 3834 §5 https://www.rfc-editor.org/rfc/rfc3834
//   - multipart/alternative, quoted-printable, base64 (76-char lines): RFC 2045/2046
//     https://www.rfc-editor.org/rfc/rfc2045
package mail

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

var errBadHeaderValue = errors.New("header value contains CR or LF")

// buildMIME returns the full message (headers + body) with CRLF line endings, ready to be written to
// the SMTP data writer (which dot-stuffs and appends the terminator). from is the parsed From (display
// name + address); to is the bare recipient address. now and rnd are injected so the layout is testable;
// production passes time.Now and crypto/rand.Reader.
func buildMIME(from, to *mail.Address, m Message, now time.Time, rnd io.Reader) ([]byte, error) {
	if strings.ContainsAny(m.Subject, "\r\n") {
		// Header injection guard: a Subject that could start a new header line is refused, never sanitised.
		return nil, errBadHeaderValue
	}
	id := make([]byte, 16)
	if _, err := io.ReadFull(rnd, id); err != nil {
		return nil, err
	}
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]

	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", from.String()) // net/mail encodes a non-ASCII display name per RFC 2047
	h("To", (&mail.Address{Address: to.Address}).String())
	h("Subject", mime.BEncoding.Encode("UTF-8", m.Subject)) // returns the input unchanged when it is pure ASCII
	h("Date", now.UTC().Format(time.RFC1123Z))              // RFC 5322 date-time, always +0000
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")

	mw := multipart.NewWriter(&b) // random 30-byte boundary
	h("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	b.WriteString("\r\n")

	// Text part: quoted-printable (readable in raw form, ASCII-safe).
	p, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=UTF-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	qw := quotedprintable.NewWriter(p)
	if _, err := qw.Write([]byte(m.Text)); err != nil {
		return nil, err
	}
	if err := qw.Close(); err != nil {
		return nil, err
	}

	// HTML part: base64, wrapped at 76 characters (RFC 2045 §6.8).
	p, err = mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/html; charset=UTF-8"},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString([]byte(m.HTML))
	for len(enc) > 76 {
		io.WriteString(p, enc[:76]+"\r\n")
		enc = enc[76:]
	}
	io.WriteString(p, enc+"\r\n")

	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
