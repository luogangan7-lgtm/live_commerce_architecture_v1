package foundation_test

// Shared harness for the merchant-password-auth gates PA03-PA09 (contracts/merchant-password-auth-v1.md
// §9). It is NOT a gate: it only builds the service under test the way cmd/api does (the fixture's
// commerce_identity login through platform.OpenIdentityPool, identity.NewPasswords, the real
// *mail.SMTP adapter against the loopback mailtest server, identityhttp.NewPasswordHandler) and gives
// black-box helpers. Helpers are prefixed pwa. Tiers: REAL_PG / HTTP_PG / MOCK. Written from the
// contract and the FROZEN briefs (auth-mail M1-M7, auth-core A1-A12), not from the implementation.
//
// Rules kept here (auth-tests brief "Tier rules"):
//   - every test gets a fresh 32-byte pepper, so every throttle bucket (HMAC(pepper, ...)) is private
//     to the test even though the fixture database is shared;
//   - emails are letters only (no digit runs), so a 6-digit code in a mail body is unambiguous;
//   - codes are read from the mailtest mailbox, never from SQL; passwords are random per test;
//   - aged timestamps / fault triggers go through the owner pool only and each use says why.
//
// HIBP fake (F6): https://haveibeenpwned.com/API/v3#PwnedPasswords, retrieved 2026-09-29 (contract §1):
// GET /range/{first 5 hex of SHA-1}, body lines SUFFIX:COUNT, Add-Padding: true pads with count-0 rows.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/mail"
	"livecommerce/internal/mail/mailtest"
)

// pwaHIBP is the loopback Pwned Passwords range API.
type pwaHIBP struct {
	srv      *httptest.Server
	mu       sync.Mutex
	breached map[string]int // SHA-1 hex (upper) -> count
	delay    time.Duration
	status   int // 0 = normal, else forced status (500 mode)
	requests atomic.Int64
	padding  atomic.Int64 // requests that carried Add-Padding: true
	last     atomic.Value // last request path
}

