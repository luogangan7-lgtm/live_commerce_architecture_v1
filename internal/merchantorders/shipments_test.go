package merchantorders

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

func sp(v string) *string { return &v }

func shippedInput() ShipmentInput {
	return ShipmentInput{ExpectedVersion: 0, Status: "SHIPPED", CarrierCode: sp("seven_eleven_cvs"), TrackingNumber: sp("0012345678")}
}

// MF01 validation rules (contract §3, §5.1 void-body rule). Pure: no database.
func TestNormalizeShipmentRules(t *testing.T) {
	for _, code := range carrierCodes {
		in := shippedInput()
		in.CarrierCode = sp(code)
		if code == "other" {
			in.CarrierName = sp("Local Courier")
		}
		if _, err := NormalizeShipment(in); err != nil {
			t.Fatalf("carrier %s rejected: %v", code, err)
		}
	}
	if len(carrierCodes) != 7 {
		t.Fatalf("carrier list drifted: %v", carrierCodes)
	}
	// Leading zeroes and the exact tracking bytes survive; surrounding space is trimmed.
	in := shippedInput()
	in.TrackingNumber = sp("  00 12-ab  ")
	out, err := NormalizeShipment(in)
	if err != nil || *out.TrackingNumber != "00 12-ab" {
		t.Fatalf("tracking: %v %v", out.TrackingNumber, err)
	}
	// NFC: decomposed é becomes the precomposed rune; the length limit is in characters.
	in = shippedInput()
	in.CarrierName = sp("Café Kurier")
	if out, err = NormalizeShipment(in); err != nil || *out.CarrierName != "Café Kurier" {
		t.Fatalf("carrier name NFC: %q %v", *out.CarrierName, err)
	}
	in.CarrierName = sp(strings.Repeat("順", 80))
	if _, err = NormalizeShipment(in); err != nil {
		t.Fatalf("80 characters must pass: %v", err)
	}
	in.CarrierName = sp(strings.Repeat("順", 81))
	if _, err = NormalizeShipment(in); !errors.Is(err, ErrInvalidCarrier) {
		t.Fatalf("81 characters: %v", err)
	}
	for name, mutate := range map[string]func(*ShipmentInput){
		"unknown carrier":       func(v *ShipmentInput) { v.CarrierCode = sp("dhl") },
		"missing carrier":       func(v *ShipmentInput) { v.CarrierCode = nil },
		"other without name":    func(v *ShipmentInput) { v.CarrierCode = sp("other") },
		"empty carrier name":    func(v *ShipmentInput) { v.CarrierName = sp("  ") },
		"control in name":       func(v *ShipmentInput) { v.CarrierName = sp("a\x00b") },
		"newline in name":       func(v *ShipmentInput) { v.CarrierName = sp("a\nb") },
		"missing tracking":      func(v *ShipmentInput) { v.TrackingNumber = nil },
		"empty tracking":        func(v *ShipmentInput) { v.TrackingNumber = sp(" ") },
		"tracking leading dash": func(v *ShipmentInput) { v.TrackingNumber = sp("-123") },
		"tracking unicode":      func(v *ShipmentInput) { v.TrackingNumber = sp("１２３４") },
		"tracking slash":        func(v *ShipmentInput) { v.TrackingNumber = sp("12/34") },
		"tracking 65 chars":     func(v *ShipmentInput) { v.TrackingNumber = sp(strings.Repeat("1", 65)) },
	} {
		v := shippedInput()
		mutate(&v)
		_, err := NormalizeShipment(v)
		if !errors.Is(err, ErrInvalidCarrier) && !errors.Is(err, ErrInvalidTracking) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := NormalizeShipment(ShipmentInput{ExpectedVersion: -1, Status: "SHIPPED"}); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("negative version: %v", err)
	}
	if _, err := NormalizeShipment(ShipmentInput{Status: "DELIVERED"}); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("status: %v", err)
	}
	// Note: 200 characters, no control characters, empty allowed, merchant-only.
	in = shippedInput()
	in.Note = sp(strings.Repeat("n", 200))
	if _, err = NormalizeShipment(in); err != nil {
		t.Fatalf("200-char note: %v", err)
	}
	for _, note := range []string{strings.Repeat("n", 201), "a\tb"} {
		in.Note = sp(note)
		if _, err = NormalizeShipment(in); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("note %q: %v", note, err)
		}
	}
	// Void-body rule: everything null except void_reason; SHIPPED forbids void_reason.
	void := ShipmentInput{ExpectedVersion: 1, Status: "VOIDED", VoidReason: sp("wrong_order")}
	if _, err = NormalizeShipment(void); err != nil {
		t.Fatalf("valid void: %v", err)
	}
	for name, mutate := range map[string]func(*ShipmentInput){
		"carrier":    func(v *ShipmentInput) { v.CarrierCode = sp("other") },
		"name":       func(v *ShipmentInput) { v.CarrierName = sp("x") },
		"tracking":   func(v *ShipmentInput) { v.TrackingNumber = sp("1") },
		"url":        func(v *ShipmentInput) { v.TrackingURL = sp("https://a.example.com") },
		"note":       func(v *ShipmentInput) { v.Note = sp("") },
		"no reason":  func(v *ShipmentInput) { v.VoidReason = nil },
		"bad reason": func(v *ShipmentInput) { v.VoidReason = sp("oops") },
	} {
		v := void
		mutate(&v)
		if _, err = NormalizeShipment(v); !errors.Is(err, ErrInvalidVoid) {
			t.Errorf("void %s: %v", name, err)
		}
	}
	in = shippedInput()
	in.VoidReason = sp("other")
	if _, err = NormalizeShipment(in); !errors.Is(err, ErrInvalidVoid) {
		t.Fatalf("shipped with void_reason: %v", err)
	}
	for _, reason := range voidReasons {
		v := void
		v.VoidReason = sp(reason)
		if _, err = NormalizeShipment(v); err != nil {
			t.Fatalf("reason %s: %v", reason, err)
		}
	}
}

