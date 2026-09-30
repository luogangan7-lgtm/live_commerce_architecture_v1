package merchantorders

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

const (
	rfOrder  = "00000000-0000-4000-8000-000000000011"
	rfRefund = "00000000-0000-4000-8000-000000000022"
)

func refundListJSON(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	list := map[string]any{"captured_minor": 10000, "refunded_minor": 3000, "pending_minor": 2000, "refundable_minor": 5000,
		"currency": "HKD", "items": []any{
			map[string]any{"refund_id": rfRefund, "amount_minor": 3000, "reason": "requested_by_customer", "state": "SUCCEEDED",
				"requested_at": "2026-09-29T01:02:03.000004Z", "updated_at": "2026-09-29T01:05:03.000004Z", "stripe_refund_id": "re_test_1"},
			map[string]any{"refund_id": "00000000-0000-4000-8000-000000000033", "amount_minor": 2000, "reason": "duplicate", "state": "REQUESTED",
				"requested_at": "2026-09-29T02:02:03.000004Z", "updated_at": "2026-09-29T02:02:03.000004Z", "stripe_refund_id": nil},
		}}
	if mutate != nil {
		mutate(list)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeRefundListAcceptsTheSQLShapeAndChecksTotals(t *testing.T) {
	got, err := decodeRefundList(refundListJSON(t, nil))
	if err != nil || got.RefundableMinor != 5000 || len(got.Items) != 2 || got.Items[0].StripeRefundID == nil || got.Items[1].StripeRefundID != nil {
		t.Fatalf("list=%+v err=%v", got, err)
	}
	items := func(l map[string]any) []any { return l["items"].([]any) }
	for name, mutate := range map[string]func(map[string]any){
		"refunded total":   func(l map[string]any) { l["refunded_minor"] = 1 },
		"pending total":    func(l map[string]any) { l["pending_minor"] = 1 },
		"refundable":       func(l map[string]any) { l["refundable_minor"] = 9999 },
		"over captured":    func(l map[string]any) { l["captured_minor"] = 100; l["refundable_minor"] = -4900 },
		"currency":         func(l map[string]any) { l["currency"] = "hkd" },
		"extra key":        func(l map[string]any) { l["debug"] = true },
		"missing items":    func(l map[string]any) { delete(l, "items") },
		"null items":       func(l map[string]any) { l["items"] = nil },
		"state":            func(l map[string]any) { items(l)[0].(map[string]any)["state"] = "DONE" },
		"reason":           func(l map[string]any) { items(l)[0].(map[string]any)["reason"] = "fraudulent" },
		"pending w/ pin":   func(l map[string]any) { items(l)[1].(map[string]any)["stripe_refund_id"] = "re_test_2" },
		"succeeded no pin": func(l map[string]any) { items(l)[0].(map[string]any)["stripe_refund_id"] = nil },
		"time order":       func(l map[string]any) { items(l)[0].(map[string]any)["updated_at"] = "2026-09-29T00:00:00.000000Z" },
		"time format":      func(l map[string]any) { items(l)[0].(map[string]any)["requested_at"] = "2026-09-29 01:02:03" },
		"failure on ok":    func(l map[string]any) { items(l)[0].(map[string]any)["failure_reason"] = "declined" },
		"refund id":        func(l map[string]any) { items(l)[0].(map[string]any)["refund_id"] = "nope" },
		"item extra key":   func(l map[string]any) { items(l)[0].(map[string]any)["arn"] = "x" },
		"21 items": func(l map[string]any) {
			many := make([]any, 21)
			for i := range many {
				many[i] = items(l)[1]
			}
			l["items"] = many
		},
	} {
		if _, err := decodeRefundList(refundListJSON(t, mutate)); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	failed := func(l map[string]any) {
		it := items(l)[0].(map[string]any)
		it["state"], it["failure_reason"] = "FAILED", "declined"
		l["refunded_minor"], l["refundable_minor"] = 0, 8000
	}
	if _, err := decodeRefundList(refundListJSON(t, failed)); err != nil {
		t.Fatalf("failed item with reason: %v", err)
	}
	if _, err := decodeRefundList([]byte(`[]`)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("array is not a list")
	}
}

func TestDecodeRefundResultIsExact(t *testing.T) {
	ok := `{"refund_id":"` + rfRefund + `","state":"REQUESTED","amount_minor":100,"currency":"TWD","refundable_minor":0}`
	if got, err := decodeRefundResult([]byte(ok)); err != nil || got.RefundID != rfRefund || got.AmountMinor != 100 {
		t.Fatalf("%+v %v", got, err)
	}
	for name, raw := range map[string]string{
		"state":    strings.Replace(ok, "REQUESTED", "SUBMITTING", 1),
		"extra":    strings.Replace(ok, "}", `,"x":1}`, 1),
		"missing":  strings.Replace(ok, `"refundable_minor":0`, `"other":0`, 1),
		"currency": strings.Replace(ok, "TWD", "twd", 1),
		"id":       strings.Replace(ok, rfRefund, "abc", 1),
		"negative": strings.Replace(ok, `"refundable_minor":0`, `"refundable_minor":-1`, 1),
	} {
		if _, err := decodeRefundResult([]byte(raw)); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestMapRefundErrorUsesOnlyStateAndFixedMessages(t *testing.T) {
	pg := func(code, message string) error {
		return &pgconn.PgError{Code: code, Message: message, Detail: "SENTINEL customer detail"}
	}
	for _, c := range []struct {
		err  error
		want error
	}{
		{pg("PT409", "refundable_changed"), ErrRefundableChanged},
		{pg("PT409", "refund key conflict"), command.ErrConflict},
		{pg("PT409", "refund job unavailable"), command.ErrConflict},
		{pg("PT422", "exceeds_refundable"), ErrExceedsRefundable},
		{pg("PT422", "amount_step"), ErrAmountStep},
		{pg("PT422", "not_refundable"), ErrNotRefundable},
		{pg("PT422", "refund_blocked_review"), ErrRefundBlockedReview},
		{pg("PT422", "refund_limit"), ErrRefundLimit},
		{pg("PT422", "something else"), command.ErrInvalid},
		{pg("PT400", "invalid"), command.ErrInvalid},
		{pg("23505", "duplicate"), command.ErrConflict},
		{pg("PT401", "x"), platform.ErrUnauthorized},
		{pg("PT403", "x"), platform.ErrForbidden},
		{pg("PT404", "x"), platform.ErrScopeNotFound},
		{pg("XX000", "SENTINEL"), ErrUnavailable},
		{errors.New("SENTINEL raw"), ErrUnavailable},
	} {
		got := mapRefundError(c.err)
		if !errors.Is(got, c.want) {
			t.Errorf("%v -> %v want %v", c.err, got, c.want)
		}
		if strings.Contains(got.Error(), "SENTINEL") {
			t.Errorf("mapped error leaks a driver value: %v", got)
		}
	}
}

func TestRefundServiceRejectsInvalidInputBeforeDatabase(t *testing.T) {
	scope := platform.Scope{TenantID: rfOrder, StoreID: rfOrder, PrincipalID: rfOrder, Revision: 1}
	token := strings.Repeat("t", 40)
	good := RefundRequest{AmountMinor: 100, Reason: "duplicate", ExpectedRefundableMinor: 100}
	if !validRefundRequest(good) {
		t.Fatal("valid request rejected")
	}
	for name, in := range map[string]RefundRequest{
		"zero":       {AmountMinor: 0, Reason: "duplicate"},
		"fraudulent": {AmountMinor: 1, Reason: "fraudulent"},
		"negative":   {AmountMinor: 1, Reason: "duplicate", ExpectedRefundableMinor: -1},
		"huge":       {AmountMinor: 1_000_000_000_001, Reason: "duplicate"},
	} {
		if validRefundRequest(in) {
			t.Errorf("%s accepted", name)
		}
	}
	// nil tx / client are refused before any statement can run.
	if _, err := RequestRefund(context.Background(), nil, nil, scope, token, "key-12345678", rfOrder, good); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil tx: %v", err)
	}
	if _, err := ListRefunds(context.Background(), nil, scope, token, rfOrder); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil tx list: %v", err)
	}
	if _, err := RefreshRefund(context.Background(), nil, nil, scope, token, rfOrder, rfRefund); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil tx refresh: %v", err)
	}
	if !refundKey.MatchString("abcdefgh") || refundKey.MatchString("short") || refundKey.MatchString(strings.Repeat("a", 129)) {
		t.Fatal("idempotency key grammar")
	}
}