func newPwaHIBP(t *testing.T) *pwaHIBP {
	h := &pwaHIBP{breached: map[string]int{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.last.Store(r.URL.Path)
		if r.Header.Get("Add-Padding") == "true" {
			h.padding.Add(1)
		}
		h.mu.Lock()
		delay, status := h.delay, h.status
		h.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 {
			http.Error(w, "forced", status)
			return
		}
		prefix := strings.TrimPrefix(r.URL.Path, "/range/")
		if r.Method != http.MethodGet || len(prefix) != 5 || prefix != strings.ToUpper(prefix) {
			http.Error(w, "bad prefix", http.StatusBadRequest)
			return
		}
		var b strings.Builder
		h.mu.Lock()
		for sum, n := range h.breached {
			if strings.HasPrefix(sum, prefix) {
				fmt.Fprintf(&b, "%s:%d\r\n", sum[5:], n)
			}
		}
		h.mu.Unlock()
		for i := 0; i < 800; i++ { // padding rows carry a zero count (F6)
			pad := make([]byte, 18)
			_, _ = rand.Read(pad)
			fmt.Fprintf(&b, "%s:0\r\n", strings.ToUpper(hex.EncodeToString(pad))[:35])
		}
		_, _ = io.WriteString(w, b.String())
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *pwaHIBP) breach(password string, count int) {
	sum := sha1.Sum([]byte(password))
	h.mu.Lock()
	h.breached[strings.ToUpper(hex.EncodeToString(sum[:]))] = count
	h.mu.Unlock()
}
func (h *pwaHIBP) setDelay(d time.Duration) { h.mu.Lock(); h.delay = d; h.mu.Unlock() }
func (h *pwaHIBP) setStatus(s int)          { h.mu.Lock(); h.status = s; h.mu.Unlock() }

// pwaEnv is one wired service instance with a private pepper and mailbox.
type pwaEnv struct {
	t       *testing.T
	f       *testFixture
	oidc    *identity.Service // existing OIDC service on the same identity login: used for /initial-store and logout
	pool    *pgxpool.Pool     // the commerce_identity login pool
	pepper  []byte
	smtp    *mailtest.Server
	from    string
	pw      *identity.Passwords
	hibp    *pwaHIBP
	bffKey  string
	srv     *httptest.Server // identityhttp.NewPasswordHandler on a real listener
	handler http.Handler
	policy  identity.PasswordPolicy
	mailer  *mail.SMTP
	started time.Time // challenges older than this belong to other tests sharing the fixture database
}

type pwaOpt func(*identity.PasswordPolicy)

func pwaCap(n int) pwaOpt { return func(p *identity.PasswordPolicy) { p.MailDailyCap = n } }

func newPwa(t *testing.T, opts ...pwaOpt) *pwaEnv {
	t.Helper()
	f := fixture(t)
	oidc, _, pool := identityFixture(t)
	smtp := mailtest.New(t)
	from := "xgdwm <" + smtp.Username + ">"
	mailer, err := mail.NewSMTP(smtp.Config(from))
	if err != nil {
		t.Fatalf("real SMTP adapter against mailtest: %v", err)
	}
	e := &pwaEnv{t: t, f: f, oidc: oidc, pool: pool, pepper: randomBytes(32), smtp: smtp, from: from, hibp: newPwaHIBP(t), bffKey: base64.RawURLEncoding.EncodeToString(randomBytes(32))}
	// started comes from the database clock that stamps email_challenges.created_at, with no slack: a
	// Go-clock start minus 1 s swept in the PENDING login challenges PA05 leaves on purpose (it calls
	// start_login_challenge directly, no mail) whenever PA08 ran right after it in one go test process.
	if err := f.owner.QueryRow(pwaBG, `SELECT clock_timestamp()`).Scan(&e.started); err != nil {
		t.Fatalf("database clock: %v", err)
	}
	policy := identity.PasswordPolicy{Pepper: e.pepper, SessionTTL: time.Hour, BreachCheck: "hibp", HIBPBaseURL: e.hibp.srv.URL, AllowLoopback: true}
	for _, o := range opts {
		o(&policy)
	}
	e.policy, e.mailer = policy, mailer
	e.pw, err = identity.NewPasswords(pool, mailer, policy)
	if err != nil {
		t.Fatalf("NewPasswords: %v", err)
	}
	// Runs before the pool closes (identityFixture registered pool.Close earlier): in-flight
	// background sends must finish before the connection goes away (A12).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.pw.Close(ctx)
	})
	e.handler, err = identityhttp.NewPasswordHandler(e.pw, e.bffKey)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.handler)
	t.Cleanup(e.srv.Close)
	return e
}

// --- identifiers ----------------------------------------------------------------------------

var pwaIPCounter atomic.Uint32

// pwaIP returns a fresh IPv4 /32 (10.x.y.z) so per-source buckets never collide inside a test.
func pwaIP() netip.Addr {
	n := pwaIPCounter.Add(1) + 1<<16
	return netip.AddrFrom4([4]byte{10, byte(n >> 16), byte(n >> 8), byte(n)})
}

func pwaLetters(n int) string {
	b := randomBytes(n)
	out := make([]byte, n)
	for i := range b {
		out[i] = 'a' + b[i]%26
	}
	return string(out)
}

// pwaEmail: letters only, so no digit run can look like a code.
func pwaEmail() string { return "pwa." + pwaLetters(14) + "@example.test" }

// pwaSecret is a random test password (not a real credential; random per call, 26 code points).
func pwaSecret() string { return "Pwa-" + pwaLetters(22) }

// --- mailbox ----------------------------------------------------------------------------------

var pwaCodeRE = regexp.MustCompile(`(?:^|[^0-9A-Za-z])([0-9]{6})(?:[^0-9A-Za-z]|$)`)

func (e *pwaEnv) mailsTo(to string) []mailtest.Received {
	var out []mailtest.Received
	for _, m := range e.smtp.Messages() {
		if strings.EqualFold(m.To, to) {
			out = append(out, m)
		}
	}
	return out
}

