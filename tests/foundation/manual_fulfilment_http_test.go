package foundation_test

// MF04, MF05, MF06 (contracts/manual-fulfilment-v1.md §5, §6): authority, HTTP surface and export.
// Prefix `mfh`. REAL_PG + HTTP_PG through the real handler and the real buyer routes. Orders are paid
// through the real capture path (rfx harness); merchants are explicit-grant principals.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/storefront"
)

// mfhBlockedRevocation holds the order row lock, starts one PUT, waits (pg_blocking_pids witness) until
// the definer is queued behind the lock, applies change, releases the lock and returns the PUT result.
func mfhBlockedRevocation(t *testing.T, e *rfxEnv, o rfxOrder, token string, change func()) (int, map[string]any) {
	t.Helper()
	ctx := context.Background()
	holder, err := e.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	var pid int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, o.order); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		out    map[string]any
	}
	done := make(chan result, 1)
	go func() {
		status, out, _ := e.mfxPut(o, token, t04Key("mfh-fence"), mfxShip(0, "sf_express", "SFFENCE1"))
		done <- result{status, out}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var blocked int
		if err := e.f.owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shipment definer never queued behind the order lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	change()
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		return r.status, r.out
	case <-time.After(15 * time.Second):
		t.Fatal("the shipment did not return after the lock was released")
		return 0, nil
	}
}

