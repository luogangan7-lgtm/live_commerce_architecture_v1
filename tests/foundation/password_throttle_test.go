package foundation_test

// PA07 TestPasswordPA07Throttle (REAL_PG + HTTP_PG) — contracts/merchant-password-auth-v1.md §6 (every
// row at N and N+1, window alignment incl. the UTC+8 day, A3 stop-at-first order), §4.2
// auth_throttle_hit purge bounds (I23), PD5, PD13, PD14, §2 (round-3 R3-3: a global-mail-login 503 is not
// refunded) and each bold §9 PA07 scenario as its own subtest. Tables: identity.auth_throttle,
// email_challenges, auth_events; functions: identity.auth_throttle_hit and the flows through
// internal/identity + identityhttp. Owner-pool use (disclosed per subtest): moving a 60 s / 1 h / 15 min
// window into the past (the wall clock cannot be moved; every "fresh window" is otherwise the fresh
// private pepper), seeding old rows for the purge test. "Starting at the UTC+8 day boundary" is met by
// the private pepper: every bucket starts empty, and pwaStableHour keeps the run away from a real boundary.
// Known contract observations recorded in output/auth-tests/README-findings (see the return message):
//   - email-mail-unauth's day limit (10) equals ip-mail-unauth's and is hit in the same calls, so one
//     source can never observe it independently; the test asserts the 11th call is throttled with a day
//     Retry-After, which either bucket satisfies.

import (
	"encoding/base64"
	"errors"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"livecommerce/internal/identity"
)

func pwaThrottled(err error) (identity.ThrottleError, bool) {
	var te identity.ThrottleError
	return te, errors.As(err, &te)
}

func (e *pwaEnv) wantThrottle(err error, what string) identity.ThrottleError {
	e.t.Helper()
	te, ok := pwaThrottled(err)
	if !ok {
		e.t.Fatalf("%s: %v, want ThrottleError (HTTP 429)", what, err)
	}
	if te.RetryAfter < time.Second {
		e.t.Errorf("%s: Retry-After %s, want >= 1 s", what, te.RetryAfter)
	}
	return te
}

func (e *pwaEnv) wantOK(err error, what string) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("%s: %v, want success", what, err)
	}
}

func pwaPrefix(ip netip.Addr) string { return netip.PrefixFrom(ip, 32).String() }

// nextDayStart is the next UTC+8 midnight (16:00 UTC).
func pwaNextDayStart() time.Time {
	now := time.Now().UTC()
	d := time.Date(now.Year(), now.Month(), now.Day(), 16, 0, 0, 0, time.UTC)
	if !d.After(now) {
		d = d.Add(24 * time.Hour)
	}
	return d
}

// elapseAll moves every row of a single-window bucket (ip, email-pw, ip-signup, binding) `secs` into the past.
// Rows move oldest first, one statement each: a single UPDATE of all rows fails with 23505 whenever the
// executor happens to visit a newer row before the older row it is about to land on.
func (e *pwaEnv) elapseAll(kind, value string, secs int) {
	e.t.Helper()
	rows, err := e.f.owner.Query(pwaBG, `SELECT bucket, window_start FROM identity.auth_throttle WHERE bucket = ANY($1) ORDER BY window_start`, e.storedKeys(kind, value))
	if err != nil {
		e.t.Fatalf("elapseAll(%s): %v", kind, err)
	}
	type row struct {
		b []byte
		s time.Time
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.b, &r.s); err != nil {
			e.t.Fatalf("elapseAll(%s): %v", kind, err)
		}
		all = append(all, r)
	}
	rows.Close()
	if len(all) == 0 {
		e.t.Fatalf("elapseAll(%s): no rows", kind)
	}
	for _, r := range all {
		if _, err := e.f.owner.Exec(pwaBG, `UPDATE identity.auth_throttle SET window_start = window_start - make_interval(secs => $3) WHERE bucket=$1 AND window_start=$2`, r.b, r.s, secs); err != nil {
			e.t.Fatalf("elapseAll(%s): %v", kind, err)
		}
	}
}

func (e *pwaEnv) wrongLoginFrom(ip netip.Addr, email string) error {
	_, err := e.pw.Login(pwaBG, ip, email, pwaSecret(), "en")
	return err
}

func (e *pwaEnv) wrongCompleteFrom(ip netip.Addr) error {
	_, err := e.pw.Complete(pwaBG, ip, identityRandomBinding(), "login", "000000", "")
	return err
}

// identityRandomBinding is a well-formed (32 random bytes, base64url) binding that has no challenge row.
// A malformed string is refused as invalid_code before any bucket is hit, so it cannot exercise `binding:`.
func identityRandomBinding() string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(32))
}

