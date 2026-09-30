package foundation_test

// SL09 (contracts/stripe-live-enable-v1.md §8, §11 SL09; ops brief O4): watchdog check W11 (a-f), REAL_PG + shell.
// Prefix `slw`. Each §8 signal is driven RED on a seeded LIVE fixture in an isolated PG container and back to GREEN by
// clearing exactly that condition, with the real deploy/scripts/watchdog.sh running inside the pinned Linux image (bash 5,
// GNU coreutils) against that PG through a stub `docker` (compose ps/config/exec/inspect/info only; `exec` pipes the
// script's SQL to psql, so the SQL under test is the script's own). Only the W11 lines and the failing-id summary are asserted:
// W1-W10 are not this gate's business and the stub gives them a benign world.
//
// Seeds use real definers wherever one exists (LIVE attempts through the hosted service, refunds through the LIVE merchant
// handler, observations through payments.apply_capture, webhook receipts through the real LIVE ingress). Disclosed
// owner-pool changes (each logged): observation rows, session/refund pins, aging of timestamps and state flips under
// session_replication_role=replica, one REFUND_HISTORY review row, ten SANDBOX QUARANTINED receipts as noise.
// Output discipline: counts and check ids only (no UUID, account, session or refund id), including the alert POST body.

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments/stripeadmin"
)

const slwDriver = `set -u
W=$(mktemp -d)
mkdir -p "$W/bin" "$W/cfg" "$W/state" /dev/shm/lcb/dumps/2026-09-30 /dev/shm/lcb/base/2026-09-30
cat >"$W/bin/docker" <<'STUB'
#!/usr/bin/env bash
args=" $* "
case "$args" in
  *" info "*) echo /dev/shm ;;
  *" inspect "*) case "$args" in *State.Status*) echo 'running|none|0' ;; *) echo 'postgres:18' ;; esac ;;
  *" compose "*)
    case "$args" in
      *" config --services "*) echo postgres ;;
      *" ps -q "*) echo cid1 ;;
      *" exec "*) exec env PGPASSWORD="$PGPASSWORD" psql -X -q -At -F '|' -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 5432 -U postgres -d lc_foundation_test ;;
    esac ;;
esac
exit 0
STUB
cat >"$W/bin/curl" <<'STUB'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do if [ "$1" = -d ]; then printf '%s' "$2" >"$CURLBODY"; fi; shift; done
exit 0
STUB
chmod +x "$W/bin/docker" "$W/bin/curl"
printf 'COMPOSE_PROJECT_NAME=slw\nCOMPOSE_PROFILES=db\nLC_ENVIRONMENT=smoke\n' >"$W/cfg/compose.env"
: >"$W/curl.body"
( env -i PATH="$W/bin:/usr/bin:/bin" HOME="$W" LC_COMPOSE_ENV="$W/cfg/compose.env" LC_CONFIG_DIR="$W/cfg" LC_STATE_DIR="$W/state" LC_BACKUP_DIR=/dev/shm/lcb \
    PGPASSWORD="$PGPASSWORD" CURLBODY="$W/curl.body" LC_ALERT_WEBHOOK_URL=http://alerts.invalid/hook __EXTRA__ bash /repo/deploy/scripts/watchdog.sh ) >"$W/out" 2>"$W/err"
rc=$?
printf '@@RC %s\n' "$rc"
sed 's/^/@@OUT /' "$W/out"; sed 's/^/@@ERR /' "$W/err"; printf '@@CURL %s\n' "$(cat "$W/curl.body")"
`

type slwOut struct {
	rc         int
	out, err   string
	w11        map[string]string // id -> full line
	failedList string            // ids named by the watchdog's failure summary on stderr
	curl       string
}

