package foundation_test

// TCV06 TestCvsStatusIngress (contracts/taiwan-cvs-logistics-v1.md §10 TCV06, §6 state machine, §7.5 status ingress, §4.3 ingest_ecpay_status).
// Prefix `tst`. Tier MOCK + HTTP_PG: signed F7 notifications (ecpaytest.SignStatus) POSTed to the mounted hooks handler; the shipments are created
// by the real dispatcher route against the fake.
// Fault injection (disclosed owner-pool write): a test-only DEFERRED constraint trigger on fulfillment.cvs_shipment_events that raises at COMMIT for
// provider_code '7777' only, dropped at cleanup; it proves "1|OK only after COMMIT".
// Recipient PII in every notification is the sentinel SENTINELRECIPIENT; it must never appear in a stored row.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
)

func (e *tcvEnv) tstEvent(order, code string) (n int, providerCode, providerMessage, providerUpdated *string) {
	e.t.Helper()
	n = e.events(order, "AND source='ecpay_status' AND ($2='' OR provider_code=$2)", code)
	_ = e.p.f.owner.QueryRow(context.Background(), `SELECT provider_code,provider_message,provider_updated_at FROM fulfillment.cvs_shipment_events WHERE order_id=$1 AND source='ecpay_status' ORDER BY received_at DESC LIMIT 1`, order).
		Scan(&providerCode, &providerMessage, &providerUpdated)
	return
}

func (e *tcvEnv) tstEventsTotal(order string) int { return e.events(order, "") }