func TestTrackingURLPolicy(t *testing.T) {
	if len(carrierTrackingTemplates) != 0 {
		t.Fatal("built-in tracking templates are empty in v1 (ruling M-2)")
	}
	for in, want := range map[string]string{
		"https://Track.Example.com/a?id=1&x=2": "https://track.example.com/a?id=1&x=2",
		"https://track.example.com":            "https://track.example.com",
		"HTTPS://track.example.com/%E4%B8%80":  "https://track.example.com/%E4%B8%80",
		"https://xn--fiq228c.example.com/x":    "https://xn--fiq228c.example.com/x",
		"https://a-b.c-d.example.co.uk/x":      "https://a-b.c-d.example.co.uk/x",
	} {
		got, err := canonicalTrackingURL(in)
		if err != nil || got != want {
			t.Errorf("%q => %q %v want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "http://track.example.com/x", "//track.example.com", "track.example.com", "ftp://track.example.com",
		"https://user@track.example.com/x", "https://user:pw@track.example.com/x", "https://track.example.com:8443/x",
		"https://track.example.com:443/x", "https://localhost/x", "https://track/x", "https://.example.com/x",
		"https://track.example.com./x", "https://a..example.com/x", "https://track.example.com/x#frag", "https://track.example.com/x#",
		"https://track.example.com/ x", "https://track.example.com/\tx", "https://track.example.com/x\n", "https://track.example.com/\x7f",
		"https://例え.example.com/x", "https://track.example.com/例", "https://[::1]/x", "javascript:alert(1)",
		"https://track.example.com/" + strings.Repeat("a", 500), "https:///x", "https://?x", "mailto:a@example.com",
		// S6: hosts WHATWG new URL() rejects (or reads as an IPv4 literal) must not be stored, or the
		// admin/storefront parsers fail the whole order view. Go's url.Parse accepts all of these.
		"https://a<b.example.com/x", "https://a>b.example.com/x", "https://a\"b.example.com/x", "https://a^b.example.com/x",
		"https://300.300.300.300/x", "https://1.2.3.4/x", "https://a.example.1/x", "https://a.0xab/x", "https://-a.example.com/x",
		"https://a-.example.com/x", "https://a_b.example.com/x", "https://a|b.example.com/x", "https://a%2eb.example.com/x",
	} {
		if got, err := canonicalTrackingURL(in); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("%q accepted as %q (%v)", in, got, err)
		}
	}
	// Exactly 512 bytes passes; 513 does not.
	base := "https://track.example.com/"
	if _, err := canonicalTrackingURL(base + strings.Repeat("a", 512-len(base))); err != nil {
		t.Fatalf("512 bytes: %v", err)
	}
	if _, err := canonicalTrackingURL(base + strings.Repeat("a", 513-len(base))); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("513 bytes: %v", err)
	}
	in := shippedInput()
	in.TrackingURL = sp("https://Track.Example.com/x")
	out, err := NormalizeShipment(in)
	if err != nil || *out.TrackingURL != "https://track.example.com/x" {
		t.Fatalf("normalized url: %v %v", out.TrackingURL, err)
	}
	in.TrackingURL = sp("http://track.example.com/x")
	if _, err = NormalizeShipment(in); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("http url: %v", err)
	}
}

