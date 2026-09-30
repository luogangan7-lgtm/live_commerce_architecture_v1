package foundation_test

// PA08 TestPasswordPA08HTTP (HTTP_PG) — contracts/merchant-password-auth-v1.md §7.1 (private Go routes,
// same rules as merchant-browser-auth-v1 §private: exact BFF key, no Origin/Cookie, strict JSON <= 64 KiB,
// no query, no-store, exactly one valid X-Commerce-Client-IP else 400, code must be 6 digits), PD3/PD6/PD11
// (password policy + HIBP k-anonymity, fail-open + audit), §6 (429 / 503 identical for known and unknown),
// A10 error shapes and the §9 PA08 row's identical-response comparisons. Routes: POST
// /v1/identity/password/{signup,login,reset,complete} via identityhttp.NewPasswordHandler on a real
// listener; tables: identity.email_challenges, auth_throttle, auth_events (read-only counts).
// The response comparison rule is the contract's: status, header NAMES, JSON key set and value types
// (binding and expires_at excluded); the static `message` text is compared as well because a different
// message would be the enumeration channel PD6 forbids. Canary scan: unique passwords, addresses and
// codes must appear in no captured response body and no captured log line.
// HIBP fake: F6 (https://haveibeenpwned.com/API/v3#PwnedPasswords, retrieved 2026-09-29).

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"livecommerce/internal/identity"
)

func pwaBody(email, password string) map[string]string {
	return map[string]string{"email": email, "password": password, "locale": "en"}
}

// sideEffects is a cheap fingerprint of "identity I/O happened".
type pwaFingerprint struct{ challenges, throttle, events, mails, data int64 }

func (e *pwaEnv) fingerprint() pwaFingerprint {
	return pwaFingerprint{
		challenges: e.q1(`SELECT count(*) FROM identity.email_challenges`),
		throttle:   e.q1(`SELECT coalesce(sum(hits),0) FROM identity.auth_throttle`),
		events:     e.q1(`SELECT count(*) FROM identity.auth_events`),
		mails:      int64(len(e.smtp.Messages())),
		data:       int64(e.smtp.DataCount()),
	}
}

// strict marks "any of the strict-JSON rejections": 400 (existing handlers) or 422 (contract text).
const strict = -422

var pwaKeyTypes = map[string]string{"code": "string", "message": "string", "request_id": "string", "retryable": "bool", "details": "map[string]interface {}"}

