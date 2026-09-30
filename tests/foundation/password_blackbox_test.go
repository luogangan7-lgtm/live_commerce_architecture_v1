package foundation_test

// Independent black-box assertions for PA01 and PA02 (ruling B2: the implementers keep TestPasswordPA01Crypto
// and TestMailPA02SMTP; auth-tests adds one assertion each, written from the contract only):
//   TestPasswordPA01IndependentBlackBox — contracts/merchant-password-auth-v1.md PD1, PD4, §9 PA01: the stored
//     hash is Argon2id with exactly m=19456,t=2,p=1, a 16-byte random salt and a 32-byte key, and it is the value an
//     independent golang.org/x/crypto/argon2.IDKey computes (not identity.VerifyPassword grading itself); the code
//     HMAC is HMAC-SHA256(pepper, binding_hash || ascii(code)) recomputed with crypto/hmac; codes are always 6 ASCII digits.
//   TestMailPA02IndependentBlackBox — §3 classification and PD7: through the public mail API only, every failure point
//     yields FAILED or UNKNOWN as specified, the fake sees exactly the DATA count the contract allows, nothing is
//     retried, and no error text carries the recipient, subject, body code, username or secret.
// No PG, no network beyond the loopback mailtest server.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
	"livecommerce/internal/identity"
	"livecommerce/internal/mail"
	"livecommerce/internal/mail/mailtest"
)

func TestPasswordPA01IndependentBlackBox(t *testing.T) {
	password := pwaSecret()
	phc, err := identity.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=19456,t=2,p=1" {
		t.Fatalf("PHC header %q, want $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash> (PD1)", strings.Join(parts[:min(len(parts), 4)], "$"))
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	key, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(salt) != 16 || len(key) != 32 {
		t.Fatalf("salt/key not unpadded std base64 of 16/32 bytes: %v %v %d %d", err1, err2, len(salt), len(key))
	}
	// Independent recomputation: the same password, salt and the PD1 constants give exactly the stored key.
	if want := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32); string(want) != string(key) {
		t.Error("the stored key is not Argon2id(password, salt, t=2, m=19456 KiB, p=1, 32 bytes)")
	}
	other, _ := identity.HashPassword(password)
	if other == phc {
		t.Error("two hashes of one password are identical: the salt is not random")
	}
	if ok, err := identity.VerifyPassword(phc, password); err != nil || !ok {
		t.Errorf("VerifyPassword rejects its own hash: %v %v", ok, err)
	}
	if ok, _ := identity.VerifyPassword(phc, password+"x"); ok {
		t.Error("VerifyPassword accepts a different password")
	}
	// A hash produced independently with the same parameters verifies (the format is the contract, not the code).
	indep := "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(argon2.IDKey([]byte("independent-"+password), salt, 2, 19456, 1, 32))
	if ok, err := identity.VerifyPassword(indep, "independent-"+password); err != nil || !ok {
		t.Errorf("an independently produced PD1 hash does not verify: %v %v", ok, err)
	}
	// Foreign or weaker schemes are never accepted.
	for name, bad := range map[string]string{
		"argon2i":   strings.Replace(phc, "$argon2id$", "$argon2i$", 1),
		"argon2d":   strings.Replace(phc, "$argon2id$", "$argon2d$", 1),
		"m too low": strings.Replace(phc, "m=19456", "m=4096", 1),
		"bcrypt":    "$2b$12$" + strings.Repeat("A", 53),
	} {
		if ok, err := identity.VerifyPassword(bad, password); ok && err == nil {
			t.Errorf("%s hash accepted", name)
		}
	}

	// PD4 code: always exactly six ASCII digits.
	digit := regexp.MustCompile(`^[0-9]{6}$`)
	seen := map[string]bool{}
	for range 2000 {
		c, err := identity.NewCode()
		if err != nil || !digit.MatchString(c) {
			t.Fatalf("code %q err %v", c, err)
		}
		seen[c] = true
	}
	if len(seen) < 1500 {
		t.Errorf("only %d distinct codes in 2000 draws", len(seen))
	}
	// Code HMAC = HMAC-SHA256(pepper, binding_hash || ascii(code)), recomputed independently.
	pepper, bh := randomBytes(32), randomBytes(32)
	m := hmac.New(sha256.New, pepper)
	m.Write(bh)
	m.Write([]byte("012345"))
	if got := identity.CodeHMAC(pepper, bh, "012345"); string(got) != string(m.Sum(nil)) {
		t.Error("CodeHMAC is not HMAC-SHA256(pepper, binding_hash || code)")
	}
	if string(identity.CodeHMAC(pepper, bh, "012345")) == string(identity.CodeHMAC(pepper, randomBytes(32), "012345")) {
		t.Error("CodeHMAC ignores the binding hash")
	}
}

