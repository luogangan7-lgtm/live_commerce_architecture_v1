// server_test.go: self-tests of the fake using stdlib net/smtp directly (not internal/mail), so the fake
// is checked independently of the adapter it is used to test.
package mailtest_test

import (
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"testing"

	"livecommerce/internal/mail/mailtest"
)

func dial(t *testing.T, s *mailtest.Server) *smtp.Client {
	t.Helper()
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.Host, RootCAs: s.RootCAs, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		t.Fatalf("greeting: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestServerAcceptsAndStoresWithDotStuffing(t *testing.T) {
	s := mailtest.New(t)
	c := dial(t, s)
	if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
		t.Fatal(err)
	}
	if c.Mail(s.Username) != nil || c.Rcpt("r@x.example") != nil {
		t.Fatal("envelope refused")
	}
	w, _ := c.Data()
	w.Write([]byte("Subject: hi\r\n\r\n.leading dot\r\nbody\r\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	m := s.Messages()
	if len(m) != 1 || s.DataCount() != 1 || m[0].To != "r@x.example" || m[0].Subject != "hi" || !strings.Contains(string(m[0].Raw), "\r\n.leading dot\r\n") {
		t.Fatalf("stored = %+v data=%d", m, s.DataCount())
	}
}

func TestServerRefusesWithoutAuthAndWrongPassword(t *testing.T) {
	s := mailtest.New(t)
	c := dial(t, s)
	var te *textproto.Error
	if err := c.Mail(s.Username); !errors.As(err, &te) || te.Code != 530 {
		t.Fatalf("MAIL before AUTH: %v", err)
	}
	if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password+"x", s.Host)); !errors.As(err, &te) || te.Code != 535 {
		t.Fatalf("bad password: %v", err)
	}
}

func TestServerFaultsAreOneShot(t *testing.T) {
	s := mailtest.New(t)
	s.SetNextFault(mailtest.FaultDial)
	if _, err := tls.Dial("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), &tls.Config{RootCAs: s.RootCAs, ServerName: s.Host}); err == nil {
		t.Fatal("FaultDial: handshake succeeded")
	}
	c := dial(t, s) // the fault was consumed: the next connection works
	s.SetNextFault(mailtest.FaultRcpt)
	if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil || c.Mail(s.Username) != nil {
		t.Fatal("setup")
	}
	var te *textproto.Error
	if err := c.Rcpt("r@x.example"); !errors.As(err, &te) || te.Code != 550 {
		t.Fatalf("FaultRcpt: %v", err)
	}
	if err := c.Rcpt("r@x.example"); err != nil {
		t.Fatalf("fault must be one-shot: %v", err)
	}
}

func TestRejectFromMismatch(t *testing.T) {
	s := mailtest.New(t)
	s.RejectFromMismatch(true)
	c := dial(t, s)
	if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
		t.Fatal(err)
	}
	var te *textproto.Error
	if err := c.Mail("other@x.example"); !errors.As(err, &te) || te.Code != 553 {
		t.Fatalf("mismatch: %v", err)
	}
	if err := c.Mail(strings.ToUpper(s.Username)); err != nil {
		t.Fatalf("own address (case-insensitive) refused: %v", err)
	}
}
