// smtp.go is the single real SMTP adapter: implicit TLS on port 465, PLAIN auth, one message per
// connection, one DATA per Send, and an outcome classified as SENT (nil), ErrFailed or ErrUnknown.
// Contract: contracts/merchant-password-auth-v1.md §3, PD7, PD15; brief docs/delivery/units/auth-mail.md
// (defaults M1-M7). Touches: the configured SMTP host only (smtp.qq.com:465 first, smtp.exmail.qq.com:465
// on upgrade; configuration, never a constant). Called by the identity service through its own Mailer
// interface (depmap is generated; no hand-written importer list).
//
// Wire constants (docs retrieved 2026-09-30):
//   - port 465 = SMTPS / implicit TLS: RFC 8314 §3.3 https://www.rfc-editor.org/rfc/rfc8314
//   - reply classes and the "reply after '.'" rule: RFC 5321 §4.2, §4.5.3.2.6 (a lost final reply is
//     unresolvable; the client must not assume failure) https://www.rfc-editor.org/rfc/rfc5321
//   - net/smtp client API (NewClient over a *tls.Conn, PlainAuth only over TLS):
//     https://pkg.go.dev/net/smtp
//   - QQ mailbox host/port/authorization code (F8) and Exmail host (F10) are quoted from the contract.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	smtpsPort     = 465              // RFC 8314 §3.3; the only port outside loopback tests (M1)
	dialTimeout   = 10 * time.Second // contract §3
	sessionBudget = 25 * time.Second // contract §3: conn deadline = min(ctx deadline, now+25s)
	replyMaxBytes = 128              // M6: stored as provider_message_id
)

// Config configures one SMTP account. Host is a DNS name; a loopback host, a non-465 port and RootCAs
// exist only for in-process tests and require AllowLoopback (M1). cmd/api never sets Port or RootCAs.
type Config struct {
	Host, Username, Password, From string
	Port                           int            // 0 or 465 (M1); anything else only with AllowLoopback and a loopback host
	RootCAs                        *x509.CertPool // nil = system roots; non-nil only with AllowLoopback
	AllowLoopback                  bool           // mirrors COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS=1
}

// String redacts credentials so an accidental %v/%+v of a Config cannot leak them (I11).
func (c Config) String() string { return "mail.Config(host=" + c.Host + ")" }

// GoString covers %#v, which would otherwise print every field.
func (c Config) GoString() string { return c.String() }

// SMTP sends mail through one configured account. Safe for concurrent use: it holds only immutable
// configuration and opens a fresh connection per Send/Probe (no pooling, brief non-goal).
type SMTP struct {
	host, addr, user, secret string
	from                     *mail.Address
	roots                    *x509.CertPool
}

// NewSMTP validates the configuration (M1, M2) and never dials.
func NewSMTP(c Config) (*SMTP, error) {
	if c.Host == "" || c.Username == "" || c.Password == "" || c.From == "" {
		return nil, errors.New("mail: host, username, password and from are required")
	}
	loop := isLoopbackHost(c.Host)
	if loop && !c.AllowLoopback {
		return nil, errors.New("mail: loopback host requires AllowLoopback")
	}
	if !loop && !isDNSName(c.Host) {
		return nil, errors.New("mail: host must be a DNS name without port or scheme")
	}
	if c.RootCAs != nil && !c.AllowLoopback {
		return nil, errors.New("mail: RootCAs requires AllowLoopback")
	}
	port := c.Port
	if port == 0 {
		port = smtpsPort
	}
	if port < 1 || port > 65535 || (port != smtpsPort && !(c.AllowLoopback && loop)) {
		return nil, errors.New("mail: port must be 465 (other ports only with AllowLoopback on a loopback host)")
	}
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return nil, errors.New("mail: from is not a valid address")
	}
	if asciiLower(from.Address) != asciiLower(c.Username) {
		// M2 / PA02: QQ and Exmail send only as the authenticated mailbox; refuse the mismatch at config time.
		return nil, errors.New("mail: from address must equal username")
	}
	return &SMTP{
		host: c.Host, addr: net.JoinHostPort(c.Host, strconv.Itoa(port)),
		user: c.Username, secret: c.Password, from: from, roots: c.RootCAs,
	}, nil
}