// awaitMails waits for at least n messages to `to` (background sends are detached) and returns them.
func (e *pwaEnv) awaitMails(to string, n int) []mailtest.Received {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := e.mailsTo(to); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("mailbox: %d message(s) to the test address after 10 s, wanted %d", len(e.mailsTo(to)), n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// code returns the 6-digit code of the nth (1-based) mail to `to`.
func (e *pwaEnv) code(to string, n int) string {
	e.t.Helper()
	m := e.awaitMails(to, n)[n-1]
	c := pwaCodeRE.FindStringSubmatch(m.Text)
	if c == nil {
		e.t.Fatalf("no 6-digit code in mail %d (text length %d)", n, len(m.Text))
	}
	return c[1]
}

// wrongCode returns a 6-digit code different from `not`.
func pwaWrongCode(not string) string {
	if not == "000000" {
		return "000001"
	}
	return "000000"
}

// --- flows ------------------------------------------------------------------------------------

var pwaBG = context.Background()

// signupFrom runs step 1 from ip and returns the challenge.
func (e *pwaEnv) signupFrom(ip netip.Addr, email, password string) identity.Challenge {
	e.t.Helper()
	c, err := e.pw.Signup(pwaBG, ip, email, password, "en")
	if err != nil {
		e.t.Fatalf("Signup: %v", err)
	}
	return c
}

// register creates a password principal end to end (sign-up + code) and returns its session.
func (e *pwaEnv) register(email, password string) identity.Session {
	e.t.Helper()
	ip := pwaIP()
	c := e.signupFrom(ip, email, password)
	s, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", e.code(email, 1), "")
	if err != nil {
		e.t.Fatalf("Complete signup: %v", err)
	}
	return s
}

// loginSession runs the full login (password + emailed code); `n` is the 1-based index of the login
// mail among all mails to the address.
func (e *pwaEnv) loginSession(email, password string, mailIndex int) identity.Session {
	e.t.Helper()
	ip := pwaIP()
	c, err := e.pw.Login(pwaBG, ip, email, password, "en")
	if err != nil {
		e.t.Fatalf("Login: %v", err)
	}
	s, err := e.pw.Complete(pwaBG, ip, c.Binding, "login", e.code(email, mailIndex), "")
	if err != nil {
		e.t.Fatalf("Complete login: %v", err)
	}
	return s
}

// onboard creates the first store for the session through the real /v1/identity/initial-store path
// (0065), which is the only way a password principal gets a membership (brief tier rules).
func (e *pwaEnv) onboard(s identity.Session) identity.Store {
	e.t.Helper()
	st, err := e.oidc.CreateInitialStore(pwaBG, s.Token, "pwa-store-"+pwaLetters(12), firstStoreRequest())
	if err != nil {
		e.t.Fatalf("initial-store: %v", err)
	}
	return st
}

// sessionLive reports whether the merchant session token is unrevoked and unexpired (owner pool read).
func (e *pwaEnv) sessionLive(token string) bool {
	e.t.Helper()
	sum := sha256.Sum256([]byte(token))
	var n int
	if err := e.f.owner.QueryRow(pwaBG, `SELECT count(*) FROM identity.sessions WHERE token_hash=$1 AND audience='merchant' AND revoked_at IS NULL AND expires_at>now()`, sum[:]).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

func (e *pwaEnv) principalOf(email string) string {
	e.t.Helper()
	var id string
	if err := e.f.owner.QueryRow(pwaBG, `SELECT principal_id::text FROM identity.password_credentials WHERE email=$1`, email).Scan(&id); err != nil {
		e.t.Fatalf("principal of %s: %v", "test address", err)
	}
	return id
}

func (e *pwaEnv) q1(query string, args ...any) (out int64) {
	e.t.Helper()
	if err := e.f.owner.QueryRow(pwaBG, query, args...).Scan(&out); err != nil {
		e.t.Fatalf("query: %v", err)
	}
	return out
}

func (e *pwaEnv) exec(query string, args ...any) {
	e.t.Helper()
	mustExec(e.t, e.f.owner, query, args...)
}

// bucket is the A2 key HMAC-SHA256(pepper, kind ":" value), used only to read rows this test owns.
func (e *pwaEnv) bucket(kind, value string) []byte {
	m := hmac.New(sha256.New, e.pepper)
	m.Write([]byte(kind + ":" + value))
	return m.Sum(nil)
}

// pwaWindows are the contract §6 windows (length, offset) of each bucket kind. auth_throttle_hit stores
// sha256(bucket || int4send(length) || int4send(offset)) (F1 fix), so rows are read through these keys.
var pwaWindows = map[string][][2]int{
	"ip": {{900, 0}}, "ip-signup": {{3600, 0}}, "ip48-signup": {{3600, 0}}, "ip48": {{900, 0}},
	"ip-mail-unauth": {{86400, 28800}}, "ip48-mail-unauth": {{86400, 28800}},
	"email-pw": {{900, 0}}, "binding": {{600, 0}},
	"email-mail-unauth": {{60, 0}, {3600, 0}, {86400, 28800}}, "email-mail-unauth-total": {{86400, 28800}},
	"email-mail-login": {{60, 0}, {3600, 0}, {86400, 28800}},
}

// storedKey is the auth_throttle.bucket value of one window of a bucket (see pwaWindows).
func (e *pwaEnv) storedKey(kind, value string, seconds, offset int) []byte {
	h := sha256.New()
	h.Write(e.bucket(kind, value))
	var w [8]byte
	binary.BigEndian.PutUint32(w[:4], uint32(seconds))
	binary.BigEndian.PutUint32(w[4:], uint32(offset))
	h.Write(w[:])
	return h.Sum(nil)
}

// storedKeys is every stored key of a bucket kind (one per contract window); the test fails on an unknown kind
// so a typo cannot turn a "no rows" assertion into a vacuous pass.
func (e *pwaEnv) storedKeys(kind, value string) [][]byte {
	e.t.Helper()
	ws, ok := pwaWindows[kind]
	if !ok {
		e.t.Fatalf("no §6 windows known for bucket kind %q", kind)
	}
	var out [][]byte
	for _, w := range ws {
		out = append(out, e.storedKey(kind, value, w[0], w[1]))
	}
	return out
}

// --- HTTP -------------------------------------------------------------------------------------

type pwaResp struct {
	Status int
	Header http.Header
	Body   []byte
	JSON   map[string]any
}

// post sends one request to the private password handler over a real listener.
func (e *pwaEnv) post(route string, ip string, body any, mods ...func(*http.Request)) pwaResp {
	e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.postRaw(route, ip, raw, mods...)
}

func (e *pwaEnv) postRaw(route, ip string, raw []byte, mods ...func(*http.Request)) pwaResp {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/identity/password/"+route, bytes.NewReader(raw))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Commerce-BFF-Key", e.bffKey)
	if ip != "" {
		req.Header.Set("X-Commerce-Client-IP", ip)
	}
	for _, m := range mods {
		if m != nil {
			m(req)
		}
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		e.t.Fatalf("POST %s: %v", route, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	out := pwaResp{Status: res.StatusCode, Header: res.Header, Body: b}
	_ = json.Unmarshal(b, &out.JSON)
	return out
}

// shape is the comparison key of the "identical response" clauses (§9 PA08): status, header names,
// JSON key set and value types. `binding` and `expires_at` are excluded by the contract; the
// per-request correlation header/field VALUES are not compared (only names and types).
func (r pwaResp) shape() string {
	names := make([]string, 0, len(r.Header))
	for k := range r.Header {
		names = append(names, strings.ToLower(k))
	}
	sortStrings(names)
	keys := make([]string, 0, len(r.JSON))
	for k, v := range r.JSON {
		if k == "binding" || k == "expires_at" {
			continue
		}
		keys = append(keys, fmt.Sprintf("%s:%T", k, v))
	}
	sortStrings(keys)
	return fmt.Sprintf("%d|%s|%s", r.Status, strings.Join(names, ","), strings.Join(keys, ","))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (r pwaResp) code() string {
	if s, ok := r.JSON["code"].(string); ok {
		return s
	}
	return ""
}

// --- throttle-row helpers (owner pool; disclosed: rows are read/aged only to reach a window boundary) --

type pwaRow struct {
	Bucket string
	Start  time.Time
	Hits   int
}

// throttleRows returns every auth_throttle row keyed by "bucket-hex|start" (whole table; the tests
// diff two snapshots to learn which rows one call touched without knowing how windows are keyed).
func (e *pwaEnv) throttleRows() map[string]pwaRow {
	e.t.Helper()
	rows, err := e.f.owner.Query(pwaBG, `SELECT encode(bucket,'hex'), window_start, hits FROM identity.auth_throttle`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]pwaRow{}
	for rows.Next() {
		var r pwaRow
		if err := rows.Scan(&r.Bucket, &r.Start, &r.Hits); err != nil {
			e.t.Fatal(err)
		}
		out[r.Bucket+"|"+r.Start.UTC().Format(time.RFC3339Nano)] = r
	}
	return out
}

// awaitSafeWindow sleeps past a window boundary when the call sequence would straddle it, so a
// fixed-window limit test cannot flake at :00 (seconds is the window length, offset its alignment).
func pwaAwaitSafeWindow(t *testing.T, seconds, offset int, margin time.Duration) {
	t.Helper()
	now := time.Now().UTC().Unix()
	into := (now - int64(offset)) % int64(seconds)
	if into < 0 {
		into += int64(seconds)
	}
	left := time.Duration(int64(seconds)-into) * time.Second
	if left < margin {
		t.Logf("waiting %s for the %ds window boundary so the test cannot straddle it", left+time.Second, seconds)
		time.Sleep(left + time.Second)
	}
}

// pwaDisclose records why a test moved time-shaped state through the owner pool (evidence text).
func pwaDisclose(t *testing.T, why string) { t.Helper(); t.Logf("OWNER-POOL DISCLOSURE: %s", why) }

// sibling is a second Passwords over the same database, pepper, mailbox and pool but another daily
// mail cap. Because buckets are HMAC(pepper, ...), it sees every counter the parent already moved:
// accounts are registered under the parent's large cap and the cap-dependent limits are then driven
// through the sibling, so 20-minimum-cap share arithmetic (8 / 3 / 9) can be exercised without the
// share of unauthenticated mail being spent on sign-ups.
func (e *pwaEnv) sibling(dailyCap int) *pwaEnv {
	e.t.Helper()
	pol := e.policy
	pol.MailDailyCap = dailyCap
	pw, err := identity.NewPasswords(e.pool, e.mailer, pol)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pw.Close(ctx)
	})
	cp := *e
	cp.pw = pw
	cp.policy = pol
	h, err := identityhttp.NewPasswordHandler(pw, e.bffKey)
	if err != nil {
		e.t.Fatal(err)
	}
	cp.handler = h
	cp.srv = httptest.NewServer(h)
	e.t.Cleanup(cp.srv.Close)
	return &cp
}

// settleMail waits until no challenge is still PENDING (every detached send has recorded its outcome),
// so message counts can be compared without racing a background sender.
func (e *pwaEnv) settleMail() {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for e.q1(`SELECT count(*) FROM identity.email_challenges WHERE mail_state='PENDING' AND created_at >= $1`, e.started) > 0 {
		if time.Now().After(deadline) {
			e.t.Fatal("challenges still PENDING after 15 s")
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
}

// sessionPrincipal resolves the principal that owns a session token (owner pool). The password
// Complete path returns only the token and expiry (§7.1), so Session.PrincipalID is not asserted.
func (e *pwaEnv) sessionPrincipal(token string) string {
	e.t.Helper()
	sum := sha256.Sum256([]byte(token))
	var id string
	if err := e.f.owner.QueryRow(pwaBG, `SELECT principal_id::text FROM identity.sessions WHERE token_hash=$1`, sum[:]).Scan(&id); err != nil {
		e.t.Fatalf("session principal: %v", err)
	}
	return id
}

// sub returns a copy of the env bound to a subtest's *testing.T: helpers call t.Fatalf, and calling
// FailNow of a parent test from a subtest goroutine aborts the whole parent, so every subtest that uses
// helpers starts with `e := e.sub(t)`.
func (e *pwaEnv) sub(t *testing.T) *pwaEnv {
	cp := *e
	cp.t = t
	return &cp
}
