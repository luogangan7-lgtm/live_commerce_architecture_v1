package foundation_test

// CB09 (contracts/customers-billing-v1.md §5 every row, §6, §7, §8, customers-core D3/D9/D12/D13 and billing-core
// B2/B3; tier HTTP_PG through the merged httpapi.NewHandler over the real commerce_runtime pool and the merged
// buyerhttp handler). Written from the contract and the FROZEN blocks only. Helper prefix `cbh`. What it proves,
// row by row: permissions (401 / 403 / 404), the not-found answer of another store or tenant being
// indistinguishable from an unknown id, strict bodies and query grammar (422), Idempotency-Key where the
// contract lists it, attachment headers (application/json or text/csv, Content-Disposition attachment,
// Cache-Control no-store), export bounds (409 export_too_large, no EXPORT row), error codes (idempotency_conflict,
// erasure_blocked, erased 410, billing_restricted 402, subscription_exists, no_billing_customer,
// billing_unavailable 503), response key sets (strict DTOs), and that no error body carries a secret, an id of
// another domain or a bearer URL. The platform webhook row is served by cmd/api mountPlatformBilling
// (package main); its handler is exercised directly in CB08 (recorded as a scope note in the evidence).
// Disclosed owner-pool fixtures: grants of the merchant principals, cloned CANCELLED order rows with triggers off
// (session_replication_role=replica) to reach the 200-order export bound.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/billing/billingtest"
	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/customers"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/storefront"
)

type cbhEnv struct {
	*cbmEnv
	on, off http.Handler // billing service configured / nil
	tokens  map[string]string
	bodies  []string // every response body seen, scanned for secrets at the end
}

type cbhResp struct {
	status int
	hdr    http.Header
	raw    []byte
	json   map[string]any
}

