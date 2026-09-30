// smtp_test.go: unit tests for the SMTP adapter against the loopback TLS fake (internal/mail/mailtest),
// including gate PA02 (TestMailPA02SMTP, contract §9). External test package because mailtest imports
// internal/mail. Every canary below is synthetic; nothing here dials a real host.
package mail_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"livecommerce/internal/mail"
	"livecommerce/internal/mail/mailtest"
)

// smtpSentinel stands in for the SMTP authorization code in leak assertions: neutral name and value so
// secret scanners (GitGuardian; PROCESS.md §6) do not read it as a credential.
const smtpSentinel = "sentinel-smtp-7c1"

const (
	canaryRcpt    = "canary-rcpt@recipient.example"
	canarySubject = "canary-subject-7731"
	canaryBody    = "canary-body-code-424242"
)

func newSender(t *testing.T) (*mailtest.Server, *mail.SMTP) {
	t.Helper()
	srv := mailtest.New(t)
	c, err := mail.NewSMTP(srv.Config("xgdwm <" + srv.Username + ">"))
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	return srv, c
}

func msg() mail.Message {
	return mail.Message{To: canaryRcpt, Subject: canarySubject, Text: "text " + canaryBody, HTML: "<p>html " + canaryBody + "</p>"}
}

func send(c *mail.SMTP, m mail.Message) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Send(ctx, m)
}

// noLeak asserts the I11 canary scan over everything an error or log line could carry.
func noLeak(t *testing.T, srv *mailtest.Server, c *mail.SMTP, err error) {
	t.Helper()
	blob := fmt.Sprintf("%v|%s|%+v|%#v|%v|%#v", err, c, c, c, srv.Config("x <"+srv.Username+">"), srv.Config("x <"+srv.Username+">"))
	for _, canary := range []string{canaryRcpt, "recipient.example", canarySubject, canaryBody, srv.Username, srv.Password, "xgdwm"} {
		if strings.Contains(blob, canary) {
			t.Errorf("leak: %q found in %q", canary, blob)
		}
	}
}

func TestNewSMTPConfig(t *testing.T) {
	base := mail.Config{Host: "smtp.qq.com", Username: "a@qq.com", Password: "p", From: "X <A@QQ.com>"}
	cases := []struct {
		name string
		mut  func(*mail.Config)
		ok   bool
	}{
		{"prod defaults, From case-insensitive (M2)", func(*mail.Config) {}, true},
		{"explicit 465", func(c *mail.Config) { c.Port = 465 }, true},
		{"exmail host", func(c *mail.Config) { c.Host = "smtp.exmail.qq.com" }, true},
		{"port 587 refused (M1)", func(c *mail.Config) { c.Port = 587 }, false},
		{"port 2525 with AllowLoopback but non-loopback host refused", func(c *mail.Config) { c.Port = 2525; c.AllowLoopback = true }, false},
		{"loopback host without AllowLoopback refused", func(c *mail.Config) { c.Host = "127.0.0.1" }, false},
		{"loopback host + port + AllowLoopback ok", func(c *mail.Config) { c.Host = "127.0.0.1"; c.Port = 40000; c.AllowLoopback = true }, true},
		{"RootCAs without AllowLoopback refused", func(c *mail.Config) { c.RootCAs = mailtest.New(t).RootCAs }, false},
		{"host with port refused", func(c *mail.Config) { c.Host = "smtp.qq.com:465" }, false},
		{"host with scheme refused", func(c *mail.Config) { c.Host = "smtps://smtp.qq.com" }, false},
		{"non-loopback IP literal refused", func(c *mail.Config) { c.Host = "203.0.113.5" }, false},
		{"From != username refused (M2)", func(c *mail.Config) { c.From = "X <b@qq.com>" }, false},
		{"From unparsable refused", func(c *mail.Config) { c.From = "not an address" }, false},
		{"empty password refused", func(c *mail.Config) { c.Password = "" }, false},
		{"empty username refused", func(c *mail.Config) { c.Username = "" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mut(&c)
			_, err := mail.NewSMTP(c)
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v want ok=%v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), c.Password+"|") {
				t.Fatalf("error leaks config: %v", err)
			}
		})
	}
}

