package foundation_test

// PA09 TestPasswordPA09MockMail (MOCK) — contracts/merchant-password-auth-v1.md §3 (mail content: subject
// without the code; text + minimal HTML with the code, purpose and validity; no links, images or
// tracking), PD7 (one send per challenge, never retried; login waits, sign-up/reset detach; FAILED on
// login is 503; UNKNOWN answers 202 and the user resends), §2 (resend = a new challenge that supersedes
// the old one), A5/A12 (8-slot background semaphore, graceful Close), §9 PA09. Everything goes through the
// REAL *mail.SMTP adapter against internal/mail/mailtest (never a stub Mailer). Tables:
// identity.email_challenges (mail_state, provider_message_id). Owner pool: reads plus moving a 60 s
// throttle window so a resend is possible (disclosed).

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"livecommerce/internal/identity"
	"livecommerce/internal/mail/mailtest"
)

var (
	pwaURLRE = regexp.MustCompile(`(?i)(https?://|www\.|<a[\s>]|<img|<link|<script|\bsrc\s*=|\bhref\s*=|url\()`)
	pwaCJKRE = regexp.MustCompile(`[\p{Han}]`)
)

func (e *pwaEnv) mailStates(email string) map[string]int {
	e.t.Helper()
	rows, err := e.f.owner.Query(pwaBG, `SELECT purpose||':'||mail_state FROM identity.email_challenges WHERE email=$1`, email)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out[s]++
	}
	return out
}

