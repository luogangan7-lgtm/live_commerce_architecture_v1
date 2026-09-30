package stripetest

// Self-tests of the refund half of the fake (refunds.go). They prove the fake behaves like the
// documented Stripe surface the RF gates rely on, so a green RF gate is not the fake agreeing
// with itself: idempotency, capacity, list order/paging, account isolation and fault shapes.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func rfDo(t *testing.T, s *Server, method, path, key, body string) (int, http.Header, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, s.URL()+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := s.http.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, res.Header, out
}

func rfBody(pi string, amount int64, ref string) string {
	v := url.Values{"payment_intent": {pi}, "amount": {strconv.FormatInt(amount, 10)}, "reason": {"requested_by_customer"},
		"metadata[lc_refund]": {ref}, "metadata[lc_attempt]": {"attempt-1"}}
	return v.Encode()
}

func rfServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := New("acct_RefundUnit1")
	t.Cleanup(s.Close)
	s.AddPaidPaymentIntent("acct_RefundUnit1", "pi_test_unit", 2500, "TWD")
	return s, "pi_test_unit"
}

func TestFakeRefundIdempotencyCapacityAndLifecycle(t *testing.T) {
	s, pi := rfServer(t)
	st, _, first := rfDo(t, s, "POST", "/v1/refunds", "k1", rfBody(pi, 1000, "r1"))
	if st != 200 || first["status"] != "succeeded" || first["amount"] != float64(1000) || first["payment_intent"] != pi {
		t.Fatalf("create: %d %v", st, first)
	}
	if md := first["metadata"].(map[string]any); md["lc_refund"] != "r1" || md["lc_attempt"] != "attempt-1" {
		t.Fatalf("metadata not echoed: %v", md)
	}
	st, h, replay := rfDo(t, s, "POST", "/v1/refunds", "k1", rfBody(pi, 1000, "r1"))
	if st != 200 || h.Get("Idempotent-Replayed") != "true" || replay["id"] != first["id"] || len(s.RefundIDs(pi)) != 1 {
		t.Fatalf("same-key replay: %d %v", st, replay)
	}
	if st, _, e := rfDo(t, s, "POST", "/v1/refunds", "k1", rfBody(pi, 900, "r1")); st != 400 || e["error"].(map[string]any)["type"] != "idempotency_error" {
		t.Fatalf("changed params under one key: %d %v", st, e)
	}
	if got := s.RefundPosts(); got != 3 {
		t.Fatalf("posts=%d want 3 (replays and faults count)", got)
	}
	// Capacity: 2500 captured, 1000 refunded -> 1501 is too large, 1500 is fine and exhausts it.
	if st, _, _ := rfDo(t, s, "POST", "/v1/refunds", "k2", rfBody(pi, 1501, "r2")); st != 400 {
		t.Fatalf("over-refund accepted: %d", st)
	}
	st, _, second := rfDo(t, s, "POST", "/v1/refunds", "k3", rfBody(pi, 1500, "r3"))
	if st != 200 {
		t.Fatalf("remainder refund: %d", st)
	}
	if st, _, e := rfDo(t, s, "POST", "/v1/refunds", "k4", rfBody(pi, 100, "r4")); st != 400 || e["error"].(map[string]any)["code"] != "charge_already_refunded" {
		t.Fatalf("refund of a fully refunded charge: %d %v", st, e)
	}
	// A failed refund frees its amount; a pending one holds it.
	if !s.SetRefundStatus(second["id"].(string), "failed", "declined") {
		t.Fatal("SetRefundStatus")
	}
	_, _, got := rfDo(t, s, "GET", "/v1/refunds/"+second["id"].(string), "", "")
	if got["status"] != "failed" || got["failure_reason"] != "declined" {
		t.Fatalf("failed refund view: %v", got)
	}
	s.HoldNextRefund("pending", "processing")
	st, _, pend := rfDo(t, s, "POST", "/v1/refunds", "k5", rfBody(pi, 1500, "r5"))
	if st != 200 || pend["status"] != "pending" || pend["pending_reason"] != "processing" {
		t.Fatalf("held refund: %d %v", st, pend)
	}
	if st, _, _ := rfDo(t, s, "POST", "/v1/refunds", "k6", rfBody(pi, 100, "r6")); st != 400 {
		t.Fatal("pending refund did not hold capacity")
	}
	if keys := s.RefundCreateKeys(); len(keys) != 8 || keys[0] != "k1" {
		t.Fatalf("keys=%v", keys)
	}
	if s.RefundByRef("r5") != pend["id"] || s.RefundByRef("nope") != "" {
		t.Fatal("RefundByRef")
	}
}

