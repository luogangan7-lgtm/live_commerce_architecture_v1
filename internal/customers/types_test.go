// types_test.go covers the pure logic of internal/customers: CD4 current-consent derivation, query and key
// validation, strict projection decoding, the export size cap and the database-error mapping. Real-PG behaviour
// (definers, RLS, erasure, keyset) is gated by customers-billing-tests CB03-CB05, not here.

package customers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/platform"
)

func ev(purpose, channel string, granted bool, at string) ConsentEvent {
	source := "buyer_settings"
	if !granted {
		source = "merchant_recorded"
	}
	return ConsentEvent{Purpose: purpose, Channel: channel, Granted: granted, Source: source, PolicyVersion: "lc-2026-10", OccurredAt: at}
}

func TestCurrentConsents(t *testing.T) {
	const t1, t2, t3 = "2026-09-30T01:00:00.000000Z", "2026-09-30T02:00:00.000000Z", "2026-09-30T03:00:00.000000Z"
	for _, tc := range []struct {
		name    string
		history []ConsentEvent
		want    Consents
	}{
		{"absence is not granted", nil, Consents{}},
		{"grant", []ConsentEvent{ev("marketing_messages", "meta_dm", true, t1)}, Consents{MarketingMessages: true}},
		{"withdrawn after grant (newest first)", []ConsentEvent{
			ev("marketing_messages", "meta_dm", false, t2), ev("marketing_messages", "meta_dm", true, t1)}, Consents{}},
		{"withdrawn after grant (oldest first still decided by time)", []ConsentEvent{
			ev("marketing_messages", "meta_dm", true, t1), ev("marketing_messages", "meta_dm", false, t2)}, Consents{}},
		{"re-grant after withdrawal", []ConsentEvent{
			ev("ads_personalization", "meta_ads", true, t3), ev("ads_personalization", "meta_ads", false, t2),
			ev("ads_personalization", "meta_ads", true, t1)}, Consents{AdsPersonalization: true}},
		{"purposes are independent", []ConsentEvent{
			ev("ads_personalization", "meta_ads", true, t2), ev("marketing_messages", "meta_dm", false, t3),
			ev("marketing_messages", "meta_dm", true, t1)}, Consents{AdsPersonalization: true}},
		{"wrong channel for the purpose is ignored", []ConsentEvent{ev("marketing_messages", "meta_ads", true, t1)}, Consents{}},
		{"unknown purpose is ignored", []ConsentEvent{ev("profiling", "meta_dm", true, t1)}, Consents{}},
		{"equal timestamps: the earlier list entry (newest first) wins", []ConsentEvent{
			ev("marketing_messages", "meta_dm", false, t1), ev("marketing_messages", "meta_dm", true, t1)}, Consents{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CurrentConsents(tc.history); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestValidPair(t *testing.T) {
	for _, tc := range []struct {
		purpose, channel string
		ok               bool
	}{
		{"marketing_messages", "meta_dm", true}, {"ads_personalization", "meta_ads", true},
		{"marketing_messages", "meta_ads", false}, {"ads_personalization", "meta_dm", false},
		{"", "", false}, {"marketing_messages", "", false}, {"Marketing_messages", "meta_dm", false},
	} {
		if ValidPair(tc.purpose, tc.channel) != tc.ok {
			t.Fatalf("%+v", tc)
		}
	}
}

func TestNormalizeQuery(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"alice", "alice", true}, {"  alice  ", "alice", true}, {"0912-345 678", "0912-345 678", true},
		{"+886 912", "+886 912", true}, {strings.Repeat("a", 40), strings.Repeat("a", 40), true},
		{strings.Repeat("陳", 40), strings.Repeat("陳", 40), true},
		{"", "", false}, {"   ", "", false}, {strings.Repeat("a", 41), "", false}, {strings.Repeat("陳", 41), "", false},
		{"a\x00b", "", false}, {"a\nb", "", false}, {"\xff", "", false},
	} {
		got, err := NormalizeQuery(tc.in)
		if (err == nil) != tc.ok || got != tc.want || (err != nil && !errors.Is(err, command.ErrInvalid)) {
			t.Fatalf("%q -> %q, %v (want %q ok=%v)", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestKeyUUID(t *testing.T) {
	a, err := KeyUUID("consent-key-0001")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := KeyUUID("consent-key-0001")
	c, _ := KeyUUID("consent-key-0002")
	if a != b || a == c || len(a) != 36 || a[14] != '5' || strings.Contains("01234567", string(a[19])) {
		t.Fatalf("a=%s b=%s c=%s", a, b, c)
	}
	for _, bad := range []string{"", "short", strings.Repeat("k", 129), "has space key", "semi;colon-key", "über-key-0001"} {
		if _, err := KeyUUID(bad); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	if _, err := KeyUUID(strings.Repeat("k", 128)); err != nil {
		t.Fatal(err)
	}
}

func TestConsentSourceFor(t *testing.T) {
	for context, want := range map[string]string{"checkout": "buyer_checkout", "settings": "buyer_settings"} {
		if got, err := consentSourceFor(context); err != nil || got != want {
			t.Fatalf("%s -> %s, %v", context, got, err)
		}
	}
	for _, bad := range []string{"", "Checkout", "merchant", "erasure", "buyer_checkout", "source"} {
		if _, err := consentSourceFor(bad); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

const (
	testTS   = "2026-09-30T04:05:06.123456Z"
	testUUID = "11111111-1111-4111-8111-111111111111"
)

func rowJSON(t *testing.T, mutate func(map[string]any)) json.RawMessage {
	t.Helper()
	m := map[string]any{"customer_id": testUUID, "first_seen_at": testTS, "last_activity_at": testTS, "display_name": "Alice",
		"phone_last3": "678", "orders_count": 2, "paid_orders_count": 1, "captured_minor": 1200, "refunded_minor": 200,
		"currency": "TWD", "claims_count": 1, "platforms": []string{"facebook"},
		"consents": map[string]bool{"marketing_messages": false, "ads_personalization": true}, "active": true}
	if mutate != nil {
		mutate(m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeCustomerStrict(t *testing.T) {
	if c, err := decodeCustomer(rowJSON(t, nil)); err != nil || c.CustomerID != testUUID || !c.Consents.AdsPersonalization {
		t.Fatalf("%+v %v", c, err)
	}
	bundleOnly := rowJSON(t, func(m map[string]any) {
		m["display_name"], m["phone_last3"], m["currency"] = nil, nil, nil
		m["orders_count"], m["paid_orders_count"], m["captured_minor"], m["refunded_minor"] = 0, 0, 0, 0
	})
	if _, err := decodeCustomer(bundleOnly); err != nil {
		t.Fatalf("bundle-only customer rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"extra key":               func(m map[string]any) { m["actor_key"] = strings.Repeat("a", 64) },
		"missing key":             func(m map[string]any) { delete(m, "active") },
		"bad id":                  func(m map[string]any) { m["customer_id"] = "nope" },
		"non-canonical time":      func(m map[string]any) { m["last_activity_at"] = "2026-09-30T04:05:06Z" },
		"negative count":          func(m map[string]any) { m["orders_count"] = -1 },
		"paid more than orders":   func(m map[string]any) { m["paid_orders_count"] = 3 },
		"refunded above captured": func(m map[string]any) { m["refunded_minor"] = 5000 },
		"bad phone":               func(m map[string]any) { m["phone_last3"] = "12" },
		"bad currency":            func(m map[string]any) { m["currency"] = "twd" },
		"orders without currency": func(m map[string]any) { m["currency"] = nil },
		"unknown platform":        func(m map[string]any) { m["platforms"] = []string{"tiktok"} },
		"unsorted platforms":      func(m map[string]any) { m["platforms"] = []string{"instagram", "facebook"} },
		"duplicate platforms":     func(m map[string]any) { m["platforms"] = []string{"facebook", "facebook"} },
		"null platforms":          func(m map[string]any) { m["platforms"] = nil },
		"unknown consent key": func(m map[string]any) {
			m["consents"] = map[string]bool{"marketing_messages": true, "ads_personalization": true, "x": true}
		},
	} {
		if _, err := decodeCustomer(rowJSON(t, mutate)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func detailJSON(t *testing.T, mutate func(map[string]any)) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rowJSON(t, nil), &m); err != nil {
		t.Fatal(err)
	}
	m["order_ids"] = []string{testUUID}
	m["claims"] = []map[string]any{{"session_id": testUUID, "platform": "facebook", "bound_at": testTS, "line_count": 2}}
	m["consent_history"] = []map[string]any{
		{"purpose": "ads_personalization", "channel": "meta_ads", "granted": true, "source": "buyer_checkout", "policy_version": "lc-2026-10", "occurred_at": testTS}}
	m["privacy_actions"] = []map[string]any{{"kind": "EXPORT", "via": "merchant", "completed_at": testTS, "summary": map[string]int{"orders": 1}}}
	if mutate != nil {
		mutate(m)
	}
	raw, _ := json.Marshal(m)
	return raw
}

func TestDecodeDetailStrict(t *testing.T) {
	d, err := decodeDetail(detailJSON(t, nil))
	if err != nil || len(d.OrderIDs) != 1 || len(d.Claims) != 1 || len(d.ConsentHistory) != 1 || len(d.PrivacyActions) != 1 {
		t.Fatalf("%+v %v", d, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"row consents disagree with history": func(m map[string]any) {
			m["consents"] = map[string]bool{"marketing_messages": true, "ads_personalization": true}
		},
		"merchant grant in history": func(m map[string]any) {
			m["consent_history"] = []map[string]any{{"purpose": "marketing_messages", "channel": "meta_dm", "granted": true,
				"source": "merchant_recorded", "policy_version": "merchant", "occurred_at": testTS}}
			m["consents"] = map[string]bool{"marketing_messages": true, "ads_personalization": false}
		},
		"actor key in claims": func(m map[string]any) {
			m["claims"] = []map[string]any{{"session_id": testUUID, "platform": "facebook", "bound_at": testTS, "line_count": 2, "actor_key": "x"}}
		},
		"bad order id": func(m map[string]any) { m["order_ids"] = []string{"x"} },
		"null history": func(m map[string]any) { m["consent_history"] = nil },
		"summary not object": func(m map[string]any) {
			m["privacy_actions"] = []map[string]any{{"kind": "EXPORT", "via": "buyer", "completed_at": testTS, "summary": 3}}
		},
		"bad action kind": func(m map[string]any) {
			m["privacy_actions"] = []map[string]any{{"kind": "DELETE", "via": "buyer", "completed_at": testTS, "summary": map[string]int{}}}
		},
		"missing key": func(m map[string]any) { delete(m, "privacy_actions") },
	} {
		if _, err := decodeDetail(detailJSON(t, mutate)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	tooMany := detailJSON(t, func(m map[string]any) {
		ids := make([]string, MaxExportOrders+2)
		for i := range ids {
			ids[i] = testUUID
		}
		m["order_ids"] = ids
	})
	if _, err := decodeDetail(tooMany); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unbounded order ids accepted: %v", err)
	}
}

func TestDecodeConsentResultAndErasure(t *testing.T) {
	ok := `{"purpose":"marketing_messages","channel":"meta_dm","granted":false,"occurred_at":"` + testTS + `"}`
	if r, err := decodeConsentResult([]byte(ok), "marketing_messages", "meta_dm"); err != nil || r.Granted || r.OccurredAt != testTS {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{
		`{"purpose":"marketing_messages","channel":"meta_dm","granted":false}`,
		`{"purpose":"marketing_messages","channel":"meta_dm","granted":false,"occurred_at":"` + testTS + `","source":"x"}`,
		`{"purpose":"ads_personalization","channel":"meta_ads","granted":false,"occurred_at":"` + testTS + `"}`,
		`{"purpose":"marketing_messages","channel":"meta_dm","granted":false,"occurred_at":"2026-01-01"}`,
	} {
		if _, err := decodeConsentResult([]byte(bad), "marketing_messages", "meta_dm"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted %s", bad)
		}
	}
	if s, err := decodeErasure([]byte(`{"consents_withdrawn":2,"sessions_revoked":1,"snapshots_redacted":3,"bundles_relabelled":0}`)); err != nil ||
		s != (ErasureSummary{2, 1, 3, 0}) {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{`{}`, `{"consents_withdrawn":-1,"sessions_revoked":1,"snapshots_redacted":3,"bundles_relabelled":0}`,
		`{"consents_withdrawn":2,"sessions_revoked":1,"snapshots_redacted":3,"bundles_relabelled":0,"name":"x"}`} {
		if _, err := decodeErasure([]byte(bad)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestMarshalBoundedExportCaps(t *testing.T) {
	small := buyerExportDoc{Format: ExportFormat, Orders: []json.RawMessage{json.RawMessage(`{"order_id":"x"}`)}}
	if body, err := marshalBounded(small, 1); err != nil || !strings.Contains(string(body), ExportFormat) ||
		strings.Contains(string(body), "customer_id") || strings.Contains(string(body), "principal") {
		t.Fatalf("%s %v", body, err)
	}
	if _, err := marshalBounded(small, MaxExportOrders); err != nil {
		t.Fatalf("exactly %d orders must pass: %v", MaxExportOrders, err)
	}
	if _, err := marshalBounded(small, MaxExportOrders+1); !errors.Is(err, ErrExportTooLarge) {
		t.Fatalf("201 orders accepted: %v", err)
	}
	big := buyerExportDoc{Format: ExportFormat, Orders: []json.RawMessage{json.RawMessage(`"` + strings.Repeat("a", MaxExportBytes) + `"`)}}
	if _, err := marshalBounded(big, 1); !errors.Is(err, ErrExportTooLarge) {
		t.Fatalf("over 1 MiB accepted: %v", err)
	}
	merchant := exportDoc{Format: ExportFormat, CustomerID: testUUID, Orders: []merchantorders.Detail{}}
	if body, err := marshalBounded(merchant, 0); err != nil || !strings.Contains(string(body), `"customer_id"`) {
		t.Fatalf("%s %v", body, err)
	}
	if s := exportSummary(1, 2, 3, 4); s != `{"orders":1,"consents":2,"claims":3,"privacy_actions":4}` || len(s) > 1024 {
		t.Fatal(s)
	}
}

func TestErrorMapping(t *testing.T) {
	pg := func(code, message string) error { return &pgconn.PgError{Code: code, Message: message} }
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"invalid", pg("PT400", "x"), command.ErrInvalid},
		{"unauthorized", pg("PT401", "x"), platform.ErrUnauthorized},
		{"forbidden", pg("PT403", "x"), platform.ErrForbidden},
		{"not found", pg("PT404", "customer not found"), platform.ErrScopeNotFound},
		{"idempotency", pg("PT409", "idempotency_conflict"), ErrIdempotencyConflict},
		{"blocked", pg("PT409", "erasure_blocked"), ErrErasureBlocked},
		{"erased", pg("PT410", "erased"), ErrErased},
		{"oversize read", pg("PT503", "x"), ErrUnavailable},
		{"unknown class", pg("XX000", "customer name Alice"), ErrUnavailable},
		{"plain error", errors.New("Alice at 0912345678"), ErrUnavailable},
	} {
		got := mapMerchantError(tc.err)
		if !errors.Is(got, tc.want) || strings.Contains(got.Error(), "Alice") {
			t.Fatalf("%s: %v", tc.name, got)
		}
	}
	if !errors.Is(mapBuyerError(pg("PT401", "x")), buyer.ErrUnauthorized) || !errors.Is(mapBuyerError(pg("PT410", "erased")), ErrErased) ||
		!errors.Is(mapBuyerError(pg("PT409", "erasure_blocked")), ErrErasureBlocked) ||
		!errors.Is(mapBuyerError(pg("PT409", "idempotency_conflict")), ErrIdempotencyConflict) ||
		!errors.Is(mapBuyerError(errors.New("boom")), ErrUnavailable) {
		t.Fatal("buyer mapping")
	}
	// deadlocks and lock timeouts are passed through so the HTTP layer can answer retry_later.
	if got := mapMerchantError(pg("40P01", "deadlock")); got == nil || errors.Is(got, ErrUnavailable) {
		t.Fatalf("deadlock hidden: %v", got)
	}
}

func TestBuyerTokenAndAuthorityInput(t *testing.T) {
	if !validBuyerToken(strings.Repeat("A", 43)) || validBuyerToken(strings.Repeat("A", 42)) || validBuyerToken(strings.Repeat("!", 43)) {
		t.Fatal("token grammar")
	}
	good := platform.Scope{TenantID: testUUID, StoreID: testUUID, PrincipalID: testUUID, Revision: 1}
	if !validAuthorityInput(good, strings.Repeat("t", 40)) || validAuthorityInput(good, "short") {
		t.Fatal("authority input")
	}
	good.Revision = 0
	if validAuthorityInput(good, strings.Repeat("t", 40)) {
		t.Fatal("zero revision accepted")
	}
	// A nil tx is refused before any database call for every entry point.
	if _, err := List(nil, nil, platform.Scope{}, "", ListRequest{}); !errors.Is(err, command.ErrInvalid) { //nolint:staticcheck // nil ctx is never reached
		t.Fatalf("list: %v", err)
	}
	if _, err := Erase(nil, nil, platform.Scope{}, "", "erase-key-0001", testUUID); !errors.Is(err, command.ErrInvalid) { //nolint:staticcheck
		t.Fatalf("erase: %v", err)
	}
	if _, err := BuyerErase(nil, nil, strings.Repeat("A", 43), testUUID, "erase-key-0001"); !errors.Is(err, command.ErrInvalid) { //nolint:staticcheck
		t.Fatalf("buyer erase: %v", err)
	}
}
