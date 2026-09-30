// Package mailtest owns the loopback implicit-TLS SMTP server and in-memory mailbox that the MOCK,
// HTTP and BROWSER gates drive the real internal/mail adapter against (contract
// contracts/merchant-password-auth-v1.md §3, PA02).
// It never listens beyond 127.0.0.1, delivers or relays mail, verifies SPF/DKIM, or holds a real
// credential; test-only: cmd/api must not import it (PA14 `go list -deps`).
// Written from RFC 5321 (command/reply grammar, §4.1, §4.2) https://www.rfc-editor.org/rfc/rfc5321 and
// RFC 4954 AUTH PLAIN https://www.rfc-editor.org/rfc/rfc4954 (retrieved 2026-09-30), never from the
// adapter. Pattern: internal/integrations/psp/stripe/stripetest.
package mailtest

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mailpkg "livecommerce/internal/mail"
)

// Fault is consumed once, by the first point where it applies. Zero-cost when FaultNone.
//   - FaultDial: next TCP connection is closed before the TLS handshake.
//   - FaultAuth: next AUTH answers 535.
//   - FaultRcpt: next RCPT answers 550.
//   - FaultData354: next DATA answers 554 instead of 354 (fails before '.').
//   - FaultReply4xx / FaultReply5xx: after '.', final reply 451 / 554.
//   - FaultFrequency550: after '.', final reply "550 Sender frequency limited" (F8).
//   - FaultDropAfterDot: after '.', the connection is closed with no final reply.
type Fault int

const (
	FaultNone Fault = iota
	FaultDial
	FaultAuth
	FaultRcpt
	FaultData354
	FaultReply4xx
	FaultReply5xx
	FaultFrequency550
	FaultDropAfterDot
)

// Received is one message the server acknowledged with a 2xx. From and To are the envelope addresses;
// Subject is RFC 2047-decoded; Text and HTML are the decoded alternative parts with LF line endings; Raw is the DATA payload
// (dot-unstuffed, CRLF) exactly as sent; Header is the parsed top-level header.
type Received struct {
	From, To, Subject, Text, HTML string
	Raw                           []byte
	Header                        textproto.MIMEHeader
}

// Server is a loopback TLS SMTP server. Username/Password are the only accepted PLAIN credentials and
// may be replaced before the first connection. RootCAs trusts the server's self-signed certificate.
type Server struct {
	Host     string
	Port     int
	RootCAs  *x509.CertPool
	Username string
	Password string

	ln    net.Listener
	tlsc  *tls.Config
	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}

	fault    Fault
	delay    time.Duration
	okReply  string
	rejectFM bool
	msgs     []Received
	data     int
	closed   bool
}

// New starts the server on 127.0.0.1 with a random port and a fresh self-signed certificate;
// t.Cleanup stops it and waits for every connection goroutine.
func New(t testing.TB) *Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mailtest: key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mailtest loopback CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("mailtest: cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("mailtest: parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mailtest: listen: %v", err)
	}
	pw := make([]byte, 12)
	_, _ = rand.Read(pw)
	s := &Server{
		Host:     "127.0.0.1",
		Port:     ln.Addr().(*net.TCPAddr).Port,
		RootCAs:  pool,
		Username: "sender@mailtest.example",
		Password: hex.EncodeToString(pw), // per-run random: no credential-shaped literal in the repo
		ln:       ln,
		tlsc: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		},
		conns: map[net.Conn]struct{}{},
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.close)
	return s
}

// Config returns a ready-to-use loopback adapter Config. from must have the same address as
// s.Username, otherwise NewSMTP refuses it (M2), which is what PA02 relies on.
func (s *Server) Config(from string) mailpkg.Config {
	return mailpkg.Config{
		Host: s.Host, Port: s.Port, RootCAs: s.RootCAs,
		Username: s.Username, Password: s.Password, From: from, AllowLoopback: true,
	}
}

// SetNextFault arms one fault (see Fault); it stays armed until a request reaches its trigger point.
func (s *Server) SetNextFault(f Fault) { s.mu.Lock(); s.fault = f; s.mu.Unlock() }

// SetDelay delays the final reply after '.' (PA08b: 400 ms).
func (s *Server) SetDelay(d time.Duration) { s.mu.Lock(); s.delay = d; s.mu.Unlock() }

// SetOKReply replaces the text of the final 250 reply (default "OK queued as <n>"); used to exercise the
// M6 reply-text cleaning (control characters, 128-byte UTF-8 cut).
func (s *Server) SetOKReply(text string) { s.mu.Lock(); s.okReply = text; s.mu.Unlock() }

// RejectFromMismatch makes MAIL FROM answer 553 when the address differs from Username (PA12 twin:
// QQ/Exmail send only as the authenticated mailbox).
func (s *Server) RejectFromMismatch(on bool) { s.mu.Lock(); s.rejectFM = on; s.mu.Unlock() }

// Messages returns a copy of the mailbox: only messages acknowledged with 2xx. A FaultDropAfterDot
// message is deliberately absent (the server never acknowledged it) but is counted by DataCount.
func (s *Server) Messages() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.msgs...)
}

// DataCount is the number of DATA commands seen, including refused ones (PA02/PA09 "zero re-sends").
func (s *Server) DataCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.data }

func (s *Server) take(want ...Fault) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range want {
		if s.fault == f {
			s.fault = FaultNone
			return true
		}
	}
	return false
}

func (s *Server) close() {
	s.mu.Lock()
	s.closed = true
	_ = s.ln.Close()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }()
			s.serve(c)
		}()
	}
}