var slwLine = regexp.MustCompile(`^(W11[a-f]) (PASS|FAIL) (.+)$`)
var slwUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func slwContainerName(t *testing.T, f *testFixture) (name, password string) {
	t.Helper()
	u, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ = u.User.Password()
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}} {{.Ports}}").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	// The isolated fixture publishes 127.0.0.1:<port>->5432/tcp; the port identifies exactly one container.
	for _, line := range strings.Split(string(out), "\n") {
		if name, ports, ok := strings.Cut(strings.TrimSpace(line), " "); ok && strings.Contains(ports, "127.0.0.1:"+u.Port()+"->5432/tcp") {
			return name, password
		}
	}
	t.Fatalf("cannot find the isolated PG container for port %s", u.Port())
	return name, password
}

// slwRun executes the real watchdog against the fixture DB; extra are LC_W11_* style NAME=VALUE overrides.
func slwRun(t *testing.T, f *testFixture, extra ...string) slwOut {
	t.Helper()
	name, password := slwContainerName(t, f)
	quoted := make([]string, len(extra))
	for i, kv := range extra {
		name, value, _ := strings.Cut(kv, "=")
		quoted[i] = name + "='" + strings.ReplaceAll(value, "'", `'\''`) + "'" // a hostile value must reach the script as one word
	}
	script := strings.Replace(slwDriver, "__EXTRA__", strings.Join(quoted, " "), 1)
	stdout, stderr, rc := slxContainer(t, script, []string{"PGPASSWORD"}, []string{"PGPASSWORD=" + password}, name)
	if rc != 0 {
		t.Fatalf("watchdog driver: rc=%d %s", rc, stderr)
	}
	o := slwOut{w11: map[string]string{}}
	for _, line := range strings.Split(stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "@@RC "):
			_, _ = fmt.Sscanf(strings.TrimPrefix(line, "@@RC "), "%d", &o.rc)
		case strings.HasPrefix(line, "@@OUT "):
			l := strings.TrimPrefix(line, "@@OUT ")
			o.out += l + "\n"
			if m := slwLine.FindStringSubmatch(l); m != nil {
				o.w11[m[1]] = l
			}
		case strings.HasPrefix(line, "@@ERR "):
			l := strings.TrimPrefix(line, "@@ERR ")
			o.err += l + "\n"
			if strings.Contains(l, "watchdog FAIL:") {
				o.failedList = l
			}
		case strings.HasPrefix(line, "@@CURL "):
			o.curl = strings.TrimPrefix(line, "@@CURL ")
		}
	}
	return o
}

// slwExpect asserts the W11 lines: every id in red must be FAIL, every other id PASS, all six present, counts/ids only.
func slwExpect(t *testing.T, what string, o slwOut, red ...string) {
	t.Helper()
	isRed := map[string]bool{}
	for _, r := range red {
		isRed[r] = true
	}
	for _, id := range []string{"W11a", "W11b", "W11c", "W11d", "W11e", "W11f"} {
		line, ok := o.w11[id]
		if !ok {
			t.Fatalf("%s: no %s line in the watchdog output:\n%s%s", what, id, o.out, o.err)
		}
		want := "PASS"
		if isRed[id] {
			want = "FAIL"
		}
		m := slwLine.FindStringSubmatch(line)
		if m == nil || m[2] != want {
			t.Errorf("%s: %s, want %s", what, line, want)
		}
		detail := m[3]
		if !regexp.MustCompile(`^[a-z0-9_=. ]+$`).MatchString(detail) || !regexp.MustCompile(`[0-9]`).MatchString(detail) || slwUUID.MatchString(line) {
			t.Errorf("%s: %s carries more than counts", what, line)
		}
	}
	if len(red) > 0 {
		if o.rc == 0 {
			t.Errorf("%s: exit 0 although %v FAIL", what, red)
		}
		for _, id := range red {
			if !strings.Contains(o.failedList, id) {
				t.Errorf("%s: the failure summary %q does not name %s", what, o.failedList, id)
			}
			if !strings.Contains(o.curl, id) {
				t.Errorf("%s: the alert body %q does not carry %s", what, o.curl, id)
			}
		}
		if !regexp.MustCompile(`^\{"source":"live-commerce-watchdog","failed":"[A-Za-z0-9 ]+"\}$`).MatchString(o.curl) {
			t.Errorf("%s: alert body %q is not ids only", what, o.curl)
		}
	} else {
		for _, id := range []string{"W11a", "W11b", "W11c", "W11d", "W11e", "W11f"} {
			if strings.Contains(o.failedList, id) || strings.Contains(o.curl, id) {
				t.Errorf("%s: %s reported although green", what, id)
			}
		}
	}
	if slwUUID.MatchString(o.out+o.err+o.curl) || strings.Contains(o.out+o.err+o.curl, "acct_") {
		t.Errorf("%s: an identifier leaked into the watchdog output", what)
	}
}

