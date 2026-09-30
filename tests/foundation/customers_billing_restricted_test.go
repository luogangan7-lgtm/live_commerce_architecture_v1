package foundation_test

// CB07 (contracts/customers-billing-v1.md §0 BD5, §8; tier REAL_PG over the rfx harness = real capture, refund
// and shipment paths, plus the merchant HTTP handler). Written from the contract only. Helper prefix `cbr`.
// What it proves: a RESTRICTED store (a canceled subscription applied through the real ingress definer) can
// still, through their real paths: place a checkout for an existing cart, apply a payment (real signed webhook
// + capture worker), request and complete a refund, record a shipment, read and export orders, and execute
// the privacy actions (merchant export, consent withdrawal, erasure; buyer export). The billing effect is the
// claim-window trigger alone: no function of the schemas checkout, payments, fulfillment, buyer, storefront
// references `billing.` (pg_proc.prosrc) and the only trigger outside billing.* that calls a billing function
// is the one on live.claim_windows. A control proves the store really is RESTRICTED (the window is refused).
// Disclosed owner-pool fixtures: cbxPin (Stripe customer row) and grants of the merchant principal.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/claims"
	"livecommerce/internal/customers"
	"livecommerce/internal/storefront"
)

func TestCustomersBillingCB07Restricted(t *testing.T) {
	e := rfxNew(t)
	ctx := context.Background()
	stop := e.startWorker(t)
	defer stop()
	a := e.stripeStore(t)
	endpoint, secret := e.endpoint(t, a)
	first := e.pay(t, a, endpoint, secret) // paid while UNBILLED: the subject of refund, shipment, reads, export
	e.grant(t, first, "orders:read", "orders:export", "payments:refund", "fulfillment:write", "customers:read", "customers:privacy", "billing:manage")
	store, tenant, tok := first.store(), first.s.p.f.tenantA, first.token()
	firstOwner := first.s.p.cap.Scope.OwnerID

	// a cart, a destination and a quote prepared BEFORE the restriction (checkout of an existing cart)
	e.ensureStock(t, first)
	pre := first.s.p
	pre.bcHarness.prepare(t, mustIssue(t, first.s.p.cqHarness.service, store), []storefront.Item{{SKUID: pre.stock.skus[0].ID, Quantity: 2}})
	// a fresh owner for the erasure (no order, no open session)
	fresh := mustIssue(t, first.s.p.cqHarness.service, store)
	// the buyer consents so the merchant has something to withdraw
	if err := buyer.WithScope(ctx, first.s.p.a.runtime, first.s.p.cap.Token, store, func(c context.Context, tx pgx.Tx, s buyer.Scope) error {
		_, err := customers.BuyerSetConsent(c, tx, s.StoreID, first.s.p.cap.Token, t04Key("cbr-consent"), customers.ConsentInput{Purpose: "marketing_messages", Channel: "meta_dm", Granted: true, Context: "settings"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// ---- restrict the store through the real ingress definer ----
	ing := cbxLogin(t, e.f, "commerce_stripe_ingress")
	cbxRestrict(t, e.f, ing, tenant, store)
	auth := cbxLogin(t, e.f, "commerce_auth")
	var standing string
	if err := auth.QueryRow(ctx, `SELECT billing.store_standing($1,$2)`, tenant, store).Scan(&standing); err != nil || standing != "RESTRICTED" {
		t.Fatalf("fixture: standing %q %v, want RESTRICTED", standing, err)
	}

	t.Run("control: the restriction is real (a claim window cannot open)", func(t *testing.T) {
		h := cblClaims(t, first)
		s := h.draft(t, store)
		h.offer(t, s, "A1", pre.stock.skus[0].ID, 5)
		if _, err := h.setWindow(h.token, store, s, claims.WindowInput{ExpectedVersion: h.board(t, s).Window.Version, State: claims.WindowOpen, MatchMode: claims.MatchExact}); !errors.Is(err, claims.ErrBillingRestricted) {
			t.Fatalf("window under RESTRICTED: %v, want ErrBillingRestricted", err)
		}
	})

	t.Run("buyer checkout of an existing cart", func(t *testing.T) {
		res, err := pre.bcHarness.begin(t04Key("cbr-checkout"))
		if err != nil || res.OrderID == "" {
			t.Fatalf("checkout under RESTRICTED: %+v %v", res, err)
		}
		pre.hold = res
		s := a
		s.p = pre
		// payment apply: the signed webhook and the capture worker settle this order too
		paid := e.pay(t, s, endpoint, secret)
		if paid.captured != 2500 || !e.has(t, paid.attempt, "CAPTURED") {
			t.Fatalf("payment apply under RESTRICTED: captured=%d", paid.captured)
		}
	})

	t.Run("refund request and completion", func(t *testing.T) {
		id := e.mustRefund(t, first, 1000, "requested_by_customer")
		e.awaitRefundFact(t, id, first.attempt, "SUCCEEDED")
		if st := e.itemState(t, first, id); st != "SUCCEEDED" {
			t.Fatalf("refund state %s", st)
		}
	})

	t.Run("shipment record", func(t *testing.T) {
		out := e.mustShip(t, first, 0, "sf_express", "SFCBR0001")
		if out == nil || e.mfxFulfilmentState(t, first) != "MERCHANT_SHIPPED" {
			t.Fatalf("shipment under RESTRICTED: %v state=%s", out, e.mfxFulfilmentState(t, first))
		}
	})

	t.Run("merchant order read and export", func(t *testing.T) {
		status, raw, _ := e.call("GET", "/v1/admin/stores/"+store+"/orders?limit=100", tok, nil, "")
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if status != 200 || json.Unmarshal(raw, &page) != nil || len(page.Items) < 2 {
			t.Fatalf("order list under RESTRICTED: %d %s", status, raw)
		}
		status, raw, hdr := e.mfxCSV(first, tok)
		if status != 200 || !strings.Contains(hdr.Get("Content-Type"), "text/csv") || len(raw) == 0 {
			t.Fatalf("order CSV export under RESTRICTED: %d %s", status, hdr.Get("Content-Type"))
		}
		status, raw, _ = e.call("GET", "/v1/admin/stores/"+store+"/orders/"+first.order, tok, nil, "")
		if status != 200 {
			t.Fatalf("order detail under RESTRICTED: %d %s", status, raw)
		}
		status, raw, _ = e.call("GET", "/v1/admin/stores/"+store+"/customers?limit=10", tok, nil, "")
		if status != 200 {
			t.Fatalf("customer list under RESTRICTED: %d %s", status, raw)
		}
	})

	t.Run("privacy actions: merchant export, consent withdrawal, erasure; buyer export", func(t *testing.T) {
		base := "/v1/admin/stores/" + store + "/customers/"
		status, raw, hdr := e.call("POST", base+firstOwner+"/exports", tok, map[string]string{"Idempotency-Key": t04Key("cbr-export")}, "")
		if status != 200 || !strings.Contains(hdr.Get("Content-Type"), "application/json") || !strings.Contains(string(raw), customers.ExportFormat) {
			t.Fatalf("export under RESTRICTED: %d %s", status, raw)
		}
		status, raw, _ = e.call("POST", base+firstOwner+"/consent-withdrawals", tok, map[string]string{"Idempotency-Key": t04Key("cbr-withdraw")}, `{"purpose":"marketing_messages","channel":"meta_dm"}`)
		if status != 201 {
			t.Fatalf("consent withdrawal under RESTRICTED: %d %s", status, raw)
		}
		status, raw, _ = e.call("POST", base+fresh.Scope.OwnerID+"/erasure", tok, map[string]string{"Idempotency-Key": t04Key("cbr-erase")}, `{"confirm":"ERASE"}`)
		if status != 200 {
			t.Fatalf("erasure under RESTRICTED: %d %s", status, raw)
		}
		var doc []byte
		if err := buyer.WithScope(ctx, first.s.p.a.runtime, first.s.p.cap.Token, store, func(c context.Context, tx pgx.Tx, s buyer.Scope) (err error) {
			doc, err = customers.BuyerExport(c, tx, s.StoreID, first.s.p.cap.Token, t04Key("cbr-buyer-export"))
			return err
		}); err != nil || !strings.Contains(string(doc), customers.ExportFormat) {
			t.Fatalf("buyer export under RESTRICTED: %v", err)
		}
	})

	t.Run("catalog: the only billing effect outside billing.* is the claim-window trigger", func(t *testing.T) {
		total := 0
		for _, schema := range []string{"checkout", "payments", "fulfillment", "buyer", "storefront"} {
			n := e.count(t, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1`, schema)
			total += n
			t.Logf("schema %s: %d functions scanned", schema, n) // storefront holds none today; its rule is the same
			if schema != "storefront" && n < 3 {
				t.Fatalf("schema %s lists %d functions: the scan would be vacuous", schema, n)
			}
		}
		if total < 40 {
			t.Fatalf("only %d functions scanned in total: the scan would be vacuous", total)
		}
		rows, err := e.f.owner.Query(ctx, `SELECT n.nspname||'.'||p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		 WHERE n.nspname IN ('checkout','payments','fulfillment','buyer','storefront') AND p.prosrc ~* '\mbilling\s*\.'`)
		if err != nil {
			t.Fatal(err)
		}
		var offenders []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			offenders = append(offenders, s)
		}
		rows.Close()
		if len(offenders) > 0 {
			t.Errorf("functions referencing billing.: %v (BD5: RESTRICTED blocks only new claim windows)", offenders)
		}
		// triggers whose function lives in billing: exactly the window guard (outside billing.*) and the row guard (inside)
		var tables []string
		trows, err := e.f.owner.Query(ctx, `SELECT c.relnamespace::regnamespace::text||'.'||c.relname||':'||t.tgname FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid
		 JOIN pg_class c ON c.oid=t.tgrelid WHERE p.pronamespace='billing'::regnamespace AND NOT t.tgisinternal`)
		if err != nil {
			t.Fatal(err)
		}
		for trows.Next() {
			var s string
			if err := trows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, s)
		}
		trows.Close()
		outside := 0
		for _, s := range tables {
			if !strings.HasPrefix(s, "billing.") {
				outside++
				if !regexp.MustCompile(`^live\.claim_windows:billing_guard_window_open$`).MatchString(s) {
					t.Errorf("a billing trigger outside billing.*: %s", s)
				}
			}
		}
		if outside != 1 {
			t.Errorf("want exactly one billing trigger outside billing.* (live.claim_windows), got %d: %v", outside, tables)
		}
	})
	_ = time.Second
}
