package foundation_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/platform"
)

var bpViewKeys = []string{"commercial_state", "currency", "handoff_expires_at", "handoff_state", "methods", "order_id", "payment_state", "test_mode", "total_minor"}
var bpMethodKeys = []string{"code", "name_en", "name_hans", "name_hant", "version"}

func bpKeys(value map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func bpView(t *testing.T, service *checkout.HostedPaymentStarter, token, storeID, orderID string) (checkout.OrderPayment, map[string]json.RawMessage) {
	t.Helper()
	result, err := service.PaymentView(context.Background(), token, storeID, orderID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || !reflect.DeepEqual(bpKeys(fields), bpViewKeys) {
		t.Fatal("BPH01 payment view has non-contract or missing top-level fields")
	}
	for _, forbidden := range []string{"attempt_id", "operation_id", "job_id", "generation", "merchant_trade_no", "connection_id", "credential_version", "qualification_id", "binding_id", "form", "hash_key", "hash_iv"} {
		if _, found := fields[forbidden]; found {
			t.Fatalf("BPH01 private field exposed: %s", forbidden)
		}
	}
	if string(fields["methods"]) == "null" || string(fields["handoff_expires_at"]) == "" {
		t.Fatal("BPH01 methods or handoff expiry has invalid null shape")
	}
	return result, fields
}

func bpState(t *testing.T, fields map[string]json.RawMessage, name, expected string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(fields[name], &got); err != nil || got != expected {
		t.Fatalf("payment projection %s=%q want %q", name, got, expected)
	}
}

func bpStarter(t *testing.T, h hpHarness, profile string, config checkout.HostedConfig) *checkout.HostedPaymentStarter {
	t.Helper()
	keys, err := accounts.NewKeyring("hosted_fixture", map[string][]byte{"hosted_fixture": h.key}, randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	return hpStarter(t, h.pool, profile, keys, config).(*checkout.HostedPaymentStarter)
}

func bpCounts(t *testing.T, h hpHarness) (base [8]int64, other [3]int64) {
	t.Helper()
	base = h.counts(t)
	if err := h.f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM payments.facts f JOIN checkout.payment_attempts a ON a.id=f.attempt_id WHERE a.owner_id=$1),
	 (SELECT count(*) FROM payments.review_cases r JOIN checkout.payment_attempts a ON a.id=r.attempt_id WHERE a.owner_id=$1),
	 (SELECT count(*) FROM inventory.ledger WHERE checkout_id=$2)`, h.cap.Scope.OwnerID, h.hold.OrderID).
		Scan(&other[0], &other[1], &other[2]); err != nil {
		t.Fatal(err)
	}
	return
}

func TestBuyerPaymentViewEligibleAndExactNames(t *testing.T) {
	h := hpSetup(t)
	before, beforeOther := bpCounts(t, h)
	view, raw := bpView(t, h.api.(*checkout.HostedPaymentStarter), h.cap.Token, h.f.storeA1, h.hold.OrderID)
	if view.OrderID != h.hold.OrderID || view.Currency != "TWD" || view.TotalMinor != 2500 || view.CommercialState != "DRAFT" || !view.TestMode {
		t.Fatal("BPH01 owned order projection changed frozen amount or test disclosure")
	}
	bpState(t, raw, "payment_state", "NOT_STARTED")
	bpState(t, raw, "handoff_state", "NONE")
	if string(raw["handoff_expires_at"]) != "null" || len(view.Methods) != 1 {
		t.Fatal("BPH01 eligible order lost one method or invented a page")
	}
	method, err := json.Marshal(view.Methods[0])
	if err != nil {
		t.Fatal(err)
	}
	var names map[string]json.RawMessage
	if err := json.Unmarshal(method, &names); err != nil || !reflect.DeepEqual(bpKeys(names), bpMethodKeys) {
		t.Fatal("BPH01 method included private identifiers or omitted names")
	}
	if view.Methods[0].Code != "payuni_credit" || view.Methods[0].Version != 1 ||
		view.Methods[0].NameHans != "测试" || view.Methods[0].NameHant != "測試" || view.Methods[0].NameEN != "Mock" {
		t.Fatal("BPH01 method names/version do not match current merchant configuration")
	}
	after, afterOther := bpCounts(t, h)
	if after != before || afterOther != beforeOther {
		t.Fatal("BPH01 GET wrote payment, inventory, event, receipt or query job facts")
	}
	for _, target := range []struct{ token, store, order string }{
		{h.cap.Token, h.f.storeA1, randomUUID()},
		{mustIssue(t, h.cqHarness.service, h.f.storeA1).Token, h.f.storeA1, h.hold.OrderID},
	} {
		if _, err := h.api.(*checkout.HostedPaymentStarter).PaymentView(context.Background(), target.token, target.store, target.order); !errors.Is(err, command.ErrNotFound) {
			t.Fatalf("BPH01 missing/foreign order did not map to not found: %v", err)
		}
	}
	if _, err := h.api.(*checkout.HostedPaymentStarter).PaymentView(context.Background(), h.cap.Token, randomUUID(), h.hold.OrderID); err == nil {
		t.Fatal("BPH01 cross-store view succeeded")
	}
	if _, err := h.api.(*checkout.HostedPaymentStarter).PaymentView(context.Background(), randomToken(), h.f.storeA1, h.hold.OrderID); err == nil {
		t.Fatal("BPH01 forged capability read payment view")
	}
}

func TestBuyerPaymentViewCandidateDrift(t *testing.T) {
	for name, change := range map[string]func(*testing.T, hpHarness){
		"market-inactive": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE pricing.markets SET active=false,version=version+1 WHERE id=(SELECT market_id FROM checkout.orders WHERE id=$1)`, h.hold.OrderID)
		},
		"method-disabled": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE payments.method_versions SET enabled=false WHERE connection_id=$1`, h.account)
		},
		"binding-disabled": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE integration.bindings SET enabled=false WHERE id=$1`, h.binding)
		},
		"binding-version": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE integration.bindings SET semantic_version=semantic_version+1 WHERE id=$1`, h.binding)
		},
		"method-hidden": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE payments.method_versions SET visible=false WHERE connection_id=$1`, h.account)
		},
		"credential-rotated": hpRotateFixtureHead,
		"qualification-revoked": func(t *testing.T, h hpHarness) {
			qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, h.proof)
		},
		"qualification-expired": func(t *testing.T, h hpHarness) {
			qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET expires_at=clock_timestamp() WHERE id=$1`, h.proof)
		},
		"qualification-future": func(t *testing.T, h hpHarness) {
			qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET observed_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, h.proof)
		},
		"hold-expired": func(t *testing.T, h hpHarness) { bcDue(t, h.bcHarness, h.hold) },
		"hold-generation": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE inventory.reservations SET generation=generation+1 WHERE id=$1`, h.hold.OrderID)
		},
		"below-config-min": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE checkout.orders SET total_minor=0 WHERE id=$1`, h.hold.OrderID)
		},
		"above-config-max": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE checkout.orders SET total_minor=20000000 WHERE id=$1`, h.hold.OrderID)
		},
		"fractional-twd": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE checkout.orders SET total_minor=2501 WHERE id=$1`, h.hold.OrderID)
		},
		"below-method-min": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE payments.method_versions SET min_amount_minor=3000 WHERE connection_id=$1`, h.account)
		},
		"above-method-max": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE payments.method_versions SET max_amount_minor=1000 WHERE connection_id=$1`, h.account)
		},
		"method-proof-swapped": func(t *testing.T, h hpHarness) {
			other := randomUUID()
			mustExec(t, h.f.owner, `INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at,revoked_at)
			 SELECT $2,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,'synthetic rejected proof',observed_at,expires_at,clock_timestamp()
			 FROM payments.account_qualifications WHERE id=$1`, h.proof, other)
			mustExec(t, h.f.owner, `UPDATE payments.method_versions SET qualification_id=$2 WHERE connection_id=$1`, h.account, other)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := hpSetup(t)
			change(t, h)
			before, beforeOther := bpCounts(t, h)
			view, raw := bpView(t, h.api.(*checkout.HostedPaymentStarter), h.cap.Token, h.f.storeA1, h.hold.OrderID)
			if view.PaymentState != "NOT_STARTED" || len(view.Methods) != 0 || string(raw["methods"]) != "[]" {
				t.Fatal("BPH01 ineligible method was shown as a payment option")
			}
			after, afterOther := bpCounts(t, h)
			if after != before || afterOther != beforeOther {
				t.Fatal("BPH01 candidate read changed financial facts")
			}
		})
	}
	t.Run("current-method-head", func(t *testing.T) {
		h := hpSetup(t)
		mustExec(t, h.f.owner, `INSERT INTO payments.method_versions
		 (tenant_id,store_id,market_id,country,code,version,provider,environment,connection_id,binding_version,currency,name_hans,name_hant,name_en,enabled,visible,sort_order,min_amount_minor,max_amount_minor,principal_id,qualification_id)
		 SELECT tenant_id,store_id,market_id,country,code,2,provider,environment,connection_id,binding_version,currency,name_hans,name_hant,'Current Head',enabled,visible,sort_order,min_amount_minor,max_amount_minor,principal_id,qualification_id
		 FROM payments.method_versions WHERE connection_id=$1 AND version=1`, h.account)
		mustExec(t, h.f.owner, `UPDATE payments.method_heads SET current_version=2 WHERE tenant_id=$1 AND store_id=$2 AND code='payuni_credit'`, h.f.tenantA, h.f.storeA1)
		view, _ := bpView(t, h.api.(*checkout.HostedPaymentStarter), h.cap.Token, h.f.storeA1, h.hold.OrderID)
		if len(view.Methods) != 1 || view.Methods[0].Version != 2 || view.Methods[0].NameEN != "Current Head" {
			t.Fatal("BPH01 projection used stale method version after head advanced")
		}
	})
	for _, trial := range []struct{ name, sql string }{
		{"binding-asset-fk", `UPDATE integration.bindings SET external_asset_id='SANDBOX:other' WHERE id=$1`},
		{"account-asset-fk", `UPDATE integration.merchant_accounts SET account_id='other' WHERE id=$1`},
		{"account-environment-fk", `UPDATE integration.merchant_accounts SET environment='LIVE' WHERE id=$1`},
		{"account-provider-check", `UPDATE integration.merchant_accounts SET provider='other' WHERE id=$1`},
	} {
		t.Run(trial.name, func(t *testing.T) {
			h := hpSetup(t)
			id := h.account
			if trial.name == "binding-asset-fk" {
				id = h.binding
			}
			_, err := h.f.owner.Exec(context.Background(), trial.sql, id)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || (pgErr.Code != "23503" && pgErr.Code != "23514") {
				t.Fatalf("BPH01 schema allowed disconnected account/binding target: %v", err)
			}
			view, _ := bpView(t, h.api.(*checkout.HostedPaymentStarter), h.cap.Token, h.f.storeA1, h.hold.OrderID)
			if len(view.Methods) != 1 {
				t.Fatal("BPH01 rejected schema drift unexpectedly removed current method")
			}
		})
	}
	t.Run("mock-not-live", func(t *testing.T) {
		h := hpSetup(t)
		view, _ := bpView(t, bpStarter(t, h, "LIVE", h.config), h.cap.Token, h.f.storeA1, h.hold.OrderID)
		if view.TestMode || len(view.Methods) != 0 {
			t.Fatal("BPH01 MOCK qualification entered LIVE method candidates")
		}
	})
}

func TestBuyerPaymentViewHandoffStatesAndProfileSwitch(t *testing.T) {
	h := hpSetup(t)
	if _, err := h.begin(t04Key("bp-view-begin")); err != nil {
		t.Fatal(err)
	}
	service := h.api.(*checkout.HostedPaymentStarter)
	before, beforeOther := bpCounts(t, h)
	prepared, raw := bpView(t, service, h.cap.Token, h.f.storeA1, h.hold.OrderID)
	if prepared.PaymentState != "PENDING" || prepared.HandoffState != "PREPARED" || prepared.HandoffExpiresAt == nil || len(prepared.Methods) != 0 || !prepared.TestMode || string(raw["handoff_expires_at"]) == "null" {
		t.Fatal("BPH02 pending/prepared projection wrong")
	}
	config := h.config
	config.NotifyURL = "https://checkout.example.test/payment/notify-other"
	mismatch, _ := bpView(t, bpStarter(t, h, "PROVIDER_MOCK", config), h.cap.Token, h.f.storeA1, h.hold.OrderID)
	if mismatch.HandoffState != "UNAVAILABLE" || mismatch.HandoffExpiresAt == nil {
		t.Fatal("BPH02 changed endpoint config could reuse page")
	}
	switched := bpStarter(t, h, "LIVE", h.config)
	historical, _ := bpView(t, switched, h.cap.Token, h.f.storeA1, h.hold.OrderID)
	if historical.PaymentState != "PENDING" || historical.HandoffState != "UNAVAILABLE" || !historical.TestMode || len(historical.Methods) != 0 {
		t.Fatal("BPH02 profile switch hid original test history or offered handoff")
	}
	if _, err := switched.TakeHosted(context.Background(), h.cap.Token, h.f.storeA1, h.hold.OrderID); err == nil {
		t.Fatal("BPH02 profile switch released historical form")
	}
	if _, err := switched.BeginHosted(context.Background(), h.cap.Token, h.f.storeA1, t04Key("bp-switch"), h.input); err == nil {
		t.Fatal("BPH02 profile switch started a second attempt")
	}
	if _, err := h.take(); err != nil {
		t.Fatal(err)
	}
	issued, _ := bpView(t, service, h.cap.Token, h.f.storeA1, h.hold.OrderID)
	if issued.HandoffState != "ISSUED" || issued.PaymentState != "PENDING" {
		t.Fatal("BPH02 redirect was misread as paid or page not issued")
	}
	after, afterOther := bpCounts(t, h)
	if after != before || afterOther != beforeOther {
		t.Fatal("BPH02 view or denied profile calls changed payment facts")
	}
	t.Run("expired-unissued", func(t *testing.T) {
		h := hpSetup(t)
		result, err := h.begin(t04Key("bp-expired"))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, h.f.owner, `UPDATE checkout.hosted_payment_pages SET prepared_at=clock_timestamp()-interval '60 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE attempt_id=$1`, result.AttemptID)
		view, _ := bpView(t, h.api.(*checkout.HostedPaymentStarter), h.cap.Token, h.f.storeA1, h.hold.OrderID)
		if view.HandoffState != "EXPIRED" || view.PaymentState != "PENDING" || view.HandoffExpiresAt == nil || !view.HandoffExpiresAt.Before(time.Now()) {
			t.Fatal("BPH02 elapsed local deadline changed pending financial truth")
		}
	})
}

