package foundation_test

// SL05 (contracts/stripe-live-enable-v1.md §11 SL05, §5.4): the LIVE webhook ingress, HTTP_PG. Prefix `sli`.
//
// The REAL handler (stripewebhook.NewInbox(..., "LIVE") + NewHandler behind httptest over the commerce_stripe_ingress
// pool), signed with the endpoint's real sealed secret (OpenLive registrar, no network). Asserted from the contract:
//   - a LIVE endpoint is admitted on the LIVE ingress (signed event of a LIVE attempt -> 200, one ACCEPTED receipt, one
//     signal and one payment_signal_v1 job);
//   - a SANDBOX endpoint id on the LIVE ingress is the existing fixed 404 and vice versa (a SANDBOX ingress rejects a LIVE
//     endpoint id): no receipt, no signal, no job;
//   - a verified event with livemode=false on a LIVE endpoint is the existing quarantine `livemode_mismatch` (200, no
//     signal, no job, stored once);
//   - no secret, body, signature or customer-PII sentinel reaches the logs or any table (SP13 sentinels).
// Owner-pool disclosure: the LIVE session pin (payments.stripe_sessions), the SANDBOX session pin, the DB-wide sentinel scan
// (read only) and the receipt/signal/job counts (read only). No fake key literal is written; whsec_ values are generated.

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/payments/stripewebhook"
	"livecommerce/internal/platform"
)

type sliResult struct {
	status int
	body   string
}

func sliPost(t *testing.T, base *httptest.Server, endpoint, secret string, body []byte) sliResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base.URL+"/v1/stripe/webhook/"+endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripetest.SignWebhook(secret, body, time.Now()))
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	return sliResult{res.StatusCode, strings.TrimSpace(string(raw))}
}

func sliExpect(t *testing.T, what string, r sliResult, status int, body string) {
	t.Helper()
	if r.status != status || r.body != body {
		t.Fatalf("%s: status=%d body=%q want %d %q", what, r.status, r.body, status, body)
	}
}

type sliCounts struct{ receipts, signals, jobs int }

