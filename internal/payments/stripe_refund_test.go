package payments

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testRefundSnapshot() stripeRefundSnapshot {
	requested := time.Unix(1790000000, 0).UTC()
	return stripeRefundSnapshot{
		TenantID: stripeTestOrder, StoreID: stripeTestOrder, AttemptID: stripeTestAttempt, OrderID: stripeTestOrder,
		ConnectionID: stripeTestOrder, Environment: "SANDBOX", AccountID: "acct_1FakeAccount000", CredentialVersion: 3,
		KeyID: "fixture_key", Nonce: make([]byte, 12), Ciphertext: make([]byte, 48), PaymentIntentID: "pi_test_1",
		Currency: "HKD", Reason: "requested_by_customer", AmountMinor: 5000, RequestedAt: requested,
		ResendUntil: requested.Add(20 * time.Hour), DBNow: requested.Add(time.Minute),
	}
}

func TestRefundArgsAndOperationFamily(t *testing.T) {
	if !validRefundArgs(paymentRefundArgs{OperationID: stripeTestAttempt, Version: 1}) {
		t.Fatal("valid args rejected")
	}
	for _, a := range []paymentRefundArgs{{OperationID: stripeTestAttempt, Version: 2}, {OperationID: "", Version: 1},
		{OperationID: "../../x", Version: 1}} {
		if validRefundArgs(a) {
			t.Fatalf("accepted %+v", a)
		}
	}
	if (paymentRefundArgs{}).Kind() != "payment_refund_v1" {
		t.Fatal("kind is part of the frozen post_river guard")
	}
	good := queryOperation{ActorKind: "PAYMENT_REFUND", Provider: "stripe", Action: "stripe.refund", Purpose: "transactional"}
	if !validRefundOperation(good) || validQueryOperation(good) {
		t.Fatal("a refund op is a refund family and not a checkout query family")
	}
	for name, mutate := range map[string]func(*queryOperation){
		"actor":    func(o *queryOperation) { o.ActorKind = "BUYER_PAYMENT_QUERY" },
		"provider": func(o *queryOperation) { o.Provider = "payuni" },
		"action":   func(o *queryOperation) { o.Action = "stripe.checkout_session" },
		"purpose":  func(o *queryOperation) { o.Purpose = "marketing" },
	} {
		o := good
		mutate(&o)
		if validRefundOperation(o) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRefundSnapshotRejectsScopeAndMoneyDrift(t *testing.T) {
	s := testRefundSnapshot()
	if !validRefundSnapshot(s, stripeTestOrder, "SANDBOX") {
		t.Fatal("valid snapshot rejected")
	}
	now := s.DBNow
	pinned := "re_test_1"
	for name, mutate := range map[string]func(*stripeRefundSnapshot){
		"tenant":        func(s *stripeRefundSnapshot) { s.TenantID = "" },
		"environment":   func(s *stripeRefundSnapshot) { s.Environment = "LIVE" },
		"credential":    func(s *stripeRefundSnapshot) { s.CredentialVersion = 0 },
		"nonce":         func(s *stripeRefundSnapshot) { s.Nonce = nil },
		"amount":        func(s *stripeRefundSnapshot) { s.AmountMinor = 0 },
		"twd step":      func(s *stripeRefundSnapshot) { s.Currency, s.AmountMinor = "TWD", 150 },
		"window":        func(s *stripeRefundSnapshot) { s.ResendUntil = s.RequestedAt.Add(19 * time.Hour) },
		"send count":    func(s *stripeRefundSnapshot) { s.SendCount = 1 },
		"sent no count": func(s *stripeRefundSnapshot) { s.FirstSentAt = &now },
		"pin only":      func(s *stripeRefundSnapshot) { s.StripeRefundID = pinned },
	} {
		c := testRefundSnapshot()
		mutate(&c)
		if validRefundSnapshot(c, stripeTestOrder, "SANDBOX") {
			t.Errorf("%s accepted", name)
		}
	}
	c := testRefundSnapshot()
	c.StripeRefundID, c.PinnedAt, c.FirstSentAt, c.SendCount = pinned, &now, &now, 1
	if !validRefundSnapshot(c, stripeTestOrder, "SANDBOX") {
		t.Fatal("sent and pinned refund rejected")
	}
	if validRefundSnapshot(s, "not-an-id", "SANDBOX") || validRefundSnapshot(s, stripeTestOrder, "LIVE") {
		t.Fatal("id and profile must be valid")
	}
}

func TestRefundBackoffPollAndSuppressionReason(t *testing.T) {
	want := map[int64]time.Duration{-3: 5 * time.Second, 2: 5 * time.Second, 3: 15 * time.Second, 4: 45 * time.Second,
		5: 120 * time.Second, 50: 120 * time.Second}
	for generation, d := range want {
		if got := refundBackoff(generation); got != d {
			t.Errorf("generation %d: %s want %s", generation, got, d)
		}
	}
	requested := time.Unix(1790000000, 0).UTC()
	for _, c := range []struct {
		status string
		age    time.Duration
		want   time.Duration
	}{{"pending", 10 * time.Minute, time.Minute}, {"requires_action", 59 * time.Minute, time.Minute},
		{"pending", time.Hour, 15 * time.Minute}, {"pending", 23 * time.Hour, 15 * time.Minute},
		{"succeeded", time.Minute, 5 * time.Second}, {"failed", 2 * time.Hour, 5 * time.Second}} {
		if got := refundPollDelay(c.status, requested.Add(c.age), requested); got != c.want {
			t.Errorf("%s at %s: %s want %s", c.status, c.age, got, c.want)
		}
	}
	s := testRefundSnapshot()
	if suppressionReason(s) != "external_refund" {
		t.Fatal("inside the window a suppression is an external refund")
	}
	s.DBNow = s.ResendUntil.Add(-30 * time.Minute)
	if suppressionReason(s) != "send_window_closed" {
		t.Fatal("inside the last hour it is the closed window")
	}
}

func TestRefundWorkerRejectsInvalidConfigurationBeforeDatabase(t *testing.T) {
	ctx := context.Background()
	o := DefaultQueryWorkerOptions()
	if _, err := newRefundWorker(ctx, nil, nil, "SANDBOX", o); !errors.Is(err, errRefundJob) {
		t.Fatalf("nil pool: %v", err)
	}
	if _, err := newRefundWorker(nil, nil, nil, "SANDBOX", o); !errors.Is(err, errRefundJob) { //nolint:staticcheck // nil ctx is the case under test
		t.Fatalf("nil ctx: %v", err)
	}
	if _, err := newRefundWorker(ctx, nil, nil, "NOPE", o); !errors.Is(err, errRefundJob) {
		t.Fatalf("bad profile: %v", err)
	}
	var w *refundWorker
	if w.Timeout(nil) != 0 {
		t.Fatal("nil worker timeout must be zero")
	}
}