// String is redacted: host only (I11).
func (c *SMTP) String() string { return "mail.SMTP(host=" + c.host + ")" }

// GoString covers %#v.
func (c *SMTP) GoString() string { return c.String() }

// Send delivers m exactly once. Returned error is nil (SENT, reply = final 2xx text), wraps ErrFailed
// (the server cannot have accepted the message) or wraps ErrUnknown ('.' was written, no final reply).
// There is no retry anywhere in this function: SMTP has no idempotency key (PD7/I06).
func (c *SMTP) Send(ctx context.Context, m Message) (string, error) {
	to, err := mail.ParseAddress(m.To)
	if err != nil || strings.ContainsAny(to.Address, "\r\n<>") {
		return "", failure(ErrFailed, "invalid_recipient", nil) // syntax only; no retry: nothing was dialled
	}
	raw, err := buildMIME(c.from, to, m, time.Now(), rand.Reader)
	if err != nil {
		return "", failure(ErrFailed, "build_message", nil) // no retry: nothing was dialled
	}
	cl, done, err := c.open(ctx)
	if err != nil {
		return "", err // already ErrFailed; no retry: SMTP has no idempotency key (PD7/I06)
	}
	defer done()

	if err := cl.Mail(c.from.Address); err != nil {
		return "", failure(ErrFailed, "mail_from", err) // no retry: SMTP has no idempotency key (PD7/I06)
	}
	if err := cl.Rcpt(to.Address); err != nil {
		return "", failure(ErrFailed, "rcpt", err) // no retry: SMTP has no idempotency key (PD7/I06)
	}

	// DATA is driven through cl.Text instead of cl.Data(): net/smtp discards the 2xx reply text at
	// Close(), and M6 stores it as provider_message_id. The classification is the same (M5).
	if _, _, err := command(cl, 354, "DATA"); err != nil {
		return "", failure(ErrFailed, "data", err) // no retry: SMTP has no idempotency key (PD7/I06)
	}
	w := cl.Text.DotWriter()
	if _, err := w.Write(raw); err != nil {
		return "", failure(ErrFailed, "body_write", err) // no retry: '.' not written, server cannot have accepted
	}
	if err := cl.Text.W.Flush(); err != nil {
		return "", failure(ErrFailed, "body_write", err) // no retry: body flushed before the terminator, so '.' not written
	}
	if err := w.Close(); err != nil { // writes "\r\n.\r\n": from here the server may accept
		return "", failure(ErrUnknown, "terminator_write", err) // no retry: '.' may have arrived (PD7/I06)
	}
	_, msg, err := cl.Text.ReadResponse(2) // 2 = any 2xx
	if err != nil {
		var te *textproto.Error
		if errors.As(err, &te) {
			// A reply was read: 3xx/4xx/5xx (incl. 550 Sender frequency limited, F8) => not accepted.
			return "", failure(ErrFailed, "smtp_rejected", err) // no retry: SMTP has no idempotency key (PD7/I06)
		}
		return "", failure(ErrUnknown, "no_final_reply", err) // no retry: timeout/EOF/reset after '.', outcome unknowable (RFC 5321 §4.5.3.2.6)
	}
	_ = cl.Quit() // ignored after a 2xx: the message is SENT either way
	return cleanReply(msg), nil
}

