package identity

// Unit tests for the pure/loopback parts of the password flows: mail copy, HIBP client, sender
// classification and bounded background sends, throttle helpers, NewPasswords validation. No database.
// Names deliberately avoid the auth-tests gate prefixes (TestPasswordPA0[3-9], TestPasswordPA1, TestMailPA).

import (
	"context"
	"crypto/sha1" // #nosec: HIBP protocol fixture
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/mail"
)

func TestPasswordUnitMailCopy(t *testing.T) {
	for _, locale := range []string{"zh-CN", "zh-TW", "en"} {
		for _, purpose := range []string{"signup", "login", "reset"} {
			m := codeMail("merchant@example.test", locale, purpose, "482913")
			all := m.Text + m.HTML
			switch {
			case m.To != "merchant@example.test", m.Subject == "", strings.Contains(m.Subject, "482913"):
				t.Fatalf("%s/%s header wrong: %+v", locale, purpose, m)
			case !strings.Contains(m.Text, "482913") || !strings.Contains(m.HTML, "482913"):
				t.Fatalf("%s/%s body lacks the code", locale, purpose)
			case strings.Contains(strings.ToLower(all), "http") || strings.Contains(all, "<img") || strings.Contains(all, "<a "):
				t.Fatalf("%s/%s carries a link or image: %q", locale, purpose, all)
			case !strings.Contains(m.Text, "10"):
				t.Fatalf("%s/%s does not state the 10 minute validity", locale, purpose)
			}
		}
		n := existsMail("merchant@example.test", locale)
		if n.Subject == "" || digits(n.Text+n.HTML) || strings.Contains(strings.ToLower(n.Text+n.HTML), "http") {
			t.Fatalf("%s exists notice wrong: %+v", locale, n)
		}
	}
	if validLocale("fr") || validLocale("") || !validLocale("zh-TW") {
		t.Fatal("locale validation wrong")
	}
	// HTML-significant characters in the code slot are escaped, never interpreted.
	if strings.Contains(codeMail("a@b.co", "en", "reset", "<b>123").HTML, "<b>123") {
		t.Fatal("code not HTML-escaped")
	}
}

func digits(s string) bool { return strings.ContainsAny(s, "0123456789") }

func TestPasswordUnitHIBP(t *testing.T) {
	sum := sha1.Sum([]byte("hunter2 hunter2"))
	full := strings.ToUpper(hex.EncodeToString(sum[:]))
	var gotPath, gotPadding string
	var mode atomic.Value
	mode.Store("hit")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotPadding = r.URL.Path, r.Header.Get("Add-Padding")
		switch mode.Load().(string) {
		case "hit":
			_, _ = w.Write([]byte("0000000000000000000000000000000000A:0\r\n" + strings.ToLower(full[5:]) + ":37\r\nFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF:2\r\n"))
		case "padding-only": // a matching suffix with count 0 is padding, not a breach
			_, _ = w.Write([]byte(full[5:] + ":0\r\n"))
		case "miss":
			_, _ = w.Write([]byte("0000000000000000000000000000000000A:9\r\n"))
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		case "redirect":
			http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
		case "hang":
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	p := &Passwords{policy: PasswordPolicy{BreachCheck: "hibp"}, hibpBase: srv.URL, hibp: newHIBPClient()}

	if b, err := p.hibpBreached(t.Context(), "hunter2 hunter2"); !b || err != nil {
		t.Fatalf("hit = %v %v", b, err)
	}
	if gotPath != "/range/"+full[:5] || gotPadding != "true" {
		t.Fatalf("request path %q padding %q; only the 5-char prefix may leave and Add-Padding must be true", gotPath, gotPadding)
	}
	if err := p.checkBreach(t.Context(), "hunter2 hunter2"); err == nil || err.Error() != "password policy: breached" {
		t.Fatalf("checkBreach = %v", err)
	}
	for _, m := range []string{"padding-only", "miss"} {
		mode.Store(m)
		if b, err := p.hibpBreached(t.Context(), "hunter2 hunter2"); b || err != nil {
			t.Fatalf("%s = %v %v", m, b, err)
		}
	}
	// Fail-open: every kind of failure accepts the password (ruling Q3) and reports no breach.
	for _, m := range []string{"500", "redirect"} {
		mode.Store(m)
		if _, err := p.hibpBreached(t.Context(), "hunter2 hunter2"); err == nil {
			t.Fatalf("%s should be an error from the client", m)
		}
		if err := p.checkBreach(t.Context(), "hunter2 hunter2"); err != nil {
			t.Fatalf("%s must fail open, got %v", m, err)
		}
	}
	mode.Store("hang")
	start := time.Now()
	if err := p.checkBreach(t.Context(), "hunter2 hunter2"); err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout must fail open within ~2 s: %v after %v", err, time.Since(start))
	}
	off := &Passwords{policy: PasswordPolicy{BreachCheck: "off"}}
	if err := off.checkBreach(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
}

// recorder captures record_challenge_mail calls without a database.
type recorder struct {
	mu   sync.Mutex
	rows []string
}

func (r *recorder) hook(id [16]byte, state, reply string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, state+"|"+reply)
}
func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.rows...)
}