func TestMailPA02IndependentBlackBox(t *testing.T) {
	const (
		rcpt   = "canary-rcpt.pa02bb@recipient.example"
		code   = "917364"
		bbSubj = "canary-subject-pa02bb"
	)
	newSender := func(t *testing.T) (*mail.SMTP, *mailtest.Server) {
		t.Helper()
		srv := mailtest.New(t)
		s, err := mail.NewSMTP(srv.Config("xgdwm <" + srv.Username + ">"))
		if err != nil {
			t.Fatal(err)
		}
		return s, srv
	}
	msg := mail.Message{To: rcpt, Subject: bbSubj, Text: "your code is " + code, HTML: "<p>your code is " + code + "</p>"}
	noLeak := func(t *testing.T, srv *mailtest.Server, s *mail.SMTP, err error) {
		t.Helper()
		blob := errString(err) + "|" + s.String()
		for _, canary := range []string{rcpt, "recipient.example", bbSubj, code, srv.Username, srv.Password} {
			if strings.Contains(blob, canary) {
				t.Errorf("error or String() leaks %q", canary)
			}
		}
	}

	t.Run("sent", func(t *testing.T) {
		s, srv := newSender(t)
		reply, err := s.Send(context.Background(), mail.Message{To: rcpt, Subject: "驗證碼 " + bbSubj, Text: msg.Text, HTML: msg.HTML})
		if err != nil {
			t.Fatal(err)
		}
		if reply == "" || len(reply) > 128 {
			t.Errorf("reply text %q must be 1..128 bytes (stored as provider_message_id)", reply)
		}
		got := srv.Messages()
		if srv.DataCount() != 1 || len(got) != 1 || got[0].To != rcpt || got[0].Subject != "驗證碼 "+bbSubj {
			t.Fatalf("DATA=%d messages=%d", srv.DataCount(), len(got))
		}
		if got[0].Header.Get("Auto-Submitted") != "auto-generated" {
			t.Errorf("Auto-Submitted = %q", got[0].Header.Get("Auto-Submitted"))
		}
		if !strings.Contains(got[0].Text, code) || !strings.Contains(got[0].HTML, code) {
			t.Error("text and HTML parts must both carry the message")
		}
		raw := strings.ToLower(string(got[0].Raw))
		if strings.Contains(raw, "http://") || strings.Contains(raw, "https://") || strings.Contains(raw, "<img") {
			t.Error("the adapter added links or images")
		}
	})

	for _, tc := range []struct {
		name      string
		fault     mailtest.Fault
		want      error
		wantData  int
		otherWant error
	}{
		{"dial refused", mailtest.FaultDial, mail.ErrFailed, 0, mail.ErrUnknown},
		{"AUTH refused", mailtest.FaultAuth, mail.ErrFailed, 0, mail.ErrUnknown},
		{"RCPT refused", mailtest.FaultRcpt, mail.ErrFailed, 0, mail.ErrUnknown},
		{"DATA refused (354 not given)", mailtest.FaultData354, mail.ErrFailed, 1, mail.ErrUnknown},
		{"final 451", mailtest.FaultReply4xx, mail.ErrFailed, 1, mail.ErrUnknown},
		{"final 554", mailtest.FaultReply5xx, mail.ErrFailed, 1, mail.ErrUnknown},
		{"final 550 sender frequency limited", mailtest.FaultFrequency550, mail.ErrFailed, 1, mail.ErrUnknown},
		{"connection dropped after the dot", mailtest.FaultDropAfterDot, mail.ErrUnknown, 1, mail.ErrFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, srv := newSender(t)
			srv.SetNextFault(tc.fault)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err := s.Send(ctx, msg)
			if !errors.Is(err, tc.want) || errors.Is(err, tc.otherWant) {
				t.Fatalf("classification: %v, want %v and not %v", err, tc.want, tc.otherWant)
			}
			time.Sleep(300 * time.Millisecond) // a retry would show up as another DATA
			if srv.DataCount() != tc.wantData {
				t.Errorf("DATA=%d, want exactly %d: SMTP has no idempotency key, so nothing is ever re-sent (PD7)", srv.DataCount(), tc.wantData)
			}
			if len(srv.Messages()) != 0 {
				t.Errorf("mailbox holds %d messages for a failed send", len(srv.Messages()))
			}
			noLeak(t, srv, s, err)
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