func shipmentVersion(status string) map[string]any {
	v := map[string]any{"version": 1, "status": status, "carrier_code": "seven_eleven_cvs", "carrier_name": nil,
		"tracking_number": "0012345678", "tracking_url": nil, "recorded_at": "2026-09-25T05:06:07.123456Z",
		"note": nil, "void_reason": nil, "principal_id": scope.PrincipalID}
	if status == "VOIDED" {
		v["void_reason"] = "wrong_order"
	}
	return v
}

type recordTx struct {
	pgx.Tx
	calls  int
	args   []any
	result []byte
	err    error
}

func (t *recordTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	t.calls++
	if t.calls == 1 {
		t.args = args
		if !strings.Contains(query, "fulfillment.record_manual_shipment") && !strings.Contains(query, "read_manual_shipment_history") {
			panic("unexpected query " + query)
		}
		return fakeRow{func(dest ...any) error {
			if t.err != nil {
				return t.err
			}
			*dest[0].(*[]byte) = t.result
			return nil
		}}
	}
	return t.authRow(query)
}

// authRow answers a platform.RequirePermission fence for the request's own scope.
func (t *recordTx) authRow(query string) pgx.Row {
	if !strings.Contains(query, "resolve_access") {
		panic("unexpected authorization query")
	}
	return fakeRow{func(dest ...any) error {
		*dest[0].(*string), *dest[1].(*string), *dest[2].(*string), *dest[3].(*int64) = "ok", scope.TenantID, scope.PrincipalID, scope.Revision
		return nil
	}}
}