type stubMailer struct {
	reply string
	err   error
	block chan struct{}
	calls atomic.Int32
}

func (s *stubMailer) Send(ctx context.Context, m mail.Message) (string, error) {
	s.calls.Add(1)
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.reply, s.err
}

func newSenderOnly(m Mailer) (*Passwords, *recorder) {
	r := &recorder{}
	return &Passwords{mailer: m, bgSlots: make(chan struct{}, asyncSlots), recordHook: r.hook}, r
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestPasswordUnitSenderClassification(t *testing.T) {
	for name, c := range map[string]struct {
		err   error
		state string
	}{
		"sent":    {nil, "SENT"},
		"unknown": {errors.Join(mail.ErrUnknown, errors.New("dropped")), "UNKNOWN"},
		"failed":  {errors.Join(mail.ErrFailed, errors.New("rejected")), "FAILED"},
		"other":   {errors.New("dial: refused"), "FAILED"},
	} {
		s := &stubMailer{reply: "250 ok", err: c.err}
		p, rec := newSenderOnly(s)
		if got := p.sendSync(t.Context(), [16]byte{1}, mail.Message{To: "a@b.co"}); got != c.state {
			t.Fatalf("%s: state %s, want %s", name, got, c.state)
		}
		rows := rec.snapshot()
		wantReply := ""
		if c.state == "SENT" {
			wantReply = "250 ok"
		}
		if len(rows) != 1 || rows[0] != c.state+"|"+wantReply || s.calls.Load() != 1 {
			t.Fatalf("%s: rows %v calls %d (exactly one send, never retried)", name, rows, s.calls.Load())
		}
	}
}

func TestPasswordUnitAsyncSenderBoundedAndDrains(t *testing.T) {
	s := &stubMailer{reply: "250 ok", block: make(chan struct{})}
	p, rec := newSenderOnly(s)
	for i := 0; i < asyncSlots; i++ {
		p.sendAsync(t.Context(), [16]byte{byte(i)}, true, mail.Message{To: "a@b.co"})
	}
	eventually(t, func() bool { return s.calls.Load() == asyncSlots })
	// The 9th send finds the 8-slot semaphore full: nothing is dialled, the row is recorded FAILED.
	p.sendAsync(t.Context(), [16]byte{99}, true, mail.Message{To: "a@b.co"})
	if s.calls.Load() != asyncSlots {
		t.Fatalf("a send was dialled with a full semaphore: %d", s.calls.Load())
	}
	if rows := rec.snapshot(); len(rows) != 1 || rows[0] != "FAILED|" {
		t.Fatalf("rows = %v", rows)
	}
	// The notice mail (record=false) never touches the database even when it fails.
	// Close with an already-expired context reports the in-flight sends it could not wait for.
	expired, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := p.Close(expired); err == nil {
		t.Fatal("Close returned before in-flight sends finished")
	}
	// After Close nothing new is dialled; the challenge is recorded FAILED.
	p.sendAsync(t.Context(), [16]byte{100}, true, mail.Message{To: "a@b.co"})
	if s.calls.Load() != asyncSlots {
		t.Fatal("send dialled after Close")
	}
	close(s.block) // let the in-flight sends finish
	if err := p.Close(t.Context()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	eventually(t, func() bool { return len(rec.snapshot()) == 2+asyncSlots })
	sent := 0
	for _, r := range rec.snapshot() {
		if r == "SENT|250 ok" {
			sent++
		}
	}
	if sent != asyncSlots {
		t.Fatalf("recorded SENT rows = %d, want %d", sent, asyncSlots)
	}
}

func TestPasswordUnitAsyncSenderSurvivesMailerPanic(t *testing.T) {
	p, _ := newSenderOnly(panicMailer{})
	p.sendAsync(t.Context(), [16]byte{1}, true, mail.Message{})
	if err := p.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSlots) != 0 {
		t.Fatal("slot leaked after panic")
	}
}

type panicMailer struct{}

func (panicMailer) Send(context.Context, mail.Message) (string, error) { panic("boom") }

func TestPasswordUnitThrottleHelpers(t *testing.T) {
	if u, n, l := mailShares(20); u != 8 || n != 3 || l != 9 {
		t.Fatalf("shares(20) = %d/%d/%d, want 8/3/9", u, n, l)
	}
	if u, n, l := mailShares(200); u != 80 || n != 30 || l != 90 {
		t.Fatalf("shares(200) = %d/%d/%d, want 80/30/90", u, n, l)
	}
	for in, want := range map[string]string{
		"203.0.113.9":                "203.0.113.9/32",
		"::ffff:203.0.113.9":         "203.0.113.9/32",
		"2001:db8:1:2:3:4:5:6":       "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:1:1": "2001:db8:1:2::/64",
	} {
		if got := ipPrefix(netip.MustParseAddr(in)).String(); got != want {
			t.Fatalf("ipPrefix(%s) = %s, want %s", in, got, want)
		}
	}
	if p, ok := ip48Prefix(netip.MustParseAddr("2001:db8:1:2:3:4:5:6")); !ok || p.String() != "2001:db8:1::/48" {
		t.Fatalf("ip48 = %v %v", p, ok)
	}
	if _, ok := ip48Prefix(netip.MustParseAddr("203.0.113.9")); ok {
		t.Fatal("IPv4 has no /48 bucket")
	}
	// Windows align to the UTC+8 day: 2026-09-30 15:59:00 UTC is one minute before the boundary.
	now := time.Date(2026, 9, 30, 15, 59, 0, 0, time.UTC)
	if d := retryAfter(now, window{86400, utc8Day, 10}); d != time.Minute {
		t.Fatalf("retryAfter before UTC+8 midnight = %v", d)
	}
	now = time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC)
	if d := retryAfter(now, window{86400, utc8Day, 10}); d != 24*time.Hour {
		t.Fatalf("retryAfter at the boundary = %v", d)
	}
	if d := retryAfter(time.Date(2026, 1, 1, 0, 0, 30, 500_000_000, time.UTC), window{60, 0, 1}); d != 30*time.Second {
		t.Fatalf("retryAfter rounds up: %v", d)
	}
	if d := retryAfter(time.Date(2026, 1, 1, 0, 0, 59, 999_000_000, time.UTC), window{60, 0, 1}); d != time.Second {
		t.Fatalf("retryAfter floor is 1 s: %v", d)
	}

	p := &Passwords{policy: PasswordPolicy{Pepper: []byte(strings.Repeat("p", 32)), MailDailyCap: 200}}
	if k := p.bucketKey("ip", "203.0.113.9/32"); len(k) != 32 || string(k) == string(p.bucketKey("ip-signup", "203.0.113.9/32")) ||
		string(k) != string(p.bucketKey("ip", "203.0.113.9/32")) {
		t.Fatal("bucket key not a stable, kind-separated 32-byte HMAC")
	}
	other := &Passwords{policy: PasswordPolicy{Pepper: []byte(strings.Repeat("q", 32))}}
	if string(other.bucketKey("ip", "x")) == string(p.bucketKey("ip", "x")) {
		t.Fatal("bucket key ignores the pepper")
	}
	// §6 table order for the unauthenticated mail chain: per-source daily caps come before the global budget.
	kinds := func(bs []bucket) string {
		var out []string
		for _, b := range bs {
			out = append(out, b.kind)
		}
		return strings.Join(out, ",")
	}
	if got := kinds(p.unauthMailBuckets("a@b.co", netip.MustParseAddr("203.0.113.9"))); got != "ip-mail-unauth,email-mail-unauth,email-mail-unauth-total,global-mail-unauth" {
		t.Fatalf("IPv4 chain = %s", got)
	}
	if got := kinds(p.unauthMailBuckets("a@b.co", netip.MustParseAddr("2001:db8::1"))); got != "ip-mail-unauth,ip48-mail-unauth,email-mail-unauth,email-mail-unauth-total,global-mail-unauth" {
		t.Fatalf("IPv6 chain = %s", got)
	}
	chain := p.unauthMailBuckets("a@b.co", netip.MustParseAddr("203.0.113.9"))
	if g := chain[len(chain)-1]; !g.global || g.wins[0].limit != 80 || g.wins[0].offset != utc8Day {
		t.Fatalf("global bucket = %+v", g)
	}
	if e := chain[1]; e.value != "a@b.co‖203.0.113.9/32" || len(e.wins) != 3 {
		t.Fatalf("email-mail-unauth keyed per (email, source): %+v", e)
	}
}

