package identityhttp

// Transport tests for /v1/identity/password/* with a fake service (no database, no mail). They pin the
// A10 error mapping, the rejection order (BFF key, then browser headers, query, client ip, body) and that
// rejected requests never reach the service. Names avoid the auth-tests gate prefixes (TestPasswordPA*).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/identity"
)

type fakePasswords struct {
	calls                 int
	err                   error
	ip                    netip.Addr
	email, password, loc  string
	binding, purpose, cod string
	newPassword           string
}

func (f *fakePasswords) challenge(ip netip.Addr, email, password, locale string) (identity.Challenge, error) {
	f.calls++
	f.ip, f.email, f.password, f.loc = ip, email, password, locale
	return identity.Challenge{Binding: testSecret, ExpiresAt: time.Now().Add(10 * time.Minute)}, f.err
}
func (f *fakePasswords) Signup(_ context.Context, ip netip.Addr, email, password, locale string) (identity.Challenge, error) {
	return f.challenge(ip, email, password, locale)
}
func (f *fakePasswords) Login(_ context.Context, ip netip.Addr, email, password, locale string) (identity.Challenge, error) {
	return f.challenge(ip, email, password, locale)
}
func (f *fakePasswords) Reset(_ context.Context, ip netip.Addr, email, locale string) (identity.Challenge, error) {
	return f.challenge(ip, email, "", locale)
}
func (f *fakePasswords) Complete(_ context.Context, ip netip.Addr, binding, purpose, code, newPassword string) (identity.Session, error) {
	f.calls++
	f.ip, f.binding, f.purpose, f.cod, f.newPassword = ip, binding, purpose, code, newPassword
	return identity.Session{Token: testSecret, ExpiresAt: time.Now().Add(time.Hour)}, f.err
}

func pwRequest(t *testing.T, f *fakePasswords, path, bodyText string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	h, err := NewPasswordHandler(f, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(bodyText))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Commerce-BFF-Key", testSecret)
	r.Header.Set("X-Commerce-Client-IP", "203.0.113.7")
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const (
	signupBody   = `{"email":"a@example.test","password":"correct horse battery","locale":"en"}`
	completeBody = `{"binding":"` + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + `","purpose":"login","code":"123456"}`
)

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

func TestPasswordHTTPSuccessShapes(t *testing.T) {
	f := &fakePasswords{}
	for path, body := range map[string]string{
		"/v1/identity/password/signup": signupBody,
		"/v1/identity/password/login":  signupBody,
		"/v1/identity/password/reset":  `{"email":"a@example.test","locale":"zh-TW"}`,
	} {
		w := pwRequest(t, f, path, body, nil)
		m := decode(t, w)
		if w.Code != http.StatusAccepted || m["binding"] != testSecret || m["expires_at"] == nil || len(m) != 2 {
			t.Fatalf("%s = %d %v", path, w.Code, m)
		}
		if w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s headers %v", path, w.Header())
		}
	}
	if f.ip != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("client ip = %v", f.ip)
	}
	w := pwRequest(t, f, "/v1/identity/password/complete", completeBody, nil)
	m := decode(t, w)
	if w.Code != http.StatusOK || m["token"] != testSecret || m["expires_at"] == nil || len(m) != 2 || f.purpose != "login" || f.cod != "123456" {
		t.Fatalf("complete = %d %v purpose=%q", w.Code, m, f.purpose)
	}
	w = pwRequest(t, f, "/v1/identity/password/complete", `{"binding":"b","purpose":"reset","code":"000001","new_password":"a brand new password"}`, nil)
	if w.Code != http.StatusOK || f.newPassword != "a brand new password" {
		t.Fatalf("reset complete = %d newPassword=%q", w.Code, f.newPassword)
	}
}

func TestPasswordHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		reason string
	}{
		{identity.PolicyError{Reason: "too_short"}, 422, "password_policy", "too_short"},
		{identity.PolicyError{Reason: "breached"}, 422, "password_policy", "breached"},
		{identity.ErrInvalidEmail, 422, "invalid_email", ""},
		{identity.ErrInvalidCredentials, 401, "invalid_credentials", ""},
		{identity.ErrInvalidCode, 401, "invalid_code", ""},
		{identity.ErrAccountExists, 409, "account_exists", ""},
		{identity.ThrottleError{RetryAfter: 37 * time.Second}, 429, "throttled", ""},
		{identity.ErrBusy, 503, "busy", ""},
		{identity.ErrMailUnavailable, 503, "mail_unavailable", ""},
		{identity.ErrInvalid, 422, "invalid_request", ""},
		{errors.New("database exploded with secret detail"), 503, "unavailable", ""},
	}
	for _, c := range cases {
		f := &fakePasswords{err: c.err}
		w := pwRequest(t, f, "/v1/identity/password/login", signupBody, nil)
		m := decode(t, w)
		if w.Code != c.status || m["code"] != c.code {
			t.Fatalf("%v -> %d %v, want %d %s", c.err, w.Code, m, c.status, c.code)
		}
		if got, _ := m["reason"].(string); got != c.reason {
			t.Fatalf("%v reason = %q, want %q", c.err, got, c.reason)
		}
		if c.reason != "" {
			if d, _ := m["details"].(map[string]any); d["reason"] != c.reason {
				t.Fatalf("details.reason = %v", m["details"])
			}
		}
		if strings.Contains(w.Body.String(), "secret detail") {
			t.Fatal("internal error text leaked")
		}
		if c.status == 429 && w.Header().Get("Retry-After") != "37" {
			t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
		}
		if c.status != 429 && w.Header().Get("Retry-After") != "" {
			t.Fatal("Retry-After on a non-429")
		}
		if m["request_id"] == "" || m["request_id"] == nil {
			t.Fatal("missing request id")
		}
	}
	// A 429 always tells the caller to wait at least one second.
	w := pwRequest(t, &fakePasswords{err: identity.ThrottleError{RetryAfter: time.Millisecond}}, "/v1/identity/password/reset", `{"email":"a@example.test","locale":"en"}`, nil)
	if w.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After floor = %q", w.Header().Get("Retry-After"))
	}
}

