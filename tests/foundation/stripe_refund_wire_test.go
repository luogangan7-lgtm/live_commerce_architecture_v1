package foundation_test

// RF01 + RF02 (contracts/stripe-refund-v1.md §3, §3.1, §9): the refund wire adapter and the
// webhook projection, written from the contract and the frozen adapter signatures, never from
// the implementation. Prefix `srw`.
//
// Tier: UNIT for everything except the "pg parity" subtest (REAL_PG: needs the shared fixture
// database, otherwise that subtest is NOT_RUN). The adapter talks to the independent
// `stripetest` fake through the same http.RoundTripper seam the worker uses; request bodies
// are read from the fake's raw capture (stripetest.RefundBodies), not from the adapter.
// Evidence label: MOCK (fake Stripe) + MODEL_ONLY until the adapter is merged.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

const (
	srwAccount = "acct_RF01Wire000001"
	srwKey     = "sk_test_" + "RF01WireKey00000000001" // split literal: not a real key
	srwRefund  = "5d0a7f52-3c1e-4b7a-8f0d-6a2b9c4e1d77"
	srwAttempt = "0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30"
	srwPI      = "pi_test_srw1"
)

// srwEnv is one fake Stripe with one account and a captured PaymentIntent.
type srwEnv struct {
	fake   *stripetest.Server
	client *stripe.Client
}

func srwNew(t *testing.T, captured int64) *srwEnv {
	t.Helper()
	fake := stripetest.New(srwAccount)
	t.Cleanup(fake.Close)
	if err := fake.RequireAPIKey(srwKey); err != nil {
		t.Fatal(err)
	}
	fake.AddPaidPaymentIntent(srwAccount, srwPI, captured, "TWD")
	client, err := stripe.NewWithMockTransport(stripe.Config{SecretKey: srwKey, AccountID: srwAccount, Environment: "SANDBOX"}, fake.Transport())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return &srwEnv{fake: fake, client: client}
}

func srwParams(amount int64, reason string) stripe.RefundParams {
	return stripe.RefundParams{PaymentIntentID: srwPI, AmountMinor: amount, Currency: "TWD", Reason: reason, RefundRef: srwRefund, AttemptRef: srwAttempt}
}

// srwGoldenBody is the contract §3 body: exactly payment_intent, amount, reason,
// metadata[lc_refund], metadata[lc_attempt]; keys sorted bytewise, url.QueryEscape on both
// sides (the stage-A EncodeCreateBody convention, stripe-psp §5.4).
func srwGoldenBody(pi string, amount int64, reason, ref, attempt string) string {
	return "amount=" + strconv.FormatInt(amount, 10) +
		"&metadata%5Blc_attempt%5D=" + url.QueryEscape(attempt) +
		"&metadata%5Blc_refund%5D=" + url.QueryEscape(ref) +
		"&payment_intent=" + url.QueryEscape(pi) +
		"&reason=" + url.QueryEscape(reason)
}