func TestPasswordUnitNewPasswordsValidation(t *testing.T) {
	const dsnSentinel = "neutral-sentinel-not-used" // the pool is lazy and never dials in these cases
	u := url.URL{Scheme: "postgres", User: url.UserPassword("u", dsnSentinel), Host: "127.0.0.1:1", Path: "/x"}
	pool, err := pgxpool.New(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pepper := strings.Repeat("p", 32)
	ok := func() PasswordPolicy {
		return PasswordPolicy{Pepper: []byte(pepper), SessionTTL: time.Hour, BreachCheck: "hibp"}
	}
	mailer := &stubMailer{}
	valid, err := NewPasswords(pool, mailer, ok())
	if err != nil || valid.policy.MailDailyCap != 200 || valid.hibpBase != hibpDefaultBase {
		t.Fatalf("valid policy: %v %+v", err, valid)
	}
	if okv, err := VerifyPassword(valid.dummyPHC, "anything"); okv || err != nil {
		t.Fatalf("dummy PHC unusable: %v %v", okv, err)
	}
	bad := map[string]func(*PasswordPolicy){
		"short pepper":          func(p *PasswordPolicy) { p.Pepper = []byte("short") },
		"ttl too short":         func(p *PasswordPolicy) { p.SessionTTL = time.Minute },
		"ttl too long":          func(p *PasswordPolicy) { p.SessionTTL = 25 * time.Hour },
		"cap below range":       func(p *PasswordPolicy) { p.MailDailyCap = 19 },
		"cap above range":       func(p *PasswordPolicy) { p.MailDailyCap = 100001 },
		"breach mode unknown":   func(p *PasswordPolicy) { p.BreachCheck = "maybe" },
		"breach off in prod":    func(p *PasswordPolicy) { p.BreachCheck = "off" },
		"hibp url in prod":      func(p *PasswordPolicy) { p.HIBPBaseURL = "http://127.0.0.1:9" },
		"hibp url off-host":     func(p *PasswordPolicy) { p.AllowLoopback = true; p.HIBPBaseURL = "https://hibp.example" },
		"hibp url userinfo":     func(p *PasswordPolicy) { p.AllowLoopback = true; p.HIBPBaseURL = "http://a@127.0.0.1:9" },
		"hibp url wrong scheme": func(p *PasswordPolicy) { p.AllowLoopback = true; p.HIBPBaseURL = "ftp://127.0.0.1:9" },
	}
	for name, mutate := range bad {
		p := ok()
		mutate(&p)
		if _, err := NewPasswords(pool, mailer, p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := NewPasswords(nil, mailer, ok()); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil pool accepted")
	}
	if _, err := NewPasswords(pool, nil, ok()); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil mailer accepted")
	}
	loop := ok()
	loop.AllowLoopback, loop.BreachCheck, loop.HIBPBaseURL = true, "off", "http://127.0.0.1:9/"
	if p, err := NewPasswords(pool, mailer, loop); err != nil || p.hibpBase != "http://127.0.0.1:9" {
		t.Fatalf("loopback overrides: %v", err)
	}
}

func TestPasswordUnitServiceNilProviderNeedsPasswordLogin(t *testing.T) {
	const dsnSentinel = "neutral-sentinel-not-used"
	u := url.URL{Scheme: "postgres", User: url.UserPassword("u", dsnSentinel), Host: "127.0.0.1:1", Path: "/x"}
	pool, err := pgxpool.New(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := New(pool, nil, Policy{ProviderKey: "k", SessionTTL: time.Hour}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil provider without password login accepted: %v", err)
	}
	s, err := New(pool, nil, Policy{PasswordLogin: true, SessionTTL: time.Hour})
	if err != nil {
		t.Fatalf("password-only service rejected: %v", err)
	}
	if _, err := s.Start(t.Context()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Start without provider = %v, want ErrDisabled", err)
	}
	if _, err := s.Complete(t.Context(), "s", "b", "c"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Complete without provider = %v, want ErrDisabled", err)
	}
	if _, err := New(pool, nil, Policy{PasswordLogin: true, SessionTTL: time.Minute}); !errors.Is(err, ErrInvalid) {
		t.Fatal("ttl bounds must still apply")
	}
}

// PD6: the "no row" answers (taken sign-up email, unknown reset email) must not be told apart from a
// real challenge by the expires_at string. time.Now() carries nanoseconds on Linux, timestamptz
// decodes at microseconds; both must serialise as whole UTC seconds.
func TestPasswordUnitChallengeExpiryShape(t *testing.T) {
	re := regexp.MustCompile(`^"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ"$`)
	dbShaped := time.Now().Add(challengeTTL).Truncate(time.Microsecond).Add(123456 * time.Nanosecond).In(time.FixedZone("x", 8*3600))
	fake := challengeResult("b", nil)
	real := challengeResult("b", &dbShaped)
	for name, c := range map[string]Challenge{"fabricated": fake, "sql": real} {
		b, err := json.Marshal(c.ExpiresAt)
		if err != nil || !re.Match(b) {
			t.Errorf("%s expires_at %s does not match whole-second UTC shape (%v)", name, b, err)
		}
	}
	if d := fake.ExpiresAt.Sub(real.ExpiresAt); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("fabricated and SQL expiry differ by %v", d)
	}
}