func TestPasswordPA07Throttle(t *testing.T) {
	// Cap 5000 (unauthenticated share 2000): the per-source and per-address limits are what these subtests
	// measure, and the subtests share one pepper, so the deployment-wide share must not run out between them.
	// The cap-dependent scenarios below use fresh environments with the contract's own numbers (20 and 200).
	e := newPwa(t, pwaCap(5000))
	logs := pwaCaptureLogs(t)
	pwaStableHour(t)

	t.Run("window_alignment_and_hit_counts_sql", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaAwaitSafeWindow(t, 60, 0, 5*time.Second)
		for _, w := range []struct{ seconds, offset int }{{60, 0}, {900, 0}, {3600, 0}, {86400, 0}, {86400, 28800}} {
			b := randomBytes(32)
			var last int
			for want := 1; want <= 3; want++ {
				if err := e.pool.QueryRow(pwaBG, `SELECT identity.auth_throttle_hit($1,$2,$3)`, b, w.seconds, w.offset).Scan(&last); err != nil {
					t.Fatal(err)
				}
				if last != want {
					t.Fatalf("window %ds+%d hit %d returned %d", w.seconds, w.offset, want, last)
				}
			}
			var start time.Time
			var hits int
			if err := e.f.owner.QueryRow(pwaBG, `SELECT window_start, hits FROM identity.auth_throttle WHERE bucket=sha256($1::bytea||int4send($2::int)||int4send($3::int))`, b, w.seconds, w.offset).Scan(&start, &hits); err != nil {
				t.Fatalf("one row per bucket and window expected: %v", err)
			}
			s := start.UTC().Unix()
			if hits != 3 || (s+int64(w.offset))%int64(w.seconds) != 0 {
				t.Errorf("window %ds+%d: hits=%d start=%s not aligned", w.seconds, w.offset, hits, start.UTC())
			}
			if now := time.Now().UTC().Unix(); s > now || now >= s+int64(w.seconds) {
				t.Errorf("window %ds+%d start %s does not contain now", w.seconds, w.offset, start.UTC())
			}
			if w.seconds == 86400 && w.offset == 28800 && (start.UTC().Hour() != 16 || start.UTC().Minute() != 0) {
				t.Errorf("UTC+8 day window starts at %s, want 16:00 UTC", start.UTC())
			}
			if w.seconds == 86400 && w.offset == 0 && start.UTC().Hour() != 0 {
				t.Errorf("UTC day window starts at %s, want 00:00 UTC", start.UTC())
			}
		}
		// buckets are independent
		var a, b int
		ba, bb := randomBytes(32), randomBytes(32)
		_ = e.pool.QueryRow(pwaBG, `SELECT identity.auth_throttle_hit($1,60,0)`, ba).Scan(&a)
		_ = e.pool.QueryRow(pwaBG, `SELECT identity.auth_throttle_hit($1,60,0)`, bb).Scan(&b)
		if a != 1 || b != 1 {
			t.Errorf("distinct buckets share a counter: %d %d", a, b)
		}
	})

	t.Run("purge_is_bounded_to_100_rows_per_call_per_table", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaDisclose(t, "owner pool seeds 250 stale rows per table plus fresh controls; the definer's own purge is the property under test")
		tag := pwaLetters(8)
		e.exec(`INSERT INTO identity.auth_throttle(bucket,window_start,hits) SELECT sha256(($1::text||g::text)::bytea), now()-interval '3 days', 1 FROM generate_series(1,250) g`, tag)
		e.exec(`INSERT INTO identity.auth_throttle(bucket,window_start,hits) SELECT sha256(($1::text||'fresh'||g::text)::bytea), now()-interval '1 day', 1 FROM generate_series(1,5) g`, tag)
		e.exec(`INSERT INTO identity.email_challenges(id,purpose,email,locale,pending_password_hash,binding_hash,code_hmac,ip_hmac,created_at,expires_at)
		        SELECT gen_random_uuid(),'signup','purge.'||$1::text||g::text||'@example.test','en',$2,sha256(($1::text||'b'||g::text)::bytea),sha256('c'::bytea),sha256('i'::bytea), now()-interval '3 days', now()-interval '2 days' FROM generate_series(1,250) g`, tag, pwaValidPHC)
		e.exec(`INSERT INTO identity.email_challenges(id,purpose,email,locale,pending_password_hash,binding_hash,code_hmac,ip_hmac,created_at,expires_at)
		        SELECT gen_random_uuid(),'signup','keep.'||$1::text||g::text||'@example.test','en',$2,sha256(($1::text||'kb'||g::text)::bytea),sha256('c'::bytea),sha256('i'::bytea), now()-interval '2 days', now()-interval '23 hours' FROM generate_series(1,5) g`, tag, pwaValidPHC)
		e.exec(`INSERT INTO identity.auth_events(action,created_at) SELECT 'throttled', now()-interval '200 days' FROM generate_series(1,250)`)
		e.exec(`INSERT INTO identity.auth_events(action,created_at) SELECT 'throttled', now()-interval '179 days' FROM generate_series(1,5)`)
		oldT := func() int64 {
			return e.q1(`SELECT count(*) FROM identity.auth_throttle WHERE window_start < now()-interval '2 days'`)
		}
		oldC := func() int64 {
			return e.q1(`SELECT count(*) FROM identity.email_challenges WHERE expires_at < now()-interval '1 day'`)
		}
		oldE := func() int64 {
			return e.q1(`SELECT count(*) FROM identity.auth_events WHERE created_at < now()-interval '180 days'`)
		}
		t0, c0, e0 := oldT(), oldC(), oldE()
		if t0 < 250 || c0 < 250 || e0 < 250 {
			t.Fatalf("setup: stale rows %d/%d/%d", t0, c0, e0)
		}
		prevT, prevC, prevE := t0, c0, e0
		for call := 1; call <= 3; call++ {
			var n int
			if err := e.pool.QueryRow(pwaBG, `SELECT identity.auth_throttle_hit($1,60,0)`, randomBytes(32)).Scan(&n); err != nil {
				t.Fatal(err)
			}
			nt, nc, ne := oldT(), oldC(), oldE()
			for name, d := range map[string]int64{"auth_throttle": prevT - nt, "email_challenges": prevC - nc, "auth_events": prevE - ne} {
				if d > 100 {
					t.Errorf("call %d purged %d %s rows, bound is 100 (I23)", call, d, name)
				}
				if d < 1 {
					t.Errorf("call %d purged nothing from %s while stale rows remain", call, name)
				}
			}
			prevT, prevC, prevE = nt, nc, ne
		}
		if e.q1(`SELECT count(*) FROM identity.auth_throttle WHERE bucket=sha256(($1::text||'fresh1')::bytea)`, tag) != 1 ||
			e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email LIKE 'keep.'||$1::text||'%'`, tag) != 5 ||
			e.q1(`SELECT count(*) FROM identity.auth_events WHERE action='throttled' AND created_at > now()-interval '180 days' AND created_at < now()-interval '178 days'`) < 5 {
			t.Error("a row younger than the retention bound (1 day of throttle age within 2 d, expired 23 h, event 179 d) was purged")
		}
	})

	t.Run("ip_30_per_15min_counts_every_step1_and_complete_call", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaStable15(t, 2*time.Minute)
		ip := pwaIP()
		for i := 0; i < 29; i++ {
			if err := e.wrongLoginFrom(ip, pwaEmail()); !errors.Is(err, identity.ErrInvalidCredentials) {
				t.Fatalf("call %d: %v", i+1, err)
			}
		}
		if err := e.wrongCompleteFrom(ip); !errors.Is(err, identity.ErrInvalidCode) { // the 30th call is a complete
			t.Fatalf("30th call (complete): %v, want ErrInvalidCode (still within the limit)", err)
		}
		email31 := pwaEmail()
		te := e.wantThrottle(e.wrongLoginFrom(ip, email31), "31st call from one source")
		if te.RetryAfter > 15*time.Minute {
			t.Errorf("Retry-After %s exceeds the 15 min window", te.RetryAfter)
		}
		// A3: the chain stopped at `ip`, so email-pw for the 31st email was never touched.
		if n := e.q1(`SELECT count(*) FROM identity.auth_throttle WHERE bucket = ANY($1)`, e.storedKeys("email-pw", email31+"‖"+pwaPrefix(ip))); n != 0 {
			t.Errorf("email-pw was hit after the ip bucket had already tripped (A3 stop-at-first): %d rows", n)
		}
		// a complete call is throttled too
		e.wantThrottle(e.wrongCompleteFrom(ip), "complete from the throttled source")
		// window alignment: 15 min
		var start time.Time
		if err := e.f.owner.QueryRow(pwaBG, `SELECT window_start FROM identity.auth_throttle WHERE bucket = ANY($1)`, e.storedKeys("ip", pwaPrefix(ip))).Scan(&start); err != nil || start.UTC().Unix()%900 != 0 {
			t.Errorf("ip window start %v (err %v) not aligned to 15 minutes", start.UTC(), err)
		}
		// over HTTP: 429 + Retry-After header, no extra keys beyond the error envelope
		r := e.post("login", ip.String(), map[string]string{"email": pwaEmail(), "password": pwaSecret(), "locale": "en"})
		if ra, err := strconv.Atoi(r.Header.Get("Retry-After")); r.Status != 429 || r.code() != "throttled" || err != nil || ra < 1 || ra > 900 {
			t.Errorf("HTTP: status %d code %q Retry-After %q", r.Status, r.code(), r.Header.Get("Retry-After"))
		}
	})

	t.Run("ip_signup_5_per_hour_ipv4_ipv6_and_mapped", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		ip := pwaIP()
		for i := 0; i < 5; i++ {
			e.signupFrom(ip, pwaEmail(), pwaSecret())
		}
		e.wantThrottle(func() error { _, err := e.pw.Signup(pwaBG, ip, pwaEmail(), pwaSecret(), "en"); return err }(), "6th sign-up in an hour from one IPv4")
		// an IPv4-mapped IPv6 spelling is the same source
		mapped := netip.AddrFrom16(ip.As16())
		e.wantThrottle(func() error { _, err := e.pw.Signup(pwaBG, mapped, pwaEmail(), pwaSecret(), "en"); return err }(), "IPv4-mapped IPv6 spelling of the same source")
		// IPv6: /64 is the source; two addresses of one /64 share the bucket
		v6 := func(net48 uint16, net64 uint16, host uint16) netip.Addr {
			b := [16]byte{0x20, 0x01, 0x0d, 0xb8, byte(net48 >> 8), byte(net48), byte(net64 >> 8), byte(net64), 0, 0, 0, 0, 0, 0, byte(host >> 8), byte(host)}
			return netip.AddrFrom16(b)
		}
		n48 := uint16(pwaIPCounter.Add(1))
		for h := uint16(1); h <= 5; h++ {
			e.signupFrom(v6(n48, 1, h), pwaEmail(), pwaSecret())
		}
		e.wantThrottle(func() error { _, err := e.pw.Signup(pwaBG, v6(n48, 1, 6), pwaEmail(), pwaSecret(), "en"); return err }(), "6th sign-up from one IPv6 /64 (different host bits)")
		// ip48-signup: 20 / hour across the /64s of one /48 (1 already used above)
		for net64 := uint16(2); net64 <= 16; net64++ { // 5 (net64=1) + 15 = 20
			e.signupFrom(v6(n48, net64, 1), pwaEmail(), pwaSecret())
		}
		e.wantThrottle(func() error { _, err := e.pw.Signup(pwaBG, v6(n48, 17, 1), pwaEmail(), pwaSecret(), "en"); return err }(), "21st sign-up from one IPv6 /48")
		// a different /48 is unaffected
		e.signupFrom(v6(n48+1, 1, 1), pwaEmail(), pwaSecret())
	})

	t.Run("ip_mail_unauth_10_per_day_shared_by_signup_exists_notice_and_reset", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		taken := []string{pwaEmail(), pwaEmail(), pwaEmail()}
		for _, em := range taken {
			e.register(em, pwaSecret())
		}
		ip := pwaIP()
		for i := 0; i < 4; i++ { // 4 resets of unknown emails (no mail, still counted before the lookup)
			if _, err := e.pw.Reset(pwaBG, ip, pwaEmail(), "en"); err != nil {
				t.Fatalf("reset %d: %v", i+1, err)
			}
		}
		for _, em := range taken { // 3 exists-notices
			if _, err := e.pw.Signup(pwaBG, ip, em, pwaSecret(), "en"); err != nil {
				t.Fatalf("exists-notice sign-up: %v", err)
			}
		}
		for i := 0; i < 3; i++ {
			if _, err := e.pw.Reset(pwaBG, ip, pwaEmail(), "en"); err != nil {
				t.Fatalf("reset: %v", err)
			}
		}
		te := e.wantThrottle(func() error { _, err := e.pw.Reset(pwaBG, ip, pwaEmail(), "en"); return err }(), "11th unauthenticated mail from one source in a UTC+8 day")
		if want := time.Until(pwaNextDayStart()); te.RetryAfter < want-10*time.Second || te.RetryAfter > want+10*time.Second {
			t.Errorf("Retry-After %s, want the time to the next UTC+8 midnight (%s)", te.RetryAfter, want.Round(time.Second))
		}
		// a sign-up of a free email from the same source is over the same bucket
		e.wantThrottle(func() error { _, err := e.pw.Signup(pwaBG, ip, pwaEmail(), pwaSecret(), "en"); return err }(), "sign-up over the shared day bucket")
		// window alignment of the day bucket
		var start time.Time
		if err := e.f.owner.QueryRow(pwaBG, `SELECT window_start FROM identity.auth_throttle WHERE bucket = ANY($1)`, e.storedKeys("ip-mail-unauth", pwaPrefix(ip))).Scan(&start); err != nil || start.UTC().Hour() != 16 || start.UTC().Minute() != 0 || start.UTC().Second() != 0 {
			t.Errorf("ip-mail-unauth window start %v (err %v), want 16:00:00 UTC (UTC+8 midnight)", start.UTC(), err)
		}
	})

	t.Run("ip48_mail_unauth_20_per_day", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		v6 := func(net48 uint16, net64 uint16) netip.Addr {
			return netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(net48 >> 8), byte(net48), byte(net64 >> 8), byte(net64), 0, 0, 0, 0, 0, 0, 0, 1})
		}
		n48 := uint16(pwaIPCounter.Add(1))
		for i := uint16(1); i <= 20; i++ {
			if _, err := e.pw.Reset(pwaBG, v6(n48, i), pwaEmail(), "en"); err != nil {
				t.Fatalf("reset %d from /64 #%d: %v", i, i, err)
			}
		}
		e.wantThrottle(func() error { _, err := e.pw.Reset(pwaBG, v6(n48, 21), pwaEmail(), "en"); return err }(), "21st unauthenticated mail from one IPv6 /48 (each from a fresh /64)")
		if _, err := e.pw.Reset(pwaBG, v6(n48+1, 1), pwaEmail(), "en"); err != nil {
			t.Errorf("a different /48 was throttled: %v", err)
		}
	})

	t.Run("ip48_60_per_15min_counts_every_step1_and_complete_call", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaStable15(t, 2*time.Minute)
		v6 := func(net48 uint16, net64 uint16) netip.Addr {
			return netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(net48 >> 8), byte(net48), byte(net64 >> 8), byte(net64), 0, 0, 0, 0, 0, 0, 0, 1})
		}
		n48 := 0xC000 | uint16(pwaIPCounter.Add(1)&0x0fff) // own range: other subtests use small n48 and n48+1
		// Every call comes from a fresh /64, so the per-/64 `ip` bucket (30) never trips; only the /48 can.
		for i := uint16(1); i <= 60; i++ {
			if i%2 == 1 {
				if err := e.wrongLoginFrom(v6(n48, i), pwaEmail()); !errors.Is(err, identity.ErrInvalidCredentials) {
					t.Fatalf("call %d (login): %v", i, err)
				}
			} else if err := e.wrongCompleteFrom(v6(n48, i)); !errors.Is(err, identity.ErrInvalidCode) {
				t.Fatalf("call %d (complete): %v", i, err)
			}
		}
		te := e.wantThrottle(e.wrongLoginFrom(v6(n48, 61), pwaEmail()), "61st login from one IPv6 /48 in 15 min")
		if te.RetryAfter > 15*time.Minute {
			t.Errorf("Retry-After %s exceeds the 15 min window", te.RetryAfter)
		}
		e.wantThrottle(e.wrongCompleteFrom(v6(n48, 62)), "62nd call (complete) from the same /48")
		// a different /48 is unaffected
		if err := e.wrongLoginFrom(v6(n48^0x1000, 1), pwaEmail()); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Errorf("a different /48 was throttled: %v", err)
		}
	})

	t.Run("email_mail_unauth_per_source_60s_hour_day_and_total_30", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		victim := pwaEmail() // unknown email: the buckets are hit whether or not it exists (PD6)
		A, B := pwaIP(), pwaIP()
		e.wantOK(func() error { _, err := e.pw.Reset(pwaBG, A, victim, "en"); return err }(), "first reset")
		te := e.wantThrottle(func() error { _, err := e.pw.Reset(pwaBG, A, victim, "en"); return err }(), "second reset within 60 s from the same source")
		if te.RetryAfter > time.Minute {
			t.Errorf("60 s window Retry-After %s", te.RetryAfter)
		}
		r := e.post("reset", A.String(), map[string]string{"email": victim, "locale": "en"})
		if ra, _ := strconv.Atoi(r.Header.Get("Retry-After")); r.Status != 429 || ra < 1 || ra > 60 {
			t.Errorf("HTTP 60 s window: status %d Retry-After %q", r.Status, r.Header.Get("Retry-After"))
		}
		// per source: B is not affected by A's exhaustion (round-2 P1)
		e.wantOK(func() error { _, err := e.pw.Reset(pwaBG, B, victim, "en"); return err }(), "another source, same email")
		// hourly 5: reset A's 60 s window between calls
		pwaDisclose(t, "owner pool moves the 60 s (and later the hour) window of email-mail-unauth into the past")
		key := victim + "‖" + pwaPrefix(A)
		for n := 2; n <= 5; n++ {
			e.elapse("email-mail-unauth", key, "minute")
			e.wantOK(func() error { _, err := e.pw.Reset(pwaBG, A, victim, "en"); return err }(), "reset "+strconv.Itoa(n)+" within the hour")
		}
		e.elapse("email-mail-unauth", key, "minute")
		te = e.wantThrottle(func() error { _, err := e.pw.Reset(pwaBG, A, victim, "en"); return err }(), "6th reset within the hour")
		if te.RetryAfter > time.Hour || te.RetryAfter <= time.Minute {
			t.Errorf("hour window Retry-After %s, want in (1 min, 1 h]", te.RetryAfter)
		}
	})

	t.Run("email_mail_unauth_day_10_then_429", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		// Fresh source: throttled calls also spend ip-mail-unauth, so the day limit is measured on its own.
		// ip-mail-unauth and email-mail-unauth both allow 10 per source and day, so the 11th call being
		// throttled with a day Retry-After is all one source can observe (see the header comment).
		victim, C := pwaEmail(), pwaIP()
		key := victim + "‖" + pwaPrefix(C)
		pwaDisclose(t, "owner pool moves the 60 s and hour windows of email-mail-unauth into the past between calls")
		for n := 1; n <= 10; n++ {
			if n > 1 {
				e.elapse("email-mail-unauth", key, "minute")
			}
			if n == 6 {
				e.elapse("email-mail-unauth", key, "hour")
			}
			e.wantOK(func() error { _, err := e.pw.Reset(pwaBG, C, victim, "en"); return err }(), "reset "+strconv.Itoa(n)+" of the day")
		}
		e.elapse("email-mail-unauth", key, "minute")
		e.elapse("email-mail-unauth", key, "hour")
		te := e.wantThrottle(func() error { _, err := e.pw.Reset(pwaBG, C, victim, "en"); return err }(), "11th reset of the day from one source")
		if want := time.Until(pwaNextDayStart()); te.RetryAfter < want-10*time.Second || te.RetryAfter > want+10*time.Second {
			t.Errorf("day Retry-After %s, want %s", te.RetryAfter, want.Round(time.Second))
		}
	})

	t.Run("email_mail_unauth_total_30_per_day_across_sources", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		victim := pwaEmail()
		for i := 1; i <= 30; i++ {
			e.wantOK(func() error { _, err := e.pw.Reset(pwaBG, pwaIP(), victim, "en"); return err }(), "source "+strconv.Itoa(i))
		}
		te := e.wantThrottle(func() error { _, err := e.pw.Reset(pwaBG, pwaIP(), victim, "en"); return err }(), "31st source for one address in a day")
		if want := time.Until(pwaNextDayStart()); te.RetryAfter < want-10*time.Second || te.RetryAfter > want+10*time.Second {
			t.Errorf("Retry-After %s, want the day remainder %s", te.RetryAfter, want.Round(time.Second))
		}
	})

	t.Run("email_pw_10_per_15min_per_source_victim_from_B_still_logs_in", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaStable15(t, 3*time.Minute)
		victim, password := pwaEmail(), pwaSecret()
		e.register(victim, password)
		A, B := pwaIP(), pwaIP()
		for i := 1; i <= 10; i++ {
			if err := e.wrongLoginFrom(A, victim); !errors.Is(err, identity.ErrInvalidCredentials) {
				t.Fatalf("wrong password %d from A: %v", i, err)
			}
		}
		te := e.wantThrottle(e.wrongLoginFrom(A, victim), "11th password check from one source")
		if te.RetryAfter > 15*time.Minute {
			t.Errorf("Retry-After %s", te.RetryAfter)
		}
		// no oracle: the CORRECT password from the throttled source is also 429
		_, err := e.pw.Login(pwaBG, A, victim, password, "en")
		e.wantThrottle(err, "correct password from the exhausted source")
		// an unknown email behaves identically (known or unknown alike)
		unknown := pwaEmail()
		for i := 1; i <= 10; i++ {
			if err := e.wrongLoginFrom(A, unknown); !errors.Is(err, identity.ErrInvalidCredentials) {
				t.Fatalf("unknown email %d: %v", i, err)
			}
		}
		e.wantThrottle(e.wrongLoginFrom(A, unknown), "11th check for an unknown email")
		// the victim from source B logs in: start + complete
		c, err := e.pw.Login(pwaBG, B, victim, password, "en")
		if err != nil {
			t.Fatalf("victim login from another source: %v", err)
		}
		s, err := e.pw.Complete(pwaBG, B, c.Binding, "login", e.code(victim, 2), "")
		if err != nil || !e.sessionLive(s.Token) {
			t.Fatalf("victim complete from B: %v", err)
		}
	})

	t.Run("email_mail_login_60s_hour_day_and_5_per_day", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		victim, password := pwaEmail(), pwaSecret()
		e.register(victim, password)
		// Wrong passwords and unauthenticated traffic never consume the login-mail bucket.
		for range 3 {
			_ = e.wrongLoginFrom(pwaIP(), victim)
			_, _ = e.pw.Reset(pwaBG, pwaIP(), victim, "en")
		}
		e.awaitMails(victim, 4) // sign-up mail + three reset mails
		if n := e.q1(`SELECT count(*) FROM identity.auth_throttle WHERE bucket = ANY($1)`, e.storedKeys("email-mail-login", victim)); n != 0 {
			t.Fatalf("email-mail-login has %d rows before any password-verified login", n)
		}
		login := func(what string) error {
			_, err := e.pw.Login(pwaBG, pwaIP(), victim, password, "en")
			_ = what
			return err
		}
		e.wantOK(login("1"), "first login mail")
		te := e.wantThrottle(login("2"), "second login start within 60 s")
		if te.RetryAfter > time.Minute {
			t.Errorf("60 s Retry-After %s", te.RetryAfter)
		}
		pwaDisclose(t, "owner pool moves the 60 s / 1 h windows of email-mail-login into the past")
		for n := 2; n <= 5; n++ {
			e.elapse("email-mail-login", victim, "minute")
			e.wantOK(login("n"), "login mail "+strconv.Itoa(n))
		}
		// F1: every window counts only its own hits. After five login mails the hour row and the day row each
		// say 5 (not 10), also in the first hour of a UTC+8 day where both windows start at the same instant.
		for _, w := range [][2]int{{3600, 0}, {86400, 28800}} {
			var hits int
			if err := e.f.owner.QueryRow(pwaBG, `SELECT hits FROM identity.auth_throttle WHERE bucket=$1`, e.storedKey("email-mail-login", victim, w[0], w[1])).Scan(&hits); err != nil || hits != 5 {
				t.Errorf("email-mail-login window %ds+%d after 5 login mails: hits=%d err=%v, want 5 (F1: windows must not share a row)", w[0], w[1], hits, err)
			}
		}
		e.elapse("email-mail-login", victim, "minute")
		te = e.wantThrottle(login("6"), "6th login mail within the hour")
		if te.RetryAfter > time.Hour {
			t.Errorf("hour Retry-After %s", te.RetryAfter)
		}
		// Age the hour window too: the day limit (5 per UTC+8 day) still stops the 6th mail.
		e.elapse("email-mail-login", victim, "hour")
		e.elapse("email-mail-login", victim, "minute")
		te = e.wantThrottle(login("6-day"), "6th login mail of the UTC+8 day with fresh 60 s and hour windows")
		if want := time.Until(pwaNextDayStart()); te.RetryAfter < want-10*time.Second || te.RetryAfter > want+10*time.Second {
			t.Errorf("day Retry-After %s, want %s", te.RetryAfter, want.Round(time.Second))
		}
		// "One account logging in at every allowed rate for 24 h consumes <= 5 login mails": simulate the
		// remaining 23 hours of the UTC+8 day, five attempts an hour, each 60 s apart. None is mailed.
		pwaDisclose(t, "owner pool moves the hour and 60 s windows of email-mail-login 23 more times to simulate the rest of the day")
		for hour := 1; hour < 24; hour++ {
			e.elapse("email-mail-login", victim, "hour")
			for k := 0; k < 5; k++ {
				e.elapse("email-mail-login", victim, "minute")
				e.wantThrottle(login("day-loop"), "login attempt "+strconv.Itoa(k+1)+" of simulated hour "+strconv.Itoa(hour+1))
			}
		}
		time.Sleep(200 * time.Millisecond)
		if n := len(e.mailsTo(victim)); n != 4+5 {
			t.Errorf("mails to the victim = %d, want 9 (1 sign-up + 3 resets + exactly 5 login mails)", n)
		}
	})

	t.Run("binding_10_per_10min", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaStable15(t, 90*time.Second)
		binding := identityRandomBinding()
		for i := 1; i <= 10; i++ {
			if _, err := e.pw.Complete(pwaBG, pwaIP(), binding, "login", "000000", ""); !errors.Is(err, identity.ErrInvalidCode) {
				t.Fatalf("complete %d: %v", i, err)
			}
		}
		_, err := e.pw.Complete(pwaBG, pwaIP(), binding, "login", "000000", "")
		te := e.wantThrottle(err, "11th complete on one binding")
		if te.RetryAfter > 10*time.Minute {
			t.Errorf("Retry-After %s exceeds the 10 min window", te.RetryAfter)
		}
	})

	// ---- bold PA07 scenarios, each an own subtest ------------------------------------------------

	t.Run("victim_login_still_works_after_attacker_saturates_every_limit", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaStable15(t, 3*time.Minute)
		victim, password := pwaEmail(), pwaSecret()
		e.register(victim, password)
		A := pwaIP()
		e.saturate(victim, A)
		// The login-mail bucket was never consumed by the attacker.
		if n := e.q1(`SELECT count(*) FROM identity.auth_throttle WHERE bucket = ANY($1)`, e.storedKeys("email-mail-login", victim)); n != 0 {
			t.Errorf("attacker traffic touched email-mail-login: %d rows", n)
		}
		B := pwaIP()
		n := len(e.mailsTo(victim))
		c, err := e.pw.Login(pwaBG, B, victim, password, "en")
		if err != nil {
			t.Fatalf("victim login start after saturation: %v", err)
		}
		s, err := e.pw.Complete(pwaBG, B, c.Binding, "login", e.code(victim, n+1), "")
		if err != nil || !e.sessionLive(s.Token) {
			t.Fatalf("victim login complete after saturation: %v", err)
		}
	})

	t.Run("disabled_credential_attacker_at_every_limit_victim_resets_from_B", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaStable15(t, 4*time.Minute)
		victim, password := pwaEmail(), pwaSecret()
		e.register(victim, password)
		e.disableByFailures(victim)
		A := pwaIP()
		e.saturate(victim, A)
		B := pwaIP()
		c, code := e.resetFrom(B, victim)
		newPassword := pwaSecret()
		s, err := e.resetComplete(B, c, code, newPassword)
		if err != nil {
			t.Fatalf("victim reset from source B after the attacker used every limit: %v", err)
		}
		if f, d, _ := e.credState(victim); f != 0 || d || !e.sessionLive(s.Token) {
			t.Errorf("after reset: failed_count=%d disabled=%v live=%v", f, d, e.sessionLive(s.Token))
		}
	})

	t.Run("one_source_at_every_limit_leaves_global_unauth_share_and_A_11th_is_429", func(t *testing.T) {
		e := newPwa(t) // default cap 200 => global-mail-unauth is 80, the number the contract's claim is about
		_ = e
		victim, password := pwaEmail(), pwaSecret()
		e.register(victim, password)
		A := pwaIP()
		for i := 1; i <= 5; i++ { // 5 sign-ups of free addresses: the ip-signup limit, and each one is an unauth mail
			if _, err := e.pw.Signup(pwaBG, A, pwaEmail(), pwaSecret(), "en"); err != nil {
				t.Fatalf("A's sign-up %d: %v", i, err)
			}
		}
		for i := 6; i <= 10; i++ { // unknown-email resets count against every bucket, incl. global-mail-unauth
			if _, err := e.pw.Reset(pwaBG, A, pwaEmail(), "en"); err != nil {
				t.Fatalf("A's unauth mail %d: %v", i, err)
			}
		}
		if _, err := e.pw.Signup(pwaBG, A, pwaEmail(), pwaSecret(), "en"); err == nil {
			t.Fatal("A's 6th sign-up in an hour was accepted (ip-signup is 5 / hour)")
		}
		_, err := e.pw.Reset(pwaBG, A, pwaEmail(), "en")
		if _, ok := pwaThrottled(err); !ok {
			t.Fatalf("A's 11th unauth mail that day: %v, want 429 (not the global 503)", err)
		}
		// default cap 200 => global-mail-unauth is 80: one source (10) leaves it usable
		c, code := e.resetFrom(pwaIP(), victim)
		if _, err := e.resetComplete(pwaIP(), c, code, pwaSecret()); err != nil {
			// complete throttles use the completing source; a fresh source is fine
			t.Fatalf("victim's reset from another source after one source used all its limits: %v", err)
		}
	})

	// ---- cap-dependent scenarios: shares floor(cap*pct/100) = 8 / 3 / 9 at the minimum cap 20 ----

	t.Run("global_mail_unauth_share_8_fail_closed_before_lookup_login_share_untouched", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		g := newPwa(t, pwaCap(20)) // own pepper: its global buckets start empty
		member, mpw := pwaEmail(), pwaSecret()
		ms := g.register(member, mpw) // 1 unauth mail
		g.onboard(ms)
		known := pwaEmail()
		g.register(known, pwaSecret()) // 2nd unauth mail
		used := 2
		var over error
		for used < 20 {
			_, over = g.pw.Reset(pwaBG, pwaIP(), pwaEmail(), "en")
			if over != nil {
				break
			}
			used++
		}
		if !errors.Is(over, identity.ErrMailUnavailable) || used != 8 {
			t.Fatalf("after %d unauth mails the next call gave %v, want ErrMailUnavailable at exactly the 9th (share 8)", used, over)
		}
		// known and unknown emails and sign-up all fail the same way (before any lookup)
		if _, err := g.pw.Reset(pwaBG, pwaIP(), known, "en"); !errors.Is(err, identity.ErrMailUnavailable) {
			t.Errorf("known email over the global share: %v", err)
		}
		if _, err := g.pw.Signup(pwaBG, pwaIP(), pwaEmail(), pwaSecret(), "en"); !errors.Is(err, identity.ErrMailUnavailable) {
			t.Errorf("sign-up over the global share: %v", err)
		}
		rk := g.post("reset", pwaIP().String(), map[string]string{"email": known, "locale": "en"})
		ru := g.post("reset", pwaIP().String(), map[string]string{"email": pwaEmail(), "locale": "en"})
		if rk.Status != 503 || rk.code() != "mail_unavailable" || rk.shape() != ru.shape() {
			t.Errorf("HTTP over-cap reset differs: %s vs %s", rk.shape(), ru.shape())
		}
		// the member's login share is untouched: start + complete work
		c, err := g.pw.Login(pwaBG, pwaIP(), member, mpw, "en")
		if err != nil {
			t.Fatalf("member login after unauth exhaustion: %v", err)
		}
		if _, err := g.pw.Complete(pwaBG, pwaIP(), c.Binding, "login", g.code(member, 2), ""); err != nil {
			t.Fatalf("member complete after unauth exhaustion: %v", err)
		}
	})

	t.Run("global_mail_login_new_share_3_not_refunded_and_member_login_works", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		g := newPwa(t) // own pepper: the shared env has already moved the global login buckets
		var storeless [4]string
		var pws [4]string
		for i := range storeless {
			storeless[i], pws[i] = pwaEmail(), pwaSecret()
			g.register(storeless[i], pws[i])
		}
		member, mpw := pwaEmail(), pwaSecret()
		g.onboard(g.register(member, mpw))
		s := g.sibling(20) // shares 8 / 3 / 9
		for i := 0; i < 3; i++ {
			if _, err := s.pw.Login(pwaBG, pwaIP(), storeless[i], pws[i], "en"); err != nil {
				t.Fatalf("store-less login %d: %v", i+1, err)
			}
		}
		data := g.smtp.DataCount()
		ip4 := pwaIP()
		// The refused login and its retry must land in the same epoch-aligned 60 s email-mail-login window: a minute
		// rollover between them makes the retry a second 503 instead of the 429 (seen once at the R2 integration run).
		pwaAwaitSafeWindow(t, 60, 0, 10*time.Second)
		if _, err := s.pw.Login(pwaBG, ip4, storeless[3], pws[3], "en"); !errors.Is(err, identity.ErrMailUnavailable) {
			t.Fatalf("4th store-less login: %v, want ErrMailUnavailable (share 3)", err)
		}
		if g.smtp.DataCount() != data {
			t.Error("SMTP was dialled for a login refused by the global share")
		}
		if st := func() string {
			var s string
			_ = g.f.owner.QueryRow(pwaBG, `SELECT mail_state FROM identity.email_challenges WHERE email=$1 AND purpose='login'`, storeless[3]).Scan(&s)
			return s
		}(); st != "FAILED" {
			t.Errorf("challenge of the refused login has mail_state %q, want FAILED", st)
		}
		// §2 / R3-3: the email-mail-login hit is not refunded, so an immediate retry is a 429, not a second 503
		_, err := s.pw.Login(pwaBG, pwaIP(), storeless[3], pws[3], "en")
		g.wantThrottle(err, "retry right after the global-share 503 (email-mail-login hit consumed)")
		// a store member's login share is separate: start + complete work
		c, err := s.pw.Login(pwaBG, pwaIP(), member, mpw, "en")
		if err != nil {
			t.Fatalf("member login after login-new exhaustion: %v", err)
		}
		ms, err := s.pw.Complete(pwaBG, pwaIP(), c.Binding, "login", g.code(member, 2), "")
		if err != nil || !g.sessionLive(ms.Token) {
			t.Fatalf("member complete after login-new exhaustion: %v", err)
		}
	})

	t.Run("global_mail_login_share_9_503_consumes_one_email_mail_login_hit", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		g := newPwa(t) // own pepper: the shared env has already moved the global login buckets
		var members [10]string
		var pws [10]string
		for i := range members {
			members[i], pws[i] = pwaEmail(), pwaSecret()
			g.onboard(g.register(members[i], pws[i]))
		}
		s := g.sibling(20)
		for i := 0; i < 9; i++ {
			if _, err := s.pw.Login(pwaBG, pwaIP(), members[i], pws[i], "en"); err != nil {
				t.Fatalf("member login %d: %v", i+1, err)
			}
		}
		if _, err := s.pw.Login(pwaBG, pwaIP(), members[9], pws[9], "en"); !errors.Is(err, identity.ErrMailUnavailable) {
			t.Fatalf("10th member login: %v, want ErrMailUnavailable (share 9)", err)
		}
		if n := g.q1(`SELECT count(*) FROM identity.auth_throttle WHERE bucket = ANY($1) AND hits=1`, g.storedKeys("email-mail-login", members[9])); n < 1 {
			t.Error("the 503 did not consume an email-mail-login hit")
		}
		_, err := s.pw.Login(pwaBG, pwaIP(), members[9], pws[9], "en")
		g.wantThrottle(err, "retry after the global-mail-login 503")
	})

	pwaNoCanary(t, "captured logs", logs.String(), map[string]string{"smtp secret": e.smtp.Password, "smtp user": e.smtp.Username})
}

// saturate drives one attacker source A against a victim to every limit of §6 that A alone can reach
// (email-pw, the per-source unauth mail buckets, ip-mail-unauth, ip) and PD5 (10 wrong reset codes in
// A's bucket), leaving each of them exhausted at the end. Owner-pool use is limited to moving the 60 s
// and hour windows of email-mail-unauth and the 15 min ip window into the past between phases (so one
// source can keep going); the final state has every limit spent.
func (e *pwaEnv) saturate(victim string, A netip.Addr) {
	e.t.Helper()
	pwaDisclose(e.t, "owner pool moves the 60 s/hour email-mail-unauth windows and the 15 min ip window of source A into the past between attacker phases")
	prefix := pwaPrefix(A)
	// (a) email-pw: 10 wrong passwords, the 11th is throttled
	for i := 1; i <= 10; i++ {
		if err := e.wrongLoginFrom(A, victim); !errors.Is(err, identity.ErrInvalidCredentials) {
			e.t.Fatalf("attacker password guess %d: %v", i, err)
		}
	}
	e.wantThrottle(e.wrongLoginFrom(A, victim), "attacker's 11th password check")
	// (b) resets from A at the maximum allowed rate; wrong reset codes on the first two challenges (PD5 bucket budget 10)
	key := victim + "‖" + prefix
	for i := 0; i < 10; i++ {
		if i > 0 {
			e.elapse("email-mail-unauth", key, "minute")
		}
		if i == 5 {
			e.elapse("email-mail-unauth", key, "hour")
		}
		e.elapseAll("ip", prefix, 1200)
		n := len(e.mailsTo(victim)) + 1
		c, err := e.pw.Reset(pwaBG, A, victim, "en")
		if err != nil {
			e.t.Fatalf("attacker reset %d: %v", i+1, err)
		}
		code := e.code(victim, n)
		wrongs := 2
		if i < 2 {
			wrongs = 5
		}
		for j := 0; j < wrongs; j++ {
			if _, err := e.pw.Complete(pwaBG, A, c.Binding, "reset", pwaWrongCode(code), pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
				e.t.Fatalf("attacker wrong reset code: %v", err)
			}
		}
	}
	e.elapse("email-mail-unauth", key, "minute")
	e.elapse("email-mail-unauth", key, "hour")
	e.elapseAll("ip", prefix, 1200)
	e.wantThrottle(func() error { _, err := e.pw.Reset(pwaBG, A, victim, "en"); return err }(), "attacker's 11th reset of the day")
	// PD5: A's bucket spent exactly the budget of 10 wrong codes, nothing beyond it was counted
	if got := e.q1(`SELECT coalesce(sum(attempts),0) FROM identity.email_challenges WHERE email=$1 AND purpose='reset' AND ip_hmac=$2`, victim, e.bucket("ip", prefix)); got != 10 {
		e.t.Errorf("wrong-code attempts recorded for the attacker's bucket = %d, want exactly 10 (PD5 budget)", got)
	}
	// (c) fill the ip bucket to 429 last, so everything is exhausted together
	e.elapseAll("ip", prefix, 1200)
	for i := 1; i <= 30; i++ {
		if err := e.wrongCompleteFrom(A); !errors.Is(err, identity.ErrInvalidCode) {
			e.t.Fatalf("ip-bucket fill %d: %v", i, err)
		}
	}
	e.wantThrottle(e.wrongCompleteFrom(A), "attacker's 31st call in the 15 min window")
}