func TestConfigAndSMTPStringRedacted(t *testing.T) {
	c, err := mail.NewSMTP(mail.Config{Host: "smtp.qq.com", Username: "u@qq.com", Password: smtpSentinel, From: "u@qq.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.String(); got != "mail.SMTP(host=smtp.qq.com)" {
		t.Fatalf("String = %q", got)
	}
	cfg := mail.Config{Host: "smtp.qq.com", Username: "u@qq.com", Password: smtpSentinel, From: "u@qq.com"}
	for _, s := range []string{fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), fmt.Sprintf("%v %+v %#v", c, c, c)} {
		if strings.Contains(s, smtpSentinel) || strings.Contains(s, "u@qq.com") {
			t.Fatalf("redaction failed: %s", s)
		}
	}
}

func TestSendReplyCleaning(t *testing.T) {
	srv, c := newSender(t)
	srv.SetOKReply("ok\x00\x07 id" + strings.Repeat("é", 100)) // 5 bytes + 2 per rune: the 128-byte limit falls inside a rune
	reply, err := send(c, msg())
	if err != nil {
		t.Fatal(err)
	}
	if len(reply) > 128 || !utf8.ValidString(reply) || strings.ContainsAny(reply, "\x00\x07") || !strings.HasPrefix(reply, "ok id") || len(reply) != 127 {
		t.Fatalf("reply not cleaned: %q (%d bytes)", reply, len(reply))
	}
}

func TestSendInputRefusedWithoutDial(t *testing.T) {
	srv, c := newSender(t)
	for name, m := range map[string]mail.Message{
		"bad recipient":        {To: "not an address", Subject: "s", Text: "t", HTML: "h"},
		"subject header inj":   {To: canaryRcpt, Subject: "a\r\nBcc: x@y.example", Text: "t", HTML: "h"},
		"recipient with angle": {To: "a@b.example>\r\nRCPT TO:<x@y.example", Subject: "s", Text: "t", HTML: "h"},
	} {
		if _, err := send(c, m); !errors.Is(err, mail.ErrFailed) {
			t.Errorf("%s: err=%v want ErrFailed", name, err)
		}
	}
	if srv.DataCount() != 0 || len(srv.Messages()) != 0 {
		t.Fatalf("something reached the server")
	}
}

func TestProbe(t *testing.T) {
	srv, c := newSender(t)
	srv.RejectFromMismatch(true)
	if code, err := c.Probe(context.Background(), srv.Username); err != nil || code != 250 {
		t.Fatalf("matching from: code=%d err=%v", code, err)
	}
	if code, err := c.Probe(context.Background(), "someone-else@mailtest.example"); err != nil || code != 553 {
		t.Fatalf("mismatched from: code=%d err=%v", code, err)
	}
	if srv.DataCount() != 0 || len(srv.Messages()) != 0 {
		t.Fatalf("probe must never send: data=%d", srv.DataCount())
	}
	srv.SetNextFault(mailtest.FaultAuth)
	if _, err := c.Probe(context.Background(), srv.Username); !errors.Is(err, mail.ErrFailed) {
		t.Fatalf("auth fault: %v", err)
	}
	if _, err := c.Probe(context.Background(), "bad address"); !errors.Is(err, mail.ErrFailed) {
		t.Fatalf("bad from: %v", err)
	}
}

