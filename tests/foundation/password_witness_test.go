package foundation_test

// Second helper file for the PA03-PA09 gates: canary scans (logs, responses, identity.* text
// columns), two-transaction witnesses (pg_blocking_pids), and clock helpers for fixed-window limits.
// Not a gate. Touches identity.* through the owner pool (reads) and the identity login pool
// (definer calls with stored hashes, so no HMAC derivation is duplicated here). Owner-pool writes are
// limited to shifting throttle rows into the past ("elapse") and are disclosed by the callers.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- log capture --------------------------------------------------------------------------------

type pwaLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *pwaLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *pwaLogBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// pwaCaptureLogs routes the process-wide slog/log output (where internal/identity logs warnings)
// into a buffer for canary scans; restored on cleanup. Tests in this package do not run in parallel.
func pwaCaptureLogs(t *testing.T) *pwaLogBuf {
	t.Helper()
	buf := &pwaLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// pwaNoCanary fails when any needle appears in haystack (needles shorter than 6 bytes are refused
// so a scan can never be trivially satisfied by an empty or tiny string).
func pwaNoCanary(t *testing.T, where, haystack string, needles map[string]string) {
	t.Helper()
	for label, n := range needles {
		if len(n) < 6 {
			t.Fatalf("canary %s too short to be meaningful", label)
		}
		if strings.Contains(haystack, n) {
			t.Errorf("%s contains the %s canary", where, label)
		}
	}
}

// canaryScanDB searches every text/varchar column of the four identity.* password tables for the
// needles. Recipient emails legitimately live in password_credentials.email and
// email_challenges.email, so callers pass only values that must never be stored in clear
// (password, code, SMTP secret) here; the email canary is applied to auth_events and the
// auth_throttle bucket only through `notInAuditTables`.
func (e *pwaEnv) canaryScanDB(needles map[string]string) {
	e.t.Helper()
	rows, err := e.f.owner.Query(pwaBG, `SELECT table_name, column_name FROM information_schema.columns WHERE table_schema='identity' AND table_name IN ('password_credentials','email_challenges','auth_throttle','auth_events') AND data_type IN ('text','character varying','character')`)
	if err != nil {
		e.t.Fatal(err)
	}
	type col struct{ table, column string }
	var cols []col
	for rows.Next() {
		var c col
		_ = rows.Scan(&c.table, &c.column)
		cols = append(cols, c)
	}
	rows.Close()
	for label, n := range needles {
		if len(n) < 6 {
			e.t.Fatalf("canary %s too short", label)
		}
		for _, c := range cols {
			var hits int64
			q := fmt.Sprintf(`SELECT count(*) FROM identity.%s WHERE position($1 in %s) > 0`, pgx.Identifier{c.table}.Sanitize(), pgx.Identifier{c.column}.Sanitize())
			if err := e.f.owner.QueryRow(pwaBG, q, n).Scan(&hits); err != nil {
				e.t.Fatal(err)
			}
			if hits > 0 {
				e.t.Errorf("identity.%s.%s holds the %s canary in %d row(s)", c.table, c.column, label, hits)
			}
		}
	}
}

// --- fixed-window clock helpers ------------------------------------------------------------------

// pwaStableHour waits until the next hour boundary has at least 4 minutes of margin on both sides
// and the first minute of an hour is over: hourly and UTC+8-day windows (day boundary = 16:00 UTC, an
// hour boundary) cannot roll over mid-test, and a 60 s window never shares its start with an hour
// window (which would make two windows one row). Disclosed by t.Logf when it waits.
func pwaStableHour(t *testing.T) {
	t.Helper()
	for {
		into := time.Now().UTC().Unix() % 3600
		switch {
		case into < 90:
			d := time.Duration(91-into) * time.Second
			t.Logf("clock guard: %s past the hour start, waiting %s so 60 s and hour windows never share a start", time.Duration(into)*time.Second, d)
			time.Sleep(d)
		case into > 3600-240:
			d := time.Duration(3600-into+91) * time.Second
			t.Logf("clock guard: %s before the hour end, waiting %s so hour/day windows cannot roll over mid-test", time.Duration(3600-into)*time.Second, d)
			time.Sleep(d)
		default:
			return
		}
	}
}

// pwaStable15 waits when fewer than `margin` remain in the current 15 minute window (ip, email-pw).
func pwaStable15(t *testing.T, margin time.Duration) { pwaAwaitSafeWindow(t, 900, 0, margin) }

// elapse moves a bucket's current window of the given class ("minute" = the 60 s window, "hour" = the 1 h
// window) into the past so the next hit opens a new window, i.e. simulates 61 s / 2 h passing without
// sleeping (owner pool; the caller states why via pwaDisclose). Each window has its own stored key (F1 fix),
// so the class picks the key; only its newest row moves (earlier elapses leave older rows). Fails when no row moved so a typo cannot silently do nothing.
func (e *pwaEnv) elapse(kind, value, class string) {
	e.t.Helper()
	var q string
	var b []byte
	switch class {
	case "minute":
		b = e.storedKey(kind, value, 60, 0)
		q = `UPDATE identity.auth_throttle SET window_start = window_start - make_interval(secs => 61 + 60*(1 + floor(random()*1000000)::int)) WHERE bucket=$1 AND window_start = (SELECT max(window_start) FROM identity.auth_throttle WHERE bucket=$1)`
	case "hour":
		b = e.storedKey(kind, value, 3600, 0)
		q = `UPDATE identity.auth_throttle SET window_start = window_start - make_interval(hours => 2 + floor(random()*10000)::int) WHERE bucket=$1 AND window_start = (SELECT max(window_start) FROM identity.auth_throttle WHERE bucket=$1)`
	default:
		e.t.Fatalf("elapse class %q", class)
	}
	tag, err := e.f.owner.Exec(pwaBG, q, b)
	if err != nil {
		e.t.Fatalf("elapse(%s,%s): %v", kind, class, err)
	}
	if tag.RowsAffected() != 1 {
		e.t.Fatalf("elapse(%s,%s): %d rows moved, want 1 (bucket key or window class wrong)", kind, class, tag.RowsAffected())
	}
}

// --- two-transaction witness ---------------------------------------------------------------------

// pwaWitness runs `first` inside an open transaction on the identity pool, starts `second` in another
// transaction on a second connection, waits (<= 10 s) until PostgreSQL reports second's backend as
// blocked by first's backend (pg_blocking_pids), then commits first and returns both results. It
// proves two definer calls serialize on the same lock instead of interleaving.
func (e *pwaEnv) pwaWitness(first, second func(ctx context.Context, tx pgx.Tx) error) (err1, err2 error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(pwaBG, 60*time.Second)
	defer cancel()
	tx1, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	var pid1 int
	if err := tx1.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid1); err != nil {
		e.t.Fatal(err)
	}
	if err1 = first(ctx, tx1); err1 != nil {
		return err1, nil
	}
	tx2, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	var pid2 int
	if err := tx2.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid2); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- second(ctx, tx2) }()
	deadline := time.Now().Add(10 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var n int
		if err := e.f.owner.QueryRow(pwaBG, `SELECT count(*) FROM pg_stat_activity WHERE pid=$2 AND $1 = ANY(pg_blocking_pids(pid))`, pid1, pid2).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		if n == 1 {
			blocked = true
			break
		}
		select {
		case err2 = <-done: // second finished without blocking: no serialization
			_ = tx1.Commit(ctx)
			e.t.Errorf("second transaction was never blocked by the first (finished with %v): the definer calls do not serialize", err2)
			return nil, err2
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !blocked {
		e.t.Errorf("second transaction not reported as blocked by the first within 10 s")
	}
	if err := tx1.Commit(ctx); err != nil {
		err1 = err
	}
	select {
	case err2 = <-done:
	case <-time.After(20 * time.Second):
		e.t.Fatal("second transaction still blocked 20 s after the first committed")
	}
	if err2 == nil {
		err2 = tx2.Commit(ctx)
	}
	return err1, err2
}

// challengeRow returns binding_hash and code_hmac stored for the nth (0-based, by creation) challenge
// of (email, purpose); definer calls made directly need exactly these two values.
func (e *pwaEnv) challengeHashes(email, purpose string, nth int) (binding, code []byte) {
	e.t.Helper()
	if err := e.f.owner.QueryRow(pwaBG, `SELECT binding_hash, code_hmac FROM identity.email_challenges WHERE email=$1 AND purpose=$2 ORDER BY created_at, id OFFSET $3 LIMIT 1`, email, purpose, nth).Scan(&binding, &code); err != nil {
		e.t.Fatalf("challenge %d of %s: %v", nth, purpose, err)
	}
	return binding, code
}

// sqlComplete calls identity.complete_email_challenge inside tx with stored hashes; a wrong code is
// any 32 random bytes. Returns the outcome column.
func sqlComplete(ctx context.Context, tx pgx.Tx, binding, code []byte, purpose string, newHash *string) (string, error) {
	var outcome string
	var exp any
	err := tx.QueryRow(ctx, `SELECT outcome, expires_at FROM identity.complete_email_challenge($1,$2,$3,$4,$5,3600)`, binding, purpose, code, newHash, randomBytes(32)).Scan(&outcome, &exp)
	return outcome, err
}