func TestPasswordPA08HTTP(t *testing.T) {
	e := newPwa(t)
	logs := pwaCaptureLogs(t)
	var bodies strings.Builder
	var bodiesMu sync.Mutex
	record := func(r pwaResp) pwaResp {
		bodiesMu.Lock()
		bodies.Write(r.Body)
		bodiesMu.Unlock()
		return r
	}

	t.Run("transport_rules_reject_before_identity_io", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		ip := pwaIP().String()
		good := pwaBody(pwaEmail(), pwaSecret())
		for _, tc := range []struct {
			name   string
			route  string
			ip     string
			body   any
			raw    []byte
			mod    func(*http.Request)
			status int // 0 = any 4xx
			code   string
		}{
			{name: "missing BFF key", route: "signup", ip: ip, body: good, mod: func(r *http.Request) { r.Header.Del("X-Commerce-BFF-Key") }, status: 401},
			{name: "wrong BFF key", route: "signup", ip: ip, body: good, mod: func(r *http.Request) { r.Header.Set("X-Commerce-BFF-Key", strings.Repeat("A", 43)) }, status: 401},
			{name: "duplicated BFF key", route: "signup", ip: ip, body: good, mod: func(r *http.Request) { r.Header.Add("X-Commerce-BFF-Key", e.bffKey) }, status: 401},
			{name: "wrong key and missing IP: key first", route: "login", ip: "", body: good, mod: func(r *http.Request) { r.Header.Set("X-Commerce-BFF-Key", strings.Repeat("B", 43)) }, status: 401},
			{name: "Origin header", route: "signup", ip: ip, body: good, mod: func(r *http.Request) { r.Header.Set("Origin", "https://admin.example.test") }},
			{name: "Cookie header", route: "signup", ip: ip, body: good, mod: func(r *http.Request) { r.Header.Set("Cookie", "a=b") }},
			{name: "query string", route: "signup?x=1", ip: ip, body: good},
			{name: "not JSON content type", route: "signup", ip: ip, body: good, mod: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
			{name: "unknown field", route: "signup", ip: ip, raw: []byte(`{"email":"a@b.test","password":"x","locale":"en","tenant_id":"t"}`), status: strict},
			{name: "trailing JSON", route: "reset", ip: ip, raw: []byte(`{"email":"a@b.test","locale":"en"} {}`), status: strict},
			{name: "JSON null", route: "reset", ip: ip, raw: []byte(`null`), status: strict},
			{name: "over 64 KiB", route: "signup", ip: ip, raw: []byte(`{"email":"a@b.test","password":"` + strings.Repeat("x", 70*1024) + `","locale":"en"}`)},
			{name: "client IP missing", route: "signup", ip: "", body: good, status: 400, code: "invalid_client_ip"},
			{name: "client IP duplicated", route: "signup", ip: ip, body: good, mod: func(r *http.Request) { r.Header.Add("X-Commerce-Client-IP", "10.9.9.9") }, status: 400},
			{name: "client IP list", route: "signup", ip: "10.9.9.8, 10.9.9.9", body: good, status: 400},
			{name: "client IP with port", route: "signup", ip: "10.9.9.9:443", body: good, status: 400},
			{name: "client IP hostname", route: "signup", ip: "example.test", body: good, status: 400},
			{name: "client IP zone", route: "signup", ip: "fe80::1%eth0", body: good, status: 400},
			{name: "client IP empty", route: "signup", ip: " ", body: good, status: 400},
			{name: "X-Forwarded-For alone is not a client IP", route: "signup", ip: "", body: good, mod: func(r *http.Request) { r.Header.Set("X-Forwarded-For", "10.9.9.9") }, status: 400},
		} {
			before := e.fingerprint()
			var r pwaResp
			if tc.raw != nil {
				r = record(e.postRaw(tc.route, tc.ip, tc.raw, tc.mod))
			} else {
				r = record(e.post(tc.route, tc.ip, tc.body, tc.mod))
			}
			if tc.status == strict && r.Status != 400 && r.Status != 422 {
				t.Errorf("%s: status %d (%s), want 400 or 422 (merchant-browser-auth-v1 says invalid = 422; the existing identity handlers answer 400 invalid_json for the same input)", tc.name, r.Status, r.code())
			} else if tc.status != strict && (tc.status != 0 && r.Status != tc.status || tc.status == 0 && (r.Status < 400 || r.Status >= 500)) {
				t.Errorf("%s: status %d (%s), want %d", tc.name, r.Status, r.code(), tc.status)
			}
			if tc.code != "" && r.code() != tc.code {
				t.Errorf("%s: code %q, want %q", tc.name, r.code(), tc.code)
			}
			if after := e.fingerprint(); after != before {
				t.Errorf("%s: identity I/O happened for a rejected transport request (%v -> %v)", tc.name, before, after)
			}
		}
		// method and route
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/identity/password/signup", nil)
		req.Header.Set("X-Commerce-BFF-Key", e.bffKey)
		req.Header.Set("X-Commerce-Client-IP", ip)
		if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 405 {
			t.Errorf("GET signup: %v %v, want 405", err, res)
		}
		if r := e.post("nonexistent", ip, good); r.Status != 404 {
			t.Errorf("unknown route: %d, want 404", r.Status)
		}
		// valid client IPs: IPv6 and IPv4-mapped are accepted
		for _, v := range []string{"2001:db8::1", "::ffff:10.20.30.40"} {
			r := record(e.post("reset", v, map[string]string{"email": pwaEmail(), "locale": "en"}))
			if r.Status != 202 {
				t.Errorf("client IP %s: status %d (%s), want 202", v, r.Status, r.code())
			}
		}
	})

	t.Run("success_shapes_headers_and_error_envelope", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		r := record(e.post("signup", pwaIP().String(), pwaBody(email, password)))
		if r.Status != 202 || len(r.JSON) != 2 || r.JSON["binding"] == nil || r.JSON["expires_at"] == nil {
			t.Fatalf("signup 202 body must be exactly {binding, expires_at}: %d %s", r.Status, r.Body)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type %q", ct)
		}
		if cc := r.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control %q, want no-store", cc)
		}
		binding, _ := r.JSON["binding"].(string)
		code := e.code(email, 1)
		ok := record(e.post("complete", pwaIP().String(), map[string]string{"binding": binding, "purpose": "signup", "code": code}))
		if ok.Status != 200 || len(ok.JSON) != 2 || ok.JSON["token"] == nil || ok.JSON["expires_at"] == nil || !strings.Contains(ok.Header.Get("Cache-Control"), "no-store") {
			t.Errorf("complete 200 must be exactly {token, expires_at} with no-store: %d %d keys", ok.Status, len(ok.JSON))
		}
		// error envelope: code/message/request_id/retryable/details; `reason` only for password_policy
		errs := map[string]pwaResp{
			"invalid_code":        record(e.post("complete", pwaIP().String(), map[string]string{"binding": binding, "purpose": "signup", "code": "12ab56"})),
			"invalid_credentials": record(e.post("login", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret()))),
			"invalid_email":       record(e.post("signup", pwaIP().String(), pwaBody("not-an-email", pwaSecret()))),
			"password_policy":     record(e.post("signup", pwaIP().String(), pwaBody(pwaEmail(), "short"))),
		}
		wantStatus := map[string]int{"invalid_code": 401, "invalid_credentials": 401, "invalid_email": 422, "password_policy": 422}
		for name, r := range errs {
			if r.Status != wantStatus[name] || r.code() != name {
				t.Errorf("%s: status %d code %q", name, r.Status, r.code())
			}
			for k, typ := range pwaKeyTypes {
				if got := typeName(r.JSON[k]); got != typ {
					t.Errorf("%s: key %s has type %s, want %s", name, k, got, typ)
				}
			}
			extra := 0
			for k := range r.JSON {
				if _, base := pwaKeyTypes[k]; !base {
					extra++
					if !(name == "password_policy" && k == "reason") {
						t.Errorf("%s: unexpected extra key %q", name, k)
					}
				}
			}
			if name == "password_policy" && (extra != 1 || r.JSON["reason"] != "too_short") {
				t.Errorf("password_policy must carry exactly the extra key reason=too_short: %v", r.JSON)
			}
			if !strings.Contains(r.Header.Get("Cache-Control"), "no-store") {
				t.Errorf("%s: missing no-store", name)
			}
		}
		// 409 account_exists: two sources hold sign-up challenges for one email; the second complete conflicts
		dup := pwaEmail()
		c1 := e.signupFrom(pwaIP(), dup, pwaSecret())
		c2 := e.signupFrom(pwaIP(), dup, pwaSecret())
		codes := []string{e.code(dup, 1), e.code(dup, 2)}
		first := record(e.post("complete", pwaIP().String(), map[string]string{"binding": c1.Binding, "purpose": "signup", "code": codes[0]}))
		second := record(e.post("complete", pwaIP().String(), map[string]string{"binding": c2.Binding, "purpose": "signup", "code": codes[1]}))
		if first.Status != 200 || second.Status != 409 || second.code() != "account_exists" {
			t.Errorf("race complete: %d then %d %q, want 200 then 409 account_exists", first.Status, second.Status, second.code())
		}
	})

	t.Run("identical_responses_new_vs_taken_known_vs_unknown_disabled_vs_unknown", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		same := func(what string, a, b pwaResp) {
			t.Helper()
			if a.shape() != b.shape() {
				t.Errorf("%s: shapes differ\n  %s\n  %s", what, a.shape(), b.shape())
			}
			if am, bm := a.JSON["message"], b.JSON["message"]; am != bm {
				t.Errorf("%s: message differs (%v vs %v): an enumeration channel", what, am, bm)
			}
			if a.code() != b.code() {
				t.Errorf("%s: code %q vs %q", what, a.code(), b.code())
			}
		}
		taken, pw := pwaEmail(), pwaSecret()
		e.register(taken, pw)
		rNew := record(e.post("signup", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret())))
		rTaken := record(e.post("signup", pwaIP().String(), pwaBody(taken, pwaSecret())))
		if rNew.Status != 202 || rTaken.Status != 202 {
			t.Fatalf("sign-up new/taken: %d %d, want 202 both", rNew.Status, rTaken.Status)
		}
		same("signup new vs taken", rNew, rTaken)
		if len(rNew.JSON["binding"].(string)) != len(rTaken.JSON["binding"].(string)) {
			t.Error("binding shapes differ")
		}
		rKnown := record(e.post("reset", pwaIP().String(), map[string]string{"email": taken, "locale": "en"}))
		rUnknown := record(e.post("reset", pwaIP().String(), map[string]string{"email": pwaEmail(), "locale": "en"}))
		if rKnown.Status != 202 || rUnknown.Status != 202 {
			t.Fatalf("reset known/unknown: %d %d, want 202 both", rKnown.Status, rUnknown.Status)
		}
		same("reset known vs unknown", rKnown, rUnknown)
		// expires_at is excluded from shape() but is itself an enumeration channel: a fabricated value with
		// nanoseconds next to a microsecond DB value is told apart by length alone. Whole UTC seconds, within 2 s.
		expiryRe := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)
		expiry := func(what string, a, b pwaResp) {
			t.Helper()
			as, _ := a.JSON["expires_at"].(string)
			bs, _ := b.JSON["expires_at"].(string)
			if !expiryRe.MatchString(as) || !expiryRe.MatchString(bs) {
				t.Errorf("%s: expires_at %q / %q must both be whole-second UTC (PD6)", what, as, bs)
				return
			}
			ta, _ := time.Parse(time.RFC3339, as)
			tb, _ := time.Parse(time.RFC3339, bs)
			if d := ta.Sub(tb); d > 2*time.Second || d < -2*time.Second {
				t.Errorf("%s: expires_at differs by %v", what, d)
			}
		}
		expiry("signup new vs taken", rNew, rTaken)
		expiry("reset known vs unknown", rKnown, rUnknown)

		disabled, dpw := pwaEmail(), pwaSecret()
		e.register(disabled, dpw)
		e.disableByFailures(disabled)
		lUnknown := record(e.post("login", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret())))
		lWrong := record(e.post("login", pwaIP().String(), pwaBody(taken, pwaSecret())))
		lDisabled := record(e.post("login", pwaIP().String(), pwaBody(disabled, pwaSecret())))
		lDisabledRight := record(e.post("login", pwaIP().String(), pwaBody(disabled, dpw)))
		for name, r := range map[string]pwaResp{"unknown": lUnknown, "wrong": lWrong, "disabled": lDisabled, "disabled+right": lDisabledRight} {
			if r.Status != 401 || r.code() != "invalid_credentials" {
				t.Errorf("login %s: %d %q", name, r.Status, r.code())
			}
		}
		same("login unknown vs wrong password", lUnknown, lWrong)
		same("login unknown vs disabled", lUnknown, lDisabled)
		same("login unknown vs disabled with the right password", lUnknown, lDisabledRight)

		// after 6 wrong codes reset `complete` answers identically for known and unknown emails
		codeFor := func(email string, known bool) []pwaResp {
			r := record(e.post("reset", pwaIP().String(), map[string]string{"email": email, "locale": "en"}))
			binding, _ := r.JSON["binding"].(string)
			var out []pwaResp
			for i := 0; i < 6; i++ {
				out = append(out, record(e.post("complete", pwaIP().String(), map[string]string{"binding": binding, "purpose": "reset", "code": pwaWrongCodeN(i), "new_password": pwaSecret()})))
			}
			return out
		}
		kn, un := codeFor(taken, true), codeFor(pwaEmail(), false)
		for i := range kn {
			if kn[i].Status != 401 || kn[i].code() != "invalid_code" {
				t.Errorf("known wrong code %d: %d %q", i+1, kn[i].Status, kn[i].code())
			}
			same("reset complete wrong code "+string(rune('1'+i)), kn[i], un[i])
		}
		// 429 identical for known and unknown (same source, immediate repeat)
		src := pwaIP().String()
		_ = record(e.post("reset", src, map[string]string{"email": taken, "locale": "en"}))
		k429 := record(e.post("reset", src, map[string]string{"email": taken, "locale": "en"}))
		src2 := pwaIP().String()
		u := pwaEmail()
		_ = record(e.post("reset", src2, map[string]string{"email": u, "locale": "en"}))
		u429 := record(e.post("reset", src2, map[string]string{"email": u, "locale": "en"}))
		if k429.Status != 429 || u429.Status != 429 {
			t.Fatalf("expected 429 twice, got %d %d", k429.Status, u429.Status)
		}
		same("429 known vs unknown", k429, u429)
		if k429.Header.Get("Retry-After") == "" || u429.Header.Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	})

	t.Run("global_cap_503_identical_for_known_and_unknown_and_signup", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		g := newPwa(t, pwaCap(20))
		known := pwaEmail()
		g.register(known, pwaSecret())
		for i := 1; i < 8; i++ { // 7 more unauth mails: the share is 8
			if _, err := g.pw.Reset(pwaBG, pwaIP(), pwaEmail(), "en"); err != nil {
				t.Fatalf("mail %d: %v", i, err)
			}
		}
		rk := record(g.post("reset", pwaIP().String(), map[string]string{"email": known, "locale": "en"}))
		ru := record(g.post("reset", pwaIP().String(), map[string]string{"email": pwaEmail(), "locale": "en"}))
		sn := record(g.post("signup", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret())))
		st := record(g.post("signup", pwaIP().String(), pwaBody(known, pwaSecret())))
		for name, r := range map[string]pwaResp{"reset known": rk, "reset unknown": ru, "signup new": sn, "signup taken": st} {
			if r.Status != 503 || r.code() != "mail_unavailable" {
				t.Errorf("%s over the global share: %d %q, want 503 mail_unavailable", name, r.Status, r.code())
			}
		}
		if rk.shape() != ru.shape() || sn.shape() != st.shape() || rk.shape() != sn.shape() {
			t.Errorf("global-cap 503 shapes differ: %s | %s | %s | %s", rk.shape(), ru.shape(), sn.shape(), st.shape())
		}
	})

	t.Run("password_policy_and_hibp", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		submit := func(pw string) pwaResp {
			return record(e.post("signup", pwaIP().String(), pwaBody(pwaEmail(), pw)))
		}
		expectReason := func(what, pw, reason string) {
			t.Helper()
			r := submit(pw)
			if reason == "" {
				if r.Status != 202 {
					t.Errorf("%s: status %d (%s/%v), want 202", what, r.Status, r.code(), r.JSON["reason"])
				}
				return
			}
			if r.Status != 422 || r.code() != "password_policy" || r.JSON["reason"] != reason {
				t.Errorf("%s: status %d code %q reason %v, want 422 password_policy %s", what, r.Status, r.code(), r.JSON["reason"], reason)
			}
		}
		expectReason("11 code points", pwaLetters(11), "too_short")
		expectReason("12 code points", pwaLetters(12), "")
		expectReason("128 code points", pwaLetters(128), "")
		expectReason("129 code points", pwaLetters(129), "too_long")
		expectReason("12 emoji count as 12 code points", strings.Repeat("🙂", 12), "")
		expectReason("11 emoji count as 11 code points", strings.Repeat("🙂", 11), "too_short")
		// NFC first: 11 NFC letters written decomposed (22 code points raw) are still too short
		expectReason("11 decomposed letters are 11 NFC code points", strings.Repeat("é", 11), "too_short")
		// equals email at sign-up
		em := "pwa." + pwaLetters(14) + "@example.test"
		if r := record(e.post("signup", pwaIP().String(), pwaBody(em, em))); r.Status != 422 || r.JSON["reason"] != "equals_email" {
			t.Errorf("password equal to the email: %d %v, want 422 equals_email", r.Status, r.JSON["reason"])
		}
		// NFC: a decomposed sign-up password logs in with the composed form
		nfd := "Pwa-café-" + pwaLetters(14)
		nfc := "Pwa-café-" + pwaLetters(0) + nfd[len("Pwa-café-"):]
		email := pwaEmail()
		s := e.register(email, nfd)
		_ = s
		if _, err := e.pw.Login(pwaBG, pwaIP(), email, nfc, "en"); err != nil {
			t.Errorf("login with the NFC form of an NFD sign-up password: %v", err)
		}
		// HIBP: breached => 422 breached; only a 5-hex upper-case prefix is sent, with Add-Padding
		breached := pwaSecret()
		e.hibp.breach(breached, 42)
		reqBefore := e.hibp.requests.Load()
		r := submit(breached)
		if r.Status != 422 || r.JSON["reason"] != "breached" {
			t.Errorf("breached password: %d %v", r.Status, r.JSON["reason"])
		}
		sum := sha1.Sum([]byte(breached))
		wantPath := "/range/" + strings.ToUpper(hex.EncodeToString(sum[:]))[:5]
		if got, _ := e.hibp.last.Load().(string); got != wantPath {
			t.Errorf("HIBP request path %q, want the 5-hex SHA-1 prefix %q only (k-anonymity)", got, wantPath)
		}
		if e.hibp.requests.Load() == reqBefore || e.hibp.padding.Load() != e.hibp.requests.Load() {
			t.Errorf("HIBP requests %d, with Add-Padding %d: every request must carry Add-Padding: true", e.hibp.requests.Load(), e.hibp.padding.Load())
		}
		// fail-open: 500 and a 2.5 s stall both accept and audit password.breach_check_unavailable
		// Contract PD11 says "audit password.breach_check_unavailable", but §4.2 has no definer that can write
		// identity.auth_events for it; auth-core documents that it logs the action name instead. Either
		// evidence is accepted (a table row, or the log line naming the action); see the return message.
		audits := func() int64 {
			return e.q1(`SELECT count(*) FROM identity.auth_events WHERE action='password.breach_check_unavailable'`) + int64(strings.Count(logs.String(), "password.breach_check_unavailable"))
		}
		a0 := audits()
		e.hibp.setStatus(500)
		if r := submit(breached); r.Status != 202 {
			t.Errorf("HIBP 500: status %d, want fail-open 202", r.Status)
		}
		if audits() <= a0 {
			t.Error("no password.breach_check_unavailable audit event after an HIBP 500")
		}
		e.hibp.setStatus(0)
		e.hibp.setDelay(2500 * time.Millisecond)
		a1 := audits()
		start := time.Now()
		if r := submit(breached); r.Status != 202 {
			t.Errorf("HIBP stall: status %d, want fail-open 202", r.Status)
		}
		if d := time.Since(start); d < 1900*time.Millisecond || d > 6*time.Second {
			t.Errorf("stalled HIBP took %s, want the 2 s client timeout to bound it", d)
		}
		if audits() <= a1 {
			t.Error("no password.breach_check_unavailable audit event after an HIBP timeout")
		}
		e.hibp.setDelay(0)
	})

	t.Run("rate_limits_run_before_hashing_hibp_mail_and_lookup", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		src := pwaIP()
		for i := 0; i < 5; i++ {
			e.signupFrom(src, pwaEmail(), pwaSecret())
		}
		e.settleMail()
		argon, hibp, mails, data := identity.ArgonCount(), e.hibp.requests.Load(), len(e.smtp.Messages()), e.smtp.DataCount()
		r := record(e.post("signup", src.String(), pwaBody(pwaEmail(), pwaSecret())))
		if r.Status != 429 {
			t.Fatalf("6th sign-up: %d, want 429", r.Status)
		}
		time.Sleep(200 * time.Millisecond)
		if identity.ArgonCount() != argon || e.hibp.requests.Load() != hibp || len(e.smtp.Messages()) != mails || e.smtp.DataCount() != data {
			t.Errorf("a throttled sign-up still hashed (%d->%d), called HIBP (%d->%d) or mailed", argon, identity.ArgonCount(), hibp, e.hibp.requests.Load())
		}
		// reset complete: a code that is not exactly 6 ASCII digits is 401 before any other work
		binding := identityRandomBinding()
		argon, hibp = identity.ArgonCount(), e.hibp.requests.Load()
		for _, code := range []string{"12345", "1234567", "12345a", "١٢٣٤٥٦", " 12345", "123 456", ""} {
			r := record(e.post("complete", pwaIP().String(), map[string]string{"binding": binding, "purpose": "reset", "code": code, "new_password": pwaSecret()}))
			if r.Status != 401 || r.code() != "invalid_code" {
				t.Errorf("code %q: %d %q, want 401 invalid_code", code, r.Status, r.code())
			}
		}
		if identity.ArgonCount() != argon || e.hibp.requests.Load() != hibp {
			t.Error("a malformed code reached HIBP or Argon2")
		}
		// binding throttle: the 11th complete on one binding is 429 before HIBP/Argon
		for i := 0; i < 10; i++ {
			_ = record(e.post("complete", pwaIP().String(), map[string]string{"binding": binding, "purpose": "reset", "code": "000000", "new_password": pwaSecret()}))
		}
		argon, hibp = identity.ArgonCount(), e.hibp.requests.Load()
		r = record(e.post("complete", pwaIP().String(), map[string]string{"binding": binding, "purpose": "reset", "code": "000000", "new_password": pwaSecret()}))
		if r.Status != 429 || identity.ArgonCount() != argon || e.hibp.requests.Load() != hibp {
			t.Errorf("11th complete on a binding: %d, Argon %d->%d HIBP %d->%d; want 429 with no hashing/HIBP", r.Status, argon, identity.ArgonCount(), hibp, e.hibp.requests.Load())
		}
	})

	t.Run("busy_503_when_the_hash_semaphore_is_held", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		// PD3: 4 slots shared by Argon2 and HIBP; a caller that waits > 2 s gets 503 busy. Twelve
		// sign-ups whose HIBP call stalls hold the slots for the 2 s HIBP timeout; the queue behind them
		// must surface at least one 503 busy, and every busy answer is the A10 shape.
		e.hibp.setDelay(2500 * time.Millisecond)
		defer e.hibp.setDelay(0)
		var wg sync.WaitGroup
		res := make(chan pwaResp, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res <- e.post("signup", pwaIP().String(), pwaBody(pwaEmail(), pwaSecret()))
			}()
		}
		wg.Wait()
		close(res)
		busy, ok := 0, 0
		for r := range res {
			record(r)
			switch {
			case r.Status == 503 && r.code() == "busy":
				busy++
				if r.JSON["retryable"] != true {
					t.Error("busy must be retryable")
				}
			case r.Status == 202:
				ok++
			default:
				t.Errorf("unexpected answer %d %q", r.Status, r.code())
			}
		}
		if busy < 1 {
			t.Errorf("no 503 busy among 12 concurrent hash-heavy requests (ok=%d)", ok)
		}
	})

	t.Run("canary_scan_of_bodies_and_logs", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		// A dedicated sequence with unique sentinels: sign-up, wrong login, policy error, throttle, reset.
		email, password := pwaEmail(), pwaSecret()
		wrongPw := pwaSecret()
		r1 := record(e.post("signup", pwaIP().String(), pwaBody(email, password)))
		code := e.code(email, 1)
		binding := r1.JSON["binding"].(string)
		r2 := record(e.post("complete", pwaIP().String(), map[string]string{"binding": binding, "purpose": "signup", "code": code}))
		tokenSeen, _ := r2.JSON["token"].(string)
		record(e.post("login", pwaIP().String(), pwaBody(email, wrongPw)))
		record(e.post("signup", pwaIP().String(), pwaBody(pwaEmail(), "short")))
		src := pwaIP().String()
		record(e.post("reset", src, map[string]string{"email": email, "locale": "en"}))
		record(e.post("reset", src, map[string]string{"email": email, "locale": "en"}))
		needles := map[string]string{"password": password, "wrong password": wrongPw, "smtp secret": e.smtp.Password, "bff key": e.bffKey}
		bodiesMu.Lock()
		all := bodies.String()
		bodiesMu.Unlock()
		pwaNoCanary(t, "response bodies", all, needles)
		if strings.Contains(all, email) {
			t.Error("a response body echoes a submitted email address")
		}
		if strings.Contains(all, "\""+code+"\"") || strings.Contains(all, code+"\n") {
			t.Error("a response carries the emailed code")
		}
		lg := logs.String()
		pwaNoCanary(t, "captured logs", lg, needles)
		for label, secret := range map[string]string{"email": email, "code": "code=" + code, "session token": tokenSeen, "binding": binding} {
			if len(secret) >= 6 && strings.Contains(lg, secret) {
				t.Errorf("captured logs contain the %s canary", label)
			}
		}
		if regexp.MustCompile(`\b` + code + `\b`).MatchString(lg) {
			t.Error("captured logs contain the emailed code")
		}
		e.canaryScanDB(map[string]string{"password": password, "wrong password": wrongPw})
	})
}

// pwaWrongCodeN yields six distinct wrong codes.
func pwaWrongCodeN(i int) string { return strings.Repeat(string(rune('0'+i)), 6) }

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "nil"
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		return "float64"
	case map[string]any:
		return "map[string]interface {}"
	default:
		return "other"
	}
}