func TestPasswordHTTPRejectionsNeverReachTheService(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		body   string
		mutate func(*http.Request)
		status int
		code   string
	}{
		{"missing bff key", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Del("X-Commerce-BFF-Key") }, 401, "unauthorized"},
		{"wrong bff key", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Set("X-Commerce-BFF-Key", strings.Repeat("B", 43)) }, 401, "unauthorized"},
		{"duplicate bff key", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Add("X-Commerce-BFF-Key", testSecret) }, 401, "unauthorized"},
		{"bad key and bad ip: key first", "/v1/identity/password/login", signupBody, func(r *http.Request) {
			r.Header.Set("X-Commerce-BFF-Key", "wrong")
			r.Header.Del("X-Commerce-Client-IP")
		}, 401, "unauthorized"},
		{"origin header", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403, "forbidden"},
		{"cookie header", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Set("Cookie", "a=b") }, 403, "forbidden"},
		{"query string", "/v1/identity/password/login?x=1", signupBody, nil, 422, "invalid_request"},
		{"missing client ip", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Del("X-Commerce-Client-IP") }, 400, "invalid_client_ip"},
		{"duplicate client ip", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Add("X-Commerce-Client-IP", "203.0.113.8") }, 400, "invalid_client_ip"},
		{"invalid client ip", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Set("X-Commerce-Client-IP", "not-an-ip") }, 400, "invalid_client_ip"},
		{"zoned client ip", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Set("X-Commerce-Client-IP", "fe80::1%eth0") }, 400, "invalid_client_ip"},
		{"ip with port", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Set("X-Commerce-Client-IP", "203.0.113.7:443") }, 400, "invalid_client_ip"},
		{"not json", "/v1/identity/password/login", signupBody, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, "json_required"},
		{"unknown field", "/v1/identity/password/login", `{"email":"a@b.co","password":"x","locale":"en","tenant_id":"t"}`, nil, 400, "invalid_json"},
		{"trailing garbage", "/v1/identity/password/login", signupBody + `{}`, nil, 400, "invalid_json"},
		{"body too large", "/v1/identity/password/signup", `{"email":"` + strings.Repeat("a", 70000) + `"}`, nil, 400, "invalid_json"},
		{"complete: short code", "/v1/identity/password/complete", strings.Replace(completeBody, "123456", "12345", 1), nil, 401, "invalid_code"},
		{"complete: long code", "/v1/identity/password/complete", strings.Replace(completeBody, "123456", "1234567", 1), nil, 401, "invalid_code"},
		{"complete: letters", "/v1/identity/password/complete", strings.Replace(completeBody, "123456", "12345a", 1), nil, 401, "invalid_code"},
		{"complete: unicode digits", "/v1/identity/password/complete", strings.Replace(completeBody, "123456", "１２３４５６", 1), nil, 401, "invalid_code"},
		{"complete: empty code", "/v1/identity/password/complete", strings.Replace(completeBody, "123456", "", 1), nil, 401, "invalid_code"},
	}
	for _, c := range cases {
		f := &fakePasswords{}
		w := pwRequest(t, f, c.path, c.body, c.mutate)
		m := decode(t, w)
		if w.Code != c.status || m["code"] != c.code || f.calls != 0 {
			t.Fatalf("%s: %d %v calls=%d, want %d %s and zero service calls", c.name, w.Code, m, f.calls, c.status, c.code)
		}
	}
	// Only POST exists.
	h, _ := NewPasswordHandler(&fakePasswords{}, testSecret)
	r := httptest.NewRequest(http.MethodGet, "/v1/identity/password/login", nil)
	r.Header.Set("X-Commerce-BFF-Key", testSecret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", w.Code)
	}
}

func TestPasswordHTTPClientIPParsing(t *testing.T) {
	for in, want := range map[string]string{"203.0.113.7": "203.0.113.7", "2001:db8::1": "2001:db8::1", "::ffff:203.0.113.7": "203.0.113.7"} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Commerce-Client-IP", in)
		if got, err := ClientIP(r); err != nil || got.String() != want {
			t.Fatalf("ClientIP(%q) = %v %v, want %s", in, got, err, want)
		}
	}
	if _, err := NewPasswordHandler(nil, testSecret); err == nil {
		t.Fatal("nil service accepted")
	}
	if _, err := NewPasswordHandler(&fakePasswords{}, "short"); err == nil {
		t.Fatal("weak bff key accepted")
	}
}