func TestCvsStatusIngress(t *testing.T) {
	e := tcvNew(t)
	f := e.p.f
	ctx := context.Background()
	e.startDispatcher()
	e.grantCreator("orders:read", "fulfillment:write", "integration:manage", "integration:read")
	e.connect("C2C")
	mustExec(t, f.owner, `UPDATE integration.ecpay_logistics_profiles SET hilife_verified=true WHERE tenant_id=$1 AND store_id=$2 AND enabled`, f.tenantA, f.storeA1) // registrar-only in production (disclosed fixture)
	e.cvsSettings(tcvAllChains, true, "20000", 500)
	api711, _, _ := e.service("cvs_711", "API", 0)
	apiFami, _, _ := e.service("cvs_familymart", "API", 0)
	apiHilife, _, _ := e.service("cvs_hilife", "API", 0)
	endpoint := e.endpointID()
	newOrder := func(kind, code string) string {
		order, _ := e.cvsOrder(tcvOrderSpec{kind: kind, code: code, paymentMode: "pay_at_pickup"})
		if st, _, raw := e.ship(e.token(), order, 0, "", true); st != 202 {
			t.Fatalf("request shipment: %d %s", st, raw)
		}
		e.awaitShip(order, "CREATED")
		return order
	}
	state := func(order string) string { s, _, _ := e.shipState(order); return s }
	post := func(order, code string) *httptest.ResponseRecorder {
		return e.postStatus(endpoint, e.statusFor(order, code))
	}
	ok := func(w *httptest.ResponseRecorder) bool { return w.Code == 200 && w.Body.String() == "1|OK" }

	t.Run("every F7 code per subtype: exact transitions; foreign-chain codes and jumps are event only", func(t *testing.T) {
		type chain struct {
			label, kind, code       string
			atDC, atStore, picked   string
			returned                string
			foreignDC, foreignStore string
		}
		for _, c := range []chain{
			{"UNIMARTC2C", "cvs_711", api711, "2030", "2073", "2067", "2074", "3024", "3018"},
			{"FAMIC2C", "cvs_familymart", apiFami, "3024", "3018", "3022", "3020", "2030", "2073"},
			{"HILIFEC2C", "cvs_hilife", apiHilife, "3024", "3018", "3022", "3020", "2030", "2073"},
		} {
			order := newOrder(c.kind, c.code)
			// the other chain's codes do nothing
			for _, foreign := range []string{c.foreignDC, c.foreignStore} {
				n := e.tstEventsTotal(order)
				if w := post(order, foreign); !ok(w) {
					t.Errorf("%s foreign code %s: %d %q (ACK anyway)", c.label, foreign, w.Code, w.Body.String())
				}
				if state(order) != "CREATED" {
					t.Errorf("%s: foreign-chain code %s moved the shipment to %s", c.label, foreign, state(order))
				}
				if e.tstEventsTotal(order) != n+1 {
					t.Errorf("%s: foreign-chain code %s must be recorded as one event", c.label, foreign)
				}
			}
			e.tppStatuses(endpoint, order, c.atDC)
			if state(order) != "AT_DC" {
				t.Errorf("%s %s: %s want AT_DC", c.label, c.atDC, state(order))
			}
			// backwards jump: the DC code again after AT_DC is a no-op; a code for "at store" moves on
			if w := post(order, c.atDC); !ok(w) || state(order) != "AT_DC" {
				t.Errorf("%s: repeating %s: %d state %s", c.label, c.atDC, w.Code, state(order))
			}
			e.tppStatuses(endpoint, order, c.atStore)
			if state(order) != "AT_STORE" {
				t.Errorf("%s %s: %s want AT_STORE", c.label, c.atStore, state(order))
			}
			// backwards from AT_STORE: event only
			n := e.tstEventsTotal(order)
			if w := post(order, c.atDC); !ok(w) || state(order) != "AT_STORE" || e.tstEventsTotal(order) != n+1 {
				t.Errorf("%s: a backwards jump must be event only (state %s)", c.label, state(order))
			}
			// unknown code: event only
			n = e.tstEventsTotal(order)
			if w := post(order, "9999"); !ok(w) || state(order) != "AT_STORE" || e.tstEventsTotal(order) != n+1 {
				t.Errorf("%s: an unknown code must be event only", c.label)
			}
			e.tppStatuses(endpoint, order, c.picked)
			if state(order) != "PICKED_UP" {
				t.Errorf("%s %s: %s want PICKED_UP", c.label, c.picked, state(order))
			}
			// terminal: nothing moves a PICKED_UP shipment
			if w := post(order, c.returned); !ok(w) || state(order) != "PICKED_UP" {
				t.Errorf("%s: %s after PICKED_UP: %d state %s (v1: terminal)", c.label, c.returned, w.Code, state(order))
			}
			// CREATED -> AT_STORE directly, then unclaimed, then re-delivery
			o2 := newOrder(c.kind, c.code)
			e.tppStatuses(endpoint, o2, c.atStore)
			if state(o2) != "AT_STORE" {
				t.Errorf("%s: CREATED -> %s must reach AT_STORE, got %s", c.label, c.atStore, state(o2))
			}
			e.tppStatuses(endpoint, o2, c.returned)
			if state(o2) != "UNCLAIMED" {
				t.Errorf("%s %s: %s want UNCLAIMED", c.label, c.returned, state(o2))
			}
			redeliver := "2098"
			if c.label != "UNIMARTC2C" {
				redeliver = "" // 2098 is a 7-ELEVEN code (F7): FamilyMart / Hi-Life have no re-delivery code
			}
			if redeliver != "" {
				e.tppStatuses(endpoint, o2, redeliver)
				if state(o2) != "AT_STORE" {
					t.Errorf("%s 2098 re-delivery: %s want AT_STORE", c.label, state(o2))
				}
			}
		}
	})

	t.Run("duplicate body: one event, still 1|OK", func(t *testing.T) {
		order := newOrder("cvs_711", api711)
		fields := e.statusFor(order, "2030")
		received := func() int { return e.events(order, "AND source='ecpay_status'") }
		before := received()
		for i := 0; i < 3; i++ {
			if w := e.postStatus(endpoint, fields); !ok(w) {
				t.Fatalf("delivery %d: %d %q", i, w.Code, w.Body.String())
			}
		}
		if got := received(); got != before+1 {
			t.Errorf("identical redeliveries wrote %d events, want 1", got-before)
		}
		if state(order) != "AT_DC" {
			t.Errorf("state %s", state(order))
		}
	})

	t.Run("bad MAC, wrong MerchantID, unknown endpoint, wrong method: no rows, no 1|OK", func(t *testing.T) {
		order := newOrder("cvs_711", api711)
		n := e.tstEventsTotal(order)
		fields := e.statusFor(order, "2030")
		bad := url.Values{}
		for k, v := range ecpaytest.SignStatus(e.mk, fields) {
			bad[k] = v
		}
		bad.Set("RtnMsg", "tampered")
		w := e.hook("/v1/cvs/ecpay/status/"+endpoint, bad, nil)
		if w.Code != 400 || w.Body.String() == "1|OK" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("bad MAC: %d %q (want 400 text/plain, never 1|OK)", w.Code, w.Body.String())
		}
		wrongMerchant := ecpaytest.StatusFields(mustTrade(t, e, order), "2030", "2026/09/30 10:00:00", "SENTINELRECIPIENT")
		wrongMerchant.Set("MerchantID", "9999999999")
		if w := e.postStatus(endpoint, wrongMerchant); w.Code != 400 || w.Body.String() == "1|OK" {
			t.Errorf("wrong MerchantID (validly signed): %d %q", w.Code, w.Body.String())
		}
		if w := e.postStatus(randomUUID(), fields); w.Code != 404 || w.Body.String() == "1|OK" {
			t.Errorf("unknown endpoint: %d %q (want 404)", w.Code, w.Body.String())
		}
		if w := e.postStatus("not-a-uuid", fields); w.Code < 400 || w.Body.String() == "1|OK" {
			t.Errorf("malformed endpoint id: %d %q", w.Code, w.Body.String())
		}
		req := httptest.NewRequest("GET", "/v1/cvs/ecpay/status/"+endpoint, nil)
		rec := httptest.NewRecorder()
		e.hooks.ServeHTTP(rec, req)
		if rec.Code < 400 || rec.Body.String() == "1|OK" {
			t.Errorf("GET on the status route: %d", rec.Code)
		}
		if e.tstEventsTotal(order) != n || state(order) != "CREATED" {
			t.Errorf("a refused notification changed events %d -> %d or state %s", n, e.tstEventsTotal(order), state(order))
		}
	})

	t.Run("an extra unknown field is MAC-covered, then dropped", func(t *testing.T) {
		order := newOrder("cvs_711", api711)
		fields := e.statusFor(order, "2030")
		fields.Set("SomethingNew", "value")
		signed := ecpaytest.SignStatus(e.mk, fields)
		tampered := url.Values{}
		for k, v := range signed {
			tampered[k] = v
		}
		tampered.Set("SomethingNew", "other")
		n := e.tstEventsTotal(order)
		if w := e.hook("/v1/cvs/ecpay/status/"+endpoint, tampered, nil); w.Code != 400 || w.Body.String() == "1|OK" {
			t.Errorf("tampering the unknown field must fail the MAC: %d %q", w.Code, w.Body.String())
		}
		if e.tstEventsTotal(order) != n {
			t.Error("a rejected notification wrote an event")
		}
		if w := e.hook("/v1/cvs/ecpay/status/"+endpoint, signed, nil); !ok(w) || state(order) != "AT_DC" {
			t.Errorf("the signed notification with an extra field: %d %q state %s", w.Code, w.Body.String(), state(order))
		}
	})

	t.Run("non-conforming UpdateStatusDate/RtnCode and a 500-char RtnMsg with control characters: stored NULL/truncated, 1|OK", func(t *testing.T) {
		order := newOrder("cvs_711", api711)
		fields := e.statusFor(order, "2030")
		fields.Set("UpdateStatusDate", "2026-09-30T10:00:00Z")
		fields.Set("RtnMsg", "M\x07"+strings.Repeat("x\x01", 249)+"end")
		if w := e.postStatus(endpoint, fields); !ok(w) {
			t.Fatalf("a vendor variant must never abort the tx and lose the report: %d %q", w.Code, w.Body.String())
		}
		var updated, msg *string
		var msgLen int
		if err := f.owner.QueryRow(ctx, `SELECT provider_updated_at,provider_message,char_length(coalesce(provider_message,'')) FROM fulfillment.cvs_shipment_events WHERE order_id=$1 AND source='ecpay_status' ORDER BY received_at DESC LIMIT 1`, order).Scan(&updated, &msg, &msgLen); err != nil {
			t.Fatal(err)
		}
		if updated != nil {
			t.Errorf("a non-conforming UpdateStatusDate is stored NULL, got %q", *updated)
		}
		if msgLen > 200 || (msg != nil && strings.ContainsAny(*msg, "\x01\x07")) {
			t.Errorf("RtnMsg must be truncated to 200 chars and stripped of control characters: len %d", msgLen)
		}
		fields2 := e.statusFor(order, "2073")
		fields2.Set("RtnCode", "ABC")
		if w := e.postStatus(endpoint, fields2); !ok(w) {
			t.Fatalf("a non-numeric RtnCode: %d %q", w.Code, w.Body.String())
		}
		var code *string
		if err := f.owner.QueryRow(ctx, `SELECT provider_code FROM fulfillment.cvs_shipment_events WHERE order_id=$1 AND source='ecpay_status' ORDER BY received_at DESC LIMIT 1`, order).Scan(&code); err != nil || code != nil {
			t.Errorf("a non-conforming RtnCode is stored NULL (event only, no transition): %v %v", code, err)
		}
		if state(order) != "AT_DC" {
			t.Errorf("state %s after a vendor-variant notification for 2030", state(order))
		}
	})

	t.Run("status for a REQUESTED attempt: 503 without 1|OK, no row", func(t *testing.T) {
		order, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api711, paymentMode: "pay_at_pickup"})
		if st, _, raw := e.ship(e.token(), order, 0, "", false); st != 202 { // job never routed: the attempt stays REQUESTED
			t.Fatalf("request: %d %s", st, raw)
		}
		tr := ecpaytest.Trade{MerchantID: e.mk.ID, TradeNo: e.tradeNo(order), LogisticsID: "7000001", SubType: "UNIMARTC2C", GoodsAmount: "25", PaymentNo: "1"}
		w := e.postStatus(endpoint, ecpaytest.StatusFields(tr, "2030", "2026/09/30 10:00:00", "SENTINELRECIPIENT"))
		if w.Code != 503 || w.Body.String() == "1|OK" {
			t.Errorf("REQUESTED attempt: %d %q (want 503, no 1|OK so ECPay retries)", w.Code, w.Body.String())
		}
		if n := e.events(order, "AND source='ecpay_status'"); n != 0 {
			t.Errorf("a notification for a REQUESTED attempt wrote %d status row(s)", n)
		}
		if s, _, _ := e.shipState(order); s != "REQUESTED" {
			t.Errorf("state %s", s)
		}
	})

	t.Run("a report for an ABANDONED attempt while attempt 2 is live: one event, duplicate_label_risk, no state change", func(t *testing.T) {
		order, _ := e.cvsOrder(tcvOrderSpec{kind: "cvs_711", code: api711, paymentMode: "pay_at_pickup"})
		e.fake.SetCreateMode(ecpaytest.Create403, "")
		if st, _, raw := e.ship(e.token(), order, 0, "", true); st != 202 {
			t.Fatalf("request: %d %s", st, raw)
		}
		e.awaitShip(order, "UNKNOWN")
		e.fake.SetCreateMode(ecpaytest.CreateOK, "")
		firstTrade := e.tradeNo(order)
		if st, _, raw := e.abandon(order); st != 200 {
			t.Fatalf("abandon: %d %s", st, raw)
		}
		_, _, v1 := e.shipState(order)
		if st, _, raw := e.ship(e.token(), order, v1, "", true); st != 202 {
			t.Fatalf("second attempt: %d %s", st, raw)
		}
		e.awaitShip(order, "CREATED")
		if _, attempt, _ := e.shipState(order); attempt != 2 {
			t.Fatalf("attempt %d, want 2", attempt)
		}
		late := ecpaytest.StatusFields(ecpaytest.Trade{MerchantID: e.mk.ID, TradeNo: firstTrade, LogisticsID: "7100001", SubType: "UNIMARTC2C", GoodsAmount: "25", PaymentNo: "1"}, "2030", "2026/09/30 10:00:00", "SENTINELRECIPIENT")
		before := e.count(`SELECT count(*) FROM fulfillment.cvs_shipment_events WHERE order_id=$1 AND attempt=1`, order)
		if w := e.postStatus(endpoint, late); !ok(w) {
			t.Fatalf("late report: %d %q", w.Code, w.Body.String())
		}
		if got := e.count(`SELECT count(*) FROM fulfillment.cvs_shipment_events WHERE order_id=$1 AND attempt=1`, order); got != before+2 && got != before+1 {
			t.Errorf("attempt 1 events %d -> %d", before, got)
		}
		if n := e.count(`SELECT count(*) FROM fulfillment.cvs_shipment_events WHERE order_id=$1 AND attempt=1 AND event_code LIKE '%duplicate_label_risk'`, order); n < 1 {
			t.Error("duplicate_label_risk alert missing")
		}
		var s1, s2 string
		_ = f.owner.QueryRow(ctx, `SELECT (SELECT state FROM fulfillment.cvs_shipments WHERE order_id=$1 AND attempt=1),(SELECT state FROM fulfillment.cvs_shipments WHERE order_id=$1 AND attempt=2)`, order).Scan(&s1, &s2)
		if s1 != "ABANDONED" || s2 != "CREATED" {
			t.Errorf("attempt states %s / %s, want ABANDONED / CREATED (the one-live index is never touched)", s1, s2)
		}
	})

	t.Run("1|OK only after COMMIT (fault injected at COMMIT)", func(t *testing.T) {
		order := newOrder("cvs_711", api711)
		mustExec(t, f.owner, `CREATE OR REPLACE FUNCTION public.tcv_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'tcv injected commit failure'; END $$`)
		mustExec(t, f.owner, `CREATE CONSTRAINT TRIGGER tcv_fail_commit AFTER INSERT ON fulfillment.cvs_shipment_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.provider_code='7777') EXECUTE FUNCTION public.tcv_fail_commit()`)
		dropped := false
		drop := func() {
			if !dropped {
				dropped = true
				mustExec(t, f.owner, `DROP TRIGGER tcv_fail_commit ON fulfillment.cvs_shipment_events`)
				mustExec(t, f.owner, `DROP FUNCTION public.tcv_fail_commit()`)
			}
		}
		t.Cleanup(drop)
		fields := e.statusFor(order, "7777")
		w := e.postStatus(endpoint, fields)
		if w.Body.String() == "1|OK" || w.Code == 200 {
			t.Errorf("the reply was %d %q although the transaction failed at COMMIT (ECPay would stop retrying and the report be lost)", w.Code, w.Body.String())
		}
		if got := e.count(`SELECT count(*) FROM fulfillment.cvs_shipment_events WHERE order_id=$1 AND provider_code='7777'`, order); got != 0 {
			t.Errorf("%d events survived a failed COMMIT", got)
		}
		drop()
		if w := e.postStatus(endpoint, fields); !ok(w) {
			t.Errorf("the retried delivery after the fault: %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("body over 16 KiB, duplicate keys and a stalled body are rejected", func(t *testing.T) {
		order := newOrder("cvs_711", api711)
		n := e.tstEventsTotal(order)
		fields := ecpaytest.SignStatus(e.mk, e.statusFor(order, "2030"))
		big := fields.Encode() + "&Filler=" + strings.Repeat("x", 17000)
		if w := e.hookRaw("/v1/cvs/ecpay/status/"+endpoint, big, "application/x-www-form-urlencoded"); w.Code < 400 || w.Body.String() == "1|OK" {
			t.Errorf("body > 16 KiB: %d %q", w.Code, w.Body.String())
		}
		dup := fields.Encode() + "&RtnCode=2067"
		if w := e.hookRaw("/v1/cvs/ecpay/status/"+endpoint, dup, "application/x-www-form-urlencoded"); w.Code != 400 || w.Body.String() == "1|OK" {
			t.Errorf("duplicate keys: %d %q (want 400)", w.Code, w.Body.String())
		}
		if state(order) != "CREATED" || e.tstEventsTotal(order) != n {
			t.Error("a rejected body changed the shipment or wrote an event")
		}
		// a stalled body: the read is bounded (5 s, ResponseController read deadline, only observable on a real listener)
		srv := httptest.NewServer(e.hooks)
		defer srv.Close()
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "POST /v1/cvs/ecpay/status/%s HTTP/1.1\r\nHost: hooks\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 600\r\n\r\nMerchantID=%s&Merch", endpoint, e.mk.ID)
		_ = conn.SetReadDeadline(time.Now().Add(9 * time.Second))
		started := time.Now()
		resp, rerr := http.ReadResponse(bufio.NewReader(conn), nil)
		took := time.Since(started)
		if rerr == nil {
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode < 400 || string(body) == "1|OK" {
				t.Errorf("stalled body answered %d %q", resp.StatusCode, body)
			}
		}
		if took > 7*time.Second {
			t.Errorf("a stalled body held the connection for %v (the contract bounds the body read at 5 s)", took)
		}
	})

	t.Run("per-endpoint concurrency cap 4", func(t *testing.T) {
		order := newOrder("cvs_711", api711)
		var pipes []*io.PipeWriter
		var wg sync.WaitGroup
		started := make(chan struct{}, 4)
		for i := 0; i < 4; i++ {
			pr, pw := io.Pipe()
			pipes = append(pipes, pw)
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest("POST", "/v1/cvs/ecpay/status/"+endpoint, pr)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				started <- struct{}{}
				e.hooks.ServeHTTP(httptest.NewRecorder(), req)
			}()
			pw.Write([]byte("Merchant")) // headers accepted, body stalled
		}
		for i := 0; i < 4; i++ {
			<-started
		}
		time.Sleep(300 * time.Millisecond)
		begin := time.Now()
		w := e.postStatus(endpoint, e.statusFor(order, "2030"))
		if w.Code < 400 || w.Body.String() == "1|OK" || time.Since(begin) > 2*time.Second {
			t.Errorf("a fifth concurrent delivery on one endpoint: %d %q after %v (want an immediate refusal)", w.Code, w.Body.String(), time.Since(begin))
		}
		for _, p := range pipes {
			_ = p.Close()
		}
		wg.Wait()
		// capacity returns: the same delivery now succeeds
		if w := e.postStatus(endpoint, e.statusFor(order, "2030")); !ok(w) {
			t.Errorf("after the stalled requests ended: %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("recipient fields are never persisted", func(t *testing.T) {
		for _, table := range []string{"fulfillment.cvs_shipment_events", "fulfillment.cvs_shipments", "integration.operations", "integration.operation_events"} {
			var n int
			q := fmt.Sprintf(`SELECT count(*) FROM %s t WHERE t::text ILIKE '%%SENTINELRECIPIENT%%'`, table)
			if err := f.owner.QueryRow(ctx, q).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Errorf("%s holds %d rows containing the recipient sentinel (TD8)", table, n)
			}
		}
		if n := e.count(`SELECT count(*) FROM ops.audit_events t WHERE t.tenant_id=$1 AND t::text ILIKE '%SENTINELRECIPIENT%'`, e.tenant()); n != 0 {
			t.Errorf("%d audit rows carry the recipient sentinel", n)
		}
	})

}

func mustTrade(t *testing.T, e *tcvEnv, order string) ecpaytest.Trade {
	t.Helper()
	tr, ok := e.fake.Trade(e.tradeNo(order))
	if !ok {
		t.Fatalf("no fake trade for %s", order)
	}
	return tr
}