func TestStripeSL09Watchdog(t *testing.T) {
	ctx := context.Background()
	f := pwIsolatedFixture(t)
	l := slrNew(t, f).live(t)

	// SANDBOX noise the LIVE-only checks must ignore: an old UNKNOWN operation, a fresh review, ten quarantined receipts and a
	// disabled endpoint under an enabled method.
	sb := slrNew(t, f)
	st := sb.seed(t, sb.p)
	sbAttempt := func() string {
		res, err := st.begin(sb.svc, t04Key("slw-sb"), st.input("zh-TW"))
		if err != nil {
			t.Fatalf("SANDBOX start: %v", err)
		}
		return res.AttemptID
	}()
	sbSecret := swhSecret()
	sbEndpoint, _, err := sb.reg.SetWebhookEndpoint(ctx, st.scope, stripeadmin.EndpointInput{ConnectionID: st.connection, AccountID: st.account, Profile: "PROVIDER_MOCK", Enabled: true,
		Secrets: accounts.StripeWebhookSecrets{CurrentSecret: sbSecret}})
	if err != nil {
		t.Fatal(err)
	}
	sb.pin(t, sbAttempt)
	crossed := sb.observe(t, sbAttempt, sb.sessionReport(t, sbAttempt, "complete", "paid", "succeeded", true)) // crossed for SANDBOX: opens a review
	if err := sb.apply(sbAttempt, crossed); err != nil {
		t.Fatal(err)
	}
	slrReplica(t, f, `UPDATE integration.operations SET created_at=created_at-interval '3 hours' WHERE id=$1`, sbAttempt)
	slrDisclose(t, "ten SANDBOX QUARANTINED receipts (noise for the LIVE-only W11e)")
	for i := 0; i < 10; i++ {
		mustExec(t, f.owner, `INSERT INTO payments.stripe_webhook_receipts(id,endpoint_id,environment,account_id,event_id,event_type,body_sha256,signed_at,disposition,reason)
		 VALUES(gen_random_uuid(),$1,'SANDBOX',$2,$3,'checkout.session.completed',$4,extract(epoch FROM now())::bigint,'QUARANTINED','livemode_mismatch')`,
			sbEndpoint, st.account, "evt_slw_sb_"+t04Tag(), randomBytes(32))
	}
	if _, _, err := sb.reg.SetWebhookEndpoint(ctx, st.scope, stripeadmin.EndpointInput{ConnectionID: st.connection, EndpointID: sbEndpoint, ExpectedVersion: 1, Profile: "PROVIDER_MOCK",
		Enabled: false, Secrets: accounts.StripeWebhookSecrets{CurrentSecret: sbSecret}}); err != nil {
		t.Fatal(err)
	}

	// LIVE attempts: A0 in flight (pinned, fresh), A1 captured with a refund, A2 crossed review, A3 review + work item.
	holds := []psHarness{l.p, l.newHold(t), l.newHold(t), l.newHold(t)}
	a0 := l.mustBegin(t, holds[0])
	session0, _ := l.pin(t, a0)
	a1 := l.mustBegin(t, holds[1])
	_, pi1 := l.capture(t, a1)
	status, out := l.requestRefund(holds[1], 2500, 2500)
	if status != 201 {
		t.Fatalf("LIVE refund answered %d: %v", status, out)
	}
	r1, _ := out["refund_id"].(string)
	a2 := l.mustBegin(t, holds[2])
	l.pin(t, a2)
	a3 := l.mustBegin(t, holds[3])
	l.pin(t, a3)

	t.Run("baseline_is_green_and_ignores_SANDBOX", func(t *testing.T) {
		o := slwRun(t, f)
		slwExpect(t, "baseline", o)
		if !strings.Contains(o.w11["W11a"], "W11a PASS ") {
			t.Fatal("W11a missing")
		}
	})

	t.Run("W11a_unknown_operation_older_than_60_minutes", func(t *testing.T) {
		slrReplica(t, f, `UPDATE integration.operations SET created_at=created_at-interval '2 hours' WHERE id=$1`, a0)
		slwExpect(t, "aged UNKNOWN LIVE session operation", slwRun(t, f), "W11a")
		// a threshold override moves the line: 300 minutes tolerates a 2 hour old operation
		slwExpect(t, "LC_W11_UNKNOWN_OP_MINUTES=300", slwRun(t, f, "LC_W11_UNKNOWN_OP_MINUTES=300"))
		slrReplica(t, f, `UPDATE integration.operations SET state='SUCCEEDED',result_code='slw_cleared' WHERE id=$1`, a0)
		slwExpect(t, "operation resolved", slwRun(t, f))
		// a LIVE refund operation counts too
		slrReplica(t, f, `UPDATE integration.operations SET created_at=created_at-interval '2 hours' WHERE id=$1`, r1)
		slwExpect(t, "aged UNKNOWN LIVE refund operation", slwRun(t, f), "W11a")
		slrReplica(t, f, `UPDATE integration.operations SET state='SUCCEEDED',result_code='slw_cleared' WHERE id=$1`, r1)
		slwExpect(t, "refund operation resolved", slwRun(t, f))
	})

	t.Run("W11b_review_case_on_a_LIVE_attempt_in_the_last_24_hours", func(t *testing.T) {
		hash := l.observe(t, a2, l.sessionReport(t, a2, "complete", "paid", "succeeded", false)) // crossed Livemode: the real definer opens a review
		if err := l.apply(a2, hash); err != nil {
			t.Fatal(err)
		}
		slwExpect(t, "fresh review case", slwRun(t, f), "W11b")
		slrReplica(t, f, `UPDATE payments.review_cases SET created_at=created_at-interval '2 days' WHERE attempt_id=$1`, a2)
		slwExpect(t, "review older than 24h", slwRun(t, f))
		slwExpect(t, "LC_W11_REVIEW_HOURS=72 brings it back", slwRun(t, f, "LC_W11_REVIEW_HOURS=72"), "W11b")
	})

	t.Run("W11c_work_item_in_REVIEW_REQUIRED", func(t *testing.T) {
		// A review followed by a paid observation: the real definer captures and parks the order for a human.
		crossedHash := l.observe(t, a3, l.sessionReport(t, a3, "complete", "paid", "succeeded", false))
		if err := l.apply(a3, crossedHash); err != nil {
			t.Fatal(err)
		}
		okHash := l.observe(t, a3, l.sessionReport(t, a3, "complete", "paid", "succeeded", true))
		if err := l.apply(a3, okHash); err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, f.owner, `SELECT count(*) FROM fulfillment.payment_work_items WHERE attempt_id=$1 AND state='REVIEW_REQUIRED'`, a3); n != 1 {
			t.Fatalf("fixture: REVIEW_REQUIRED work items=%d", n)
		}
		slrReplica(t, f, `UPDATE payments.review_cases SET created_at=created_at-interval '2 days' WHERE attempt_id=$1`, a3) // keep W11b out of this case
		slwExpect(t, "REVIEW_REQUIRED work item", slwRun(t, f), "W11c")
		slrReplica(t, f, `UPDATE fulfillment.payment_work_items SET state='READY' WHERE attempt_id=$1`, a3)
		slwExpect(t, "work item handled", slwRun(t, f))
	})

	t.Run("W11d_refund_without_a_terminal_fact_or_refund_review", func(t *testing.T) {
		slrReplica(t, f, `UPDATE payments.stripe_refunds SET requested_at=requested_at-interval '2 days',resend_until=resend_until-interval '2 days' WHERE id=$1`, r1)
		slwExpect(t, "unfinished LIVE refund older than 24h", slwRun(t, f), "W11d")
		re := l.pinRefund(t, r1, true)
		hash := l.observe(t, a1, l.refundReport(t, r1, a1, pi1, re, "succeeded", 2500, true))
		if err := l.apply(a1, hash); err != nil {
			t.Fatal(err)
		}
		slwExpect(t, "refund has its terminal fact", slwRun(t, f))
		// an open REFUND_HISTORY review counts whatever its age
		slrDisclose(t, "payments.review_cases REFUND_HISTORY (aged 3 days) and its removal")
		var obs []byte
		if err := f.owner.QueryRow(ctx, `SELECT report_hash FROM payments.provider_observations WHERE attempt_id=$1 ORDER BY received_at LIMIT 1`, a1).Scan(&obs); err != nil {
			t.Fatal(err)
		}
		mustExec(t, f.owner, `INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash,created_at) VALUES($1,$2,$3,'REFUND_HISTORY',$4,now()-interval '3 days')`,
			l.scope.TenantID, l.scope.StoreID, a1, obs)
		slwExpect(t, "old REFUND_HISTORY review", slwRun(t, f), "W11d")
		slrReplica(t, f, `DELETE FROM payments.review_cases WHERE attempt_id=$1 AND reason='REFUND_HISTORY'`, a1)
		slwExpect(t, "review closed", slwRun(t, f))
	})

	t.Run("W11e_quarantined_or_ignored_LIVE_receipts_in_the_last_hour", func(t *testing.T) {
		send := func(n int) {
			for i := 0; i < n; i++ {
				body := stripetest.EventBody(stripetest.EventOpts{ID: swhEventID("slwq"), Type: "checkout.session.completed", SessionID: session0, ClientRef: a0, Attempt: a0, Livemode: false})
				if st := l.deliver(t, l.endp, body); st != 200 {
					t.Fatalf("LIVE webhook answered %d", st)
				}
			}
		}
		send(5)
		slwExpect(t, "5 quarantined receipts is the threshold (> 5 fails)", slwRun(t, f))
		send(1)
		slwExpect(t, "6 quarantined receipts", slwRun(t, f), "W11e")
		slwExpect(t, "LC_W11_RECEIPT_MAX=6 tolerates them", slwRun(t, f, "LC_W11_RECEIPT_MAX=6"))
		slrReplica(t, f, `UPDATE payments.stripe_webhook_receipts SET received_at=received_at-interval '2 hours' WHERE endpoint_id=$1 AND environment='LIVE' AND disposition='QUARANTINED'`, l.endp)
		slwExpect(t, "receipts older than the window", slwRun(t, f))
	})

	// §8 row 1 says "UNKNOWN older than 60 min" and LD9 says W11 surfaces LIVE trouble. The payment worker settles an operation with
	// integration.finish_stripe_query/finish_stripe_refund, which complete it as state UNKNOWN with the terminal reason codes
	// stripe_terminal_observed / stripe_refund_terminal (real definers, real leases, below). A paid and refunded LIVE order is not
	// trouble: if W11a counted it, every LIVE payment would page the owner 60 minutes after it succeeded.
	t.Run("W11a_a_settled_LIVE_payment_and_refund_are_not_trouble", func(t *testing.T) {
		hold := l.newHold(t)
		a4 := l.mustBegin(t, hold)
		_, pi4 := l.capture(t, a4)
		status, out := l.requestRefund(hold, 2500, 2500)
		if status != 201 {
			t.Fatalf("LIVE refund answered %d: %v", status, out)
		}
		r4, _ := out["refund_id"].(string)
		re4 := l.pinRefund(t, r4, true)
		if err := l.apply(a4, l.observe(t, a4, l.refundReport(t, r4, a4, pi4, re4, "succeeded", 2500, true))); err != nil {
			t.Fatal(err)
		}
		finish := func(id, sql string) {
			token := randomBytes(32)
			var disposition string
			var generation int64
			if err := l.p.worker.QueryRow(ctx, `SELECT disposition,generation FROM integration.claim_operation($1::uuid,60,$2::bytea)`, id, token).Scan(&disposition, &generation); err != nil || disposition != "claimed" {
				t.Fatalf("claim %s: %q %v", id, disposition, err)
			}
			if _, err := l.p.worker.Exec(ctx, sql, id, generation, token); err != nil {
				t.Fatalf("finish: %v", err)
			}
		}
		finish(a4, `SELECT integration.finish_stripe_query($1::uuid,$2::bigint,$3::bytea,'LIVE','stripe_terminal_observed')`)
		finish(r4, `SELECT integration.finish_stripe_refund($1::uuid,$2::bigint,$3::bytea,'LIVE','stripe_refund_terminal')`)
		for _, id := range []string{a4, r4} {
			var state, code string
			if err := f.owner.QueryRow(ctx, `SELECT state,result_code FROM integration.operations WHERE id=$1`, id).Scan(&state, &code); err != nil {
				t.Fatal(err)
			}
			t.Logf("settled operation state=%s result_code=%s (what the worker leaves behind)", state, code)
			slrReplica(t, f, `UPDATE integration.operations SET created_at=created_at-interval '2 hours' WHERE id=$1`, id)
			// Restore afterwards whatever the verdict, so a W11a defect here cannot mask the W11b-f subtests that follow.
			t.Cleanup(func() {
				slrReplica(t, f, `UPDATE integration.operations SET created_at=created_at+interval '2 hours' WHERE id=$1`, id)
			})
		}
		slwExpect(t, "settled LIVE payment and refund older than 60 minutes", slwRun(t, f))
	})

	t.Run("W11f_LIVE_endpoint_disabled_while_the_method_is_open", func(t *testing.T) {
		if _, _, err := l.regLive.SetWebhookEndpoint(ctx, l.scope, stripeadmin.EndpointInput{ConnectionID: l.conn, EndpointID: l.endp, ExpectedVersion: 1, Profile: "LIVE", Enabled: false,
			Secrets: accounts.StripeWebhookSecrets{CurrentSecret: l.secret}}); err != nil {
			t.Fatalf("disable the LIVE endpoint: %v", err)
		}
		slwExpect(t, "disabled endpoint under an enabled+visible method", slwRun(t, f), "W11f")
		if _, err := l.twMethod(t, l.qual, l.version, false, 5000); err != nil {
			t.Fatal(err)
		}
		slwExpect(t, "method paused: no exposure left", slwRun(t, f))
	})

	t.Run("threshold_overrides_are_validated_integers", func(t *testing.T) {
		for _, bad := range []string{"LC_W11_REVIEW_MAX=abc", "LC_W11_UNKNOWN_OP_MINUTES=1;DROP", "LC_W11_RECEIPT_MAX=-1", "LC_W11_REFUND_HOURS=1000000000"} {
			o := slwRun(t, f, bad)
			name := strings.SplitN(bad, "=", 2)[0]
			if o.rc == 0 || len(o.w11) != 0 || !strings.Contains(o.err, name) {
				t.Errorf("%s: rc=%d w11=%d stderr=%q, want a refusal naming the variable before any SQL", bad, o.rc, len(o.w11), o.err)
			}
		}
	})
}