func TestFakeRefundRejectsUnknownAndForbiddenShapes(t *testing.T) {
	s, pi := rfServer(t)
	for name, mutate := range map[string]func(url.Values){
		"charge":           func(v url.Values) { v.Set("charge", "ch_x") },
		"reverse_transfer": func(v url.Values) { v.Set("reverse_transfer", "true") },
		"bad reason":       func(v url.Values) { v.Set("reason", "because") },
		"zero amount":      func(v url.Values) { v.Set("amount", "0") },
		"missing amount":   func(v url.Values) { v.Del("amount") },
		"unknown metadata": func(v url.Values) { v.Set("metadata[x]", "1") },
		"foreign pi":       func(v url.Values) { v.Set("payment_intent", "pi_other") },
	} {
		v, _ := url.ParseQuery(rfBody(pi, 100, "r"))
		mutate(v)
		if st, _, _ := rfDo(t, s, "POST", "/v1/refunds", "k-"+name, v.Encode()); st != 400 && st != 404 {
			t.Fatalf("%s accepted: %d", name, st)
		}
	}
	if n := len(s.RefundIDs(pi)); n != 0 {
		t.Fatalf("a refused shape created %d refunds", n)
	}
	// Stripe itself accepts reason=fraudulent; the adapter (not the fake) must refuse to send it.
	v, _ := url.ParseQuery(rfBody(pi, 100, "r"))
	v.Set("reason", "fraudulent")
	if st, _, _ := rfDo(t, s, "POST", "/v1/refunds", "k-fraud", v.Encode()); st != 200 {
		t.Fatalf("fake must mirror Stripe accepting fraudulent: %d", st)
	}
}

func TestFakeRefundFaults(t *testing.T) {
	s, pi := rfServer(t)
	s.SetNextFault(Fault{Cached500: true})
	if st, _, _ := rfDo(t, s, "POST", "/v1/refunds", "kf", rfBody(pi, 500, "rf")); st != 500 {
		t.Fatalf("cached500: %d", st)
	}
	st, h, _ := rfDo(t, s, "POST", "/v1/refunds", "kf", rfBody(pi, 500, "rf"))
	if st != 500 || h.Get("Idempotent-Replayed") != "true" || len(s.RefundIDs(pi)) != 1 {
		t.Fatalf("cached 500 replay: %d ids=%v (the refund did execute)", st, s.RefundIDs(pi))
	}
	s.SetNextFault(Fault{Cached500NoSession: true})
	rfDo(t, s, "POST", "/v1/refunds", "kn", rfBody(pi, 500, "rn"))
	if s.RefundByRef("rn") != "" {
		t.Fatal("Cached500NoSession left a refund behind")
	}
	s.SetNextFault(Fault{RateLimit: true})
	if st, _, _ := rfDo(t, s, "POST", "/v1/refunds", "kr", rfBody(pi, 500, "rr")); st != 429 {
		t.Fatalf("rate limit: %d", st)
	}
	if s.RefundByRef("rr") != "" {
		t.Fatal("pre-execution fault executed")
	}
	s.SetNextFault(Fault{DropAfterExecute: true})
	s.http.Client().CloseIdleConnections() // avoid net/http's transparent retry of an idempotent request
	req, _ := http.NewRequest("POST", s.URL()+"/v1/refunds", strings.NewReader(rfBody(pi, 500, "rd")))
	req.Header.Set("Idempotency-Key", "kd")
	if res, err := s.http.Client().Do(req); err == nil {
		res.Body.Close()
		t.Fatal("drop-after-execute answered")
	}
	if s.RefundByRef("rd") == "" {
		t.Fatal("drop-after-execute did not execute")
	}
	st, h, _ = rfDo(t, s, "POST", "/v1/refunds", "kd", rfBody(pi, 500, "rd"))
	if st != 200 || h.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("same-key replay after drop: %d", st)
	}
	s.FailNext("refund_retrieve", 503)
	if st, _, _ := rfDo(t, s, "GET", "/v1/refunds/"+s.RefundByRef("rd"), "", ""); st != 503 {
		t.Fatalf("FailNext refund_retrieve: %d", st)
	}
}

func TestFakeRefundListPagingAndPaymentIntentRead(t *testing.T) {
	s, pi := rfServer(t)
	var ids []string
	for i := 0; i < 25; i++ {
		_, _, r := rfDo(t, s, "POST", "/v1/refunds", "kl"+strconv.Itoa(i), rfBody(pi, 100, "rl"+strconv.Itoa(i)))
		ids = append(ids, r["id"].(string))
	}
	_, _, page := rfDo(t, s, "GET", "/v1/refunds?payment_intent="+pi+"&limit=10", "", "")
	data := page["data"].([]any)
	if len(data) != 10 || page["has_more"] != true || data[0].(map[string]any)["id"] != ids[24] {
		t.Fatalf("list must be newest first and paged: n=%d first=%v", len(data), data[0].(map[string]any)["id"])
	}
	last := data[9].(map[string]any)["id"].(string)
	_, _, page2 := rfDo(t, s, "GET", "/v1/refunds?payment_intent="+pi+"&limit=100&starting_after="+last, "", "")
	if d := page2["data"].([]any); len(d) != 15 || page2["has_more"] != false {
		t.Fatalf("second page n=%d", len(d))
	}
	if st, _, _ := rfDo(t, s, "GET", "/v1/refunds?limit=10", "", ""); st != 400 {
		t.Fatal("list without payment_intent accepted")
	}
	_, _, plain := rfDo(t, s, "GET", "/v1/payment_intents/"+pi, "", "")
	if plain["latest_charge"] != "ch_test_"+pi {
		t.Fatalf("unexpanded latest_charge: %v", plain["latest_charge"])
	}
	_, _, exp := rfDo(t, s, "GET", "/v1/payment_intents/"+pi+"?expand[]=latest_charge", "", "")
	ch := exp["latest_charge"].(map[string]any)
	if ch["amount_captured"] != float64(2500) || ch["amount_refunded"] != float64(2500) || ch["refunded"] != true || ch["disputed"] != false {
		t.Fatalf("expanded charge: %v", ch)
	}
	last2 := s.Requests()[len(s.Requests())-1]
	if last2.RawQuery != "expand[]=latest_charge" || last2.Path != "/v1/payment_intents/"+pi {
		t.Fatalf("query not audited: %+v", last2)
	}
	s.PatchCharge(pi, map[string]any{"disputed": true, "amount_captured": 2400})
	_, _, exp = rfDo(t, s, "GET", "/v1/payment_intents/"+pi+"?expand[]=latest_charge", "", "")
	if ch = exp["latest_charge"].(map[string]any); ch["disputed"] != true || ch["amount_captured"] != float64(2400) {
		t.Fatalf("PatchCharge: %v", ch)
	}
}