func TestRecordShipmentBindingsAndFence(t *testing.T) {
	tx := &recordTx{result: raw(shipmentVersion("SHIPPED"))}
	in := shippedInput()
	in.TrackingNumber = sp(" 0012345678 ")
	in.TrackingURL = sp("https://Track.example.com/x")
	got, err := RecordShipment(context.Background(), tx, scope, token, "key-00000001", orderID, in)
	if err != nil || got.Version != 1 || got.Status != "SHIPPED" || got.PrincipalID != scope.PrincipalID || tx.calls != 2 {
		t.Fatalf("record: %+v calls=%d err=%v", got, tx.calls, err)
	}
	hash := sha256.Sum256([]byte(token))
	if string(tx.args[0].([]byte)) != string(hash[:]) || tx.args[1] != scope.StoreID || tx.args[2] != orderID || tx.args[3] != "key-00000001" {
		t.Fatalf("identity args: %v", tx.args[:4])
	}
	// Canonical values reach SQL; the request hash covers the exact submitted body and the order.
	if *(tx.args[9].(*string)) != "0012345678" || *(tx.args[10].(*string)) != "https://track.example.com/x" || tx.args[5] != int64(0) || tx.args[6] != "SHIPPED" {
		t.Fatalf("canonical args: %v", tx.args[5:])
	}
	first := string(tx.args[4].([]byte))
	tx2 := &recordTx{result: raw(shipmentVersion("SHIPPED"))}
	other := in
	other.TrackingNumber = sp("0012345678")
	if _, err = RecordShipment(context.Background(), tx2, scope, token, "key-00000001", orderID, other); err != nil {
		t.Fatal(err)
	}
	if first == string(tx2.args[4].([]byte)) {
		t.Fatal("request hash ignored the submitted body")
	}
	tx3 := &recordTx{result: raw(shipmentVersion("SHIPPED"))}
	if _, err = RecordShipment(context.Background(), tx3, scope, token, "key-00000001", "44444444-4444-4444-8444-444444444445", in); err != nil {
		t.Fatal(err)
	}
	if first == string(tx3.args[4].([]byte)) {
		t.Fatal("request hash ignored the order id")
	}
	// Void replies are decoded with their status.
	void := &recordTx{result: raw(shipmentVersion("VOIDED"))}
	got, err = RecordShipment(context.Background(), void, scope, token, "key-00000002", orderID, ShipmentInput{ExpectedVersion: 1, Status: "VOIDED", VoidReason: sp("wrong_order")})
	if err != nil || got.Status != "VOIDED" || got.VoidReason == nil || void.args[7] != (*string)(nil) {
		t.Fatalf("void: %+v %v %v", got, err, void.args)
	}
	// Invalid input never reaches the database.
	for name, call := range map[string]func() error{
		"bad key": func() error {
			_, e := RecordShipment(context.Background(), &recordTx{}, scope, token, "short", orderID, in)
			return e
		},
		"bad order": func() error {
			_, e := RecordShipment(context.Background(), &recordTx{}, scope, token, "key-00000001", "x", in)
			return e
		},
		"bad body": func() error {
			b := in
			b.CarrierCode = sp("dhl")
			_, e := RecordShipment(context.Background(), &recordTx{}, scope, token, "key-00000001", orderID, b)
			return e
		},
		"nil tx": func() error {
			_, e := RecordShipment(context.Background(), nil, scope, token, "key-00000001", orderID, in)
			return e
		},
		"short token": func() error {
			_, e := RecordShipment(context.Background(), &recordTx{}, scope, "short", "key-00000001", orderID, in)
			return e
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A reply that disagrees with the request or is malformed is a projection failure, not data.
	for name, result := range map[string][]byte{
		"status mismatch":  raw(shipmentVersion("VOIDED")),
		"extra key":        raw(func() map[string]any { v := shipmentVersion("SHIPPED"); v["owner_id"] = orderID; return v }()),
		"bad principal":    raw(func() map[string]any { v := shipmentVersion("SHIPPED"); v["principal_id"] = "x"; return v }()),
		"not json":         []byte("nope"),
		"voided no reason": raw(func() map[string]any { v := shipmentVersion("SHIPPED"); v["void_reason"] = "other"; return v }()),
	} {
		if _, err := RecordShipment(context.Background(), &recordTx{result: result}, scope, token, "key-00000001", orderID, in); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestShipmentErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code, msg string
		want      error
	}{
		{"PT409", "version_changed", ErrVersionChanged}, {"PT409", "idempotency_conflict", command.ErrConflict}, {"23505", "duplicate key", command.ErrConflict},
		{"PT422", "not_shippable", ErrNotShippable}, {"PT422", "invalid_carrier", ErrInvalidCarrier},
		{"PT422", "invalid_tracking", ErrInvalidTracking}, {"PT422", "invalid_url", ErrInvalidURL},
		{"PT422", "void_requires_shipped", ErrVoidRequiresShipped}, {"PT422", "invalid_void", ErrInvalidVoid},
		{"PT422", "something else", command.ErrInvalid}, {"PT400", "x", command.ErrInvalid}, {"23514", "check", command.ErrInvalid},
		{"PT401", "x", platform.ErrUnauthorized}, {"PT403", "x", platform.ErrForbidden}, {"PT404", "x", platform.ErrScopeNotFound},
		{"40P01", "deadlock detail with customer value", ErrUnavailable}, {"XX000", "internal", ErrUnavailable},
	} {
		tx := &recordTx{err: &pgconn.PgError{Code: tc.code, Message: tc.msg}}
		_, err := RecordShipment(context.Background(), tx, scope, token, "key-00000001", orderID, shippedInput())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s/%s => %v want %v", tc.code, tc.msg, err, tc.want)
		}
	}
}

func TestShipmentHistoryDecoding(t *testing.T) {
	second := shipmentVersion("VOIDED")
	second["version"] = 2
	tx := &recordTx{result: raw([]any{shipmentVersion("SHIPPED"), second})}
	got, err := ShipmentHistory(context.Background(), tx, scope, token, orderID)
	if err != nil || len(got) != 2 || got[1].VoidReason == nil || tx.calls != 2 {
		t.Fatalf("history: %+v calls=%d err=%v", got, tx.calls, err)
	}
	empty, err := ShipmentHistory(context.Background(), &recordTx{result: []byte("[]")}, scope, token, orderID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty history must be []: %v %v", empty, err)
	}
	gap := shipmentVersion("SHIPPED")
	gap["version"] = 3
	for name, result := range map[string][]byte{"gap": raw([]any{gap}), "object": []byte("{}"), "junk": []byte("[1]")} {
		if _, err := ShipmentHistory(context.Background(), &recordTx{result: result}, scope, token, orderID); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ShipmentHistory(context.Background(), &recordTx{err: &pgconn.PgError{Code: "PT404"}}, scope, token, orderID); !errors.Is(err, platform.ErrScopeNotFound) {
		t.Fatalf("missing order: %v", err)
	}
	if _, err := ShipmentHistory(context.Background(), &recordTx{}, scope, token, "bad"); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("bad order id: %v", err)
	}
}

// Version JSON carries merchant-only fields exactly as the frozen ShipmentVersion shape.
func TestShipmentVersionJSONShape(t *testing.T) {
	v, err := decodeVersion(raw(shipmentVersion("SHIPPED")))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(v)
	var keys map[string]any
	_ = json.Unmarshal(out, &keys)
	for _, k := range append(append([]string{}, shipmentKeys...), "note", "void_reason", "principal_id") {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing %s in %s", k, out)
		}
	}
	if len(keys) != 10 {
		t.Fatalf("unexpected keys: %s", out)
	}
}