func (c *cbhEnv) do(h http.Handler, method, path, token string, hdr map[string]string, body any) cbhResp {
	c.t.Helper()
	var raw []byte
	switch v := body.(type) {
	case nil:
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		var err error
		if raw, err = json.Marshal(v); err != nil {
			c.t.Fatal(err)
		}
	}
	var rd io.Reader // nil: httptest then gives the request http.NoBody exactly like a real server does for a GET
	if len(raw) > 0 {
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if len(raw) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	out := cbhResp{status: w.Code, hdr: w.Header(), raw: w.Body.Bytes()}
	_ = json.Unmarshal(out.raw, &out.json)
	c.bodies = append(c.bodies, string(out.raw))
	return out
}

func (c *cbhEnv) key() map[string]string { return map[string]string{"Idempotency-Key": t04Key("cbh")} }

func cbhArr(v any) []any          { a, _ := v.([]any); return a }
func cbhMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func cbhNum(v any) float64 { f, _ := v.(float64); return f }

func firstOf(a []any) any {
	if len(a) == 0 {
		return nil
	}
	return a[0]
}

func cbhKeys(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func cbhCode(r cbhResp) string {
	if r.json == nil {
		return ""
	}
	s, _ := r.json["code"].(string)
	return s
}

// want asserts the status and, when code != "", the error code of the frozen error envelope.
func (c *cbhEnv) want(name string, r cbhResp, status int, code string) {
	c.t.Helper()
	if r.status != status || (code != "" && cbhCode(r) != code) {
		c.t.Errorf("%s: HTTP %d code=%q, want %d %q (%s)", name, r.status, cbhCode(r), status, code, strings.TrimSpace(string(r.raw)))
	}
}

// reject asserts a client error without naming which: a wrong method answers 405 from the mux or 404 from a
// fallback route, both are "refused" (the contract only forbids a 2xx or a 5xx).
func (c *cbhEnv) reject(name string, r cbhResp) {
	c.t.Helper()
	if r.status < 400 || r.status >= 500 {
		c.t.Errorf("%s: HTTP %d, want a 4xx refusal (%s)", name, r.status, strings.TrimSpace(string(r.raw)))
	}
}

func (c *cbhEnv) noStore(name string, r cbhResp) {
	c.t.Helper()
	if cc := r.hdr.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		c.t.Errorf("%s: Cache-Control %q lacks no-store", name, cc)
	}
}

func TestCustomersBillingCB09HTTP(t *testing.T) {
	m := cbmSetup(t, false, false)
	f, ctx := m.f, m.ctx
	h := &lcHarness{cqHarness: m.p.cqHarness, ctx: ctx}
	h.actor, h.token = lcPrincipal(t, f, m.tenant, []string{m.store}, "store:read", "live:read", "live:manage")
	var err error
	if h.labels, err = claims.NewLabelKey(randomBytes(32)); err != nil {
		t.Fatal(err)
	}
	c := &cbhEnv{cbmEnv: m}
	c.on = httpapi.NewHandler(f.runtime, httpapi.Options{Billing: m.svc, ClaimLabels: &h.labels, Studio: true})
	c.off = httpapi.NewHandler(f.runtime, httpapi.Options{})
	mk := func(perms ...string) string { return lcTokenFor(t, f, m.tenant, m.store, perms...) }
	c.tokens = map[string]string{
		"none":       mk(),
		"read":       mk("customers:read"),
		"privacy":    mk("customers:privacy"),
		"full":       mk("customers:read", "customers:privacy", "orders:read", "orders:export", "billing:manage"),
		"ordersRO":   mk("orders:read"),
		"exportOnly": mk("orders:export"),
		"billing":    mk("billing:manage"),
	}
	// the second tenant and a second store of the first: their members hold every permission on THEIR store
	p2 := psSetup(t)
	_, foreign := lcPrincipal(t, p2.f, p2.f.tenantA, []string{p2.f.storeA1}, "store:read", "customers:read", "customers:privacy", "orders:read", "orders:export", "billing:manage")
	full, base := c.tokens["full"], "/v1/admin/stores/"+m.store
	owner := m.p.cap.Scope.OwnerID // has one order (the psSetup hold): a customer

	getStatus := func(token string) cbhResp { return c.do(c.on, "GET", base+"/customers?limit=50", token, nil, nil) }
	const listItemKeys = "active,captured_minor,claims_count,consents,currency,customer_id,display_name,first_seen_at,last_activity_at,orders_count,paid_orders_count,phone_last3,platforms,refunded_minor"

	t.Run("GET customers: permissions, grammar, shape, paging, method", func(t *testing.T) {
		c.want("no token", getStatus(""), 401, "")
		c.want("garbage token", getStatus(strings.Repeat("x", 40)), 401, "")
		c.want("no permission", getStatus(c.tokens["none"]), 403, "")
		c.want("customers:privacy alone", getStatus(c.tokens["privacy"]), 403, "")
		for name, tok := range map[string]string{"read": c.tokens["read"], "full": full} {
			r := getStatus(tok)
			c.want(name, r, 200, "")
			c.noStore(name, r)
			items, _ := r.json["items"].([]any)
			if len(items) < 1 || r.json["next_cursor"] == nil {
				t.Fatalf("%s: %s", name, r.raw)
			}
			if got := cbhKeys(items[0].(map[string]any)); got != listItemKeys {
				t.Errorf("%s: list row keys\n got  %s\n want %s", name, got, listItemKeys)
			}
			if cbhKeys(r.json) != "items,next_cursor" {
				t.Errorf("%s: page keys %s", name, cbhKeys(r.json))
			}
		}
		// the not-found answers of another store / another tenant / a missing store id cannot be told apart
		a := c.do(c.on, "GET", "/v1/admin/stores/"+p2.f.storeA1+"/customers", full, nil, nil)
		b := c.do(c.on, "GET", "/v1/admin/stores/"+randomUUID()+"/customers", full, nil, nil)
		c.want("another tenant's store", a, 404, "")
		c.want("a store that does not exist", b, 404, "")
		if cbhCode(a) != cbhCode(b) || a.hdr.Get("Content-Type") != b.hdr.Get("Content-Type") || len(a.raw) != len(b.raw) {
			t.Errorf("another tenant's store and a missing store answer differently: %s vs %s", a.raw, b.raw)
		}
		c.want("foreign member on this store", c.do(c.on, "GET", base+"/customers", foreign, nil, nil), 404, "")
		// grammar
		for name, q := range map[string]string{"limit 101": "limit=101", "limit text": "limit=abc", "limit negative": "limit=-1", "unknown param": "x=1", "bad cursor": "after=%25%25", "q 41 chars": "q=" + strings.Repeat("a", 41), "q blank": "q=%20", "empty query key": "="} {
			c.want(name, c.do(c.on, "GET", base+"/customers?"+q, full, nil, nil), 422, "invalid_request")
		}
		c.reject("Idempotency-Key on GET", c.do(c.on, "GET", base+"/customers", full, c.key(), nil))
		c.reject("body on GET", c.do(c.on, "GET", base+"/customers", full, nil, `{}`))
		for _, method := range []string{"PUT", "DELETE", "PATCH"} {
			c.reject(method, c.do(c.on, method, base+"/customers", full, nil, nil))
		}
		if r := c.do(c.on, "GET", base+"/customers?q=Synthetic", full, nil, nil); r.status != 200 {
			t.Errorf("q by name prefix: %d %s", r.status, r.raw)
		}
		// paging: a second customer makes limit=1 return a cursor that yields another row
		second := c.bundleOwner(t, h, m)
		one := c.do(c.on, "GET", base+"/customers?limit=1", full, nil, nil)
		next, _ := one.json["next_cursor"].(string)
		if next == "" || len(cbhArr(one.json["items"])) != 1 {
			t.Fatalf("limit=1: %s", one.raw)
		}
		two := c.do(c.on, "GET", base+"/customers?limit=1&after="+next, full, nil, nil)
		c.want("second page", two, 200, "")
		if a, b := cbhMap(firstOf(cbhArr(one.json["items"])))["customer_id"], cbhMap(firstOf(cbhArr(two.json["items"])))["customer_id"]; a == nil || a == b {
			t.Errorf("the cursor returned the same customer twice (%v)", a)
		}
		_ = second
	})

	t.Run("GET customers/{id}: shape, permissions, cross-store", func(t *testing.T) {
		r := c.do(c.on, "GET", base+"/customers/"+owner, full, nil, nil)
		c.want("detail", r, 200, "")
		c.noStore("detail", r)
		wantKeys := strings.Split(listItemKeys+",claims,consent_history,orders,privacy_actions", ",")
		sort.Strings(wantKeys)
		want := strings.Join(wantKeys, ",")
		if got := cbhKeys(r.json); got != want {
			t.Errorf("detail keys\n got  %s\n want %s", got, want)
		}
		orders, _ := r.json["orders"].([]any)
		if len(orders) != 1 {
			t.Fatalf("orders: %v", r.json["orders"])
		}
		summary := c.do(c.on, "GET", base+"/orders?limit=5", full, nil, nil)
		var page struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.Unmarshal(summary.raw, &page)
		if len(page.Items) == 0 || cbhKeys(orders[0].(map[string]any)) != cbhKeys(page.Items[0]) {
			t.Errorf("detail orders[] must have the merchant-orders Summary shape: %v vs %v", cbhKeys(orders[0].(map[string]any)), page.Items)
		}
		c.want("no permission", c.do(c.on, "GET", base+"/customers/"+owner, c.tokens["none"], nil, nil), 403, "")
		c.want("no token", c.do(c.on, "GET", base+"/customers/"+owner, "", nil, nil), 401, "")
		unknown := c.do(c.on, "GET", base+"/customers/"+randomUUID(), full, nil, nil)
		other := c.do(c.on, "GET", "/v1/admin/stores/"+p2.f.storeA1+"/customers/"+owner, foreign, nil, nil) // the id of another store's customer
		c.want("unknown id", unknown, 404, "")
		c.want("another store's customer", other, 404, "")
		if cbhCode(unknown) != cbhCode(other) || len(unknown.raw) != len(other.raw) {
			t.Errorf("unknown and another store's customer answer differently: %s vs %s", unknown.raw, other.raw)
		}
		c.reject("malformed id", c.do(c.on, "GET", base+"/customers/not-a-uuid", full, nil, nil))
		c.reject("query on detail", c.do(c.on, "GET", base+"/customers/"+owner+"?x=1", full, nil, nil))
	})

	t.Run("POST consent-withdrawals: key, strict body, permission, replay, conflict", func(t *testing.T) {
		path := base + "/customers/" + owner + "/consent-withdrawals"
		body := map[string]any{"purpose": "marketing_messages", "channel": "meta_dm"}
		key := map[string]string{"Idempotency-Key": t04Key("cbh-w")}
		r := c.do(c.on, "POST", path, full, key, body)
		c.want("withdrawal", r, 201, "")
		if cbhKeys(r.json) != "channel,granted,occurred_at,purpose" || r.json["granted"] != false {
			t.Errorf("withdrawal body: %s", r.raw)
		}
		again := c.do(c.on, "POST", path, full, key, body)
		c.want("replay", again, 201, "")
		if string(again.raw) != string(r.raw) {
			t.Errorf("replay changed the body: %s vs %s", again.raw, r.raw)
		}
		c.want("same key, another pair", c.do(c.on, "POST", path, full, key, map[string]any{"purpose": "ads_personalization", "channel": "meta_ads"}), 409, "idempotency_conflict")
		c.reject("missing key", c.do(c.on, "POST", path, full, nil, body))
		c.reject("two keys", c.do(c.on, "POST", path, full, map[string]string{"Idempotency-Key": "aaaaaaaa, bbbbbbbb"}, body))
		c.reject("short key", c.do(c.on, "POST", path, full, map[string]string{"Idempotency-Key": "abc"}, body))
		for name, b := range map[string]any{
			"unknown key":      map[string]any{"purpose": "marketing_messages", "channel": "meta_dm", "granted": true},
			"missing channel":  map[string]any{"purpose": "marketing_messages"},
			"invalid pair":     map[string]any{"purpose": "marketing_messages", "channel": "meta_ads"},
			"unknown purpose":  map[string]any{"purpose": "sms", "channel": "meta_dm"},
			"not JSON":         "purpose=marketing_messages",
			"array":            `[]`,
			"empty body":       nil,
			"null":             `null`,
			"duplicate keys":   `{"purpose":"marketing_messages","purpose":"ads_personalization","channel":"meta_dm"}`,
			"trailing garbage": `{"purpose":"marketing_messages","channel":"meta_dm"} x`,
			"a huge body":      `{"purpose":"` + strings.Repeat("a", 70000) + `","channel":"meta_dm"}`,
		} {
			c.reject(name, c.do(c.on, "POST", path, full, c.key(), b))
		}
		c.want("customers:read alone", c.do(c.on, "POST", path, c.tokens["read"], c.key(), body), 403, "")
		c.want("no token", c.do(c.on, "POST", path, "", c.key(), body), 401, "")
		c.want("another tenant", c.do(c.on, "POST", "/v1/admin/stores/"+p2.f.storeA1+"/customers/"+owner+"/consent-withdrawals", foreign, c.key(), body), 404, "")
		c.reject("wrong method", c.do(c.on, "GET", path, full, nil, nil))
	})

	t.Run("POST exports: attachment headers, envelope, key, permission, bound", func(t *testing.T) {
		path := base + "/customers/" + owner + "/exports"
		key := map[string]string{"Idempotency-Key": t04Key("cbh-x")}
		r := c.do(c.on, "POST", path, full, key, nil)
		c.want("export", r, 200, "")
		if ct := r.hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type %q", ct)
		}
		if cd := r.hdr.Get("Content-Disposition"); cd != `attachment; filename="customer-`+owner+`.json"` {
			t.Errorf("Content-Disposition %q", cd)
		}
		c.noStore("export", r)
		if len(r.raw) > customers.MaxExportBytes {
			t.Errorf("export is %d bytes (> 1 MiB)", len(r.raw))
		}
		if got := cbhKeys(r.json); got != "claims,consents,customer_id,format,generated_at,orders,privacy_actions,store" {
			t.Errorf("export keys %s", got)
		}
		if r.json["format"] != customers.ExportFormat || r.json["customer_id"] != owner || len(cbhMap(r.json["store"])) != 1 || cbhMap(r.json["store"])["name"] == "" {
			t.Errorf("export envelope: %s", r.raw)
		}
		for _, secret := range cbhSecrets(t, f) {
			if strings.Contains(string(r.raw), secret) {
				t.Errorf("the export leaks %q", secret)
			}
		}
		c.want("replay with the same key", c.do(c.on, "POST", path, full, key, nil), 200, "")
		if n := countRows(t, f.owner, `SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='EXPORT'`, owner); n != 1 {
			t.Errorf("%d EXPORT rows after a replay, want 1", n)
		}
		c.reject("a body on export", c.do(c.on, "POST", path, full, c.key(), `{}`))
		c.reject("missing key", c.do(c.on, "POST", path, full, nil, nil))
		c.want("customers:read alone", c.do(c.on, "POST", path, c.tokens["read"], c.key(), nil), 403, "")
		c.want("no token", c.do(c.on, "POST", path, "", c.key(), nil), 401, "")
		c.want("another tenant", c.do(c.on, "POST", "/v1/admin/stores/"+p2.f.storeA1+"/customers/"+owner+"/exports", foreign, c.key(), nil), 404, "")
		c.want("unknown customer", c.do(c.on, "POST", base+"/customers/"+randomUUID()+"/exports", full, c.key(), nil), 404, "")
		// bound: 200 orders export, 201 do not (409 export_too_large, the transaction rolls back: no EXPORT row)
		dropClones := func() {
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM checkout.orders WHERE owner_id=$1 AND id<>$2`, owner, m.p.hold.OrderID); err != nil {
				t.Fatalf("drop cloned orders: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		// the shared fixture audits queue/order consistency (psStarter): the clones must not outlive this subtest
		defer dropClones()
		clone := func(n int, first int) {
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO checkout.orders(tenant_id,store_id,owner_id,id,creator_session_id,cart_id,cart_version,quote_id,destination_id,market_id,country,service_code,service_version,allocation_version,currency,total_minor,commercial_state,fulfillment_state,generation,expires_at,job_id,snapshot,created_at,updated_at)
			 SELECT o.tenant_id,o.store_id,o.owner_id,gen_random_uuid(),o.creator_session_id,o.cart_id,o.cart_version,o.quote_id,o.destination_id,o.market_id,o.country,o.service_code,o.service_version,o.allocation_version,o.currency,o.total_minor,'CANCELLED','CANCELLED',o.generation,
			  o.expires_at-g*interval '1 second',(SELECT max(job_id) FROM checkout.orders)+g,o.snapshot,o.created_at-g*interval '1 second',o.updated_at
			 FROM checkout.orders o, generate_series($3::int,$4::int) g WHERE o.owner_id=$1 AND o.id=$2`, owner, m.p.hold.OrderID, first, first+n-1); err != nil {
				t.Fatalf("clone orders: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		clone(199, 1) // 1 real + 199 = 200
		r200 := c.do(c.on, "POST", path, full, c.key(), nil)
		if r200.status != 200 {
			t.Fatalf("export of exactly 200 orders: HTTP %d %s (limit: <= 200 orders and <= 1 MiB)", r200.status, r200.raw[:min(len(r200.raw), 300)])
		}
		if len(r200.raw) > customers.MaxExportBytes || len(cbhArr(r200.json["orders"])) != 200 {
			t.Errorf("200-order export: %d bytes, %d orders", len(r200.raw), len(cbhArr(r200.json["orders"])))
		}
		exports := countRows(t, f.owner, `SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='EXPORT'`, owner)
		clone(1, 200)
		r201 := c.do(c.on, "POST", path, full, c.key(), nil)
		c.want("export of 201 orders", r201, 409, "export_too_large")
		if got := countRows(t, f.owner, `SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='EXPORT'`, owner); got != exports {
			t.Errorf("a refused oversize export left an EXPORT row (%d -> %d)", exports, got)
		}
	})

	t.Run("POST erasure: typed confirm, blocked, key, permission, replay, retained customer", func(t *testing.T) {
		key := map[string]string{"Idempotency-Key": t04Key("cbh-e")}
		blocked := base + "/customers/" + owner + "/erasure" // the psSetup order is an unexpired hold
		c.want("blocked by an open order", c.do(c.on, "POST", blocked, full, c.key(), map[string]string{"confirm": "ERASE"}), 409, "erasure_blocked")
		victim := c.bundleOwner(t, h, m)
		path := base + "/customers/" + victim + "/erasure"
		for name, b := range map[string]any{
			"lower case": map[string]string{"confirm": "erase"}, "empty": map[string]string{"confirm": ""}, "missing": map[string]string{}, "no body": nil,
			"extra key": map[string]string{"confirm": "ERASE", "force": "1"}, "wrong type": `{"confirm":true}`, "padded": map[string]string{"confirm": "ERASE "},
		} {
			c.reject(name, c.do(c.on, "POST", path, full, c.key(), b))
		}
		c.reject("missing key", c.do(c.on, "POST", path, full, nil, map[string]string{"confirm": "ERASE"}))
		c.want("customers:read alone", c.do(c.on, "POST", path, c.tokens["read"], c.key(), map[string]string{"confirm": "ERASE"}), 403, "")
		c.want("no token", c.do(c.on, "POST", path, "", c.key(), map[string]string{"confirm": "ERASE"}), 401, "")
		c.want("another tenant", c.do(c.on, "POST", "/v1/admin/stores/"+p2.f.storeA1+"/customers/"+victim+"/erasure", foreign, c.key(), map[string]string{"confirm": "ERASE"}), 404, "")
		if n := countRows(t, f.owner, `SELECT count(*) FROM customers.privacy_actions WHERE owner_id=$1 AND kind='ERASURE'`, victim); n != 0 {
			t.Fatalf("a refused erasure left %d ERASURE rows", n)
		}
		r := c.do(c.on, "POST", path, full, key, map[string]string{"confirm": "ERASE"})
		c.want("erasure", r, 200, "")
		if cbhKeys(r.json) != "bundles_relabelled,consents_withdrawn,sessions_revoked,snapshots_redacted" || r.json["bundles_relabelled"] != float64(1) {
			t.Errorf("erasure summary: %s", r.raw)
		}
		again := c.do(c.on, "POST", path, full, key, map[string]string{"confirm": "ERASE"})
		c.want("replay", again, 200, "")
		if string(again.raw) != string(r.raw) {
			t.Errorf("replay changed the summary: %s vs %s", again.raw, r.raw)
		}
		// the customer is retained (orders/bundles), deactivated, and readable
		d := c.do(c.on, "GET", base+"/customers/"+victim, full, nil, nil)
		c.want("detail after erasure", d, 200, "")
		if d.json["active"] != false || len(cbhArr(d.json["privacy_actions"])) != 1 {
			t.Errorf("after erasure: active=%v privacy_actions=%v", d.json["active"], d.json["privacy_actions"])
		}
		if pa := cbhMap(firstOf(cbhArr(d.json["privacy_actions"]))); cbhKeys(pa) != "completed_at,kind,summary,via" || pa["kind"] != "ERASURE" || pa["via"] != "merchant" {
			t.Errorf("privacy action row: %v", pa)
		}
	})

	t.Run("GET finance/summary and summary.csv", func(t *testing.T) {
		q := "?from=2026-01-01&to=2026-01-31"
		r := c.do(c.on, "GET", base+"/finance/summary"+q, c.tokens["ordersRO"], nil, nil)
		c.want("summary", r, 200, "")
		c.noStore("summary", r)
		if cbhKeys(r.json) != "from,rows,timezone,to,totals" || r.json["timezone"] != "Asia/Taipei" || r.json["from"] != "2026-01-01" || r.json["to"] != "2026-01-31" {
			t.Errorf("summary: %s", r.raw)
		}
		if rows, ok := r.json["rows"].([]any); !ok || len(rows) != 0 {
			if !ok {
				t.Errorf("rows must be an array, not null: %s", r.raw)
			}
		}
		c.want("no permission", c.do(c.on, "GET", base+"/finance/summary"+q, c.tokens["none"], nil, nil), 403, "")
		c.want("no token", c.do(c.on, "GET", base+"/finance/summary"+q, "", nil, nil), 401, "")
		for name, qq := range map[string]string{"missing from": "?to=2026-01-31", "missing to": "?from=2026-01-01", "no params": "", "92 days": "?from=2026-01-01&to=2026-04-03",
			"to before from": "?from=2026-02-01&to=2026-01-01", "bad date": "?from=2026-1-1&to=2026-01-31", "impossible date": "?from=2026-02-30&to=2026-03-01", "unknown param": q + "&x=1",
			"timestamp": "?from=2026-01-01T00:00:00Z&to=2026-01-31"} {
			c.want(name, c.do(c.on, "GET", base+"/finance/summary"+qq, c.tokens["ordersRO"], nil, nil), 422, "invalid_request")
		}
		c.want("91 days", c.do(c.on, "GET", base+"/finance/summary?from=2026-01-01&to=2026-04-02", c.tokens["ordersRO"], nil, nil), 200, "")
		csv := c.do(c.on, "GET", base+"/finance/summary.csv"+q, full, nil, nil)
		c.want("csv", csv, 200, "")
		if ct := csv.hdr.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
			t.Errorf("csv Content-Type %q", ct)
		}
		if cd := csv.hdr.Get("Content-Disposition"); cd != `attachment; filename="finance-2026-01-01-2026-01-31.csv"` {
			t.Errorf("csv Content-Disposition %q", cd)
		}
		c.noStore("csv", csv)
		if !strings.HasPrefix(string(csv.raw), "day,currency,environment,captured_count,captured_minor,refunded_minor,net_minor") {
			t.Errorf("csv header: %q", csv.raw)
		}
		c.want("csv with orders:read only", c.do(c.on, "GET", base+"/finance/summary.csv"+q, c.tokens["ordersRO"], nil, nil), 403, "")
		c.want("csv with orders:export only", c.do(c.on, "GET", base+"/finance/summary.csv"+q, c.tokens["exportOnly"], nil, nil), 403, "")
		c.want("csv another tenant", c.do(c.on, "GET", "/v1/admin/stores/"+p2.f.storeA1+"/finance/summary.csv"+q, full, nil, nil), 404, "")
		if n := countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='finance.exported'`, m.store); n < 1 {
			t.Error("the CSV export left no finance.exported audit row")
		}
	})

	t.Run("billing: GET billing, GET standing, POST checkout, POST portal", func(t *testing.T) {
		r := c.do(c.on, "GET", base+"/billing", c.tokens["billing"], nil, nil)
		c.want("billing", r, 200, "")
		c.noStore("billing", r)
		if got := cbhKeys(r.json); got != "customer_pinned,payment_pending,plans,stale,standing,subscriptions,usage" {
			t.Errorf("billing keys %s", got)
		}
		if r.json["standing"] != "UNBILLED" || len(cbhArr(r.json["plans"])) != 1 {
			t.Errorf("billing body: %s", r.raw)
		}
		if got := cbhKeys(cbhMap(r.json["usage"])); got != "claim_windows_opened,members,paid_orders,period_end,period_start,private_replies_sent" {
			t.Errorf("usage keys %s", got)
		}
		c.want("billing without billing:manage", c.do(c.on, "GET", base+"/billing", c.tokens["read"], nil, nil), 403, "")
		c.want("billing no token", c.do(c.on, "GET", base+"/billing", "", nil, nil), 401, "")
		c.want("billing another tenant", c.do(c.on, "GET", "/v1/admin/stores/"+p2.f.storeA1+"/billing", c.tokens["billing"], nil, nil), 404, "")
		s := c.do(c.on, "GET", base+"/billing/standing", c.tokens["none"], nil, nil) // any member (store:read) sees the banner
		c.want("standing", s, 200, "")
		if cbhKeys(s.json) != "standing" || s.json["standing"] != "UNBILLED" {
			t.Errorf("standing body: %s", s.raw)
		}
		c.want("standing no token", c.do(c.on, "GET", base+"/billing/standing", "", nil, nil), 401, "")
		c.want("standing another tenant", c.do(c.on, "GET", "/v1/admin/stores/"+p2.f.storeA1+"/billing/standing", c.tokens["none"], nil, nil), 404, "")
		// portal before any customer: 409 no_billing_customer
		c.want("portal without a customer", c.do(c.on, "POST", base+"/billing/portal", c.tokens["billing"], nil, nil), 409, "no_billing_customer")
		body := map[string]string{"price_id": "price_Cb08Month"}
		co := c.do(c.on, "POST", base+"/billing/checkout", c.tokens["billing"], nil, body)
		c.want("checkout", co, 200, "")
		if cbhKeys(co.json) != "url" || !strings.HasPrefix(fmt.Sprint(co.json["url"]), "https://checkout.stripe.com/") {
			t.Errorf("checkout body keys %s", cbhKeys(co.json))
		}
		c.noStore("checkout", co)
		po := c.do(c.on, "POST", base+"/billing/portal", c.tokens["billing"], nil, nil)
		c.want("portal", po, 200, "")
		if cbhKeys(po.json) != "url" || !strings.HasPrefix(fmt.Sprint(po.json["url"]), "https://billing.stripe.com/") {
			t.Errorf("portal body keys %s", cbhKeys(po.json))
		}
		for name, b := range map[string]any{"unknown key": map[string]string{"price_id": "price_Cb08Month", "x": "1"}, "missing price": map[string]string{}, "not JSON": "price_id=x",
			"no body": nil, "price not configured": map[string]string{"price_id": "price_Nope"}, "wrong type": `{"price_id":1}`} {
			c.reject("checkout body "+name, c.do(c.on, "POST", base+"/billing/checkout", c.tokens["billing"], nil, b))
		}
		c.want("checkout without billing:manage", c.do(c.on, "POST", base+"/billing/checkout", c.tokens["read"], nil, body), 403, "")
		c.want("portal without billing:manage", c.do(c.on, "POST", base+"/billing/portal", c.tokens["read"], nil, nil), 403, "")
		c.want("checkout no token", c.do(c.on, "POST", base+"/billing/checkout", "", nil, body), 401, "")
		c.want("checkout another tenant", c.do(c.on, "POST", "/v1/admin/stores/"+p2.f.storeA1+"/billing/checkout", c.tokens["billing"], nil, body), 404, "")
		c.reject("portal with a body", c.do(c.on, "POST", base+"/billing/portal", c.tokens["billing"], nil, `{"x":1}`))
		c.reject("checkout wrong method", c.do(c.on, "GET", base+"/billing/checkout", c.tokens["billing"], nil, nil))
		// 409 subscription_exists (a live subscription at Stripe), never a second session
		cus := cbxOne(t, f, `SELECT stripe_customer_id FROM billing.store_customers WHERE store_id=$1`, m.store)
		sessions := len(m.fake.CallsExact("POST", "/v1/checkout/sessions"))
		m.fake.AddSubscription(billingtest.Sub{Customer: cus, Status: "active", StoreMeta: m.store})
		c.want("subscription exists", c.do(c.on, "POST", base+"/billing/checkout", c.tokens["billing"], nil, body), 409, "subscription_exists")
		if len(m.fake.CallsExact("POST", "/v1/checkout/sessions")) != sessions {
			t.Error("a session was created although a subscription exists")
		}
		// billing disabled (no service): GETs work, the POSTs are 503 billing_unavailable
		d := c.do(c.off, "GET", base+"/billing", c.tokens["billing"], nil, nil)
		c.want("billing GET with the service off", d, 200, "")
		if plans, ok := d.json["plans"].([]any); !ok || len(plans) != 0 {
			t.Errorf("plans with the service off: %s", d.raw)
		}
		c.want("standing with the service off", c.do(c.off, "GET", base+"/billing/standing", c.tokens["none"], nil, nil), 200, "")
		c.want("checkout with the service off", c.do(c.off, "POST", base+"/billing/checkout", c.tokens["billing"], nil, body), 503, "billing_unavailable")
		c.want("portal with the service off", c.do(c.off, "POST", base+"/billing/portal", c.tokens["billing"], nil, nil), 503, "billing_unavailable")
	})

	t.Run("billing_restricted is HTTP 402 on the claim-window route (B9, B12)", func(t *testing.T) {
		wp := psSetup(t)
		wf := wp.f
		hh := &lcHarness{cqHarness: wp.cqHarness, ctx: ctx}
		hh.actor, hh.token = lcPrincipal(t, wf, wf.tenantA, []string{wf.storeA1}, "store:read", "live:read", "live:manage")
		hh.labels, _ = claims.NewLabelKey(randomBytes(32))
		handler := httpapi.NewHandler(wf.runtime, httpapi.Options{ClaimLabels: &hh.labels, Studio: true})
		ing := cbxLogin(t, wf, "commerce_stripe_ingress")
		cbxRestrict(t, wf, ing, wf.tenantA, wf.storeA1)
		s := hh.draft(t, wf.storeA1)
		hh.offer(t, s, "A1", wp.stock.skus[0].ID, 5)
		path := "/v1/admin/stores/" + wf.storeA1 + "/live-sessions/" + s + "/claims/window"
		r := c.do(handler, "POST", path, hh.token, c.key(), map[string]any{"expected_version": hh.board(t, s).Window.Version, "state": "OPEN", "match_mode": "EXACT"})
		c.want("window under RESTRICTED", r, 402, "billing_restricted")
		if r.status == 403 || strings.Contains(strings.ToLower(string(r.raw)), "permission") {
			t.Errorf("a billing refusal must not read as a permission error: %s", r.raw)
		}
		if w := hh.board(t, s).Window; w.State == claims.WindowOpen {
			t.Error("the refused window is open")
		}
		// closing and the read stay available
		c.want("board read", c.do(handler, "GET", "/v1/admin/stores/"+wf.storeA1+"/live-sessions/"+s+"/claims", hh.token, nil, nil), 200, "")
	})

	t.Run("buyer routes: privacy state, consents, export, erasure (410 erased)", func(t *testing.T) {
		bh := bhSetup(t)
		issue := func() (string, buyer.Capability) {
			out := bhRead[struct {
				Token string `json:"token"`
			}](t, bh.request(t, "POST", "/v1/buyer/session", "", "", struct{}{}, nil), 200)
			var cp buyer.Capability
			if err := buyer.WithScope(ctx, bh.a.runtime, out.Token, bh.f.storeA1, func(_ context.Context, _ pgx.Tx, s buyer.Scope) error {
				cp = buyer.Capability{Token: out.Token, Scope: s}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return out.Token, cp
		}
		tok, cp := issue()
		g := bh.request(t, "GET", "/v1/buyer/privacy", tok, "", nil, nil)
		var priv map[string]any
		if g.status != 200 || json.Unmarshal(g.body, &priv) != nil || cbhKeys(priv) != "consents,erased" || priv["erased"] != false ||
			cbhKeys(cbhMap(priv["consents"])) != "ads_personalization,marketing_messages" {
			t.Fatalf("GET privacy: %d %s", g.status, g.body)
		}
		if r := bh.request(t, "GET", "/v1/buyer/privacy", "", "", nil, nil); r.status != 401 {
			t.Errorf("GET privacy without a capability: %d, want 401", r.status)
		}
		put := func(token, key string, body any) bhResponse {
			return bh.request(t, "PUT", "/v1/buyer/consents", token, key, body, nil)
		}
		good := map[string]any{"purpose": "marketing_messages", "channel": "meta_dm", "granted": true, "context": "settings"}
		if r := put(tok, "", good); r.status < 400 || r.status >= 500 {
			t.Errorf("PUT consents without Idempotency-Key: %d, want a 4xx", r.status)
		}
		r := put(tok, t04Key("cbh-consent"), good)
		var res map[string]any
		if r.status != 200 || json.Unmarshal(r.body, &res) != nil || cbhKeys(res) != "channel,granted,occurred_at,purpose" || res["granted"] != true {
			t.Fatalf("PUT consents: %d %s", r.status, r.body)
		}
		if r := put(tok, t04Key("cbh-consent"), good); r.status != 200 {
			t.Errorf("a new key with the same state still records: %d", r.status)
		}
		g = bh.request(t, "GET", "/v1/buyer/privacy", tok, "", nil, nil)
		if json.Unmarshal(g.body, &priv); cbhMap(priv["consents"])["marketing_messages"] != true {
			t.Errorf("GET privacy after the grant: %s", g.body)
		}
		if r := bh.request(t, "DELETE", "/v1/buyer/consents", tok, t04Key("cbh"), nil, nil); r.status < 400 || r.status >= 500 {
			t.Errorf("DELETE consents: %d, want a 4xx", r.status)
		}
		// export
		ek := t04Key("cbh-bx")
		x := bh.request(t, "POST", "/v1/buyer/privacy/export", tok, ek, nil, nil)
		var doc map[string]any
		if x.status != 200 || json.Unmarshal(x.body, &doc) != nil {
			t.Fatalf("buyer export: %d %s", x.status, x.body)
		}
		if got := cbhKeys(doc); got != "claims,consents,format,generated_at,orders,privacy_actions,store" || doc["format"] != customers.ExportFormat {
			t.Errorf("buyer export keys %s (same envelope as the merchant's, no customer_id or principal ids)", got)
		}
		if !strings.HasPrefix(x.header.Get("Content-Type"), "application/json") || !strings.HasPrefix(x.header.Get("Content-Disposition"), "attachment;") || !strings.Contains(x.header.Get("Cache-Control"), "no-store") {
			t.Errorf("buyer export headers: %v", x.header)
		}
		for _, secret := range cbhSecrets(t, bh.f) {
			if strings.Contains(string(x.body), secret) {
				t.Errorf("the buyer export leaks %q", secret)
			}
		}
		if r := bh.request(t, "POST", "/v1/buyer/privacy/export", tok, ek, nil, nil); r.status != 200 {
			t.Errorf("export replay: %d", r.status)
		}
		if r := bh.request(t, "POST", "/v1/buyer/privacy/export", tok, "", nil, nil); r.status < 400 || r.status >= 500 {
			t.Errorf("export without a key: %d, want a 4xx", r.status)
		}
		if r := bh.request(t, "POST", "/v1/buyer/privacy/export", tok, t04Key("cbh-bx"), map[string]string{"x": "1"}, nil); r.status < 400 || r.status >= 500 {
			t.Errorf("export with a body: %d, want a 4xx", r.status)
		}
		if r := bh.request(t, "GET", "/v1/buyer/privacy/export", tok, "", nil, nil); r.status < 400 || r.status >= 500 {
			t.Errorf("GET export: %d, want a 4xx", r.status)
		}
		// erasure: strict body, blocked by an open order, success, 410 erased afterwards
		items := []storefront.Item{{SKUID: bh.stock.skus[0].ID, Quantity: 1}}
		bh.prepare(t, cp, items)
		if _, err := bh.begin(t04Key("cbh-hold")); err != nil {
			t.Fatalf("checkout for the blocked-erasure case: %v", err)
		}
		for name, b := range map[string]any{"lower case": map[string]string{"confirm": "erase"}, "missing": map[string]string{}, "extra key": map[string]string{"confirm": "ERASE", "x": "1"}, "no body": nil} {
			if r := bh.request(t, "POST", "/v1/buyer/privacy/erasure", tok, t04Key("cbh-be"), b, nil); r.status < 400 || r.status >= 500 {
				t.Errorf("erasure body %s: %d, want a 4xx", name, r.status)
			}
		}
		bhError(t, bh.request(t, "POST", "/v1/buyer/privacy/erasure", tok, t04Key("cbh-be"), map[string]string{"confirm": "ERASE"}, nil), 409, "erasure_blocked")
		if r := bh.request(t, "GET", "/v1/buyer/privacy", tok, "", nil, nil); r.status != 200 {
			t.Errorf("a refused erasure must keep the capability: %d", r.status)
		}
		tok2, _ := issue()
		ke := t04Key("cbh-be2")
		er := bh.request(t, "POST", "/v1/buyer/privacy/erasure", tok2, ke, map[string]string{"confirm": "ERASE"}, nil)
		var body map[string]any
		if er.status != 200 || json.Unmarshal(er.body, &body) != nil {
			t.Fatalf("buyer erasure: %d %s", er.status, er.body)
		}
		// the response carries the four counts (wherever the handler nests them) and tells the buyer that order records are kept
		sum := body
		if nested := cbhMap(body["summary"]); nested != nil {
			sum = nested
		}
		if cbhKeys(sum) != "bundles_relabelled,consents_withdrawn,sessions_revoked,snapshots_redacted" || !(cbhNum(sum["sessions_revoked"]) >= 1) {
			t.Fatalf("buyer erasure summary: %s", er.body)
		}
		kept := false
		for k, v := range body {
			kept = kept || (strings.Contains(k, "retain") && v == true)
		}
		if !kept {
			t.Errorf("the erasure response must tell the buyer that order records are retained (contract §5 Buyer): %s", er.body)
		}
		bhError(t, bh.request(t, "POST", "/v1/buyer/privacy/erasure", tok2, ke, map[string]string{"confirm": "ERASE"}, nil), 410, "erased")
		bhError(t, bh.request(t, "POST", "/v1/buyer/privacy/erasure", tok2, t04Key("cbh-be3"), map[string]string{"confirm": "ERASE"}, nil), 410, "erased")
		if r := bh.request(t, "GET", "/v1/buyer/privacy", tok2, "", nil, nil); r.status != 401 {
			t.Errorf("GET privacy after erasure: %d, want 401 (the capability is revoked)", r.status)
		}
		if r := put(tok2, t04Key("cbh-after"), good); r.status != 401 && r.status != 410 {
			t.Errorf("PUT consents after erasure: %d, want 401/410", r.status)
		}
	})

	t.Run("no error body or response carries a secret, a bearer URL or another domain's id", func(t *testing.T) {
		pat := regexp.MustCompile(`(sk_|rk_)test_[A-Za-z0-9]{6,}|whsec_[A-Za-z0-9]{6,}|acct_[A-Za-z0-9]{6,}|\bcus_[A-Za-z0-9]{6,}|\bsub_[A-Za-z0-9]{6,}|\bcs_test_[A-Za-z0-9]{2,}|fakebearer|hash_?key`)
		for i, b := range c.bodies {
			if strings.Contains(b, `"url"`) {
				continue // the two bearer-link answers of checkout/portal are the point of those routes
			}
			if loc := pat.FindString(b); loc != "" {
				t.Errorf("response %d carries %q: %.200s", i, loc, b)
			}
			for _, tok := range c.tokens {
				if strings.Contains(b, tok) {
					t.Errorf("response %d echoes a bearer token", i)
				}
			}
		}
		if len(c.bodies) < 100 {
			t.Errorf("only %d responses were scanned: the matrix did not run", len(c.bodies))
		}
	})
}

// cbhSecrets are the strings no customer/export output may contain: actor keys, capability sessions and hashes.
func cbhSecrets(t *testing.T, f *testFixture) (out []string) {
	t.Helper()
	for _, q := range []string{`SELECT actor_key FROM claims.bundles`, `SELECT id::text FROM buyer.capability_sessions`, `SELECT encode(token_hash,'hex') FROM buyer.capability_sessions`} {
		out = append(out, cbsEnv{t: t, f: f}.list(q)...)
	}
	return out
}

// bundleOwner makes a customer (a bound manual bundle, no order) in the store of m and returns its owner id.
func (c *cbhEnv) bundleOwner(t *testing.T, h *lcHarness, m *cbmEnv) string {
	t.Helper()
	s := h.draft(t, m.store)
	h.offer(t, s, "A1", m.p.stock.skus[0].ID, 5)
	h.open(t, s, claims.MatchExact)
	r := h.accepted(t, s, "", "cbh-"+t04Tag(), "A1+1")
	cp := mustIssue(t, m.p.cqHarness.service, m.store)
	if _, err := h.redeem(cp, t04Key("cbh-bundle"), h.link(t, s, r.BundleID, 0, false).Token, r.BundleVersion); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	h.closeWindow(t, s)
	return cp.Scope.OwnerID
}

// TestCustomersBillingCB09Permissions is the permission-set half of CB09. §5 (amended, R1 review P1-2) lists one
// permission per merchant row except the two rows that return order PII through merchantorders.Get: detail needs
// customers:read + orders:read and export customers:privacy + customers:read + orders:read. The list, withdraw and erase
// rows return no order PII and need exactly customers:read / customers:privacy. It is a separate top-level test so a
// deviation between the frozen table and an implementation cannot hide the other CB09 assertions. The fail-closed
// halves (the row permission alone is 403) prove no order PII leaks to a principal without orders:read.
func TestCustomersBillingCB09Permissions(t *testing.T) {
	m := cbmSetup(t, false, false)
	f, ctx := m.f, m.ctx
	h := &lcHarness{cqHarness: m.p.cqHarness, ctx: ctx}
	h.actor, h.token = lcPrincipal(t, f, m.tenant, []string{m.store}, "store:read", "live:read", "live:manage")
	var err error
	if h.labels, err = claims.NewLabelKey(randomBytes(32)); err != nil {
		t.Fatal(err)
	}
	c := &cbhEnv{cbmEnv: m}
	c.on = httpapi.NewHandler(f.runtime, httpapi.Options{Billing: m.svc, ClaimLabels: &h.labels, Studio: true})
	mk := func(perms ...string) string { return lcTokenFor(t, f, m.tenant, m.store, perms...) }
	c.tokens = map[string]string{"read": mk("customers:read"), "privacy": mk("customers:privacy"),
		"read+orders": mk("customers:read", "orders:read"), "privacy+read": mk("customers:privacy", "customers:read"),
		"privacy+read+orders": mk("customers:privacy", "customers:read", "orders:read")}
	base := "/v1/admin/stores/" + m.store
	owner := m.p.cap.Scope.OwnerID
	if r := c.do(c.on, "GET", base+"/customers", c.tokens["read"], nil, nil); r.status != 200 {
		t.Errorf("GET customers (list, no order PII) with customers:read alone: HTTP %d code=%q, want 200", r.status, cbhCode(r))
	}
	// §5 amendment (lane close): the list row carries minimized destination PII by design (display_name, phone_last3 <= 3 digits)
	// under customers:read alone; nothing else of the destination may appear.
	if r := c.do(c.on, "GET", base+"/customers", c.tokens["read"], nil, nil); r.status == 200 {
		items, _ := r.json["items"].([]any)
		if len(items) < 1 {
			t.Errorf("list with customers:read alone returned no rows: %s", r.raw)
		}
		for _, it := range items {
			row := it.(map[string]any)
			if last3, ok := row["phone_last3"].(string); ok && !regexp.MustCompile(`^[0-9]{0,3}$`).MatchString(last3) {
				t.Errorf("phone_last3 %q is not at most 3 digits", last3)
			}
			if strings.Contains(string(r.raw), "address") || strings.Contains(string(r.raw), "line1") {
				t.Errorf("list leaks destination fields beyond display_name/phone_last3: %s", r.raw)
			}
		}
	}
	if r := c.do(c.on, "GET", base+"/customers/"+owner, c.tokens["read"], nil, nil); r.status != 403 {
		t.Errorf("GET customers/{id} with customers:read alone: HTTP %d code=%q, want 403 (order PII needs orders:read)", r.status, cbhCode(r))
	}
	if r := c.do(c.on, "GET", base+"/customers/"+owner, c.tokens["read+orders"], nil, nil); r.status != 200 {
		t.Errorf("GET customers/{id} with customers:read + orders:read: HTTP %d code=%q, want 200 (§5 amended)", r.status, cbhCode(r))
	}
	for _, name := range []string{"privacy", "privacy+read"} {
		if r := c.do(c.on, "POST", base+"/customers/"+owner+"/exports", c.tokens[name], c.key(), nil); r.status != 403 {
			t.Errorf("POST exports with %s: HTTP %d code=%q, want 403 (order PII needs orders:read)", name, r.status, cbhCode(r))
		}
	}
	if r := c.do(c.on, "POST", base+"/customers/"+owner+"/exports", c.tokens["privacy+read+orders"], c.key(), nil); r.status != 200 {
		t.Errorf("POST exports with customers:privacy + customers:read + orders:read: HTTP %d code=%q, want 200 (§5 amended)", r.status, cbhCode(r))
	}
	if r := c.do(c.on, "POST", base+"/customers/"+owner+"/consent-withdrawals", c.tokens["privacy"], c.key(), map[string]any{"purpose": "ads_personalization", "channel": "meta_ads"}); r.status != 201 {
		t.Errorf("POST consent-withdrawals with customers:privacy alone: HTTP %d code=%q, want 201", r.status, cbhCode(r))
	}
	victim := c.bundleOwner(t, h, m)
	if r := c.do(c.on, "POST", base+"/customers/"+victim+"/erasure", c.tokens["privacy"], c.key(), map[string]string{"confirm": "ERASE"}); r.status != 200 {
		t.Errorf("POST erasure with customers:privacy alone: HTTP %d code=%q, want 200", r.status, cbhCode(r))
	}
}
