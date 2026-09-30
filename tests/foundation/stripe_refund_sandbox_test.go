package foundation_test

// RF10 (contracts/stripe-refund-v1.md §9, ruling R-10, RD13): SANDBOX refund probe against the real
// api.stripe.com in TEST mode only, through the real adapter (internal/integrations/psp/stripe).
//
// Gate: STRIPE_SANDBOX=1 plus STRIPE_SECRET_KEY (the key the PRODUCT would hold: a restricted key is
// recommended) and STRIPE_ACCOUNT_ID = the fixture account. Optional: STRIPE_HARNESS_KEY (a test key
// allowed to create test PaymentIntents, used only by this harness so the product key can stay a
// least-privilege RAK) and STRIPE_SECRET_KEY_ROTATED (a second test key of the SAME account, for the
// RD13 rotated-key replay). Otherwise t.Skip("NOT_RUN: ..."), which is recorded as NOT_RUN, never PASS.
//
// Test PaymentIntent (R-10): created here with payment_method=pm_card_visa and confirm=true, from the
// test harness only; product code never creates one. HKD 4.00 (400 minor) is the fixture amount of SP16.
// No key value is ever logged: only ids, request ids and statuses. Live keys are refused before any call.
//
// Evidence: when LC_EVIDENCE_DIR is set the test writes rf10-sandbox.json (key KIND, per-operation
// status, request ids, the least-permission observation). Docs: https://docs.stripe.com/api/refunds,
// https://docs.stripe.com/api/payment_intents/create, https://docs.stripe.com/testing (pm_card_visa),
// retrieved 2026-09-29.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
)

const rf10Account = "acct_1UJDb0RusP6Wwj7e"

func rf10Kind(key string) string {
	switch {
	case strings.HasPrefix(key, "sk_test_"):
		return "sk_test"
	case strings.HasPrefix(key, "rk_test_"):
		return "rk_test"
	}
	return ""
}