// Probe is PA12 only: dial, TLS, AUTH, MAIL FROM:<from>, RSET, QUIT. It never calls Rcpt or Data, so
// nothing can be delivered. It returns the MAIL FROM reply code with a nil error for any reply the
// server sent (250 accepted, 553 rejected, ...); err is non-nil (wraps ErrFailed) only when no such
// reply was obtained (dial, TLS, AUTH, transport).
func (c *SMTP) Probe(ctx context.Context, from string) (int, error) {
	a, err := mail.ParseAddress(from)
	if err != nil || strings.ContainsAny(a.Address, "\r\n<>") {
		return 0, failure(ErrFailed, "invalid_from", nil)
	}
	cl, done, err := c.open(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	code, _, err := command(cl, 0, "MAIL FROM:<%s>", a.Address)
	if err != nil {
		return 0, failure(ErrFailed, "mail_from", err)
	}
	_ = cl.Reset() // RSET: abandon the envelope; errors do not change the probe result
	_ = cl.Quit()
	return code, nil
}

// open dials, negotiates implicit TLS, reads the greeting and authenticates. Every error is classified
// ErrFailed (no message exists yet). done stops the ctx watcher and closes the connection.
func (c *SMTP) open(ctx context.Context) (*smtp.Client, func(), error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{ServerName: c.host, MinVersion: tls.VersionTLS12, RootCAs: c.roots},
	}
	conn, err := d.DialContext(dctx, "tcp", c.addr)
	if err != nil {
		return nil, nil, failure(ErrFailed, "dial_tls", err) // no retry: SMTP has no idempotency key (PD7/I06)
	}
	dl := time.Now().Add(sessionBudget)
	if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
		dl = cd
	}
	_ = conn.SetDeadline(dl)
	// net/smtp is not context-aware: cancelling the context expires the connection deadline instead.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	// conn is a *tls.Conn, so NewClient marks the session as TLS and PlainAuth will send credentials.
	cl, err := smtp.NewClient(conn, c.host)
	if err != nil {
		stop()
		_ = conn.Close()
		return nil, nil, failure(ErrFailed, "greeting", err) // no retry: SMTP has no idempotency key (PD7/I06)
	}
	done := func() { stop(); _ = cl.Close() }
	if err := cl.Auth(smtp.PlainAuth("", c.user, c.secret, c.host)); err != nil {
		done()
		return nil, nil, failure(ErrFailed, "auth", err) // no retry: SMTP has no idempotency key (PD7/I06)
	}
	return cl, done, nil
}

// command sends one command line and reads its reply. expect follows textproto.ReadResponse (0 = any).
func command(cl *smtp.Client, expect int, format string, args ...any) (int, string, error) {
	id, err := cl.Text.Cmd(format, args...)
	if err != nil {
		return 0, "", err
	}
	cl.Text.StartResponse(id)
	defer cl.Text.EndResponse(id)
	return cl.Text.ReadResponse(expect)
}

// sendErr carries only a fixed kind, the reply code and a timeout flag: never the recipient, subject,
// body, username, secret or server text (I11). Unwrap exposes the sentinel for errors.Is.
type sendErr struct {
	sentinel error
	kind     string
	code     int
	timeout  bool
}

func (e *sendErr) Error() string {
	s := fmt.Sprintf("%s: kind=%s", e.sentinel, e.kind)
	if e.code != 0 {
		s += " code=" + strconv.Itoa(e.code)
	}
	if e.timeout {
		s += " timeout"
	}
	return s
}

func (e *sendErr) Unwrap() error { return e.sentinel }

func failure(sentinel error, kind string, cause error) error {
	e := &sendErr{sentinel: sentinel, kind: kind}
	var te *textproto.Error
	if errors.As(cause, &te) {
		e.code = te.Code
	}
	var ne net.Error
	e.timeout = errors.As(cause, &ne) && ne.Timeout()
	return e
}

// cleanReply implements M6: control characters removed, at most 128 bytes, cut on a UTF-8 boundary.
func cleanReply(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	if len(s) <= replyMaxBytes {
		return s
	}
	n := replyMaxBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if 'A' <= ch && ch <= 'Z' {
			b[i] = ch + 'a' - 'A'
		}
	}
	return string(b)
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isDNSName is a syntax guard only (letters, digits, '-', '.'): rejects "host:465", URLs, IP literals.
func isDNSName(h string) bool {
	if net.ParseIP(h) != nil || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