func TestStripeRF01Wire(t *testing.T) {
	ctx := context.Background()

	t.Run("golden body, exact key set, byte-identical resend", func(t *testing.T) {
		e := srwNew(t, 2500)
		for i, tc := range []struct {
			amount int64
			reason string
		}{{1000, "requested_by_customer"}, {500, "duplicate"}} {
			ref := fmt.Sprintf("5d0a7f52-3c1e-4b7a-8f0d-6a2b9c4e1d7%d", i)
			p := stripe.RefundParams{PaymentIntentID: srwPI, AmountMinor: tc.amount, Currency: "TWD", Reason: tc.reason, RefundRef: ref, AttemptRef: srwAttempt}
			first, meta, err := e.client.CreateRefund(ctx, p)
			if err != nil || first.ID == "" || first.Status != "succeeded" || first.Amount != tc.amount || meta.HTTPStatus != 200 {
				t.Fatalf("create %+v: %+v %+v %v", tc, first, meta, err)
			}
			if first.MetadataRefund != ref || first.MetadataAttempt != srwAttempt || first.PaymentIntentID != srwPI || first.Currency != "TWD" || first.Livemode {
				t.Fatalf("projection of the create response: %+v", first)
			}
			// Resend: same params -> byte-identical body, same key, provider replays the SAME refund.
			for n := 0; n < 3; n++ {
				again, m2, err := e.client.CreateRefund(ctx, p)
				if err != nil || again.ID != first.ID || !m2.IdempotentReplayed {
					t.Fatalf("resend %d: %+v replayed=%v err=%v", n, again, m2.IdempotentReplayed, err)
				}
			}
			bodies := e.fake.RefundBodies()
			want := srwGoldenBody(srwPI, tc.amount, tc.reason, ref, srwAttempt)
			if len(bodies) != 4*(i+1) {
				t.Fatalf("bodies=%d", len(bodies))
			}
			for _, b := range bodies[4*i:] {
				if b != want {
					t.Fatalf("golden body mismatch:\n got %s\nwant %s", b, want)
				}
			}
			keys := e.fake.RefundCreateKeys()
			for _, k := range keys[4*i:] {
				if k != stripe.RefundIdempotencyKey(ref) {
					t.Fatalf("resend used key %q, want %q", k, stripe.RefundIdempotencyKey(ref))
				}
			}
		}
		// The key set is exactly five keys; nothing Connect-only, no `charge`, no fraud reason.
		vals, err := url.ParseQuery(e.fake.RefundBodies()[0])
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for k := range vals {
			names = append(names, k)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != "amount,metadata[lc_attempt],metadata[lc_refund],payment_intent,reason" {
			t.Fatalf("key set = %v", names)
		}
		for _, forbidden := range []string{"charge", "fraudulent", "reverse_transfer", "refund_application_fee", "instructions_email", "origin", "expand"} {
			if strings.Contains(e.fake.RefundBodies()[0], forbidden) {
				t.Fatalf("forbidden token %q in body", forbidden)
			}
		}
		if len(e.fake.RefundIDs(srwPI)) != 2 {
			t.Fatalf("resends created extra refunds: %v", e.fake.RefundIDs(srwPI))
		}
		// Determinism: many fresh calls with one param set never change a byte.
		e2 := srwNew(t, 999_999_900)
		seen := map[string]bool{}
		for i := 0; i < 20; i++ {
			if _, _, err := e2.client.CreateRefund(ctx, srwParams(100, "requested_by_customer")); err != nil { // same params: replays
				t.Fatal(err)
			}
		}
		for _, b := range e2.fake.RefundBodies() {
			seen[b] = true
		}
		if len(seen) != 1 {
			t.Fatalf("non-deterministic body: %d variants", len(seen))
		}
	})

	t.Run("invalid input is refused before any request", func(t *testing.T) {
		e := srwNew(t, 2500)
		for name, mutate := range map[string]func(*stripe.RefundParams){
			"fraudulent reason":    func(p *stripe.RefundParams) { p.Reason = "fraudulent" }, // RD12
			"empty reason":         func(p *stripe.RefundParams) { p.Reason = "" },
			"unknown reason":       func(p *stripe.RefundParams) { p.Reason = "other" },
			"zero amount":          func(p *stripe.RefundParams) { p.AmountMinor = 0 }, // amount always present, positive
			"negative amount":      func(p *stripe.RefundParams) { p.AmountMinor = -100 },
			"amount over max":      func(p *stripe.RefundParams) { p.AmountMinor = 1_000_000_000_000 },
			"empty payment intent": func(p *stripe.RefundParams) { p.PaymentIntentID = "" },
			"empty refund ref":     func(p *stripe.RefundParams) { p.RefundRef = "" },
			"empty attempt ref":    func(p *stripe.RefundParams) { p.AttemptRef = "" },
			"currency outside v1":  func(p *stripe.RefundParams) { p.Currency = "JPY" },
		} {
			p := srwParams(1000, "requested_by_customer")
			mutate(&p)
			if _, _, err := e.client.CreateRefund(ctx, p); !errors.Is(err, stripe.ErrInvalid) {
				t.Fatalf("%s: want ErrInvalid, got %v", name, err)
			}
		}
		if n := e.fake.RefundPosts(); n != 0 {
			t.Fatalf("ErrInvalid inputs reached the provider: %d POSTs", n)
		}
	})

	t.Run("idempotency key format", func(t *testing.T) {
		re := regexp.MustCompile(`^lc:stripe:refund:v1:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
		k := stripe.RefundIdempotencyKey(srwRefund)
		if k != "lc:stripe:refund:v1:"+srwRefund || !re.MatchString(k) || len(k) > 255 {
			t.Fatalf("key %q", k)
		}
		if stripe.RefundIdempotencyKey(srwRefund) == stripe.RefundIdempotencyKey("5d0a7f52-3c1e-4b7a-8f0d-6a2b9c4e1d78") {
			t.Fatal("two refunds share a key")
		}
	})

	t.Run("classification (stripe-psp 5.6) per call", func(t *testing.T) {
		e := srwNew(t, 999_999_900)
		fresh := func(n int) stripe.RefundParams {
			p := srwParams(100, "requested_by_customer")
			p.RefundRef = fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
			return p
		}
		// CREATE: definitive only on the caller's first-send rule; the adapter reports ErrRejected for 400/404.
		for i, tc := range []struct {
			name  string
			fault stripetest.Fault
			want  error
		}{
			{"400 invalid_request_error", stripetest.Fault{Validation: true}, stripe.ErrRejected},
			{"400 idempotency_error", stripetest.Fault{IdempotencyError: true}, stripe.ErrIdempotency},
			{"409 concurrent", stripetest.Fault{Conflict: true}, stripe.ErrConflict},
			{"429 rate limit", stripetest.Fault{RateLimit: true}, stripe.ErrRateLimited},
			{"cached 500", stripetest.Fault{Cached500: true}, stripe.ErrUncertain},
		} {
			e.fake.SetNextFault(tc.fault)
			if _, _, err := e.client.CreateRefund(ctx, fresh(100+i)); !errors.Is(err, tc.want) {
				t.Fatalf("create %s: want %v, got %v", tc.name, tc.want, err)
			}
		}
		// A dropped connection after execution is uncertain. It runs on a fresh client because net/http
		// silently retries an idempotent request on a reused connection (which would replay the result).
		d := srwNew(t, 2500)
		d.fake.SetNextFault(stripetest.Fault{DropAfterExecute: true})
		if _, _, err := d.client.CreateRefund(ctx, srwParams(1000, "requested_by_customer")); !errors.Is(err, stripe.ErrUncertain) {
			t.Fatalf("create with dropped connection: want ErrUncertain, got %v", err)
		}
		if d.fake.RefundByRef(srwRefund) == "" {
			t.Fatal("fixture bug: the dropped create must have executed at the provider")
		}
		missing := fresh(200)
		missing.PaymentIntentID = "pi_test_does_not_exist"
		if _, _, err := e.client.CreateRefund(ctx, missing); !errors.Is(err, stripe.ErrRejected) {
			t.Fatalf("create 404: want ErrRejected, got %v", err)
		}
		// > charge but inside the §4.2 step table (max 99_999_900 TWD, the §0.2 table without its minimum), so
		// the POST happens: Stripe 400 amount_too_large is a definitive first-send reject. It needs a small
		// capture (this env captured 999_999_900, above the table max); 1e9 would be refused locally as
		// ErrInvalid by the same table, with zero POSTs.
		small := srwNew(t, 2500)
		if _, _, err := small.client.CreateRefund(ctx, srwParams(99_999_900, "requested_by_customer")); !errors.Is(err, stripe.ErrRejected) {
			t.Fatalf("create over-refund: want ErrRejected, got %v", err)
		}
		// A wrong API key is 401 before idempotency: ErrAuthentication on every call type.
		bad, err := stripe.NewWithMockTransport(stripe.Config{SecretKey: "sk_test_" + "WrongKey000000000000001", AccountID: srwAccount, Environment: "SANDBOX"}, e.fake.Transport())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := bad.CreateRefund(ctx, fresh(300)); !errors.Is(err, stripe.ErrAuthentication) {
			t.Fatalf("create with wrong key: %v", err)
		}
		if _, _, err := bad.RetrieveRefund(ctx, "re_x"); !errors.Is(err, stripe.ErrAuthentication) {
			t.Fatalf("retrieve with wrong key: %v", err)
		}
		if _, _, err := bad.ListRefunds(ctx, srwPI, ""); !errors.Is(err, stripe.ErrAuthentication) {
			t.Fatalf("list with wrong key: %v", err)
		}
		if _, _, err := bad.RetrievePaymentCharge(ctx, srwPI); !errors.Is(err, stripe.ErrAuthentication) {
			t.Fatalf("charge read with wrong key: %v", err)
		}
		// RETRIEVE / LIST / PI READ: never a definitive negative except success.
		ok, _, err := e.client.CreateRefund(ctx, fresh(400))
		if err != nil {
			t.Fatal(err)
		}
		got, meta, err := e.client.RetrieveRefund(ctx, ok.ID)
		if err != nil || got.ID != ok.ID || meta.HTTPStatus != 200 || got.Status != "succeeded" {
			t.Fatalf("retrieve: %+v %v", got, err)
		}
		if _, _, err := e.client.RetrieveRefund(ctx, "re_test_nope"); !errors.Is(err, stripe.ErrUncertain) {
			t.Fatalf("retrieve 404 must be ErrUncertain (never 'not found'): %v", err)
		}
		for _, tc := range []struct {
			op     string
			status int
			want   error
			call   func() error
		}{
			{"refund_retrieve", 500, stripe.ErrUncertain, func() error { _, _, err := e.client.RetrieveRefund(ctx, ok.ID); return err }},
			{"refund_retrieve", 429, stripe.ErrRateLimited, func() error { _, _, err := e.client.RetrieveRefund(ctx, ok.ID); return err }},
			{"refund_retrieve", 409, stripe.ErrConflict, func() error { _, _, err := e.client.RetrieveRefund(ctx, ok.ID); return err }},
			{"refund_list", 503, stripe.ErrUncertain, func() error { _, _, err := e.client.ListRefunds(ctx, srwPI, ""); return err }},
			{"refund_list", 429, stripe.ErrRateLimited, func() error { _, _, err := e.client.ListRefunds(ctx, srwPI, ""); return err }},
			{"refund_list", 404, stripe.ErrUncertain, func() error { _, _, err := e.client.ListRefunds(ctx, srwPI, ""); return err }},
			{"pi_retrieve", 500, stripe.ErrUncertain, func() error { _, _, err := e.client.RetrievePaymentCharge(ctx, srwPI); return err }},
			{"pi_retrieve", 404, stripe.ErrUncertain, func() error { _, _, err := e.client.RetrievePaymentCharge(ctx, srwPI); return err }},
			{"pi_retrieve", 429, stripe.ErrRateLimited, func() error { _, _, err := e.client.RetrievePaymentCharge(ctx, srwPI); return err }},
		} {
			e.fake.FailNext(tc.op, tc.status)
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("%s HTTP %d: want %v, got %v", tc.op, tc.status, tc.want, err)
			}
		}
		if _, _, err := e.client.RetrievePaymentCharge(ctx, "pi_test_does_not_exist"); !errors.Is(err, stripe.ErrUncertain) {
			t.Fatalf("PI read of an unknown id must be ErrUncertain: %v", err)
		}
	})

	t.Run("payment charge read", func(t *testing.T) {
		e := srwNew(t, 2500)
		if _, _, err := e.client.CreateRefund(ctx, srwParams(1000, "requested_by_customer")); err != nil {
			t.Fatal(err)
		}
		e.fake.DashboardRefund(srwPI, 300)
		c, meta, err := e.client.RetrievePaymentCharge(ctx, srwPI)
		if err != nil || meta.HTTPStatus != 200 {
			t.Fatalf("charge read: %v", err)
		}
		if c.PaymentIntentID != srwPI || c.ChargeID != "ch_test_"+srwPI || c.Currency != "TWD" || c.AmountCaptured != 2500 || c.AmountRefunded != 1300 || c.Refunded || c.Disputed || c.Livemode {
			t.Fatalf("charge projection: %+v", c)
		}
		// The read must ask for the expanded latest_charge and nothing else (contract §3).
		last := e.fake.Requests()[len(e.fake.Requests())-1]
		// Compare the decoded query: expand%5B%5D is the same parameter as expand[] (B1 encodes it that way too).
		q, qerr := url.ParseQuery(last.RawQuery)
		if last.Method != http.MethodGet || last.Path != "/v1/payment_intents/"+srwPI || qerr != nil || len(q) != 1 || len(q["expand[]"]) != 1 || q.Get("expand[]") != "latest_charge" || last.IdempotencyKey != "" {
			t.Fatalf("charge read request: %+v", last)
		}
		e.fake.PatchCharge(srwPI, map[string]any{"disputed": true})
		if c, _, err = e.client.RetrievePaymentCharge(ctx, srwPI); err != nil || !c.Disputed {
			t.Fatalf("disputed not projected: %+v %v", c, err)
		}
	})

	t.Run("list follows pages, newest first, caps at 10 pages", func(t *testing.T) {
		e := srwNew(t, 10_000_000)
		var ids []string
		for i := 0; i < 1000; i++ {
			ids = append(ids, e.fake.DashboardRefund(srwPI, 1))
		}
		list, _, err := e.client.ListRefunds(ctx, srwPI, "")
		if err != nil || len(list) != 1000 {
			t.Fatalf("1000 refunds = 10 full pages must list completely: n=%d err=%v", len(list), err)
		}
		if list[0].ID != ids[999] || list[999].ID != ids[0] {
			t.Fatal("list is not newest first")
		}
		gets := func() (n int) {
			for _, r := range e.fake.Requests() {
				if r.Method == http.MethodGet && r.Path == "/v1/refunds" {
					n++
				}
			}
			return
		}
		if gets() != 10 {
			t.Fatalf("1000 refunds took %d list calls, want 10", gets())
		}
		e.fake.DashboardRefund(srwPI, 1) // 1001st: needs an 11th page
		before := gets()
		if l, _, err := e.client.ListRefunds(ctx, srwPI, ""); !errors.Is(err, stripe.ErrUncertain) || len(l) != 0 {
			t.Fatalf("11 pages must be ErrUncertain with no partial result (never 'not found'): n=%d err=%v", len(l), err)
		}
		if gets()-before != 10 {
			t.Fatalf("over-cap list made %d requests, want exactly 10", gets()-before)
		}
		// starting_after resumes after a cursor.
		tail, _, err := e.client.ListRefunds(ctx, srwPI, ids[900])
		if err != nil || len(tail) != 900 || tail[0].ID != ids[899] {
			t.Fatalf("starting_after: n=%d err=%v", len(tail), err)
		}
	})

	t.Run("adapter projections never carry ARN, receipt, billing details", func(t *testing.T) {
		e := srwNew(t, 2500)
		r, _, err := e.client.CreateRefund(ctx, srwParams(1000, "requested_by_customer"))
		if err != nil {
			t.Fatal(err)
		}
		c, _, err := e.client.RetrievePaymentCharge(ctx, srwPI)
		if err != nil {
			t.Fatal(err)
		}
		list, _, err := e.client.ListRefunds(ctx, srwPI, "")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(struct {
			R stripe.Refund
			C stripe.PaymentCharge
			L []stripe.Refund
		}{r, c, list})
		dump := fmt.Sprintf("%+v %#v %v %s", r, c, list, raw)
		for _, s := range []string{stripetest.SentinelARN, stripetest.SentinelReceiptURL, stripetest.SentinelEmail, "receipt", "billing", "destination_details"} {
			if strings.Contains(dump, s) {
				t.Fatalf("projection leaks %q: %s", s, dump)
			}
		}
	})

	t.Run("RefundAmountOK spec table", func(t *testing.T) {
		for _, tc := range []struct {
			currency string
			amount   int64
			want     bool
		}{
			{"TWD", 100, true}, {"TWD", 200, true}, {"TWD", 99_999_900, true}, {"TWD", 150, false}, {"TWD", 199, false}, {"TWD", 1, false}, {"TWD", 0, false}, {"TWD", -100, false},
			// §4.2: the §0.2 currency table without its minimum, so the table max (TWD 99_999_900, others
			// 99_999_999) applies, not the column CHECK's 999_999_999_999.
			{"TWD", 100_000_000, false}, {"TWD", 999_999_999_900, false}, {"TWD", 1_000_000_000_000, false}, {"TWD", 1_000_000_000_100, false},
			{"USD", 1, true}, {"USD", 49, true}, {"HKD", 1, true}, {"SGD", 199, true}, {"MYR", 1, true}, {"USD", 99_999_999, true}, {"USD", 100_000_000, false}, {"USD", 999_999_999_999, false},
			{"USD", 0, false}, {"USD", -1, false}, {"USD", 1_000_000_000_000, false},
			{"JPY", 100, false}, {"EUR", 100, false}, {"", 100, false}, {"XXX", 100, false},
		} {
			if got := stripe.RefundAmountOK(tc.currency, tc.amount); got != tc.want {
				t.Fatalf("RefundAmountOK(%q,%d) = %v want %v", tc.currency, tc.amount, got, tc.want)
			}
		}
	})

	t.Run("pg parity payments.stripe_refund_amount_ok", func(t *testing.T) {
		f := fixture(t) // REAL_PG; skipped as NOT_RUN without LC_TEST_DATABASE_*
		currencies := []string{"TWD", "USD", "HKD", "SGD", "MYR", "JPY", "EUR", "twd", "usd", "", "XXX", "TW", "TWDX"}
		amounts := []int64{-100, -1, 0, 1, 2, 49, 50, 99, 100, 101, 150, 199, 200, 399, 400, 1000, 12345, 99_999_900, 99_999_999, 100_000_000, 999_999_999_900, 999_999_999_999, 1_000_000_000_000, 1_000_000_000_100}
		for i := int64(0); i < 200; i++ { // deterministic spread across the range, not a random test
			amounts = append(amounts, i*i*i*7+i, i*100, i*1_234_567)
		}
		for _, c := range currencies {
			for _, a := range amounts {
				var sql bool
				if err := f.owner.QueryRow(ctx, `SELECT payments.stripe_refund_amount_ok($1::text,$2::bigint)`, c, a).Scan(&sql); err != nil {
					t.Fatalf("SQL function missing or failing for (%q,%d): %v", c, a, err)
				}
				if goOK := stripe.RefundAmountOK(c, a); goOK != sql {
					t.Fatalf("Go/SQL divergence for (%q,%d): Go=%v SQL=%v", c, a, goOK, sql)
				}
			}
		}
	})
}

// srwVector mirrors tests/payments/stripe-webhook-vectors.json (extended by the RF unit).
type srwVector struct {
	Name    string   `json:"name"`
	Secrets []string `json:"secrets"`
	Header  string   `json:"header"`
	Body    *string  `json:"body"`
	BodyB64 *string  `json:"body_b64"`
	Now     int64    `json:"now"`
	Expect  string   `json:"expect"`
	Event   *struct {
		ID                string  `json:"id"`
		Type              string  `json:"type"`
		ObjectType        string  `json:"object_type"`
		SessionID         string  `json:"session_id"`
		ClientReferenceID string  `json:"client_reference_id"`
		MetadataAttempt   string  `json:"metadata_attempt"`
		PaymentIntentID   *string `json:"payment_intent_id"`
		MetadataRefund    *string `json:"metadata_refund"`
		Livemode          bool    `json:"livemode"`
		AccountPresent    bool    `json:"account_present"`
		Probe             bool    `json:"probe"`
	} `json:"event"`
}

func TestStripeRF02WebhookProjection(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	vectorsPath := filepath.Join(root, "tests", "payments", "stripe-webhook-vectors.json")
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []srwVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	t.Run("vectors through the Go verifier", func(t *testing.T) {
		refundVectors := 0
		for _, v := range file.Vectors {
			if !strings.HasPrefix(v.Name, "refund_") && !strings.HasPrefix(v.Name, "charge_") && !strings.HasPrefix(v.Name, "malformed_refund_") && !strings.HasPrefix(v.Name, "checkout_session_projection") {
				continue
			}
			refundVectors++
			verifier, err := stripe.NewWebhookVerifier(stripe.WebhookConfig{Secrets: v.Secrets, AccountID: "acct_1FakeAccount000", Environment: "SANDBOX"})
			if err != nil {
				t.Fatal(err)
			}
			var body []byte
			if v.BodyB64 != nil {
				body, _ = base64.StdEncoding.DecodeString(*v.BodyB64)
			} else if v.Body != nil {
				body = []byte(*v.Body)
			}
			ev, err := verifier.Verify(body, v.Header, time.Unix(v.Now, 0))
			switch v.Expect {
			case "signature":
				if !errors.Is(err, stripe.ErrSignature) || ev != (stripe.Event{}) {
					t.Fatalf("%s: want ErrSignature, got %v", v.Name, err)
				}
			case "malformed":
				if err != nil || !ev.Malformed || ev.BodySHA256 != sha256.Sum256(body) || ev.ID != "" {
					t.Fatalf("%s: want malformed, got err=%v malformed=%v", v.Name, err, ev.Malformed)
				}
			case "ok":
				e := v.Event
				if err != nil || ev.Malformed || e == nil || ev.ID != e.ID || ev.Type != e.Type || ev.ObjectType != e.ObjectType || ev.SessionID != e.SessionID ||
					ev.ClientReferenceID != e.ClientReferenceID || ev.MetadataAttempt != e.MetadataAttempt || ev.Livemode != e.Livemode || ev.AccountPresent != e.AccountPresent {
					t.Fatalf("%s: base projection mismatch err=%v event=%+v", v.Name, err, ev)
				}
				if e.PaymentIntentID != nil && ev.PaymentIntentID != *e.PaymentIntentID {
					t.Fatalf("%s: PaymentIntentID=%q want %q", v.Name, ev.PaymentIntentID, *e.PaymentIntentID)
				}
				if e.MetadataRefund != nil && ev.MetadataRefund != *e.MetadataRefund {
					t.Fatalf("%s: MetadataRefund=%q want %q", v.Name, ev.MetadataRefund, *e.MetadataRefund)
				}
				if s, _ := json.Marshal(ev); strings.Contains(string(s), "re_1Vector") || strings.Contains(fmt.Sprintf("%+v %#v", ev, ev), "ARN_VECTOR") {
					t.Fatalf("%s: Event formats raw content", v.Name)
				}
			}
		}
		if refundVectors < 10 {
			t.Fatalf("only %d refund/charge vectors found: the vector file lost its RF02 cases", refundVectors)
		}
	})

	t.Run("fresh events rendered by the fake", func(t *testing.T) {
		const secret = "whsec_" + "RF02VectorSecret_not_real_0123456789"
		e := srwNew(t, 2500)
		if _, _, err := e.client.CreateRefund(context.Background(), srwParams(1000, "requested_by_customer")); err != nil {
			t.Fatal(err)
		}
		v, err := stripe.NewWebhookVerifier(stripe.WebhookConfig{Secrets: []string{secret}, AccountID: srwAccount, Environment: "SANDBOX"})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		refundID := e.fake.RefundByRef(srwRefund)
		body := e.fake.RefundEventBody("evt_1RF02Refund", "refund.updated", refundID, false)
		ev, err := v.Verify(body, stripetest.SignWebhook(secret, body, now), now)
		if err != nil || ev.Malformed || ev.ObjectType != "refund" || ev.SessionID != refundID || ev.PaymentIntentID != srwPI || ev.MetadataRefund != srwRefund || ev.ClientReferenceID != "" {
			t.Fatalf("refund event projection: %+v err=%v", ev, err)
		}
		if !strings.Contains(string(body), stripetest.SentinelARN) {
			t.Fatal("fixture bug: the fake must put an ARN sentinel into the refund object")
		}
		dash := e.fake.DashboardRefund(srwPI, 200)
		body = e.fake.RefundEventBody("evt_1RF02Dash", "refund.created", dash, false)
		if ev, err = v.Verify(body, stripetest.SignWebhook(secret, body, now), now); err != nil || ev.Malformed || ev.MetadataRefund != "" || ev.PaymentIntentID != srwPI {
			t.Fatalf("dashboard refund event: %+v err=%v", ev, err)
		}
		body = e.fake.ChargeEventBody("evt_1RF02Charge", srwPI, false)
		if ev, err = v.Verify(body, stripetest.SignWebhook(secret, body, now), now); err != nil || ev.Malformed || ev.ObjectType != "charge" || ev.Type != "charge.refunded" || ev.PaymentIntentID != srwPI || ev.SessionID != "ch_test_"+srwPI {
			t.Fatalf("charge event projection: %+v err=%v", ev, err)
		}
		// Checkout-session events keep their projection (other objects unchanged).
		body = stripetest.EventBody(stripetest.EventOpts{ID: "evt_1RF02Sess", SessionID: "cs_test_rf02", ClientRef: srwAttempt, Attempt: srwAttempt, PendingWebhooks: 1})
		if ev, err = v.Verify(body, stripetest.SignWebhook(secret, body, now), now); err != nil || ev.Malformed || ev.ObjectType != "checkout.session" || ev.SessionID != "cs_test_rf02" || ev.ClientReferenceID != srwAttempt || ev.MetadataAttempt != srwAttempt {
			t.Fatalf("checkout.session projection changed: %+v err=%v", ev, err)
		}
		// A refund body whose signature covers different bytes is refused, never projected.
		bad := append([]byte(nil), body...)
		bad[len(bad)-3] ^= 1
		if ev, err = v.Verify(bad, stripetest.SignWebhook(secret, body, now), now); !errors.Is(err, stripe.ErrSignature) || ev != (stripe.Event{}) {
			t.Fatalf("tampered body: %v", err)
		}
	})

	t.Run("node half", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("NOT_RUN: node is not on PATH")
		}
		out, err := exec.Command(node, filepath.Join(root, "scripts", "dev", "stripe-webhook-check.mjs"), vectorsPath).CombinedOutput()
		if err != nil {
			t.Fatalf("node vector check failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "vectors agree") {
			t.Fatalf("node vector check printed no agreement line: %s", out)
		}
	})
}