func (s *Server) serve(raw net.Conn) {
	defer raw.Close()
	if s.take(FaultDial) {
		return // closed before the TLS handshake
	}
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second)) // leak guard, not a behaviour
	c := tls.Server(raw, s.tlsc)
	if c.Handshake() != nil {
		return
	}
	tp := textproto.NewConn(c)
	reply := func(format string, a ...any) { _ = tp.PrintfLine(format, a...) }
	reply("220 mailtest ESMTP")
	var authed bool
	var from, to string
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			reply("250-mailtest")
			reply("250-AUTH PLAIN")
			reply("250 8BITMIME")
		case "HELO":
			reply("250 mailtest")
		case "AUTH":
			mech, ir, _ := strings.Cut(arg, " ")
			if !strings.EqualFold(mech, "PLAIN") {
				reply("504 unsupported mechanism")
				continue
			}
			if ir == "" {
				reply("334 ")
				if ir, err = tp.ReadLine(); err != nil {
					return
				}
			}
			dec, err := base64.StdEncoding.DecodeString(ir)
			parts := bytes.Split(dec, []byte{0})
			if s.take(FaultAuth) || err != nil || len(parts) != 3 ||
				string(parts[1]) != s.Username || string(parts[2]) != s.Password {
				reply("535 authentication failed")
				continue
			}
			authed = true
			reply("235 ok")
		case "MAIL":
			if !authed {
				reply("530 authentication required")
				continue
			}
			addr := angle(arg)
			s.mu.Lock()
			rej := s.rejectFM && !strings.EqualFold(addr, s.Username)
			s.mu.Unlock()
			if rej {
				reply("553 sender not owned by authenticated user")
				continue
			}
			from, to = addr, ""
			reply("250 ok")
		case "RCPT":
			if from == "" {
				reply("503 need MAIL first")
			} else if s.take(FaultRcpt) {
				reply("550 recipient rejected")
			} else {
				to = angle(arg)
				reply("250 ok")
			}
		case "DATA":
			s.mu.Lock()
			s.data++
			s.mu.Unlock()
			if to == "" {
				reply("503 need RCPT first")
				continue
			}
			if s.take(FaultData354) {
				reply("554 transaction failed")
				continue
			}
			reply("354 end with <CRLF>.<CRLF>")
			body, err := readData(tp.R)
			if err != nil {
				return
			}
			s.mu.Lock()
			d := s.delay
			s.mu.Unlock()
			time.Sleep(d)
			switch {
			case s.take(FaultDropAfterDot):
				return // no final reply
			case s.take(FaultReply4xx):
				reply("451 try again later")
			case s.take(FaultReply5xx):
				reply("554 transaction failed")
			case s.take(FaultFrequency550):
				reply("550 Sender frequency limited")
			default:
				rec := parse(from, to, body)
				s.mu.Lock()
				s.msgs = append(s.msgs, rec)
				n := len(s.msgs)
				s.mu.Unlock()
				ok := s.okReply
				if ok == "" {
					ok = "OK queued as " + strconv.Itoa(n)
				}
				reply("250 %s", ok)
			}
			from, to = "", ""
		case "RSET":
			from, to = "", ""
			reply("250 ok")
		case "NOOP":
			reply("250 ok")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 command not implemented")
		}
	}
}

// readData reads the DATA payload up to the lone "." line, undoing dot-stuffing but keeping CRLF as
// sent (textproto.DotReader would rewrite CRLF to LF and hide line-ending bugs).
func readData(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if string(line) == ".\r\n" {
			return out, nil
		}
		out = append(out, bytes.TrimPrefix(line, []byte("."))...)
	}
}

// angle extracts the address of "FROM:<a@b>" / "TO:<a@b>" (parameters after '>' are ignored).
func angle(arg string) string {
	i, j := strings.IndexByte(arg, '<'), strings.IndexByte(arg, '>')
	if i < 0 || j < i {
		return ""
	}
	return arg[i+1 : j]
}

// parse decodes the DATA payload; malformed input yields a Received with only Raw and the envelope.
func parse(from, to string, raw []byte) Received {
	rec := Received{From: from, To: to, Raw: raw}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return rec
	}
	rec.Header = textproto.MIMEHeader(msg.Header)
	rec.Subject, _ = new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		b, _ := io.ReadAll(msg.Body)
		rec.Text = lf(b)
		return rec
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart() // decodes quoted-printable itself; base64 is decoded below
		if err != nil {
			break
		}
		var r io.Reader = p
		switch strings.ToLower(p.Header.Get("Content-Transfer-Encoding")) {
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, &stripCRLF{p})
		case "quoted-printable":
			r = quotedprintable.NewReader(p)
		}
		b, _ := io.ReadAll(r)
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		switch ct {
		case "text/plain":
			rec.Text = lf(b)
		case "text/html":
			rec.HTML = lf(b)
		}
	}
	return rec
}

// lf normalises the CRLF line endings SMTP requires back to "\n" so tests compare against the source text.
func lf(b []byte) string { return strings.ReplaceAll(string(b), "\r\n", "\n") }

// stripCRLF removes line breaks so the base64 decoder accepts wrapped lines.
type stripCRLF struct{ r io.Reader }

func (s *stripCRLF) Read(p []byte) (int, error) {
	buf := make([]byte, len(p))
	n, err := s.r.Read(buf)
	out := 0
	for _, b := range buf[:n] {
		if b != '\r' && b != '\n' {
			p[out] = b
			out++
		}
	}
	if out == 0 && err == nil {
		return s.Read(p)
	}
	return out, err
}