func TestSendCanceledContextBeforeDotIsFailed(t *testing.T) {
	_, c := newSender(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Send(ctx, msg()); !errors.Is(err, mail.ErrFailed) {
		t.Fatalf("err=%v want ErrFailed (nothing was sent)", err)
	}
}

// TestMailPA02SMTP is gate PA02 (contract §9): implicit TLS + PLAIN, exact MIME headers, the §3
// classification table, zero re-sends in every branch, From != username refused at config, and no
// recipient/code/username/secret in errors or String().
func TestMailPA02SMTP(t *testing.T) {
	t.Run("sent: implicit TLS, PLAIN auth, MIME exact, one DATA", func(t *testing.T) {
		srv, c := newSender(t)
		m := mail.Message{To: "Buyer <" + canaryRcpt + ">", Subject: "验证码 xgdwm", Text: "你好 " + canaryBody + "\nline2", HTML: "<p>你好 " + canaryBody + "</p>" + strings.Repeat("<i>x</i>", 60)}
		reply, err := send(c, m)
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if !strings.HasPrefix(reply, "OK queued as ") {
			t.Fatalf("reply = %q", reply)
		}
		if srv.DataCount() != 1 || len(srv.Messages()) != 1 {
			t.Fatalf("data=%d msgs=%d", srv.DataCount(), len(srv.Messages()))
		}
		got := srv.Messages()[0]
		if got.From != srv.Username || got.To != canaryRcpt {
			t.Errorf("envelope from=%q to=%q", got.From, got.To)
		}
		if got.Subject != m.Subject {
			t.Errorf("Subject = %q", got.Subject)
		}
		if got.Text != m.Text || got.HTML != m.HTML {
			t.Errorf("decoded parts differ:\ntext=%q\nhtml=%q", got.Text, got.HTML)
		}
		head, _, _ := strings.Cut(string(got.Raw), "\r\n\r\n")
		if regexp.MustCompile(`[^\r]\n`).Match(got.Raw) {
			t.Errorf("bare LF in message")
		}
		for _, want := range []string{
			"From: \"xgdwm\" <" + srv.Username + ">",
			"To: <" + canaryRcpt + ">",
			"MIME-Version: 1.0",
			"Auto-Submitted: auto-generated",
		} {
			if !strings.Contains(head, want+"\r\n") && !strings.HasSuffix(head, want) {
				t.Errorf("missing header line %q in:\n%s", want, head)
			}
		}
		if s := got.Header.Get("Subject"); !strings.HasPrefix(s, "=?UTF-8?b?") {
			t.Errorf("non-ASCII Subject not RFC 2047 B-encoded: %q", s)
		}
		if id := got.Header.Get("Message-Id"); !regexp.MustCompile(`^<[0-9a-f]{32}@mailtest\.example>$`).MatchString(id) {
			t.Errorf("Message-ID = %q", id)
		}
		if d, err := time.Parse(time.RFC1123Z, got.Header.Get("Date")); err != nil || d.UTC().Format("-0700") != "+0000" || time.Since(d) > time.Minute {
			t.Errorf("Date = %q (%v)", got.Header.Get("Date"), err)
		}
		if ct := got.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/alternative; boundary=") {
			t.Errorf("Content-Type = %q", ct)
		}
		body := string(got.Raw)
		if !strings.Contains(body, "Content-Transfer-Encoding: quoted-printable") || !strings.Contains(body, "Content-Transfer-Encoding: base64") ||
			!strings.Contains(body, "text/plain; charset=UTF-8") || !strings.Contains(body, "text/html; charset=UTF-8") {
			t.Errorf("part headers wrong:\n%s", body)
		}
		if strings.Contains(strings.ToLower(body), "http") || strings.Contains(strings.ToLower(body), "<img") {
			t.Errorf("adapter added links/tracking")
		}
		for _, line := range strings.Split(body, "\r\n") {
			if len(line) > 998 {
				t.Errorf("line longer than RFC 5322 limit: %d", len(line))
			}
		}
		noLeak(t, srv, c, nil) // String()/Config redaction on the success path too
	})

	t.Run("ASCII subject stays unencoded", func(t *testing.T) {
		srv, c := newSender(t)
		if _, err := send(c, msg()); err != nil {
			t.Fatal(err)
		}
		if s := srv.Messages()[0].Header.Get("Subject"); s != canarySubject {
			t.Errorf("Subject = %q", s)
		}
	})

	failedBefore := []struct {
		name  string
		fault mailtest.Fault
		data  int // DATA commands the server may see
	}{
		{"dial/TLS", mailtest.FaultDial, 0},
		{"AUTH", mailtest.FaultAuth, 0},
		{"RCPT", mailtest.FaultRcpt, 0},
		{"DATA 354 refused", mailtest.FaultData354, 1},
	}
	for _, tc := range failedBefore {
		t.Run("failure before '.' => FAILED: "+tc.name, func(t *testing.T) {
			srv, c := newSender(t)
			srv.SetNextFault(tc.fault)
			_, err := send(c, msg())
			if !errors.Is(err, mail.ErrFailed) || errors.Is(err, mail.ErrUnknown) {
				t.Fatalf("err=%v want ErrFailed only", err)
			}
			if srv.DataCount() != tc.data || len(srv.Messages()) != 0 {
				t.Fatalf("data=%d msgs=%d (zero re-sends)", srv.DataCount(), len(srv.Messages()))
			}
			noLeak(t, srv, c, err)
		})
	}

	finals := []struct {
		name  string
		fault mailtest.Fault
		code  string
	}{
		{"4xx", mailtest.FaultReply4xx, "code=451"},
		{"5xx", mailtest.FaultReply5xx, "code=554"},
		{"550 Sender frequency limited (F8)", mailtest.FaultFrequency550, "code=550"},
	}
	for _, tc := range finals {
		t.Run("final reply "+tc.name+" => FAILED smtp_rejected, no re-send", func(t *testing.T) {
			srv, c := newSender(t)
			srv.SetNextFault(tc.fault)
			_, err := send(c, msg())
			if !errors.Is(err, mail.ErrFailed) || errors.Is(err, mail.ErrUnknown) {
				t.Fatalf("err=%v want ErrFailed only", err)
			}
			if !strings.Contains(err.Error(), "kind=smtp_rejected") || !strings.Contains(err.Error(), tc.code) {
				t.Errorf("error lacks kind/code: %v", err)
			}
			if strings.Contains(err.Error(), "frequency") {
				t.Errorf("server text must not be copied into the error: %v", err)
			}
			if srv.DataCount() != 1 || len(srv.Messages()) != 0 {
				t.Fatalf("data=%d msgs=%d", srv.DataCount(), len(srv.Messages()))
			}
			noLeak(t, srv, c, err)
		})
	}

	t.Run("no final reply after '.' => UNKNOWN, no re-send", func(t *testing.T) {
		srv, c := newSender(t)
		srv.SetNextFault(mailtest.FaultDropAfterDot)
		_, err := send(c, msg())
		if !errors.Is(err, mail.ErrUnknown) || errors.Is(err, mail.ErrFailed) {
			t.Fatalf("err=%v want ErrUnknown only", err)
		}
		if srv.DataCount() != 1 {
			t.Fatalf("data=%d want exactly 1 (never retried)", srv.DataCount())
		}
		noLeak(t, srv, c, err)
	})

	t.Run("timeout waiting for the final reply => UNKNOWN", func(t *testing.T) {
		srv, c := newSender(t)
		srv.SetDelay(700 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.Send(ctx, msg())
		if !errors.Is(err, mail.ErrUnknown) || time.Since(start) > 650*time.Millisecond {
			t.Fatalf("err=%v after %v want ErrUnknown at the ctx deadline", err, time.Since(start))
		}
		if srv.DataCount() != 1 {
			t.Fatalf("data=%d", srv.DataCount())
		}
		noLeak(t, srv, c, err)
	})

	t.Run("delay below the deadline still SENT (PA08b 400 ms)", func(t *testing.T) {
		srv, c := newSender(t)
		srv.SetDelay(400 * time.Millisecond)
		start := time.Now()
		if _, err := send(c, msg()); err != nil || time.Since(start) < 400*time.Millisecond {
			t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
		}
	})

	t.Run("wrong password => FAILED at AUTH", func(t *testing.T) {
		srv := mailtest.New(t)
		cfg := srv.Config("xgdwm <" + srv.Username + ">")
		cfg.Password += "x"
		c, err := mail.NewSMTP(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = send(c, msg())
		if !errors.Is(err, mail.ErrFailed) || !strings.Contains(err.Error(), "kind=auth") || !strings.Contains(err.Error(), "code=535") {
			t.Fatalf("err=%v", err)
		}
		if srv.DataCount() != 0 {
			t.Fatalf("data=%d", srv.DataCount())
		}
		noLeak(t, srv, c, err)
	})

	t.Run("untrusted certificate => FAILED, credentials never sent", func(t *testing.T) {
		srv := mailtest.New(t)
		cfg := srv.Config("xgdwm <" + srv.Username + ">")
		cfg.RootCAs = nil // system roots do not contain the fake CA
		c, err := mail.NewSMTP(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = send(c, msg())
		if !errors.Is(err, mail.ErrFailed) || !strings.Contains(err.Error(), "kind=dial_tls") {
			t.Fatalf("err=%v", err)
		}
		noLeak(t, srv, c, err)
	})

	t.Run("From != username refused at config (M2)", func(t *testing.T) {
		srv := mailtest.New(t)
		if _, err := mail.NewSMTP(srv.Config("xgdwm <other@mailtest.example>")); err == nil {
			t.Fatal("NewSMTP accepted a From different from the username")
		}
	})

	t.Run("sequential sends are independent: one DATA each", func(t *testing.T) {
		srv, c := newSender(t)
		for i := 0; i < 3; i++ {
			if _, err := send(c, msg()); err != nil {
				t.Fatal(err)
			}
		}
		if srv.DataCount() != 3 || len(srv.Messages()) != 3 {
			t.Fatalf("data=%d msgs=%d", srv.DataCount(), len(srv.Messages()))
		}
	})
}
