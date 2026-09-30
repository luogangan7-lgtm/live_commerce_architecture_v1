package identity

// Pre-authentication rate limits (contract §6). Counters live in identity.auth_throttle behind the
// identity.auth_throttle_hit definer; the bucket key is HMAC-SHA256(auth_pepper, kind ":" value) so no
// raw ip or email is stored (A2). Every check here runs before hashing, HIBP, mail or any existence
// lookup, so an over-limit answer is identical for known and unknown emails.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"log/slog"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
)

// ThrottleError is a per-client limit hit (HTTP 429 throttled). RetryAfter is the time to the end of
// the window that was exceeded, rounded up to whole seconds.
type ThrottleError struct{ RetryAfter time.Duration }

func (e ThrottleError) Error() string { return "throttled" }

// utc8Day is the finance-day offset (ruling Q11): zone offset east of UTC, so daily windows start at
// 00:00 UTC+8 = 16:00 UTC.
const utc8Day = 8 * 3600

// window is one fixed window of a bucket.
type window struct{ seconds, offset, limit int }

var (
	winIP        = []window{{900, 0, 30}}                                   // ip: 30 / 15 min
	winIP48      = []window{{900, 0, 60}}                                   // ip48: 60 / 15 min (IPv6 /48)
	winIPSignup  = []window{{3600, 0, 5}}                                   // ip-signup: 5 / hour
	winIP48Sign  = []window{{3600, 0, 20}}                                  // ip48-signup: 20 / hour
	winIPMail    = []window{{86400, utc8Day, 10}}                           // ip-mail-unauth: 10 / UTC+8 day
	winIP48Mail  = []window{{86400, utc8Day, 20}}                           // ip48-mail-unauth: 20 / UTC+8 day
	winEmailPW   = []window{{900, 0, 10}}                                   // email-pw: 10 / 15 min
	winEmailMail = []window{{60, 0, 1}, {3600, 0, 5}, {86400, utc8Day, 10}} // email-mail-unauth
	winEmailTot  = []window{{86400, utc8Day, 30}}                           // email-mail-unauth-total
	winMailLogin = []window{{60, 0, 1}, {3600, 0, 5}, {86400, utc8Day, 5}}  // email-mail-login
	winBinding   = []window{{600, 0, 10}}                                   // binding: 10 / 10 min
)

// bucket is one throttle key with its windows. global buckets answer ErrMailUnavailable (503, fail
// closed, O-B) instead of ThrottleError.
type bucket struct {
	kind, value string
	wins        []window
	global      bool
}

// bucketKey is HMAC-SHA256(pepper, kind ":" value) (A2). Also used for the challenge/audit ip_hmac.
func (p *Passwords) bucketKey(kind, value string) []byte {
	m := hmac.New(sha256.New, p.policy.Pepper)
	m.Write([]byte(kind + ":" + value))
	return m.Sum(nil)
}

// ipPrefix is the PD14 bucket of an address: IPv4 /32, IPv6 /64, IPv4-mapped IPv6 unmapped first.
func ipPrefix(ip netip.Addr) netip.Prefix {
	ip = ip.Unmap()
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32)
	}
	return netip.PrefixFrom(ip, 64).Masked()
}

// ip48Prefix is the IPv6 /48 bucket used for every step-1/complete call, sign-up and unauthenticated mail; ok is false for IPv4.
func ip48Prefix(ip netip.Addr) (netip.Prefix, bool) {
	ip = ip.Unmap()
	if !ip.Is6() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(ip, 48).Masked(), true
}