func (e *pwaEnv) awaitState(email, want string) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for e.mailStates(email)[want] == 0 {
		if time.Now().After(deadline) {
			e.t.Fatalf("no challenge reached %s in 15 s: %v", want, e.mailStates(email))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestPasswordPA09MockMail(t *testing.T) {
	t.Run("content_per_locale_and_one_data_per_challenge", func(t *testing.T) {
		e := newPwa(t)
		type msgs struct{ signup, login, reset mailtest.Received }
		got := map[string]msgs{}
		var used []string
		for _, locale := range []string{"zh-CN", "zh-TW", "en"} {
			email, password := pwaEmail(), pwaSecret()
			used = append(used, email)
			ip := pwaIP()
			c, err := e.pw.Signup(pwaBG, ip, email, password, locale)
			if err != nil {
				t.Fatal(err)
			}
			signupMail := e.awaitMails(email, 1)[0]
			if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", e.code(email, 1), ""); err != nil {
				t.Fatal(err)
			}
			lip := pwaIP()
			lc, err := e.pw.Login(pwaBG, lip, email, password, locale)
			if err != nil {
				t.Fatal(err)
			}
			loginMail := e.awaitMails(email, 2)[1]
			if _, err := e.pw.Complete(pwaBG, lip, lc.Binding, "login", e.code(email, 2), ""); err != nil {
				t.Fatal(err)
			}
			rip := pwaIP()
			if _, err := e.pw.Reset(pwaBG, rip, email, locale); err != nil {
				t.Fatal(err)
			}
			resetMail := e.awaitMails(email, 3)[2]
			got[locale] = msgs{signupMail, loginMail, resetMail}
			for purpose, m := range map[string]mailtest.Received{"signup": signupMail, "login": loginMail, "reset": resetMail} {
				n := map[string]int{"signup": 1, "login": 2, "reset": 3}[purpose]
				code := e.code(email, n)
				where := locale + "/" + purpose
				if !strings.EqualFold(m.To, email) {
					t.Errorf("%s: recipient differs from the submitted address", where)
				}
				if !strings.Contains(m.Text, code) || !strings.Contains(m.HTML, code) {
					t.Errorf("%s: text or HTML part lacks the code", where)
				}
				if strings.Contains(m.Subject, code) || pwaCodeRE.MatchString(" "+m.Subject+" ") {
					t.Errorf("%s: the subject carries a 6-digit code: %q", where, m.Subject)
				}
				if pwaURLRE.MatchString(m.Text) || pwaURLRE.MatchString(m.HTML) {
					t.Errorf("%s: mail contains a URL, link, image or script", where)
				}
				if strings.Contains(m.Text+m.HTML+m.Subject, password) {
					t.Errorf("%s: mail contains the password", where)
				}
				if !strings.Contains(m.Text, "10") {
					t.Errorf("%s: text does not state the 10 minute validity", where)
				}
				if locale == "en" {
					if pwaCJKRE.MatchString(m.Text+m.Subject) || !strings.Contains(strings.ToLower(m.Text), "10 minutes") {
						t.Errorf("%s: English mail must be CJK-free and say '10 minutes'", where)
					}
				} else if len(pwaCJKRE.FindAllString(m.Text, -1)) < 8 || !pwaCJKRE.MatchString(m.Subject) {
					t.Errorf("%s: Chinese mail lacks Chinese text/subject", where)
				}
			}
			// the purpose must be identifiable: the three bodies differ once the code is masked
			mask := func(m mailtest.Received) string { return pwaCodeRE.ReplaceAllString(m.Text, " CODE ") }
			if a, b, c := mask(signupMail), mask(loginMail), mask(resetMail); a == b || b == c || a == c {
				t.Errorf("%s: two purposes share the same mail text; the purpose (sign-up / sign-in / reset) must be stated", locale)
			}
		}
		for _, purpose := range []string{"signup", "login", "reset"} {
			pick := func(l string) mailtest.Received {
				return map[string]mailtest.Received{"signup": got[l].signup, "login": got[l].login, "reset": got[l].reset}[purpose]
			}
			if pick("zh-CN").Text == pick("zh-TW").Text || pick("zh-CN").Text == pick("en").Text || pick("zh-TW").Text == pick("en").Text {
				t.Errorf("%s: locales share identical text", purpose)
			}
		}
		e.settleMail()
		challenges := e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email = ANY($1)`, used)
		if int64(e.smtp.DataCount()) != challenges || int64(len(e.smtp.Messages())) != challenges || challenges != 9 {
			t.Errorf("DATA=%d messages=%d challenges=%d, want 9 each (exactly one send per challenge)", e.smtp.DataCount(), len(e.smtp.Messages()), challenges)
		}
		// §4.2 record_challenge_mail audits mail.sent / mail.failed / mail.unknown against the challenge
		if n := e.q1(`SELECT count(*) FROM identity.auth_events WHERE action='mail.sent' AND challenge_id IN (SELECT id FROM identity.email_challenges WHERE email = ANY($1))`, used); n != challenges {
			t.Errorf("mail.sent audit events = %d, want one per challenge (%d)", n, challenges)
		}
		if n := e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email = ANY($1) AND mail_state='SENT' AND provider_message_id IS NOT NULL AND length(provider_message_id) BETWEEN 1 AND 128`, used); n != challenges {
			t.Errorf("SENT challenges with a stored reply text = %d of %d", n, challenges)
		}
	})

	t.Run("exists_notice_has_no_code_and_no_row", func(t *testing.T) {
		e := newPwa(t)
		email := pwaEmail()
		e.register(email, pwaSecret())
		if _, err := e.pw.Signup(pwaBG, pwaIP(), email, pwaSecret(), "en"); err != nil {
			t.Fatal(err)
		}
		notice := e.awaitMails(email, 2)[1]
		if pwaCodeRE.MatchString(notice.Text) || pwaCodeRE.MatchString(notice.HTML) || pwaURLRE.MatchString(notice.Text+notice.HTML) {
			t.Error("the account-exists notice carries a code or a link")
		}
		e.settleMail()
		if e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email=$1`, email) != 1 {
			t.Error("the notice created a challenge row")
		}
		if e.smtp.DataCount() != 2 {
			t.Errorf("DATA=%d, want 2 (sign-up code + notice)", e.smtp.DataCount())
		}
	})

	t.Run("login_FAILED_is_503_and_UNKNOWN_is_202_and_resend_supersedes", func(t *testing.T) {
		e := newPwa(t)
		pwaStableHour(t)
		email, password := pwaEmail(), pwaSecret()
		e.register(email, password)
		// FAILED (final 550 after the '.'): 503, one DATA, never retried
		data := e.smtp.DataCount()
		e.smtp.SetNextFault(mailtest.FaultFrequency550)
		_, err := e.pw.Login(pwaBG, pwaIP(), email, password, "en")
		if !errors.Is(err, identity.ErrMailUnavailable) {
			t.Fatalf("login with a rejected mail: %v, want ErrMailUnavailable", err)
		}
		if e.smtp.DataCount() != data+1 {
			t.Errorf("DATA %d -> %d, want exactly one attempt (no retry)", data, e.smtp.DataCount())
		}
		if e.mailStates(email)["login:FAILED"] != 1 {
			t.Errorf("mail states %v, want one login:FAILED", e.mailStates(email))
		}
		// HTTP mapping with a fresh account: FAILED => 503 mail_unavailable
		e2, p2 := pwaEmail(), pwaSecret()
		e.register(e2, p2)
		e.smtp.SetNextFault(mailtest.FaultReply5xx)
		hr := e.post("login", pwaIP().String(), pwaBody(e2, p2))
		if hr.Status != 503 || hr.code() != "mail_unavailable" {
			t.Errorf("HTTP login with a failed mail: %d %q, want 503 mail_unavailable", hr.Status, hr.code())
		}
		// a failure BEFORE DATA (RCPT 550) is FAILED too and sends nothing
		e3, p3 := pwaEmail(), pwaSecret()
		e.register(e3, p3)
		d3 := e.smtp.DataCount()
		e.smtp.SetNextFault(mailtest.FaultRcpt)
		if _, err := e.pw.Login(pwaBG, pwaIP(), e3, p3, "en"); !errors.Is(err, identity.ErrMailUnavailable) {
			t.Errorf("RCPT refused: %v, want ErrMailUnavailable", err)
		}
		if e.smtp.DataCount() != d3 {
			t.Error("DATA was issued after RCPT was refused")
		}

		// UNKNOWN (connection dropped after the '.'): 202, state UNKNOWN, the user can resend
		e4, p4 := pwaEmail(), pwaSecret()
		e.register(e4, p4)
		d4 := e.smtp.DataCount()
		e.smtp.SetNextFault(mailtest.FaultDropAfterDot)
		ip := pwaIP()
		c1, err := e.pw.Login(pwaBG, ip, e4, p4, "en")
		if err != nil {
			t.Fatalf("login with an UNKNOWN mail outcome must answer 202: %v", err)
		}
		if e.smtp.DataCount() != d4+1 {
			t.Errorf("DATA %d -> %d after UNKNOWN, want exactly one (never retried)", d4, e.smtp.DataCount())
		}
		if e.mailStates(e4)["login:UNKNOWN"] != 1 {
			t.Errorf("mail states %v, want login:UNKNOWN", e.mailStates(e4))
		}
		if n := len(e.mailsTo(e4)); n != 1 {
			t.Errorf("mailbox holds %d messages for the UNKNOWN case, the dropped one was never acknowledged", n)
		}
		// resend = repeat step 1 => a new challenge, new code; the old binding is invalid
		pwaDisclose(t, "owner pool moves the 60 s email-mail-login window so the user can press resend")
		e.elapse("email-mail-login", e4, "minute")
		c2, err := e.pw.Login(pwaBG, pwaIP(), e4, p4, "en")
		if err != nil {
			t.Fatalf("resend: %v", err)
		}
		code2 := e.code(e4, 2)
		if c1.Binding == c2.Binding {
			t.Error("resend reused the binding")
		}
		if _, err := e.pw.Complete(pwaBG, ip, c1.Binding, "login", code2, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("old binding after the resend: %v, want ErrInvalidCode", err)
		}
		if _, err := e.pw.Complete(pwaBG, pwaIP(), c2.Binding, "login", code2, ""); err != nil {
			t.Errorf("new binding with the new code: %v", err)
		}
		if e.smtp.DataCount() != d4+2 {
			t.Errorf("DATA=%d, want %d (one per challenge)", e.smtp.DataCount(), d4+2)
		}

		// UNKNOWN on sign-up/reset: the response is unchanged (202) and the state is recorded UNKNOWN
		su := pwaEmail()
		e.smtp.SetNextFault(mailtest.FaultDropAfterDot)
		if _, err := e.pw.Signup(pwaBG, pwaIP(), su, pwaSecret(), "en"); err != nil {
			t.Fatalf("sign-up with an UNKNOWN mail: %v", err)
		}
		e.awaitState(su, "signup:UNKNOWN")
		// FAILED on sign-up/reset: still 202, recorded FAILED
		su2 := pwaEmail()
		e.smtp.SetNextFault(mailtest.FaultFrequency550)
		if _, err := e.pw.Signup(pwaBG, pwaIP(), su2, pwaSecret(), "en"); err != nil {
			t.Fatalf("sign-up with a FAILED mail must still answer 202: %v", err)
		}
		e.awaitState(su2, "signup:FAILED")
		rk := pwaEmail()
		e.register(rk, pwaSecret())
		e.smtp.SetNextFault(mailtest.FaultReply4xx)
		if _, err := e.pw.Reset(pwaBG, pwaIP(), rk, "en"); err != nil {
			t.Fatalf("reset with a FAILED mail must still answer 202: %v", err)
		}
		e.awaitState(rk, "reset:FAILED")
		for action, email := range map[string]string{"mail.failed": su2, "mail.unknown": su} {
			if n := e.q1(`SELECT count(*) FROM identity.auth_events WHERE action=$1 AND challenge_id IN (SELECT id FROM identity.email_challenges WHERE email=$2)`, action, email); n != 1 {
				t.Errorf("audit %s for the recorded outcome = %d, want 1", action, n)
			}
		}
	})

	t.Run("background_semaphore_full_records_FAILED_and_the_response_is_unchanged", func(t *testing.T) {
		e := newPwa(t, pwaCap(5000))
		e.smtp.SetDelay(3 * time.Second)
		var wg sync.WaitGroup
		emails := make([]string, 10)
		res := make([]pwaResp, 10)
		for i := range emails {
			emails[i] = pwaEmail()
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i] = e.post("signup", pwaIP().String(), pwaBody(emails[i], pwaSecret()))
			}()
		}
		wg.Wait()
		for i, r := range res {
			if r.Status != 202 || r.shape() != res[0].shape() {
				t.Errorf("response %d: %d %s, want 202 with one shape (a skipped send must not change the response)", i, r.Status, r.shape())
			}
		}
		e.settleMail()
		sent := e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email = ANY($1) AND mail_state='SENT'`, emails)
		failed := e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email = ANY($1) AND mail_state='FAILED'`, emails)
		if sent != 8 || failed != 2 {
			t.Errorf("SENT=%d FAILED=%d, want 8 and 2 (8 background slots, the rest skipped and recorded FAILED)", sent, failed)
		}
		if e.smtp.DataCount() != 8 {
			t.Errorf("DATA=%d, want 8: a skipped send must not dial", e.smtp.DataCount())
		}
	})

	t.Run("close_waits_for_in_flight_sends_then_records_FAILED", func(t *testing.T) {
		e := newPwa(t)
		e.smtp.SetDelay(1500 * time.Millisecond)
		email := pwaEmail()
		if _, err := e.pw.Signup(pwaBG, pwaIP(), email, pwaSecret(), "en"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := contextWithTimeout(20 * time.Second)
		defer cancel()
		start := time.Now()
		if err := e.pw.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if time.Since(start) < 500*time.Millisecond {
			t.Error("Close returned before the in-flight send finished")
		}
		if e.mailStates(email)["signup:SENT"] != 1 {
			t.Errorf("after Close the in-flight send is %v, want signup:SENT (graceful drain)", e.mailStates(email))
		}
		data := e.smtp.DataCount()
		email2 := pwaEmail()
		if _, err := e.pw.Signup(pwaBG, pwaIP(), email2, pwaSecret(), "en"); err != nil {
			t.Fatalf("sign-up after Close must still answer 202: %v", err)
		}
		e.awaitState(email2, "signup:FAILED")
		if e.smtp.DataCount() != data {
			t.Error("a send was dialled after Close")
		}
	})
}