func TestFakeRefundDashboardAccountIsolationAndEvents(t *testing.T) {
	s := New("acct_RefundUnit1")
	defer s.Close()
	// Written split so no key-shaped literal exists (PROCESS.md §6, CI "No key-shaped secret literals").
	firstKey, secondKey := "sk_"+"test_firstaccount0000001", "sk_"+"test_secondaccount000001"
	if err := s.AddAccount("acct_RefundUnit2", secondKey); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireAPIKey(firstKey); err != nil {
		t.Fatal(err)
	}
	s.AddPaidPaymentIntent("acct_RefundUnit1", "pi_test_iso", 1000, "twd")
	call := func(key, method, path string) int {
		req, _ := http.NewRequest(method, s.URL()+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := s.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if call(firstKey, "GET", "/v1/payment_intents/pi_test_iso") != 200 || call(secondKey, "GET", "/v1/payment_intents/pi_test_iso") != 404 {
		t.Fatal("PaymentIntent must be visible to its own account only")
	}
	id := s.DashboardRefund("pi_test_iso", 400)
	if id == "" || s.RefundPosts() != 0 {
		t.Fatalf("dashboard refund id=%q posts=%d", id, s.RefundPosts())
	}
	if call(secondKey, "GET", "/v1/refunds/"+id) != 404 || call(firstKey, "GET", "/v1/refunds/"+id) != 200 {
		t.Fatal("refund scope")
	}
	var ev map[string]any
	body := s.RefundEventBody("evt_r1", "refund.updated", id, false)
	if err := json.Unmarshal(body, &ev); err != nil || ev["type"] != "refund.updated" {
		t.Fatalf("refund event: %v %s", err, body)
	}
	obj := ev["data"].(map[string]any)["object"].(map[string]any)
	if obj["object"] != "refund" || obj["payment_intent"] != "pi_test_iso" || !strings.Contains(string(body), SentinelARN) {
		t.Fatalf("refund event object: %v", obj)
	}
	if _, has := obj["client_reference_id"]; has {
		t.Fatal("refund objects carry no client_reference_id in Stripe")
	}
	if md := obj["metadata"].(map[string]any); len(md) != 0 {
		t.Fatalf("dashboard refund has no lc metadata: %v", md)
	}
	var cev map[string]any
	_ = json.Unmarshal(s.ChargeEventBody("evt_c1", "pi_test_iso", false), &cev)
	c := cev["data"].(map[string]any)["object"].(map[string]any)
	if cev["type"] != "charge.refunded" || c["object"] != "charge" || c["amount_refunded"] != float64(400) || c["payment_intent"] != "pi_test_iso" || c["refunded"] != false {
		t.Fatalf("charge event: %v", cev)
	}
	if s.RefundEventBody("e", "refund.updated", "re_nope", false) != nil || s.ChargeEventBody("e", "pi_nope", false) != nil {
		t.Fatal("events for unknown objects must be nil")
	}
}

func TestFakeRefundPaymentIntentFromPaidSession(t *testing.T) {
	s := New("acct_RefundUnit1")
	defer s.Close()
	_, _, sess, err := postCreate(t, s.http.Client(), s.URL(), "ks", testParams())
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err := json.Unmarshal(sess, &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)
	if s.PaymentIntentForSession(id) != "" {
		t.Fatal("unpaid session has a PaymentIntent")
	}
	if !s.SetState(id, "complete", "paid") {
		t.Fatal("SetState")
	}
	pi := s.PaymentIntentForSession(id)
	if pi != "pi_test_"+id {
		t.Fatalf("pi=%q", pi)
	}
	s.SetState(id, "complete", "paid") // idempotent: must not reset refunds
	rfDo(t, s, "POST", "/v1/refunds", "kx", rfBody(pi, 500, "rx"))
	s.SetState(id, "complete", "paid")
	if len(s.RefundIDs(pi)) != 1 {
		t.Fatal("re-pay reset the refund collection")
	}
}