// rf10PaymentIntent creates and confirms a card test PaymentIntent with pm_card_visa (harness only).
func rf10PaymentIntent(t *testing.T, key string, amount int64) string {
	t.Helper()
	form := url.Values{"amount": {fmt.Sprint(amount)}, "currency": {"hkd"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"},
		"automatic_payment_methods[enabled]": {"true"}, "automatic_payment_methods[allow_redirects]": {"never"}, "metadata[lc_rf10]": {"1"}}
	req, err := http.NewRequest(http.MethodPost, "https://api.stripe.com/v1/payment_intents", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Stripe-Version", stripe.APIVersion)
	req.Header.Set("Idempotency-Key", "lc:rf10:pi:"+randomUUID())
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("harness PaymentIntent create failed: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Livemode bool   `json:"livemode"`
	}
	if res.StatusCode != 200 || json.Unmarshal(raw, &out) != nil || out.ID == "" {
		t.Fatalf("harness PaymentIntent create: HTTP %d (the harness key needs PaymentIntents write in test mode; set STRIPE_HARNESS_KEY)", res.StatusCode)
	}
	if out.Livemode || out.Status != "succeeded" {
		t.Fatalf("harness PaymentIntent %s: livemode=%v status=%s", out.ID, out.Livemode, out.Status)
	}
	return out.ID
}

func TestStripeRF10Sandbox(t *testing.T) {
	if os.Getenv("STRIPE_SANDBOX") != "1" {
		t.Skip("NOT_RUN: RF10 requires STRIPE_SANDBOX=1 with a Stripe test key (owner-provided; SANDBOX only, LIVE refused)")
	}
	key, account := os.Getenv("STRIPE_SECRET_KEY"), os.Getenv("STRIPE_ACCOUNT_ID")
	if account != rf10Account {
		t.Fatalf("refused: STRIPE_ACCOUNT_ID must be the fixture account %s", rf10Account)
	}
	kind := rf10Kind(key)
	if kind == "" {
		t.Fatal("refused: STRIPE_SECRET_KEY is not a test key (sk_test_/rk_test_); LIVE is never allowed here")
	}
	harnessKey := os.Getenv("STRIPE_HARNESS_KEY")
	if harnessKey == "" {
		harnessKey = key
	}
	if rf10Kind(harnessKey) == "" {
		t.Fatal("refused: STRIPE_HARNESS_KEY is not a test key")
	}
	client, err := stripe.New(stripe.Config{SecretKey: key, AccountID: account, Environment: "SANDBOX"})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	if m, err := client.VerifyAccount(ctx); err != nil {
		t.Fatalf("VerifyAccount: %v status=%d", err, m.HTTPStatus)
	}
	evidence := map[string]any{"key_kind": kind, "account": account, "api_version": stripe.APIVersion, "operations": map[string]any{}}
	ops := evidence["operations"].(map[string]any)
	record := func(name string, meta stripe.CallMeta, err error) {
		ops[name] = map[string]any{"http_status": meta.HTTPStatus, "request_id": meta.RequestID, "ok": err == nil, "replayed": meta.IdempotentReplayed}
	}
	defer func() {
		if dir := os.Getenv("LC_EVIDENCE_DIR"); dir != "" {
			if raw, err := json.MarshalIndent(evidence, "", "  "); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "rf10-sandbox.json"), raw, 0o644)
			}
		}
	}()

	pi := rf10PaymentIntent(t, harnessKey, 400)
	evidence["payment_intent"] = pi
	attempt, ref1, ref2, ref3 := randomUUID(), randomUUID(), randomUUID(), randomUUID()
	p1 := stripe.RefundParams{PaymentIntentID: pi, AmountMinor: 100, Currency: "HKD", Reason: "requested_by_customer", RefundRef: ref1, AttemptRef: attempt}

	first, meta, err := client.CreateRefund(ctx, p1)
	record("create_partial", meta, err)
	if err != nil || !strings.HasPrefix(first.ID, "re_") || first.Amount != 100 || first.Currency != "HKD" || first.Livemode ||
		first.MetadataRefund != ref1 || first.MetadataAttempt != attempt || first.PaymentIntentID != pi || (first.Status != "succeeded" && first.Status != "pending") {
		t.Fatalf("partial refund: %+v err=%v status=%d type=%s code=%s", first, err, meta.HTTPStatus, meta.ErrorType, meta.ErrorCode)
	}
	// Same key, same bytes: Stripe replays the saved result (F4).
	replay, meta, err := client.CreateRefund(ctx, p1)
	record("replay_same_key", meta, err)
	if err != nil || replay.ID != first.ID || !meta.IdempotentReplayed {
		t.Fatalf("replay: %+v replayed=%v err=%v", replay, meta.IdempotentReplayed, err)
	}
	// Same key, changed parameters: idempotency_error.
	changed := p1
	changed.AmountMinor = 101
	_, meta, err = client.CreateRefund(ctx, changed)
	record("same_key_changed_params", meta, err)
	if !errors.Is(err, stripe.ErrIdempotency) {
		t.Fatalf("changed params under one key: want ErrIdempotency, got %v (status %d type %s)", err, meta.HTTPStatus, meta.ErrorType)
	}
	// The remainder, then an over-refund (400: a definitive first-send rejection).
	p2 := stripe.RefundParams{PaymentIntentID: pi, AmountMinor: 300, Currency: "HKD", Reason: "duplicate", RefundRef: ref2, AttemptRef: attempt}
	second, meta, err := client.CreateRefund(ctx, p2)
	record("create_remainder", meta, err)
	if err != nil || second.Amount != 300 || second.ID == first.ID {
		t.Fatalf("remainder refund: %+v err=%v", second, err)
	}
	p3 := stripe.RefundParams{PaymentIntentID: pi, AmountMinor: 100, Currency: "HKD", Reason: "requested_by_customer", RefundRef: ref3, AttemptRef: attempt}
	_, meta, err = client.CreateRefund(ctx, p3)
	record("over_refund", meta, err)
	if !errors.Is(err, stripe.ErrRejected) || meta.HTTPStatus != 400 {
		t.Fatalf("over-refund: want ErrRejected/400, got %v status=%d", err, meta.HTTPStatus)
	}
	// Retrieve and list.
	got, meta, err := client.RetrieveRefund(ctx, first.ID)
	record("retrieve_refund", meta, err)
	if err != nil || got.ID != first.ID || got.MetadataRefund != ref1 {
		t.Fatalf("retrieve: %+v err=%v", got, err)
	}
	list, meta, err := client.ListRefunds(ctx, pi, "")
	record("list_refunds", meta, err)
	if err != nil || len(list) != 2 {
		t.Fatalf("list by PaymentIntent: n=%d err=%v", len(list), err)
	}
	seen := map[string]bool{}
	for _, r := range list {
		seen[r.MetadataRefund] = true
	}
	if !seen[ref1] || !seen[ref2] {
		t.Fatalf("list lacks our metadata references: %v", seen)
	}
	charge, meta, err := client.RetrievePaymentCharge(ctx, pi)
	record("retrieve_payment_charge", meta, err)
	if err != nil || charge.AmountCaptured != 400 || charge.AmountRefunded != 400 || !charge.Refunded || charge.Disputed || charge.Livemode || charge.Currency != "HKD" {
		t.Fatalf("charge after two refunds: %+v err=%v", charge, err)
	}
	evidence["least_permission_observation"] = fmt.Sprintf("the %s key performed create/retrieve refund, list refunds and read PaymentIntent+latest_charge; a restricted key needs exactly Refunds write, PaymentIntents read, Charges read (R-6)", kind)

	t.Run("rotated key: same-key replay on the same account returns the same refund (RD13)", func(t *testing.T) {
		rotated := os.Getenv("STRIPE_SECRET_KEY_ROTATED")
		if rotated == "" {
			t.Skip("NOT_RUN: needs STRIPE_SECRET_KEY_ROTATED, a second test key of the same account; until proven, RD13 must revert to request-time keys for resends")
		}
		if rf10Kind(rotated) == "" {
			t.Fatal("refused: STRIPE_SECRET_KEY_ROTATED is not a test key")
		}
		other, err := stripe.New(stripe.Config{SecretKey: rotated, AccountID: account, Environment: "SANDBOX"})
		if err != nil {
			t.Fatalf("rotated config refused: %v", err)
		}
		if _, err := other.VerifyAccount(ctx); err != nil {
			t.Fatalf("the rotated key does not verify as %s: %v", account, err)
		}
		pi2 := rf10PaymentIntent(t, harnessKey, 400)
		p := stripe.RefundParams{PaymentIntentID: pi2, AmountMinor: 100, Currency: "HKD", Reason: "requested_by_customer", RefundRef: randomUUID(), AttemptRef: randomUUID()}
		one, _, err := client.CreateRefund(ctx, p)
		if err != nil {
			t.Fatalf("create with key A: %v", err)
		}
		two, meta, err := other.CreateRefund(ctx, p)
		evidence["rd13_rotated_key_replay"] = map[string]any{"same_refund": err == nil && two.ID == one.ID, "replayed": meta.IdempotentReplayed, "http_status": meta.HTTPStatus}
		if err != nil || two.ID != one.ID || !meta.IdempotentReplayed {
			t.Fatalf("RD13 assumption failed: replay under key B returned %+v replayed=%v err=%v (status %d); the contract then reverts RD13 to request-time key versions for resends", two, meta.IdempotentReplayed, err, meta.HTTPStatus)
		}
	})
}
