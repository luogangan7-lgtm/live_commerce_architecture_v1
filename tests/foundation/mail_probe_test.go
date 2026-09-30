package foundation_test

// PA12 TestMailPA12SMTPProbe (LIVE read-only, owner-run on the server, never CI) —
// contracts/merchant-password-auth-v1.md §3 (From must equal the authenticated mailbox, F8), §9 PA12 and
// auth-mail M7: against the configured mailbox make the TLS handshake, AUTH, MAIL FROM=username (must be
// accepted), RSET, QUIT; then MAIL FROM=<another address> and RECORD the reply code (QQ/Exmail refuse
// senders they do not own). It calls (*mail.SMTP).Probe only, which never issues RCPT or DATA, so nothing
// can be delivered. SKIP is NOT_RUN: the test needs the sending mailbox and its secret FILE (never a
// literal), so it is skipped unless LC_MAIL_PROBE=1. Environment (same names as production, contract §5):
//   COMMERCE_SMTP_HOST, COMMERCE_SMTP_USERNAME, COMMERCE_SMTP_PASSWORD_FILE.
// The companion TestMailPA12ProbeLoopbackSelfCheck runs the identical probe body against the loopback
// mailtest server in CI, so the owner-run test's logic is itself exercised (MOCK), and it asserts that no
// DATA is ever issued. Neither test prints the secret, the username or an address.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/mail"
	"livecommerce/internal/mail/mailtest"
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// pwaProbeBody is the shared probe sequence; other is a well-formed address that is not the username.
func pwaProbeBody(t *testing.T, s *mail.SMTP, username, other string) (own, foreign int) {
	t.Helper()
	ctx, cancel := contextWithTimeout(45 * time.Second)
	defer cancel()
	own, err := s.Probe(ctx, username)
	if err != nil {
		t.Fatalf("probe as the mailbox itself: %v", err)
	}
	foreign, err = s.Probe(ctx, other)
	if err != nil {
		t.Fatalf("probe as another sender: %v", err)
	}
	return own, foreign
}

func TestMailPA12SMTPProbe(t *testing.T) {
	if os.Getenv("LC_MAIL_PROBE") != "1" {
		t.Skip("NOT_RUN: LC_MAIL_PROBE=1 plus COMMERCE_SMTP_HOST, COMMERCE_SMTP_USERNAME and COMMERCE_SMTP_PASSWORD_FILE are required; this LIVE read-only probe is owner-run on the server and never runs in CI (PA12)")
	}
	host, user, file := os.Getenv("COMMERCE_SMTP_HOST"), os.Getenv("COMMERCE_SMTP_USERNAME"), os.Getenv("COMMERCE_SMTP_PASSWORD_FILE")
	if host == "" || user == "" || file == "" {
		t.Fatal("LC_MAIL_PROBE=1 needs COMMERCE_SMTP_HOST, COMMERCE_SMTP_USERNAME, COMMERCE_SMTP_PASSWORD_FILE (a path, never the secret itself)")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read the secret file: %v", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		t.Fatal("secret file is empty")
	}
	s, err := mail.NewSMTP(mail.Config{Host: host, Port: 465, Username: user, Password: secret, From: user})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	own, foreign := pwaProbeBody(t, s, user, "probe-not-the-mailbox@example.invalid")
	if own < 200 || own > 299 {
		t.Errorf("MAIL FROM as the mailbox itself answered %d, want 2xx", own)
	}
	t.Logf("PA12 LIVE read-only: MAIL FROM=username reply code %d; MAIL FROM=another sender reply code %d (recorded, not asserted: providers differ); no RCPT/DATA was issued", own, foreign)
}

func TestMailPA12ProbeLoopbackSelfCheck(t *testing.T) {
	srv := mailtest.New(t)
	srv.RejectFromMismatch(true)
	s, err := mail.NewSMTP(srv.Config("xgdwm <" + srv.Username + ">"))
	if err != nil {
		t.Fatal(err)
	}
	own, foreign := pwaProbeBody(t, s, srv.Username, "probe-not-the-mailbox@example.invalid")
	if own != 250 {
		t.Errorf("own-sender probe answered %d, want 250", own)
	}
	if foreign != 553 {
		t.Errorf("foreign-sender probe answered %d, want the fake's 553 (RejectFromMismatch)", foreign)
	}
	if srv.DataCount() != 0 || len(srv.Messages()) != 0 {
		t.Errorf("the probe issued DATA (%d) or delivered mail (%d): it must be read-only", srv.DataCount(), len(srv.Messages()))
	}
}