// actionsTx answers each RequirePermission probe from a per-permission status.
type actionsTx struct {
	pgx.Tx
	status map[string]string
	asked  []string
}

func (t *actionsTx) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	permission := args[2].(string)
	t.asked = append(t.asked, permission)
	return fakeRow{func(dest ...any) error {
		status := t.status[permission]
		*dest[0].(*string), *dest[1].(*string), *dest[2].(*string), *dest[3].(*int64) = status, "", "", 0
		if status == "ok" {
			*dest[1].(*string), *dest[2].(*string), *dest[3].(*int64) = scope.TenantID, scope.PrincipalID, scope.Revision
		}
		return nil
	}}
}

func TestActionsProbeIsPermissionExact(t *testing.T) {
	for name, tc := range map[string]struct {
		status map[string]string
		want   OrderActions
		err    error
	}{
		"all":            {map[string]string{"payments:refund": "ok", "fulfillment:write": "ok", "orders:export": "ok"}, OrderActions{true, true, true}, nil},
		"none":           {map[string]string{"payments:refund": "forbidden", "fulfillment:write": "forbidden", "orders:export": "forbidden"}, OrderActions{}, nil},
		"ship only":      {map[string]string{"payments:refund": "forbidden", "fulfillment:write": "ok", "orders:export": "forbidden"}, OrderActions{false, true, false}, nil},
		"export only":    {map[string]string{"payments:refund": "forbidden", "fulfillment:write": "forbidden", "orders:export": "ok"}, OrderActions{false, false, true}, nil},
		"revoked":        {map[string]string{"payments:refund": "unauthorized", "fulfillment:write": "unauthorized", "orders:export": "unauthorized"}, OrderActions{}, platform.ErrUnauthorized},
		"store vanished": {map[string]string{"payments:refund": "not_found", "fulfillment:write": "not_found", "orders:export": "not_found"}, OrderActions{}, platform.ErrScopeNotFound},
	} {
		tx := &actionsTx{status: tc.status}
		got, err := Actions(context.Background(), tx, scope, token)
		if got != tc.want || !errors.Is(err, tc.err) || (tc.err == nil && len(tx.asked) != 3) {
			t.Errorf("%s: %+v %v asked=%v", name, got, err, tx.asked)
		}
	}
	out, _ := json.Marshal(OrderActions{true, false, true})
	if string(out) != `{"refund":true,"fulfillment_write":false,"orders_export":true}` {
		t.Fatalf("JSON keys: %s", out)
	}
	if _, err := Actions(context.Background(), nil, scope, token); err == nil {
		t.Fatal("nil tx accepted")
	}
}