func TestManualFulfilmentMF04Authority(t *testing.T) {
	e := rfxNew(t)
	e.startWorker(t)
	a1 := e.payInStore(t, e.storeFor(t))
	a2 := e.payMore(t, a1)
	a3 := e.payMore(t, a1)
	foreign := e.payInStore(t, e.storeFor(t)) // another tenant, another store, another buyer
	reader, _ := e.member(t, a1, "orders:read")
	exportOnly, _ := e.member(t, a1, "orders:export")
	writeOnly, _ := e.member(t, a1, "fulfillment:write")
	exportRead, _ := e.member(t, a1, "orders:export", "orders:read")
	nothing, _ := e.member(t, a1, "catalog:read")
	buyerToken := a1.s.p.cap.Token

	t.Run("other-store and cross-tenant orders are indistinguishable from missing ones", func(t *testing.T) {
		missing := rfxOrder{s: a1.s, order: randomUUID()}
		crossOrder := rfxOrder{s: a1.s, order: foreign.order}
		s1, o1, _ := e.mfxPut(crossOrder, a1.token(), t04Key("mfh-x"), mfxShip(0, "sf_express", "SF1"))
		s2, o2, _ := e.mfxPut(missing, a1.token(), t04Key("mfh-x"), mfxShip(0, "sf_express", "SF1"))
		if s1 != 404 || s2 != 404 || srqCode(o1) != srqCode(o2) || o1["message"] != o2["message"] {
			t.Fatalf("PUT: cross-tenant order %d %v vs missing %d %v", s1, o1, s2, o2)
		}
		for _, suffix := range []string{"/history"} {
			c1, _, _ := e.call("GET", mfxPath(crossOrder)+suffix, a1.token(), nil, "")
			c2, _, _ := e.call("GET", mfxPath(missing)+suffix, a1.token(), nil, "")
			if c1 != 404 || c2 != 404 {
				t.Fatalf("history of a foreign/missing order: %d %d", c1, c2)
			}
		}
		// the foreign tenant's own principal cannot reach store A at all
		for name, run := range map[string]func() int{
			"PUT": func() int {
				s, _, _ := e.mfxPut(rfxOrder{s: a1.s, order: a1.order}, foreign.token(), t04Key("mfh-t"), mfxShip(0, "sf_express", "SF1"))
				return s
			},
			"history": func() int { s, _, _ := e.call("GET", mfxPath(a1)+"/history", foreign.token(), nil, ""); return s },
			"csv":     func() int { s, _, _ := e.mfxCSV(a1, foreign.token()); return s },
			"list": func() int {
				s, _, _ := e.call("GET", "/v1/admin/stores/"+a1.store()+"/orders?state=unshipped", foreign.token(), nil, "")
				return s
			},
		} {
			if s := run(); s != 404 {
				t.Fatalf("foreign tenant %s on store A: %d (want 404)", name, s)
			}
		}
		// the export of A never carries B's order
		if status, csvBody, _ := e.mfxCSV(a1, a1.token()); status != 200 || mfxContains(csvBody, foreign.order) {
			t.Fatalf("export of store A: %d, contains foreign order: %v", status, mfxContains(csvBody, foreign.order))
		}
		if e.mfxVersions(t, foreign) != 0 || e.mfxVersions(t, a1) != 0 {
			t.Fatal("a refused cross-scope command wrote a version")
		}
	})

	t.Run("permission matrix: neither orders:read, fulfillment:write nor orders:export implies another", func(t *testing.T) {
		type want struct{ put, history, csv, list int }
		for _, c := range []struct {
			name, token string
			want        want
		}{
			{"orders:read only", reader, want{403, 200, 403, 200}},
			{"orders:export only (needs read too)", exportOnly, want{403, 403, 403, 403}},
			{"fulfillment:write only", writeOnly, want{200, 403, 403, 403}},
			{"orders:export + orders:read", exportRead, want{403, 200, 200, 200}},
			{"no orders permission", nothing, want{403, 403, 403, 403}},
			{"anonymous", "", want{401, 401, 401, 401}},
			{"expired merchant session", e.f.tokens["expired"], want{401, 401, 401, 401}},
			{"revoked merchant session", e.f.tokens["revoked"], want{401, 401, 401, 401}},
			{"buyer capability token", buyerToken, want{401, 401, 401, 401}},
		} {
			target := a2
			if c.want.put == 200 {
				target = a3 // the one writer gets its own order so later cases stay unshipped
			}
			put, _, raw := e.mfxPut(target, c.token, t04Key("mfh-perm"), mfxShip(0, "sf_express", "SFPERM1"))
			hist, _, _ := e.call("GET", mfxPath(a2)+"/history", c.token, nil, "")
			csvStatus, _, _ := e.mfxCSV(a2, c.token)
			list, _, _ := e.call("GET", "/v1/admin/stores/"+a2.store()+"/orders?state=unshipped", c.token, nil, "")
			if put != c.want.put || hist != c.want.history || csvStatus != c.want.csv || list != c.want.list {
				t.Fatalf("%s: PUT %d history %d csv %d list %d, want %+v (%s)", c.name, put, hist, csvStatus, list, c.want, raw)
			}
		}
		if e.mfxVersions(t, a2) != 0 {
			t.Fatal("a refused permission case wrote a version on a2")
		}
	})

	t.Run("a session or grant revoked while the command waits on the order lock is rejected by the fresh final auth", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			want   int
			change func(token, principal string)
		}{
			{"revoked session", 401, func(token, principal string) {
				mustExec(t, e.f.owner, `UPDATE identity.sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1`, tokenHash(token))
			}},
			{"fulfillment:write revoked", 403, func(token, principal string) {
				mustExec(t, e.f.owner, `DELETE FROM identity.store_grants WHERE principal_id=$1 AND permission='fulfillment:write'`, principal)
			}},
			{"authz revision changed", 403, func(token, principal string) {
				mustExec(t, e.f.owner, `UPDATE identity.memberships SET authz_revision=authz_revision+1 WHERE principal_id=$1`, principal)
			}},
		} {
			token, principal := e.member(t, a2, "fulfillment:write")
			auditBefore := e.mfxAudit(t, a2, "fulfillment.shipment_recorded") // store-scoped count: compare, not 0
			status, out := mfhBlockedRevocation(t, e, a2, token, func() { c.change(token, principal) })
			if status != c.want {
				t.Fatalf("%s: %d %v, want %d", c.name, status, out, c.want)
			}
			if e.mfxVersions(t, a2) != 0 || e.mfxAudit(t, a2, "fulfillment.shipment_recorded") != auditBefore || e.mfxFulfilmentState(t, a2) == "MERCHANT_SHIPPED" {
				t.Fatalf("%s: a command rejected by the final auth left rows behind", c.name)
			}
		}
	})

	t.Run("a buyer cannot read another buyer's shipment", func(t *testing.T) {
		// a3 was shipped (v1, SFPERM1) by the fulfillment:write-only member above
		srv := e.serveHTTP(t, a1.s)
		own := srv.request(t, "GET", "/v1/buyer/orders/"+a3.order, a3.s.p.cap.Token, "", nil, nil)
		if own.status != 200 || !bytes.Contains(own.body, []byte("SFPERM1")) {
			t.Fatalf("the buyer cannot see their own shipment: %d %s", own.status, own.body)
		}
		other := mustIssue(t, a1.s.p.cqHarness.service, a1.store())
		resp := srv.request(t, "GET", "/v1/buyer/orders/"+a3.order, other.Token, "", nil, nil)
		if resp.status != 404 || bytes.Contains(resp.body, []byte("SFPERM1")) || bytes.Contains(resp.body, []byte("shipment")) {
			t.Fatalf("second buyer read the first buyer's order: %d %s", resp.status, resp.body)
		}
	})
}

// ---- MF05 -----------------------------------------------------------------------------------

