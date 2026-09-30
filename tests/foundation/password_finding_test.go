// F1 regression gate (merchant-password-auth-v1 §4.2, amended 2026-09-30): identity.auth_throttle_hit keys each
// window of a bucket separately. Originally a red-on-purpose reproduction behind the `finding` build tag; the
// fix landed with the tag removed, so this is now part of the default suite.

package foundation_test

// FINDING F1 (not a PA gate; named outside the ^TestPasswordPA regex). Reproduces a defect the PA gates could not
// expose because the wall clock cannot be moved.
//
// Contract §4.1 keys identity.auth_throttle by (bucket, window_start) and §6 lets ONE bucket carry several
// windows (email-mail-unauth: 60 s, 1 h, UTC+8 day; email-mail-login: 60 s, 1 h, UTC+8 day). Whenever two
// windows of the same bucket start at the same instant they land on the SAME row and each hit is counted
// twice: the 60 s and 1 h windows coincide during the first minute of every hour, and the 1 h and day windows
// coincide for the whole first hour of every UTC+8 day (16:00-17:00 UTC). In that hour a merchant's login-mail
// limit (5 / hour, 5 / day) trips on the 3rd mail instead of the 6th, and sign-up/reset mail (5 / hour) on the 4th.
// This test builds the coincidence deterministically at any time of day by choosing the day window's offset so its
// start equals the current minute start, then asks for the counts of two windows of one bucket.
// Expected (contract intent): each window counts its own hits, so both calls return 1 in 2 rows.
// Code: migrations/0070_merchant_password_auth.sql identity.auth_throttle_hit stores sha256(p_bucket || length || offset)
// (amended §4.2), so the rows are counted through the two derived keys.

import (
	"testing"
	"time"
)

func TestPasswordFindingF1ThrottleWindowsShareRows(t *testing.T) {
	e := newPwa(t)
	for attempt := 0; attempt < 5; attempt++ {
		now := time.Now().UTC().Unix()
		minuteStart := now - now%60
		offset := (86400 - minuteStart%86400) % 86400 // a day window that starts exactly at this minute's start
		b := randomBytes(32)
		var minuteHits, dayHits int
		if err := e.pool.QueryRow(pwaBG, `SELECT identity.auth_throttle_hit($1,60,0)`, b).Scan(&minuteHits); err != nil {
			t.Fatal(err)
		}
		if err := e.pool.QueryRow(pwaBG, `SELECT identity.auth_throttle_hit($1,86400,$2)`, b, offset).Scan(&dayHits); err != nil {
			t.Fatal(err)
		}
		if time.Now().UTC().Unix()/60 != minuteStart/60 {
			continue // the minute rolled between the two calls; measure again
		}
		var rows int
		if err := e.f.owner.QueryRow(pwaBG, `SELECT count(*) FROM identity.auth_throttle WHERE bucket IN (sha256($1::bytea||int4send(60)||int4send(0)), sha256($1::bytea||int4send(86400)||int4send($2::int)))`, b, offset).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		t.Logf("first window (60 s) hits=%d, second window (day, same start) hits=%d, rows=%d", minuteHits, dayHits, rows)
		if minuteHits != 1 || dayHits != 1 || rows != 2 {
			t.Fatalf("two windows of one bucket that start at the same instant share a counter: hits %d then %d in %d row(s), want 1 and 1 in 2 rows (F1)", minuteHits, dayHits, rows)
		}
		return
	}
	t.Fatal("the minute rolled over on every attempt")
}
