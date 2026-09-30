package foundation_test

// PA08b TestPasswordPA08bTiming (HTTP_PG) — contracts/merchant-password-auth-v1.md PD6 ("response time
// does not depend on existence because sign-up/reset never wait for SMTP and do the same hashing work")
// and PD7 (login waits for the send), §9 PA08b: with the fake SMTP delay at 400 ms the median latency
// of reset known vs unknown (n=50 each), sign-up new vs taken (n=50 each) and login disabled vs unknown
// email with a wrong password (n=50 each) differ by < 50 ms, and a login that passes the password waits
// for the send (>= 400 ms). A fourth pair (sign-up and reset with a 400 ms fake versus a 0 ms fake, n=20)
// proves neither awaits SMTP even when both arms of a pair would send. Routes: POST
// /v1/identity/password/{signup,reset,login}; the fake is the real *mail.SMTP against mailtest.
// Raw latencies are logged (t.Logf) for the evidence file; a flake is re-run by the operator and BOTH
// runs are reported, the test itself never retries. The daily mail cap is raised to 5000 so the
// deployment-wide unauthenticated share (2000) is not what is being measured.

import (
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

func pwaMedian(d []time.Duration) time.Duration {
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

func pwaMS(d []time.Duration) string {
	var b strings.Builder
	for i, v := range d {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(v.Round(time.Millisecond).String())
	}
	return b.String()
}

func (e *pwaEnv) timed(route string, ip string, body any, wantStatus int) time.Duration {
	e.t.Helper()
	start := time.Now()
	r := e.post(route, ip, body)
	d := time.Since(start)
	if r.Status != wantStatus {
		e.t.Fatalf("%s: status %d (%s), want %d", route, r.Status, r.code(), wantStatus)
	}
	return d
}

func TestPasswordPA08bTiming(t *testing.T) {
	const n = 50
	e := newPwa(t, pwaCap(5000))
	e.smtp.SetDelay(400 * time.Millisecond)
	// A consistent, warm process: one throw-away request of each kind, unmeasured.
	e.timed("reset", pwaIP().String(), map[string]string{"email": pwaEmail(), "locale": "en"}, http.StatusAccepted)

	// Accounts: five known/taken (10 requests each stays under the 30-per-address daily total) and one disabled.
	var known [5]string
	for i := range known {
		known[i] = pwaEmail()
		e.smtp.SetDelay(0)
		e.register(known[i], pwaSecret())
		e.smtp.SetDelay(400 * time.Millisecond)
	}
	disabled, dpw := pwaEmail(), pwaSecret()
	e.smtp.SetDelay(0)
	e.register(disabled, dpw)
	e.disableByFailures(disabled)
	e.smtp.SetDelay(400 * time.Millisecond)

	compare := func(name string, a, b []time.Duration) {
		t.Helper()
		ma, mb := pwaMedian(a), pwaMedian(b)
		diff := ma - mb
		if diff < 0 {
			diff = -diff
		}
		t.Logf("%s: median A=%s median B=%s |diff|=%s (limit 50ms)\n  A raw: %s\n  B raw: %s", name, ma.Round(time.Millisecond), mb.Round(time.Millisecond), diff.Round(time.Millisecond), pwaMS(a), pwaMS(b))
		if diff >= 50*time.Millisecond {
			t.Errorf("%s: medians differ by %s (>= 50 ms): response time leaks existence", name, diff)
		}
	}

	// Interleave the two arms so drift (GC, CPU frequency, background sends) hits both equally.
	var resetKnown, resetUnknown []time.Duration
	for i := 0; i < n; i++ {
		resetKnown = append(resetKnown, e.timed("reset", pwaIP().String(), map[string]string{"email": known[i%5], "locale": "en"}, 202))
		resetUnknown = append(resetUnknown, e.timed("reset", pwaIP().String(), map[string]string{"email": pwaEmail(), "locale": "en"}, 202))
	}
	compare("reset known vs unknown", resetKnown, resetUnknown)

	var signupNew, signupTaken []time.Duration
	for i := 0; i < n; i++ {
		signupNew = append(signupNew, e.timed("signup", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret()), 202))
		signupTaken = append(signupTaken, e.timed("signup", pwaIP().String(), pwaBody(known[i%5], pwaSecret()), 202))
	}
	compare("signup new vs taken", signupNew, signupTaken)

	var loginDisabled, loginUnknown []time.Duration
	for i := 0; i < n; i++ {
		loginDisabled = append(loginDisabled, e.timed("login", pwaIP().String(), pwaBody(disabled, pwaSecret()), 401))
		loginUnknown = append(loginUnknown, e.timed("login", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret()), 401))
	}
	compare("login disabled vs unknown (wrong password)", loginDisabled, loginUnknown)

	// Sign-up and reset never await SMTP: same work with a 0 ms and a 400 ms fake.
	e.settleMail()
	var reset400, reset0, signup400, signup0 []time.Duration
	for i := 0; i < 20; i++ {
		e.smtp.SetDelay(400 * time.Millisecond)
		reset400 = append(reset400, e.timed("reset", pwaIP().String(), map[string]string{"email": known[i%5], "locale": "en"}, 202))
		signup400 = append(signup400, e.timed("signup", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret()), 202))
		e.smtp.SetDelay(0)
		reset0 = append(reset0, e.timed("reset", pwaIP().String(), map[string]string{"email": known[i%5], "locale": "en"}, 202))
		signup0 = append(signup0, e.timed("signup", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret()), 202))
	}
	compare("reset: 400 ms SMTP vs 0 ms SMTP (never awaits)", reset400, reset0)
	compare("signup: 400 ms SMTP vs 0 ms SMTP (never awaits)", signup400, signup0)
	e.settleMail()

	// Login waits for the send: every password-verified login takes at least the fake's delay.
	e.smtp.SetDelay(400 * time.Millisecond)
	var login []time.Duration
	for i := 0; i < 5; i++ {
		em, pw := pwaEmail(), pwaSecret()
		e.smtp.SetDelay(0)
		e.register(em, pw)
		e.smtp.SetDelay(400 * time.Millisecond)
		login = append(login, e.timed("login", pwaIP().String(), pwaBody(em, pw), 202))
	}
	t.Logf("login with a 400 ms SMTP reply: %s", pwaMS(login))
	for i, d := range login {
		if d < 400*time.Millisecond {
			t.Errorf("login %d returned after %s, before the send finished (PD7: login waits for the send)", i+1, d)
		}
	}
	e.settleMail()
}
