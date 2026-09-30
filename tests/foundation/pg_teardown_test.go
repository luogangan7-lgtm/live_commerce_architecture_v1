package foundation_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL backend teardown is asynchronous. When a client process exits or
// is killed, or a client closes a connection (pgconn.Close sends Terminate and
// closes the socket without waiting for the server; pgxpool.Close does that
// per connection), the server backend keeps its pg_stat_activity row and its
// session-level locks, including session advisory locks, until it has read
// the Terminate/EOF, aborted any open transaction and run proc_exit. A backend
// whose client vanished while it waited on a heavyweight lock only notices
// after that lock is granted and its statement finishes. On a loaded host, or
// in the 1-vCPU isolated fixture containers, an immediate snapshot taken right
// after cmd.Wait()/Close() can therefore still see the exiting backend.
//
// waitBackendsGone is NOT a relaxed gate. Its assertion subject is exactly
// "no backend matching `where` remains after its owning client exited or
// closed", and PostgreSQL only guarantees that eventually. A real leak (a live
// process still holding a pool, a pool that was never closed, a connection
// wedged in a transaction) never converges to 0 within the bound and still
// fails, now with the offending backends' pid, application_name, state,
// backend_start and query so it is diagnosable. The bound is a fixed constant,
// the helper is never wrapped in a retry, and any observer query error fails
// immediately.
//
// Use it ONLY where the owning client has already exited or closed. Counts
// taken while a process is still running, nonzero expectations and positive
// controls must remain single snapshots (mrPoolCount, miCount).
const (
	pgTeardownBound = 5 * time.Second
	pgTeardownPoll  = 25 * time.Millisecond
)

// waitBackendsGone polls `SELECT count(*) FROM pg_stat_activity a WHERE <where>`
// every pgTeardownPoll until it is 0, failing with `failure`, the last count and
// the matching backends once pgTeardownBound has elapsed. `where` is a
// predicate over pg_catalog.pg_stat_activity aliased as `a`; args bind to it.
func waitBackendsGone(t *testing.T, observer *pgxpool.Pool, failure, where string, args ...any) {
	t.Helper()
	count := func() int64 {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var n int64
		if err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity a WHERE `+where, args...).Scan(&n); err != nil {
			t.Fatalf("%s: backend teardown count failed: %v", failure, err)
		}
		return n
	}
	started := time.Now()
	deadline := started.Add(pgTeardownBound)
	for polls := 1; ; polls++ {
		n := count()
		if n == 0 {
			if polls > 1 {
				// Evidence that the snapshot race is real, not a leak.
				t.Logf("pg teardown: %q backends reached 0 after %s (%d polls)", failure, time.Since(started).Round(time.Millisecond), polls)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d backend(s) still present %s after the owning client exited/closed: %s",
				failure, n, pgTeardownBound, pgDescribeBackends(t, observer, where, args...))
		}
		time.Sleep(pgTeardownPoll) // bounded observation of async backend exit; not causal ordering
	}
}

// pgDescribeBackends renders the backends matching `where` for a failure message.
func pgDescribeBackends(t *testing.T, observer *pgxpool.Pool, where string, args ...any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := observer.Query(ctx, `SELECT a.pid, coalesce(a.application_name,''), coalesce(a.state,''),
	 coalesce(a.backend_start::text,''), coalesce(a.wait_event_type||':'||a.wait_event,''), coalesce(left(a.query,200),'')
	 FROM pg_catalog.pg_stat_activity a WHERE `+where+` ORDER BY a.pid`, args...)
	if err != nil {
		return fmt.Sprintf("<describe failed: %v>", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pid int32
		var app, state, start, wait, query string
		if err := rows.Scan(&pid, &app, &state, &start, &wait, &query); err != nil {
			return fmt.Sprintf("<describe scan failed: %v>", err)
		}
		out = append(out, fmt.Sprintf("{pid=%d application_name=%q state=%q backend_start=%s wait=%q query=%q}", pid, app, state, start, wait, query))
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("<describe failed: %v>", err)
	}
	if len(out) == 0 {
		return "<no rows at describe time>"
	}
	return strings.Join(out, " ")
}

// waitPoolsGone is the post-exit/post-close counterpart of mrPoolCount: the
// same application_name filter, observed until the exiting backends are gone.
func waitPoolsGone(t *testing.T, f *testFixture, failure string, names ...string) {
	t.Helper()
	waitBackendsGone(t, f.owner, failure, `a.datname='lc_foundation_test' AND a.application_name=ANY($1::text[])`, names)
}

// waitAdvisoryLockReleased waits until no backend in the observer's database
// holds the one-argument (bigint) advisory lock `key`. For that form PostgreSQL
// stores classid = high 32 bits, objid = low 32 bits and objsubid = 1 (for
// 718020260920: classid=167, objid=760722488). A session advisory lock is only
// released when its owning backend exits, so this is the same eventual
// guarantee as waitBackendsGone and uses the same fixed bound and diagnostics.
func waitAdvisoryLockReleased(t *testing.T, observer *pgxpool.Pool, failure string, key int64) {
	t.Helper()
	high, low := int64(uint32(uint64(key)>>32)), int64(uint32(uint64(key)))
	waitBackendsGone(t, observer, failure, `a.pid IN (SELECT l.pid FROM pg_catalog.pg_locks l
	 WHERE l.locktype='advisory' AND l.granted AND l.classid::bigint=$1 AND l.objid::bigint=$2 AND l.objsubid=1
	 AND l.database=(SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname=pg_catalog.current_database()))`, high, low)
}