// throttle hits the buckets in order, one hit per window, in ONE transaction that always commits (a hit
// must persist even when it is over the limit). The first over-limit window stops the chain: later
// buckets and windows are not consumed (A3). A database failure fails closed as ErrUnavailable.
func (p *Passwords) throttle(ctx context.Context, buckets ...bucket) error {
	var over *bucket
	var overWin window
	err := withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		for i := range buckets {
			b := &buckets[i]
			for _, w := range b.wins {
				var hits int
				// identity.auth_throttle_hit: upsert +1 in the aligned window and return the count;
				// the definer keys every window of a bucket separately (F1, contract §15 A1) and also
				// purges old rows (I23). The limit is enforced here, not in SQL.
				if err := tx.QueryRow(ctx, `SELECT identity.auth_throttle_hit($1,$2,$3)`, p.bucketKey(b.kind, b.value), w.seconds, w.offset).Scan(&hits); err != nil {
					return err
				}
				if hits > w.limit {
					over, overWin = b, w
					return nil
				}
			}
		}
		return nil
	})
	if err != nil {
		return ErrUnavailable
	}
	if over == nil {
		return nil
	}
	// I11: the audit vocabulary has a `throttled` action but no definer to write it with the bucket
	// kind (contract §4.2), so the kind is logged instead. No ip, email or key material.
	slog.Warn("password_auth_throttled", "bucket", over.kind)
	if over.global {
		return ErrMailUnavailable
	}
	return ThrottleError{RetryAfter: retryAfter(time.Now(), overWin)}
}

// retryAfter is the time from now to the end of the window w containing now, rounded up to seconds (>= 1 s).
// offset is the zone offset east of UTC (28800 = UTC+8), so daily windows start at 16:00 UTC; it must
// match identity.auth_throttle_hit's alignment.
func retryAfter(now time.Time, w window) time.Duration {
	sec := now.Unix()
	start := ((sec+int64(w.offset))/int64(w.seconds))*int64(w.seconds) - int64(w.offset)
	end := time.Unix(start+int64(w.seconds), 0)
	d := end.Sub(now)
	if d < time.Second {
		return time.Second
	}
	return ((d + time.Second - 1) / time.Second) * time.Second
}

// mailShares are the §6 splits of COMMERCE_MAIL_DAILY_CAP per UTC+8 day: floor(cap x pct / 100).
func mailShares(dailyCap int) (unauth, loginNew, login int) {
	return dailyCap * 40 / 100, dailyCap * 15 / 100, dailyCap * 45 / 100
}

func (p *Passwords) globalBucket(kind string, limit int) bucket {
	return bucket{kind: kind, wins: []window{{86400, utc8Day, limit}}, global: true}
}

// unauthMailBuckets is the §6 chain for sign-up, exists-notice and reset mail, in table order; every
// bucket is hit before the existence lookup (PD6).
func (p *Passwords) unauthMailBuckets(email string, ip netip.Addr) []bucket {
	pfx := ipPrefix(ip).String()
	out := []bucket{{kind: "ip-mail-unauth", value: pfx, wins: winIPMail}}
	if p48, ok := ip48Prefix(ip); ok {
		out = append(out, bucket{kind: "ip48-mail-unauth", value: p48.String(), wins: winIP48Mail})
	}
	unauth, _, _ := mailShares(p.policy.MailDailyCap)
	return append(out,
		bucket{kind: "email-mail-unauth", value: email + "‖" + pfx, wins: winEmailMail},
		bucket{kind: "email-mail-unauth-total", value: email, wins: winEmailTot},
		p.globalBucket("global-mail-unauth", unauth))
}

// ipBuckets are the buckets hit by every step-1 and complete call: `ip` (/32 or /64) and, for IPv6, `ip48`.
// ip48 bounds what one /48 (65,536 /64s) can push into the PD3 hash limiter before any per-binding or
// per-email bucket can stop it (round-1 review P2: random bindings / dummy-PHC verifies).
func ipBuckets(ip netip.Addr) []bucket {
	out := []bucket{{kind: "ip", value: ipPrefix(ip).String(), wins: winIP}}
	if p48, ok := ip48Prefix(ip); ok {
		out = append(out, bucket{kind: "ip48", value: p48.String(), wins: winIP48})
	}
	return out
}
