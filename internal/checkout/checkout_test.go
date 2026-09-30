package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
)

const testID = "00000000-0000-0000-0000-000000000001"

func TestInputHasOnlyFrozenFields(t *testing.T) {
	in := Input{QuoteID: testID, DestinationID: testID, CartVersion: 1, ServiceVersion: 1, AllocationVersion: 1}
	if !validInput(in) {
		t.Fatal("valid input rejected")
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"quote_id":"00000000-0000-0000-0000-000000000001","destination_id":"00000000-0000-0000-0000-000000000001","cart_version":1,"service_version":1,"allocation_version":1}` {
		t.Fatalf("input digest shape drifted: %s", body)
	}
	for _, edit := range []func(*Input){
		func(v *Input) { v.QuoteID = "bad" },
		func(v *Input) { v.DestinationID = "bad" },
		func(v *Input) { v.CartVersion = 0 },
		func(v *Input) { v.ServiceVersion = 0 },
		func(v *Input) { v.AllocationVersion = 0 },
	} {
		invalid := in
		edit(&invalid)
		if validInput(invalid) {
			t.Fatal("invalid input accepted")
		}
	}
}

func TestSQLFailuresAreBounded(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{
		{"PT400", command.ErrInvalid},
		{"PT401", buyer.ErrUnauthorized},
		{"PT402", command.ErrInsufficient},
		{"PT409", command.ErrConflict},
		{"23505", errCheckoutDatabase},
	} {
		got := safeError(context.Background(), &pgconn.PgError{Code: tc.code, Message: "contains private SQL detail"})
		if !errors.Is(got, tc.want) || strings.Contains(got.Error(), "private SQL") {
			t.Fatalf("SQLSTATE %s mapped to %v", tc.code, got)
		}
	}
}

func TestExpiryJobContainsOnlyDurableIdentity(t *testing.T) {
	args := expiryArgs{OrderID: testID, Generation: 1, Version: 1}
	if args.Kind() != "checkout_expiry_v1" {
		t.Fatal("expiry kind drifted")
	}
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"order_id":"00000000-0000-0000-0000-000000000001","generation":1,"version":1}` {
		t.Fatalf("private expiry args drifted: %s", body)
	}
}

func TestExpiryWorkerRejectsInvalidInvocation(t *testing.T) {
	if _, err := NewExpiryWorker(nil, nil); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := NewExpiryWorker(context.Background(), nil); err == nil {
		t.Fatal("nil worker pool accepted")
	}
	if err := (&ExpiryWorker{}).Work(context.Background(), nil); !errors.Is(err, errInvalidExpiryJob) {
		t.Fatalf("invalid job result: %v", err)
	}
}

// manual-fulfilment-v1 §5.2: the buyer order always carries "shipment" (null unless SHIPPED) and the
// shipment object has exactly the buyer-safe keys: no note, void_reason, principal or version.
func TestOrderShipmentShapeIsBuyerSafe(t *testing.T) {
	raw, err := json.Marshal(Order{})
	if err != nil || !strings.Contains(string(raw), `"shipment":null`) {
		t.Fatalf("null shipment not emitted: %s %v", raw, err)
	}
	name := "Local Courier"
	raw, err = json.Marshal(BuyerShipment{Status: "SHIPPED", CarrierCode: "other", CarrierName: &name, TrackingNumber: "0012345678"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err = json.Unmarshal(raw, &keys); err != nil || len(keys) != 6 {
		t.Fatalf("buyer shipment keys: %s", raw)
	}
	for _, key := range []string{"status", "carrier_code", "carrier_name", "tracking_number", "tracking_url", "recorded_at"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("missing %s in %s", key, raw)
		}
	}
	for _, forbidden := range []string{"note", "void_reason", "principal_id", "version"} {
		if _, ok := keys[forbidden]; ok {
			t.Errorf("merchant-only key %s reached the buyer shape", forbidden)
		}
	}
}