func TestManualFulfilmentMF05HTTP(t *testing.T) {
	logs := &swhLogBuffer{}
	oldSlog, oldLog := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(logs)
	t.Cleanup(func() { slog.SetDefault(oldSlog); log.SetOutput(oldLog) })

	e := rfxNew(t)
	e.startWorker(t)
	h1 := e.payInStore(t, e.storeFor(t))
	h2 := e.payMore(t, h1)
	h3 := e.payMore(t, h1)
	h4 := e.payMore(t, h1)
	reader, _ := e.member(t, h1, "orders:read")
	for _, o := range []rfxOrder{h1, h2, h3, h4} {
		e.grant(t, o, "orders:read", "fulfillment:write", "orders:export", "payments:refund")
	}

	t.Run("error codes of §5.1", func(t *testing.T) {
		before := e.mfxSideEffects(t)
		for name, c := range map[string]struct {
			body   string
			status int
			code   string
		}{
			"unknown carrier":             {mfxShip(0, "fedex", "SF1"), 422, "invalid_carrier"},
			"other without a name":        {mfxShip(0, "other", "SF1"), 422, "invalid_carrier"},
			"bad tracking number":         {mfxShip(0, "sf_express", "bad_track"), 422, "invalid_tracking"},
			"missing tracking number":     {mfxShipBody(0, "SHIPPED", "sf_express", "", "", "", "", ""), 422, "invalid_tracking"},
			"http tracking url":           {mfxShipBody(0, "SHIPPED", "sf_express", "", "SF1", "http://track.example.com/x", "", ""), 422, "invalid_url"},
			"tracking url with userinfo":  {mfxShipBody(0, "SHIPPED", "sf_express", "", "SF1", "https://u:p@track.example.com/x", "", ""), 422, "invalid_url"},
			"tracking url host with <":    {mfxShipBody(0, "SHIPPED", "sf_express", "", "SF1", "https://a<b.example.com/x", "", ""), 422, "invalid_url"},
			"void of an unshipped order":  {mfxVoid(0, "wrong_order"), 422, "void_requires_shipped"},
			"void with a carrier":         {mfxShipBody(0, "VOIDED", "sf_express", "", "", "", "", "wrong_order"), 422, "invalid_void"},
			"void with a tracking number": {mfxShipBody(0, "VOIDED", "", "", "SF1", "", "", "wrong_order"), 422, "invalid_void"},
			"void with a note":            {mfxShipBody(0, "VOIDED", "", "", "", "", "oops", "wrong_order"), 422, "invalid_void"},
			"shipped with a void_reason":  {mfxShipBody(0, "SHIPPED", "sf_express", "", "SF1", "", "", "wrong_order"), 422, "invalid_void"},
			"stale expected_version":      {mfxShip(5, "sf_express", "SF1"), 409, "version_changed"},
		} {
			status, out, raw := e.mfxPut(h1, h1.token(), t04Key("mfh-code"), c.body)
			if status != c.status || srqCode(out) != c.code {
				t.Fatalf("%s: %d %s, want %d %s", name, status, raw, c.status, c.code)
			}
		}
		if e.mfxVersions(t, h1) != 0 {
			t.Fatal("a refused command wrote a version")
		}
		if e.mfxSideEffects(t) != before {
			t.Fatal("refused commands created provider-side rows")
		}
	})

	t.Run("strict body, query and method rules write nothing", func(t *testing.T) {
		key := func() map[string]string { return map[string]string{"Idempotency-Key": t04Key("mfh-strict")} }
		good := mfxShip(0, "sf_express", "SF1")
		bad4xx := func(name string, status int) {
			t.Helper()
			if status < 400 || status >= 500 {
				t.Fatalf("%s: HTTP %d, want a 4xx", name, status)
			}
		}
		put := func(hdr map[string]string, body, path string) int {
			s, _, _ := e.call("PUT", path, h1.token(), hdr, body)
			return s
		}
		p := mfxPath(h1)
		bad4xx("extra key", put(key(), strings.Replace(good, `"note":null`, `"note":null,"x":1`, 1), p))
		bad4xx("missing key", put(key(), `{"expected_version":0,"status":"SHIPPED","carrier_code":"sf_express","carrier_name":null,"tracking_number":"SF1","tracking_url":null,"note":null}`, p))
		bad4xx("duplicate key", put(key(), strings.Replace(good, `"expected_version":0`, `"expected_version":0,"expected_version":0`, 1), p))
		bad4xx("string expected_version", put(key(), strings.Replace(good, `"expected_version":0`, `"expected_version":"0"`, 1), p))
		bad4xx("negative expected_version", put(key(), strings.Replace(good, `"expected_version":0`, `"expected_version":-1`, 1), p))
		bad4xx("array body", put(key(), `[]`, p))
		bad4xx("empty body", put(key(), ``, p))
		bad4xx("null body", put(key(), `null`, p))
		bad4xx("trailing data", put(key(), good+`{}`, p))
		bad4xx("over 64 KiB", put(key(), strings.Replace(good, `"note":null`, `"note":"`+strings.Repeat("a", 64<<10)+`"`, 1), p))
		bad4xx("no Idempotency-Key", put(nil, good, p))
		bad4xx("empty Idempotency-Key", put(map[string]string{"Idempotency-Key": ""}, good, p))
		bad4xx("query string", put(key(), good, p+"?x=1"))
		bad4xx("wrong media type", put(map[string]string{"Idempotency-Key": t04Key("mfh-mt"), "Content-Type": "text/plain"}, good, p))
		bad4xx("malformed order id", put(key(), good, "/v1/admin/stores/"+h1.store()+"/orders/nope/shipment"))
		for _, m := range []string{"POST", "PATCH", "DELETE", "HEAD"} {
			if s, _, _ := e.call(m, p, h1.token(), key(), good); s != 405 {
				t.Fatalf("%s on /shipment: %d, want 405", m, s)
			}
		}
		if s, _, _ := e.call("GET", p, h1.token(), nil, ""); s != 405 {
			t.Fatalf("GET on /shipment: %d, want 405 (history is a separate route)", s)
		}
		// history and csv take no query, no body, no key
		for name, run := range map[string]func() int{
			"history with a query": func() int { s, _, _ := e.call("GET", p+"/history?x=1", h1.token(), nil, ""); return s },
			"history with a body":  func() int { s, _, _ := e.call("GET", p+"/history", h1.token(), nil, "{}"); return s },
			"history with a key":   func() int { s, _, _ := e.call("GET", p+"/history", h1.token(), key(), ""); return s },
			"csv with a query": func() int {
				s, _, _ := e.call("GET", "/v1/admin/stores/"+h1.store()+"/orders/unshipped.csv?limit=1", h1.token(), nil, "")
				return s
			},
			"csv with a key": func() int {
				s, _, _ := e.call("GET", "/v1/admin/stores/"+h1.store()+"/orders/unshipped.csv", h1.token(), key(), "")
				return s
			},
			"list with a bad state": func() int {
				s, _, _ := e.call("GET", "/v1/admin/stores/"+h1.store()+"/orders?state=delivered", h1.token(), nil, "")
				return s
			},
		} {
			bad4xx(name, run())
		}
		if s, _, _ := e.call("POST", "/v1/admin/stores/"+h1.store()+"/orders/unshipped.csv", h1.token(), key(), ""); s != 405 {
			t.Fatalf("POST on the export: %d", s)
		}
		if e.mfxVersions(t, h1) != 0 {
			t.Fatal("a refused transport case wrote a version")
		}
	})

	t.Run("CSV response headers and audit", func(t *testing.T) {
		before := e.mfxAudit(t, h1, "orders.export_unshipped")
		status, body, hdr := e.mfxCSV(h1, h1.token())
		if status != 200 {
			t.Fatalf("export: %d %s", status, body)
		}
		if hdr.Get("Content-Type") != "text/csv; charset=utf-8" || hdr.Get("Cache-Control") != "no-store, private" || hdr.Get("X-Export-Truncated") != "false" {
			t.Fatalf("headers: %v", hdr)
		}
		wantName := regexp.MustCompile(`^attachment; filename="unshipped-` + regexp.QuoteMeta(h1.store()[:8]) + `-\d{12}\.csv"$`)
		if !wantName.MatchString(hdr.Get("Content-Disposition")) {
			t.Fatalf("Content-Disposition %q", hdr.Get("Content-Disposition"))
		}
		if m := regexp.MustCompile(`-(\d{12})\.csv`).FindStringSubmatch(hdr.Get("Content-Disposition")); m == nil {
			t.Fatal("no timestamp in filename")
		} else if ts, err := time.Parse("200601021504", m[1]); err != nil || time.Since(ts) > 10*time.Minute || time.Until(ts) > time.Minute {
			t.Fatalf("filename timestamp %s is not the current UTC minute: %v", m[1], err)
		}
		if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) || !bytes.Contains(body, []byte(mfuHeader+"\r\n")) {
			t.Fatalf("export body lacks BOM/header: %q", body)
		}
		if after := e.mfxAudit(t, h1, "orders.export_unshipped"); after != before+1 {
			t.Fatalf("audit rows %d -> %d, want exactly one per export", before, after)
		}
		var principal string
		if err := e.f.owner.QueryRow(context.Background(), `SELECT principal_id::text FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action='orders.export_unshipped' ORDER BY created_at DESC LIMIT 1`, h1.s.p.f.tenantA, h1.store()).Scan(&principal); err != nil || principal != h1.s.p.f.principalA {
			t.Fatalf("export audit principal %q err=%v (the export sets tenant/store/principal GUCs itself, E4)", principal, err)
		}
		// a refused export writes no audit row
		if s, _, _ := e.mfxCSV(h1, reader); s != 403 {
			t.Fatalf("reader export status %d", s)
		}
		if after := e.mfxAudit(t, h1, "orders.export_unshipped"); after != before+1 {
			t.Fatalf("a refused export wrote an audit row: %d", after)
		}
	})

	t.Run("merchant list and detail decode shipped, refunded and reviewed orders; buyer sees the shipment", func(t *testing.T) {
		srv := e.serveHTTP(t, h1.s)
		buyerDetail := func(o rfxOrder) map[string]any {
			resp := srv.request(t, "GET", "/v1/buyer/orders/"+o.order, o.s.p.cap.Token, "", nil, nil)
			if resp.status != 200 {
				t.Fatalf("buyer order detail: %d %s", resp.status, resp.body)
			}
			return sraJSON(resp.body)
		}
		detail := func(o rfxOrder) merchantorders.Detail {
			t.Helper()
			status, raw, _ := e.call("GET", "/v1/admin/stores/"+o.store()+"/orders/"+o.order, o.token(), nil, "")
			if status != 200 {
				t.Fatalf("merchant detail: %d %s", status, raw)
			}
			var d merchantorders.Detail
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
		// before shipping: buyer `shipment` is present and null
		if d := buyerDetail(h1); d["shipment"] != nil {
			t.Fatalf("shipment before shipping: %v", d["shipment"])
		} else if _, present := d["shipment"]; !present {
			t.Fatal("buyer order detail must always emit `shipment` (null when not shipped)")
		}
		if detail(h1).Shipment != nil {
			t.Fatal("merchant detail shipment before shipping")
		}
		status, _, raw := e.mfxPut(h1, h1.token(), t04Key("mfh-ship"), mfxShipBody(0, "SHIPPED", "seven_eleven_cvs", "", "0012345678", "https://track.example.com/t?n=1", "internal note NOTE-SENTINEL", ""))
		if status != 200 {
			t.Fatalf("ship: %d %s", status, raw)
		}
		d := detail(h1)
		if d.FulfillmentState != "MERCHANT_SHIPPED" || d.Shipment == nil || d.Shipment.Version != 1 || d.Shipment.Status != "SHIPPED" || d.Shipment.CarrierCode != "seven_eleven_cvs" || d.Shipment.TrackingNumber != "0012345678" ||
			d.Shipment.TrackingURL == nil || *d.Shipment.TrackingURL != "https://track.example.com/t?n=1" || d.Shipment.CarrierName != nil {
			t.Fatalf("merchant detail after shipping: %+v shipment=%+v", d.Summary, d.Shipment)
		}
		// buyer: exactly the six buyer keys, never the merchant-only ones
		b := buyerDetail(h1)
		ship, _ := b["shipment"].(map[string]any)
		if !reflect.DeepEqual(sraKeys(ship), []string{"carrier_code", "carrier_name", "recorded_at", "status", "tracking_number", "tracking_url"}) ||
			ship["status"] != "SHIPPED" || ship["carrier_code"] != "seven_eleven_cvs" || ship["tracking_number"] != "0012345678" || ship["tracking_url"] != "https://track.example.com/t?n=1" {
			t.Fatalf("buyer shipment: %v", ship)
		}
		sraTime(t, ship["recorded_at"])
		rawBuyer, _ := json.Marshal(b)
		for _, s := range []string{"NOTE-SENTINEL", "void_reason", "principal_id", h1.s.p.f.principalA, `"version"`} {
			if strings.Contains(string(rawBuyer), s) && s != `"version"` {
				t.Fatalf("buyer order detail leaks %q: %s", s, rawBuyer)
			}
		}
		if b["fulfillment_state"] != "MERCHANT_SHIPPED" {
			t.Fatalf("buyer detail fulfillment_state %v", b["fulfillment_state"])
		}
		hist := srv.request(t, "GET", "/v1/buyer/orders?limit=100", h1.s.p.cap.Token, "", nil, nil)
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(hist.body, &page); err != nil || hist.status != 200 {
			t.Fatalf("buyer history: %d %s", hist.status, hist.body)
		}
		seen := false
		for _, it := range page.Items {
			if it["order_id"] == h1.order {
				seen = true
				if it["fulfillment_state"] != "MERCHANT_SHIPPED" {
					t.Fatalf("buyer history state %v", it["fulfillment_state"])
				}
			}
		}
		if !seen {
			t.Fatal("shipped order missing from the buyer history")
		}
		// void: buyer sees null again, merchant detail shipment null, state back to MANUAL_UNASSIGNED
		if status, _, raw := e.mfxPut(h1, h1.token(), t04Key("mfh-void"), mfxVoid(1, "not_dispatched")); status != 200 {
			t.Fatalf("void: %d %s", status, raw)
		}
		if b := buyerDetail(h1); b["shipment"] != nil || b["fulfillment_state"] != "MANUAL_UNASSIGNED" {
			t.Fatalf("buyer detail after a void: shipment=%v state=%v", b["shipment"], b["fulfillment_state"])
		}
		if detail(h1).Shipment != nil {
			t.Fatal("merchant detail shipment after a void")
		}
		// a refunded shipped order decodes (partial refund keeps the shipment and the READY work)
		e.mustShip(t, h2, 0, "familymart_cvs", "FM0001")
		rid := e.mustRefund(t, h2, 500, "requested_by_customer")
		e.awaitRefundFact(t, rid, h2.attempt, "SUCCEEDED")
		d2 := detail(h2)
		if d2.FulfillmentState != "MERCHANT_SHIPPED" || d2.PaymentState != "PARTIALLY_REFUNDED" || d2.Shipment == nil || d2.WorkState != "READY" {
			t.Fatalf("shipped + partially refunded order: %+v", d2.Summary)
		}
		// a READY order with a refund review decodes
		srqReview(t, e, h3.attempt, "REFUND_HISTORY")
		d3 := detail(h3)
		if d3.WorkState != "READY" || d3.PaymentState != "REVIEW_REQUIRED" {
			t.Fatalf("READY + refund review: %+v", d3.Summary)
		}
		if status, _, raw := e.call("GET", "/v1/admin/stores/"+h1.store()+"/orders?limit=100", h1.token(), nil, ""); status != 200 {
			t.Fatalf("list with mixed states: %d %s", status, raw)
		}
		// filters follow the head
		_, shipped := e.mfxOrdersState(t, h1, h1.token(), "shipped")
		_, unshipped := e.mfxOrdersState(t, h1, h1.token(), "unshipped")
		if !shipped[h2.order] || shipped[h1.order] || unshipped[h2.order] || !unshipped[h1.order] || unshipped[h3.order] {
			t.Fatalf("filters: shipped=%v unshipped=%v", shipped, unshipped)
		}
	})

	t.Run("no PII in logs", func(t *testing.T) {
		e.mfxCSV(h1, h1.token())
		out := logs.String()
		for _, s := range []string{"Synthetic Buyer", "+886900000001", "0900000001", "Synthetic home address", "Synthetic city", "NOTE-SENTINEL", "0012345678", "track.example.com", mfuHeader} {
			if strings.Contains(out, s) {
				t.Fatalf("logs contain %q (logs record only store UUID, row count and duration)", s)
			}
		}
	})
	_ = h4
}