func TestBuyerPaymentViewSignedFinancialPrecedence(t *testing.T) {
	for _, state := range []string{"PENDING", "AUTHORIZED", "CAPTURED", "REVIEW_REQUIRED"} {
		t.Run(state, func(t *testing.T) {
			q := pqSetup(t)
			pool, err := platform.OpenHostedPool(context.Background(), hpRole(t, q.f))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			config := checkout.HostedConfig{ReturnURL: "https://checkout.example.test/payment/return", NotifyURL: "https://checkout.example.test/payment/notify"}
			service := hpStarter(t, pool, "PROVIDER_MOCK", q.keys, config).(*checkout.HostedPaymentStarter)
			if state != "PENDING" {
				report := pcFull(q)
				switch state {
				case "AUTHORIZED":
					report["CloseStatus"] = "9"
					delete(report, "CloseAmountTWD")
				case "REVIEW_REQUIRED":
					report["CardRefundStatus"], report["CardRefundAmountTWD"] = "2", int64(1)
				}
				hash := pcRecord(t, q, report)
				if err := pcApply(q.worker, q.result.AttemptID, hash); err != nil {
					t.Fatal(err)
				}
			}
			h := hpHarness{psHarness: q.psHarness}
			before, beforeOther := bpCounts(t, h)
			view, raw := bpView(t, service, q.cap.Token, q.f.storeA1, q.hold.OrderID)
			if view.PaymentState != state || view.HandoffState != "NONE" || view.HandoffExpiresAt != nil || len(view.Methods) != 0 || !view.TestMode || string(raw["handoff_expires_at"]) != "null" {
				t.Fatal("BPH02 signed financial facts projected incorrectly")
			}
			after, afterOther := bpCounts(t, h)
			if after != before || afterOther != beforeOther {
				t.Fatal("BPH02 view wrote signed financial or inventory facts")
			}
			switched := hpStarter(t, pool, "LIVE", q.keys, config).(*checkout.HostedPaymentStarter)
			historical, _ := bpView(t, switched, q.cap.Token, q.f.storeA1, q.hold.OrderID)
			if historical.PaymentState != state || historical.HandoffState != "UNAVAILABLE" || !historical.TestMode || len(historical.Methods) != 0 {
				t.Fatal("BPH02 different profile could not read frozen historical facts safely")
			}
		})
	}
}
