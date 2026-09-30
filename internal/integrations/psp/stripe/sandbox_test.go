// sandbox_test.go: SP16 SANDBOX — real api.stripe.com with a test key against the
// fixture account acct_1UJDb0RusP6Wwj7e only. It creates exactly one Checkout Session
// (HKD 4.00, card, 40-minute expiry), never pays, refunds or touches live mode, and
// expires the session before returning. Non-goals: the below-minimum probe (the adapter
// refuses it locally, so it cannot be sent through the frozen API) and RAK permission
// discovery beyond recording the key kind. The 29-minute expiry probe is opt-in
// (STRIPE_SANDBOX_EXPIRY_PROBE=1) because Stripe accepted it once, creating a second session.
// Gate: STRIPE_SANDBOX=1 plus STRIPE_SECRET_KEY and STRIPE_ACCOUNT_ID from the
// environment; otherwise t.Skip("NOT_RUN: …"), which the harness records as NOT_RUN.
// The key is never logged: only session ids, request ids and statuses are.

package stripe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sandboxAccountID = "acct_1UJDb0RusP6Wwj7e"

func newUUIDv4(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func sandboxFields(attempt, order, returnURL string, expiresAt int64) map[string]string {
	f := fixtureFields()
	for k, v := range map[string]string{
		"client_reference_id":                           attempt,
		"metadata[lc_attempt]":                          attempt,
		"payment_intent_data[metadata][lc_attempt]":     attempt,
		"metadata[lc_order]":                            order,
		"payment_intent_data[metadata][lc_order]":       order,
		"line_items[0][price_data][product_data][name]": "Order " + strings.ToUpper(order[:8]),
		"line_items[0][price_data][currency]":           "hkd",
		"line_items[0][price_data][unit_amount]":        "400", // HKD 4.00: the §4 local minimum
		"expires_at":                                    strconv.FormatInt(expiresAt, 10),
		"cancel_url":                                    returnURL,
		"success_url":                                   returnURL,
		"metadata[lc_profile]":                          "SANDBOX",
	} {
		f[k] = v
	}
	return f
}

func TestStripeSP16Sandbox(t *testing.T) {
	if os.Getenv("STRIPE_SANDBOX") != "1" {
		t.Skip("NOT_RUN: SP16 requires STRIPE_SANDBOX=1 with a Stripe test key")
	}
	key, account := os.Getenv("STRIPE_SECRET_KEY"), os.Getenv("STRIPE_ACCOUNT_ID")
	// Refuse before any call unless this is a test key for the one fixture account.
	if account != sandboxAccountID {
		t.Fatalf("refused: STRIPE_ACCOUNT_ID must be %s", sandboxAccountID)
	}
	keyKind := ""
	switch {
	case strings.HasPrefix(key, "sk_test_"):
		keyKind = "sk_test"
	case strings.HasPrefix(key, "rk_test_"):
		keyKind = "rk_test"
	default:
		t.Fatal("refused: STRIPE_SECRET_KEY is not a test key")
	}
	c, err := New(Config{SecretKey: key, AccountID: account, Environment: "SANDBOX"})
	if err != nil {
		t.Fatalf("config refused: %v", err) // fixed code only; never the key
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	t.Logf("key kind=%s account=%s api_version=%s", keyKind, account, APIVersion)

	if m, err := c.VerifyAccount(ctx); err != nil {
		t.Fatalf("VerifyAccount: %v status=%d type=%s code=%s", err, m.HTTPStatus, m.ErrorType, m.ErrorCode)
	}

	returnURL := os.Getenv("COMMERCE_PAYMENT_RETURN_URL")
	if returnURL == "" {
		returnURL = "https://example.com/livecommerce/payment/return"
	}
	attempt, order := newUUIDv4(t), newUUIDv4(t)
	expiresAt := time.Now().Add(40 * time.Minute).Unix() // D4: fixed 40-minute session
	params := CreateParams{Fields: sandboxFields(attempt, order, returnURL, expiresAt)}

	s, m, err := c.CreateCheckoutSession(ctx, params)
	if err != nil {
		t.Fatalf("create: %v status=%d type=%s code=%s request=%s", err, m.HTTPStatus, m.ErrorType, m.ErrorCode, m.RequestID)
	}
	sessionID := s.ID
	expired := false
	t.Cleanup(func() {
		if expired {
			return
		}
		// Best effort: never leave an open test session behind.
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_, _, _ = c.ExpireCheckoutSession(cctx, sessionID, ExpireIdempotencyKey(attempt, 99))
	})
	t.Logf("created session=%s request=%s", sessionID, m.RequestID)
	checkOpen := func(label string, s Session) {
		t.Helper()
		if s.ID != sessionID || s.Status != "open" || s.PaymentStatus != "unpaid" || s.Livemode ||
			s.Currency != "HKD" || s.AmountTotal == nil || *s.AmountTotal != 400 ||
			s.AmountSubtotal == nil || *s.AmountSubtotal != 400 ||
			strings.Join(s.PaymentMethodTypes, ",") != "card" || s.ClientReferenceID != attempt ||
			s.MetadataAttempt != attempt || s.MetadataProfile != "SANDBOX" || s.Mode != "payment" ||
			s.PresentmentCurrency != "" || s.PresentmentAmount != nil || s.CurrencyConversion ||
			s.ExpiresAt == nil || *s.ExpiresAt != expiresAt || s.URL() == "" {
			t.Fatalf("%s: unexpected session projection status=%s pay=%s cur=%s pres=%s methods=%v",
				label, s.Status, s.PaymentStatus, s.Currency, s.PresentmentCurrency, s.PaymentMethodTypes)
		}
	}
	checkOpen("create", s)

	r, m, err := c.RetrieveCheckoutSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("retrieve: %v status=%d", err, m.HTTPStatus)
	}
	checkOpen("retrieve", r)

	// Same key, same bytes: Stripe replays the saved result (F4).
	rep, m, err := c.CreateCheckoutSession(ctx, params)
	if err != nil || rep.ID != sessionID || !m.IdempotentReplayed {
		t.Fatalf("replay: err=%v same=%v replayed=%v", err, rep.ID == sessionID, m.IdempotentReplayed)
	}
	t.Logf("replay same session=%v Idempotent-Replayed=%v", rep.ID == sessionID, m.IdempotentReplayed)

	// Same key, changed params: idempotency_error, never a second session.
	changed := CreateParams{Fields: sandboxFields(attempt, order, returnURL, expiresAt)}
	changed.Fields["locale"] = "en"
	if _, m, err := c.CreateCheckoutSession(ctx, changed); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("changed params: want ErrIdempotency, got %v status=%d type=%s", err, m.HTTPStatus, m.ErrorType)
	}

	// The list window around creation finds exactly this session (F10: immediately consistent).
	if r.Created == nil {
		t.Fatal("created missing")
	}
	found, _, err := c.FindCheckoutSessions(ctx, attempt, *r.Created-120, *r.Created+120)
	if err != nil || len(found) != 1 || found[0].ID != sessionID {
		t.Fatalf("find: err=%v matches=%d", err, len(found))
	}

	e, m, err := c.ExpireCheckoutSession(ctx, sessionID, ExpireIdempotencyKey(attempt, 1))
	if err != nil || e.Status != "expired" || e.PaymentStatus != "unpaid" {
		t.Fatalf("expire: err=%v status=%s pay=%s http=%d", err, e.Status, e.PaymentStatus, m.HTTPStatus)
	}
	expired = true
	got, _, err := c.RetrieveCheckoutSession(ctx, sessionID)
	if err != nil || got.Status != "expired" || got.PaymentStatus != "unpaid" || got.URL() != "" {
		t.Fatalf("retrieve expired: err=%v status=%s pay=%s url_present=%v", err, got.Status, got.PaymentStatus, got.URL() != "")
	}
	t.Logf("expired session=%s status=%s payment_status=%s", sessionID, got.Status, got.PaymentStatus)

	// A second expire with a new generation key is 400 → ErrNotOpen (F2).
	if _, m, err := c.ExpireCheckoutSession(ctx, sessionID, ExpireIdempotencyKey(attempt, 2)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("second expire: want ErrNotOpen, got %v http=%d type=%s", err, m.HTTPStatus, m.ErrorType)
	}

	// §14 expects expires_at = now+29 min to be a first-send 400 (F1: 30 min..24 h).
	// Observed 2026-09-28: Stripe ACCEPTED 29 minutes, contradicting F1/§14 — escalated to
	// the integrator, not relaxed here. The probe creates a second session when Stripe
	// accepts it, so it runs only on explicit request.
	t.Run("expires_at_29min_rejected", func(t *testing.T) {
		if os.Getenv("STRIPE_SANDBOX_EXPIRY_PROBE") != "1" {
			t.Skip("NOT_RUN: set STRIPE_SANDBOX_EXPIRY_PROBE=1 (may create and expire a second test session)")
		}
		attempt2, order2 := newUUIDv4(t), newUUIDv4(t)
		short := CreateParams{Fields: sandboxFields(attempt2, order2, returnURL, time.Now().Add(29*time.Minute).Unix())}
		bad, m, err := c.CreateCheckoutSession(ctx, short)
		if err == nil {
			_, _, _ = c.ExpireCheckoutSession(ctx, bad.ID, ExpireIdempotencyKey(attempt2, 1))
			t.Fatalf("29-minute expiry accepted by Stripe (session %s expired again)", bad.ID)
		}
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("29-minute expiry: want ErrRejected, got %v http=%d type=%s", err, m.HTTPStatus, m.ErrorType)
		}
		t.Logf("29-minute expiry rejected: http=%d type=%s code=%s", m.HTTPStatus, m.ErrorType, m.ErrorCode)
	})
}