func sliCount(t *testing.T, f *testFixture, endpoints ...string) sliCounts {
	t.Helper()
	var c sliCounts
	if err := f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM payments.stripe_webhook_receipts WHERE endpoint_id=ANY($1::uuid[])),
	 (SELECT count(*) FROM payments.stripe_signals WHERE receipt_id IN (SELECT id FROM payments.stripe_webhook_receipts WHERE endpoint_id=ANY($1::uuid[]))),
	 (SELECT count(*) FROM river_payment.river_job WHERE kind='payment_signal_v1')`, endpoints).Scan(&c.receipts, &c.signals, &c.jobs); err != nil {
		t.Fatal(err)
	}
	return c
}

func sliReceipt(t *testing.T, f *testFixture, endpoint, event string) (disposition, reason string, signal, ok bool) {
	t.Helper()
	err := f.owner.QueryRow(context.Background(), `SELECT disposition,reason,signal_id IS NOT NULL FROM payments.stripe_webhook_receipts WHERE endpoint_id=$1::uuid AND event_id=$2`, endpoint, event).Scan(&disposition, &reason, &signal)
	return disposition, reason, signal, err == nil
}

func TestStripeSL05LiveIngress(t *testing.T) {
	ctx := context.Background()
	logs := &swhLogBuffer{}
	oldSlog, oldLog := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(logs)
	t.Cleanup(func() { slog.SetDefault(oldSlog); log.SetOutput(oldLog) })

	base := pwIsolatedFixture(t)
	l := slrNew(t, base).live(t)
	f := l.f
	attempt := l.mustBegin(t, l.p)
	session, _ := l.pin(t, attempt)
	liveEvent := func(kind string, mutate func(*stripetest.EventOpts)) (string, []byte) {
		o := stripetest.EventOpts{ID: swhEventID(kind), Type: "checkout.session.completed", SessionID: session, ClientRef: attempt, Attempt: attempt, Livemode: true}
		if mutate != nil {
			mutate(&o)
		}
		return o.ID, stripetest.EventBody(o)
	}

	// A SANDBOX endpoint in another store, and a SANDBOX-profile ingress over the same ingress login family.
	sb := slrNew(t, base)
	st := sb.seed(t, sb.p)
	sbSecret := swhSecret()
	sbEndpoint, _, err := sb.reg.SetWebhookEndpoint(ctx, st.scope, stripeadmin.EndpointInput{ConnectionID: st.connection, AccountID: st.account, Profile: "PROVIDER_MOCK", Enabled: true,
		Secrets: accounts.StripeWebhookSecrets{CurrentSecret: sbSecret}})
	if err != nil {
		t.Fatalf("SANDBOX endpoint: %v", err)
	}
	pool, err := platform.OpenStripeIngressPool(ctx, sstLogin(t, f, "commerce_stripe_ingress"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sandboxInbox, err := stripewebhook.NewInbox(ctx, pool, l.signing, "PROVIDER_MOCK")
	if err != nil {
		t.Fatalf("SANDBOX-profile inbox: %v", err)
	}
	sandboxHandler, err := stripewebhook.NewHandler(sandboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	sandboxServer := httptest.NewServer(sandboxHandler)
	t.Cleanup(sandboxServer.Close)

	t.Run("live_endpoint_is_admitted_and_produces_one_signal_and_job", func(t *testing.T) {
		before := sliCount(t, f, l.endp)
		id, body := liveEvent("ok", nil)
		sliExpect(t, "LIVE event on the LIVE endpoint", sliPost(t, l.hook, l.endp, l.secret, body), 200, `{"received":true}`)
		d, reason, signal, ok := sliReceipt(t, f, l.endp, id)
		if !ok || d != "ACCEPTED" || reason != "accepted" || !signal {
			t.Fatalf("receipt: %s %s signal=%v ok=%v", d, reason, signal, ok)
		}
		after := sliCount(t, f, l.endp)
		if after.receipts != before.receipts+1 || after.signals != before.signals+1 || after.jobs != before.jobs+1 {
			t.Fatalf("counts %+v -> %+v, want +1/+1/+1", before, after)
		}
		var env, live string
		if err := f.owner.QueryRow(context.Background(), `SELECT r.environment,e.environment||'/'||e.execution_profile FROM payments.stripe_webhook_receipts r JOIN payments.stripe_webhook_endpoints e ON e.endpoint_id=r.endpoint_id WHERE r.endpoint_id=$1 AND r.event_id=$2`, l.endp, id).Scan(&env, &live); err != nil {
			t.Fatal(err)
		}
		if env != "LIVE" || live != "LIVE/LIVE" {
			t.Fatalf("receipt environment=%s endpoint=%s, want LIVE and LIVE/LIVE", env, live)
		}
		// A redelivery is deduplicated exactly as on SANDBOX (no second signal or job).
		sliExpect(t, "redelivery", sliPost(t, l.hook, l.endp, l.secret, body), 200, `{"received":true}`)
		if again := sliCount(t, f, l.endp); again != after {
			t.Fatalf("redelivery created rows: %+v -> %+v", after, again)
		}
	})

	t.Run("livemode_false_on_a_live_endpoint_is_quarantined", func(t *testing.T) {
		before := sliCount(t, f, l.endp)
		id, body := liveEvent("testmode", func(o *stripetest.EventOpts) { o.Livemode = false })
		sliExpect(t, "livemode=false event", sliPost(t, l.hook, l.endp, l.secret, body), 200, `{"received":true}`)
		d, reason, signal, ok := sliReceipt(t, f, l.endp, id)
		if !ok || d != "QUARANTINED" || reason != "livemode_mismatch" || signal {
			t.Fatalf("receipt: %s %s signal=%v ok=%v", d, reason, signal, ok)
		}
		after := sliCount(t, f, l.endp)
		if after.receipts != before.receipts+1 || after.signals != before.signals || after.jobs != before.jobs {
			t.Fatalf("a quarantined event changed signals or jobs: %+v -> %+v", before, after)
		}
	})

	t.Run("sandbox_endpoint_is_404_on_the_live_ingress_and_live_endpoint_404_on_the_sandbox_ingress", func(t *testing.T) {
		before := sliCount(t, f, l.endp, sbEndpoint)
		_, sbBody := func() (string, []byte) {
			o := stripetest.EventOpts{ID: swhEventID("sbx"), Type: "checkout.session.completed", SessionID: "cs_test_" + t04Tag(), ClientRef: randomUUID(), Attempt: randomUUID()}
			return o.ID, stripetest.EventBody(o)
		}()
		sliExpect(t, "SANDBOX endpoint on the LIVE ingress", sliPost(t, l.hook, sbEndpoint, sbSecret, sbBody), 404, swhErr("not_found"))
		_, liveBody := liveEvent("crossed", nil)
		sliExpect(t, "LIVE endpoint on the SANDBOX ingress", sliPost(t, sandboxServer, l.endp, l.secret, liveBody), 404, swhErr("not_found"))
		// An unknown endpoint id is the same fixed 404 (no oracle for which endpoints exist).
		sliExpect(t, "unknown endpoint on the LIVE ingress", sliPost(t, l.hook, randomUUID(), l.secret, liveBody), 404, swhErr("not_found"))
		if after := sliCount(t, f, l.endp, sbEndpoint); after != before {
			t.Fatalf("a refused delivery changed rows: %+v -> %+v", before, after)
		}
	})

	t.Run("no_secret_body_signature_or_pii_in_logs_or_tables", func(t *testing.T) {
		tag := t04Tag()
		sentinels := []string{"sli-email-" + tag + "@example.invalid", "Sli Sentinel Buyer " + tag, "https://checkout.stripe.com/c/pay/cs_sli_" + tag,
			"sli-client-secret-" + tag, "sli-card-4242-" + tag}
		id, body := liveEvent("pii", func(o *stripetest.EventOpts) {
			o.Extra = map[string]any{"customer_details": map[string]any{"email": sentinels[0], "name": sentinels[1]},
				"url": sentinels[2], "client_secret": sentinels[3], "payment_method_details": map[string]any{"card": sentinels[4]}}
		})
		sig := stripetest.SignWebhook(l.secret, body, time.Now())
		post := func(signature string) sliResult {
			req, err := http.NewRequest(http.MethodPost, l.hook.URL+"/v1/stripe/webhook/"+l.endp, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Stripe-Signature", signature)
			res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
			return sliResult{res.StatusCode, strings.TrimSpace(string(raw))}
		}
		if r := post(sig); r.status != 200 || strings.Contains(r.body, tag) {
			t.Fatalf("PII event: %+v", r)
		}
		if _, _, _, ok := sliReceipt(t, f, l.endp, id); !ok {
			t.Fatal("PII event not admitted")
		}
		post(strings.Replace(sig, "v1=", "v1=0", 1)) // provoke the failure path so its logs are captured
		needles := []string{sig, sig[strings.Index(sig, "v1=")+3:], l.secret, strings.TrimPrefix(l.secret, "whsec_"), string(body)}
		needles = append(needles, sentinels...)
		for _, n := range needles {
			if strings.Contains(logs.String(), n) {
				t.Fatalf("logs contain a forbidden value (%d bytes)", len(n))
			}
		}
		if hits := swhScan(t, f, needles...); len(hits) != 0 {
			t.Fatalf("database rows store a forbidden value: %v", hits)
		}
	})
}