// ---- MF06 -----------------------------------------------------------------------------------

// mfhExportSQL calls identity.export_unshipped_orders directly (the SQL boundary) with a row limit.
func mfhExportSQL(t *testing.T, e *rfxEnv, o rfxOrder, token string, limit int) ([]map[string]any, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.f.runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true),set_config('app.principal_id',$3,true)`, o.s.p.f.tenantA, o.store(), o.s.p.f.principalA); err != nil {
		return nil, err
	}
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT identity.export_unshipped_orders($1::bytea,$2::uuid,$3::integer)`, tokenHash(token), o.store(), limit).Scan(&raw); err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err = json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	return rows, tx.Commit(ctx)
}

func TestManualFulfilmentMF06Export(t *testing.T) {
	e := rfxNew(t)
	e.startWorker(t)
	base := e.payInStore(t, e.storeFor(t))
	// eligible (MD6): plain, partially refunded, presentment-drift review (allowed), then pickup orders.
	e1 := base
	e2 := e.payMore(t, base)
	e3 := e.payMore(t, base)
	// ineligible in the SAME store, so the predicate is proven against real neighbours
	x1 := e.payMore(t, base) // already shipped
	x2 := e.payMore(t, base) // full refund succeeded
	x3 := e.payMore(t, base) // full refund in flight
	x4 := e.payMore(t, base) // REFUND_HISTORY review
	x5 := e.payMore(t, base) // CONFLICTING_REPORT review
	e.ensureStock(t, base)
	draftHold := sstMoreHold(t, base.s.p)
	x7 := rfxOrder{s: base.s, order: draftHold.hold.OrderID, endpoint: base.endpoint, secret: base.secret}
	x7.s.p = draftHold
	x6 := rfxOrder{s: base.s, endpoint: base.endpoint, secret: base.secret}
	e.ensureStock(t, base)
	awaitingHold := sstMoreHold(t, base.s.p)
	x6.s.p = awaitingHold
	x6.order = awaitingHold.hold.OrderID
	res, _ := e.pinned(t, x6.s)
	x6.attempt = res.AttemptID
	converted := false
	e4 := e.mfxCVSOrder(t, base, "017888", &converted)
	e5 := e.mfxCVSOrder(t, base, "007123", &converted)
	for _, o := range []rfxOrder{e1, e2, e3, e4, e5, x1, x2, x3, x4, x5, x6, x7} {
		e.grant(t, o, "orders:read", "payments:refund", "fulfillment:write", "orders:export")
	}
	for _, o := range []rfxOrder{e1, e2, e3, e4, e5, x1, x2, x3, x4, x5} {
		e.await(t, "capture jobs settled", o.attempt, 60*time.Second, `SELECT NOT EXISTS(SELECT 1 FROM river_payment.river_job WHERE args->>'operation_id'=$1 AND state NOT IN ('completed','cancelled','discarded'))`)
	}
	e.mustShip(t, x1, 0, "sf_express", "SFX1")
	rid := e.mustRefund(t, x2, x2.captured, "requested_by_customer")
	e.awaitRefundFact(t, rid, x2.attempt, "SUCCEEDED")
	e.fake.HoldNextRefund("pending", "processing")
	rid = e.mustRefund(t, x3, x3.captured, "requested_by_customer")
	e.awaitRefund(t, "refund pinned", rid, x3.attempt, 45*time.Second, `SELECT stripe_refund_id IS NOT NULL FROM payments.stripe_refunds WHERE id=$1`)
	srqReview(t, e, x4.attempt, "REFUND_HISTORY")
	srqReview(t, e, x5.attempt, "CONFLICTING_REPORT")
	rid = e.mustRefund(t, e2, 500, "requested_by_customer") // a partial refund stays eligible
	e.awaitRefundFact(t, rid, e2.attempt, "SUCCEEDED")
	srqReview(t, e, e3.attempt, "PROVIDER_PRESENTMENT_DRIFT") // money matched: still eligible

	eligible := []rfxOrder{e1, e2, e3, e4, e5}
	ineligible := map[string]rfxOrder{"shipped": x1, "full refund": x2, "full refund in flight": x3, "REFUND_HISTORY": x4, "CONFLICTING_REPORT": x5, "awaiting payment": x6, "draft": x7}

	parse := func(raw []byte) [][]string {
		return mfuRecords(t, raw)
	}
	ids := func(recs [][]string) (out []string) {
		for _, r := range recs[1:] {
			out = append(out, r[0])
		}
		return
	}
	wantOrder := func() []string {
		var list []string
		for _, o := range eligible {
			list = append(list, o.order)
		}
		rows, err := e.f.owner.Query(context.Background(), `SELECT id::text FROM checkout.orders WHERE id=ANY($1::uuid[]) ORDER BY created_at ASC, id ASC`, list)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			out = append(out, id)
		}
		return out
	}()

	var firstBytes []byte
	t.Run("eligibility is exactly MD6 and oldest first", func(t *testing.T) {
		status, body, hdr := e.mfxCSV(e1, e1.token())
		if status != 200 || hdr.Get("X-Export-Truncated") != "false" {
			t.Fatalf("export: %d %v", status, hdr)
		}
		firstBytes = body
		got := ids(parse(body))
		if !reflect.DeepEqual(got, wantOrder) {
			t.Fatalf("export rows\n got  %v\n want %v (eligible orders only, created_at ASC then id)", got, wantOrder)
		}
		for name, o := range ineligible {
			if mfxContains(body, o.order) {
				t.Fatalf("ineligible order (%s) is in the export", name)
			}
		}
		// the same predicate in the list filter and in the record command
		_, listed := e.mfxOrdersState(t, e1, e1.token(), "unshipped")
		for _, o := range eligible {
			if !listed[o.order] {
				t.Fatalf("eligible order %s missing from state=unshipped", o.order)
			}
		}
		for name, o := range ineligible {
			if listed[o.order] {
				t.Fatalf("ineligible order (%s) listed under state=unshipped", name)
			}
			if name == "shipped" {
				continue
			}
			if status, out, raw := e.mfxPut(o, o.token(), t04Key("mfh-md6"), mfxShip(0, "sf_express", "SFMD6")); status != 422 || srqCode(out) != "not_shippable" {
				t.Fatalf("record on ineligible order (%s): %d %s", name, status, raw)
			}
		}
		if len(listed) != len(eligible) {
			t.Fatalf("state=unshipped lists %d orders, want %d", len(listed), len(eligible))
		}
	})

	t.Run("columns carry the frozen destination, pickup and item values", func(t *testing.T) {
		recs := parse(firstBytes)
		if strings.Join(recs[0], ",") != mfuHeader {
			t.Fatalf("header %v", recs[0])
		}
		col := map[string]int{}
		for i, name := range recs[0] {
			col[name] = i
		}
		byID := map[string][]string{}
		for _, r := range recs[1:] {
			byID[r[0]] = r
		}
		for _, o := range eligible {
			row := byID[o.order]
			if row == nil {
				t.Fatalf("order %s missing", o.order)
			}
			status, raw, _ := e.call("GET", "/v1/admin/stores/"+o.store()+"/orders/"+o.order, o.token(), nil, "")
			if status != 200 {
				t.Fatalf("detail: %d", status)
			}
			var d merchantorders.Detail
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			var items []string
			for _, it := range d.Items {
				items = append(items, fmt.Sprintf("%s×%d", it.Code, it.Quantity))
			}
			phone := strings.NewReplacer("+886", "0").Replace(d.Destination.Phone)
			want := map[string]string{"service_code": d.ServiceCode, "destination_kind": d.Destination.Kind, "recipient_name": d.Destination.RecipientName, "phone": phone, "country": d.Destination.Country,
				"region": d.Destination.HomeAddress.Region, "city": d.Destination.HomeAddress.City, "postal_code": d.Destination.HomeAddress.PostalCode, "line1": d.Destination.HomeAddress.Line1,
				"line2": d.Destination.HomeAddress.Line2, "items": strings.Join(items, "; "), "total_minor": fmt.Sprint(d.TotalMinor), "currency": d.Currency}
			want["pickup_source"] = "" // home delivery
			if p := d.Destination.Pickup; p != nil {
				want["pickup_namespace"], want["pickup_code"], want["pickup_name"], want["pickup_address"] = p.Namespace, p.Code, p.Name, p.Address
				want["pickup_source"] = "merchant_attested" // every pickup these gates create is MANUAL_ATTESTED (C4)
			}
			for name, w := range want {
				if row[col[name]] != w {
					t.Fatalf("order %s column %s = %q, want %q", o.order, name, row[col[name]], w)
				}
			}
			if ts, err := time.Parse(time.RFC3339, row[col["created_at_utc"]]); err != nil || !strings.HasSuffix(row[col["created_at_utc"]], "Z") {
				t.Fatalf("created_at_utc %q: %v", row[col["created_at_utc"]], err)
			} else if created, _ := time.Parse(time.RFC3339Nano, d.CreatedAt); created.Truncate(time.Second).After(ts) || created.Sub(ts) > time.Second {
				t.Fatalf("created_at_utc %s disagrees with the order %s", ts, d.CreatedAt)
			}
		}
		// pickup codes keep their leading zeroes in the file bytes
		if !bytes.Contains(firstBytes, []byte(",017888,")) || !bytes.Contains(firstBytes, []byte(",007123,")) {
			t.Fatalf("pickup codes lost leading zeroes in the bytes: %q", firstBytes)
		}
		if byID[e4.order][col["pickup_code"]] != "017888" || byID[e4.order][col["destination_kind"]] == "home" || byID[e1.order][col["pickup_code"]] != "" {
			t.Fatal("pickup columns are wrong for pickup vs home orders")
		}
	})

	t.Run("frozen values survive catalog, address and pickup edits", func(t *testing.T) {
		mustExec(t, e.f.owner, `UPDATE catalog.products SET name='Renamed after checkout' WHERE id=$1`, base.s.p.stock.product.ID)
		mustExec(t, e.f.owner, `UPDATE catalog.skus SET code='NEW-CODE',price_minor=99999 WHERE id=$1`, base.s.p.stock.skus[0].ID)
		mustExec(t, e.f.owner, `UPDATE control.stores SET name='Renamed store after checkout' WHERE id=$1`, base.store())
		if _, err := bdSet(e1.s.p.cqHarness, t04Key("mfh-new-dest"), storefront.DestinationInput{ExpectedVersion: e1.s.p.bcHarness.destination.Version, CartVersion: e1.s.p.bcHarness.input.CartVersion, Kind: "home", Country: "TW",
			RecipientName: "New current recipient", Phone: "+886900000099", HomeAddress: storefront.HomeAddress{City: "Other city", Line1: "Other current address"}}); err != nil {
			t.Fatalf("current destination edit: %v", err)
		}
		status, after, _ := e.mfxCSV(e1, e1.token())
		if status != 200 || !bytes.Equal(after, firstBytes) {
			t.Fatalf("mutable sources rewrote the export (status %d)", status)
		}
	})

	t.Run("row_limit bounds the SQL export oldest first", func(t *testing.T) {
		rows, err := mfhExportSQL(t, e, e1, e1.token(), 2)
		if err != nil || len(rows) != 2 || rows[0]["order_id"] != wantOrder[0] || rows[1]["order_id"] != wantOrder[1] {
			t.Fatalf("row_limit 2: %v err=%v want %v", rows, err, wantOrder[:2])
		}
		all, err := mfhExportSQL(t, e, e1, e1.token(), 1001)
		if err != nil || len(all) != len(wantOrder) {
			t.Fatalf("row_limit 1001: %d rows err=%v", len(all), err)
		}
		for _, bad := range []int{0, -1, 1002, 100000} {
			if _, err := mfhExportSQL(t, e, e1, e1.token(), bad); err == nil {
				t.Fatalf("row_limit %d accepted (1..1001 only)", bad)
			}
		}
		// NOT_RUN in this test: the end-to-end 1000-row cap with X-Export-Truncated: true needs 1001 eligible
		// paid orders; there is no bulk product path and fabricating them would defeat the gate (see return).
	})

	t.Run("audit row per export, nothing persisted", func(t *testing.T) {
		before := e.mfxAudit(t, e1, "orders.export_unshipped")
		for i := 0; i < 3; i++ {
			if status, _, _ := e.mfxCSV(e1, e1.token()); status != 200 {
				t.Fatal("export failed")
			}
		}
		if after := e.mfxAudit(t, e1, "orders.export_unshipped"); after != before+3 {
			t.Fatalf("audit rows %d -> %d, want +3", before, after)
		}
		reader, _ := e.member(t, e1, "orders:read")
		if status, _, _ := e.mfxCSV(e1, reader); status != 403 {
			t.Fatalf("reader export: %d", status)
		}
		if after := e.mfxAudit(t, e1, "orders.export_unshipped"); after != before+3 {
			t.Fatalf("a refused export wrote an audit row: %d", after)
		}
		// The exported phone form (0900000001) and the header line exist in no table: CSV bytes are never stored.
		if hits := swhScan(t, e.f, mfuHeader, "0900000001", "0900000002"); len(hits) != 0 {
			t.Fatalf("CSV bytes persisted in %v", hits)
		}
	})

	t.Run("the record predicate agrees: an eligible order ships, then leaves the export", func(t *testing.T) {
		if status, _, raw := e.mfxPut(e3, e3.token(), t04Key("mfh-drift"), mfxShip(0, "okmart_cvs", "OK123")); status != 200 {
			t.Fatalf("PROVIDER_PRESENTMENT_DRIFT order must ship (MD6 excludes only other reasons): %d %s", status, raw)
		}
		status, body, _ := e.mfxCSV(e1, e1.token())
		got := ids(parse(body))
		var want []string
		for _, id := range wantOrder {
			if id != e3.order {
				want = append(want, id)
			}
		}
		sort.Strings(want)
		sort.Strings(got)
		if status != 200 || !reflect.DeepEqual(got, want) {
			t.Fatalf("export after shipping e3: %v want %v", got, want)
		}
	})
}
