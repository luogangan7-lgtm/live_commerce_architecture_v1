// Package merchantorders owns the private merchant order projection (identity.read_merchant_orders:
// payment, refund-amount and shipment fields), the merchant-arranged manual shipment command and history
// (manual-fulfilment-v1) and the unshipped-orders CSV export. Checkout remains the only transaction and
// payment state owner. refunds.go (refund-core) shares this package's decoders.
//
// Its order projections also carry the taiwan-cvs-logistics-v1 fields (pickup_source, payment_mode,
// collection_state, PROVIDER_LABEL_CREATED) read-only from identity.read_merchant_orders.
//
// It never writes ledger, stock, reservation, payment-fact or refund state, never creates a provider
// operation or River job, never calls a carrier or fetches a tracking URL, and never stores or logs an
// export file or recipient data (ECPay shipments live in internal/fulfillment).
package merchantorders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/internal/pricing"
	"livecommerce/internal/storefront"
)

const timestampLayout = "2006-01-02T15:04:05.000000Z"

var ErrUnavailable = errors.New("merchant order read unavailable")
var serviceCode = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
var skuCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
var pickupNamespace = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)
var pickupCode = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

type ListRequest struct {
	Page  pagination.Request
	State string
}

type Summary struct {
	OrderID          string `json:"order_id"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	Currency         string `json:"currency"`
	TotalMinor       int64  `json:"total_minor"`
	CommercialState  string `json:"commercial_state"`
	FulfillmentState string `json:"fulfillment_state"`
	PaymentState     string `json:"payment_state"`
	TestMode         bool   `json:"test_mode"`
	WorkState        string `json:"work_state"`
	// RefundedMinor / RefundPendingMinor come from identity.read_merchant_orders (0063, stripe-refund-v1
	// §7.1): succeeded-and-not-reversed vs held-but-not-succeeded refund amounts of the order's attempt.
	RefundedMinor      int64 `json:"refunded_minor"`
	RefundPendingMinor int64 `json:"refund_pending_minor"`
	// C4 (taiwan-cvs-logistics-v1 §16.1/§16.2): how the pickup store was obtained (ecpay_directory | buyer_entered |
	// merchant_attested; null for home delivery), how the order is paid, and the pay-at-pickup collection state (null for
	// card orders). Read-only projections of identity.read_merchant_orders (migrations/0073).
	PickupSource    *string `json:"pickup_source"`
	PaymentMode     string  `json:"payment_mode"`
	CollectionState *string `json:"collection_state"`
}

type Item struct {
	SKUID          string     `json:"sku_id"`
	Code           string     `json:"code"`
	Name           string     `json:"name"`
	Quantity       int64      `json:"quantity"`
	UnitPriceMinor int64      `json:"unit_price_minor"`
	Amount         LineAmount `json:"amount"`
}

type LineAmount = pricing.LineAmount

type Totals struct {
	SubtotalMinor    int64 `json:"subtotal_minor"`
	DiscountMinor    int64 `json:"discount_minor"`
	ShippingMinor    int64 `json:"shipping_minor"`
	ShippingTaxMinor int64 `json:"shipping_tax_minor"`
	TaxMinor         int64 `json:"tax_minor"`
	TotalMinor       int64 `json:"total_minor"`
}

type HomeAddress = storefront.HomeAddress

type Pickup struct {
	Kind             string `json:"kind"`
	Namespace        string `json:"namespace"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	Address          string `json:"address"`
	VerificationKind string `json:"verification_kind"`
}

type Destination struct {
	Kind          string      `json:"kind"`
	Country       string      `json:"country"`
	RecipientName string      `json:"recipient_name"`
	Phone         string      `json:"phone"`
	HomeAddress   HomeAddress `json:"home_address"`
	Pickup        *Pickup     `json:"pickup"`
}

type Detail struct {
	Summary
	Country     string      `json:"country"`
	ServiceCode string      `json:"service_code"`
	Items       []Item      `json:"items"`
	Totals      Totals      `json:"totals"`
	Destination Destination `json:"destination"`
	// Shipment is the current SHIPPED head (manual-fulfilment-v1 §4.1); null otherwise.
	Shipment *Shipment `json:"shipment"`
}

func List(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string, in ListRequest) (pagination.Page[Summary], error) {
	empty := pagination.Page[Summary]{Items: []Summary{}}
	if tx == nil || !validState(in.State) || !validAuthorityInput(scope, token) {
		return empty, command.ErrInvalid
	}
	if in.State == "" {
		in.State = "all"
	}
	binding := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "merchant-orders", Filter: in.State}
	limit, keys, err := pagination.Decode(in.Page, binding, 2)
	if err != nil {
		return empty, err
	}
	var afterTime, afterID any
	if len(keys) == 2 {
		afterTime, afterID = keys[0], keys[1]
	}
	raw, err := read(ctx, tx, scope, token, nil, limit+1, afterTime, afterID, in.State)
	if err != nil {
		return empty, err
	}
	if len(raw) > 128<<10 {
		return empty, ErrUnavailable
	}
	objects, err := decodeArray(raw, limit+1)
	if err != nil {
		return empty, err
	}
	for _, object := range objects {
		item, err := decodeSummary(object)
		if err != nil {
			return empty, err
		}
		if !matchesState(in.State, item) {
			return empty, ErrUnavailable
		}
		empty.Items = append(empty.Items, item)
	}
	if len(empty.Items) > limit {
		empty.Items = empty.Items[:limit]
		last := empty.Items[len(empty.Items)-1]
		empty.NextCursor, err = pagination.Encode(binding, []string{last.CreatedAt, last.OrderID})
		if err != nil {
			return pagination.Page[Summary]{}, ErrUnavailable
		}
	}
	return empty, nil
}

func Get(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, orderID string) (Detail, error) {
	if tx == nil || !validAuthorityInput(scope, token) || !command.ValidID(orderID) {
		return Detail{}, command.ErrInvalid
	}
	raw, err := read(ctx, tx, scope, token, orderID, 1, nil, nil, "all")
	if err != nil {
		return Detail{}, err
	}
	if len(raw) > 256<<10 {
		return Detail{}, ErrUnavailable
	}
	objects, err := decodeArray(raw, 1)
	if err != nil {
		return Detail{}, err
	}
	if len(objects) == 0 {
		return Detail{}, command.ErrNotFound
	}
	item, err := decodeDetail(objects[0])
	if err != nil || item.OrderID != orderID {
		return Detail{}, ErrUnavailable
	}
	return item, nil
}

func read(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string, orderID any, limit int, afterTime, afterID any, state string) ([]byte, error) {
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT identity.read_merchant_orders($1,$2::uuid,$3::uuid,$4,$5::timestamptz,$6::uuid,$7)`,
		hash[:], scope.StoreID, orderID, limit, afterTime, afterID, state).Scan(&raw)
	if err != nil {
		return nil, mapError(err)
	}
	// The SQL function has its own fresh post-query authority fence. Preserve the
	// original Go Scope as a second fence before decoding or returning data.
	if err := platform.RequirePermission(ctx, tx, scope, token, "orders:read"); err != nil {
		return nil, mapError(err)
	}
	if len(raw) == 0 {
		return nil, ErrUnavailable
	}
	return raw, nil
}

func mapError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "PT401":
			return platform.ErrUnauthorized
		case "PT403":
			return platform.ErrForbidden
		case "PT404":
			return platform.ErrScopeNotFound
		}
	}
	if errors.Is(err, platform.ErrUnauthorized) || errors.Is(err, platform.ErrForbidden) || errors.Is(err, platform.ErrScopeNotFound) {
		return err
	}
	return ErrUnavailable
}

func validAuthorityInput(scope platform.Scope, token string) bool {
	return command.ValidID(scope.TenantID) && command.ValidID(scope.StoreID) && command.ValidID(scope.PrincipalID) &&
		scope.Revision > 0 && len(token) >= 32 && len(token) <= 512
}

func validState(state string) bool {
	switch state {
	case "", "all", "DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED", "shipped", "unshipped", "cvs_pending":
		return true
	}
	return false
}

// matchesState re-checks the SQL filter on every decoded row (a wrong-filter page is a projection
// failure, not data). "unshipped" is the MD6 subset observable in a summary; the SQL adds the review
// and refund clauses.
func matchesState(state string, v Summary) bool {
	switch state {
	case "", "all":
		return true
	case "shipped":
		return v.FulfillmentState == "MERCHANT_SHIPPED"
	case "unshipped":
		// Card orders carry a READY work item; a pay_at_pickup order never has a payment attempt (work NONE).
		return v.CommercialState == "CONFIRMED" && v.FulfillmentState == "MANUAL_UNASSIGNED" &&
			(v.WorkState == "READY" || (v.PaymentMode == "pay_at_pickup" && v.WorkState == "NONE"))
	case "cvs_pending":
		// C4: the forwarder's daily drop list (the SQL adds "current attempt CREATED").
		return v.FulfillmentState == "PROVIDER_LABEL_CREATED"
	}
	return v.CommercialState == state
}

func decodeArray(raw []byte, max int) ([]json.RawMessage, error) {
	var objects []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &objects) != nil || len(objects) > max {
		return nil, ErrUnavailable
	}
	return objects, nil
}

var summaryKeys = []string{"order_id", "created_at", "updated_at", "currency", "total_minor", "commercial_state", "fulfillment_state", "payment_state", "test_mode", "work_state", "refunded_minor", "refund_pending_minor", "pickup_source", "payment_mode", "collection_state"}

// summaryNullable are the summary keys whose value may be an explicit JSON null (the key itself is still required).
var summaryNullable = []string{"pickup_source", "collection_state"}

func decodeSummary(raw json.RawMessage) (Summary, error) {
	if _, err := exactNullable(raw, summaryNullable, summaryKeys...); err != nil {
		return Summary{}, err
	}
	var out Summary
	if json.Unmarshal(raw, &out) != nil || !validSummary(out) {
		return Summary{}, ErrUnavailable
	}
	return out, nil
}

func validSummary(v Summary) bool {
	created, e1 := canonicalTime(v.CreatedAt)
	updated, e2 := canonicalTime(v.UpdatedAt)
	if !command.ValidID(v.OrderID) || e1 != nil || e2 != nil || updated.Before(created) || !currency(v.Currency) || !money(v.TotalMinor) {
		return false
	}
	switch v.CommercialState {
	case "DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED":
	default:
		return false
	}
	switch v.FulfillmentState {
	case "MANUAL_UNASSIGNED", "CANCELLED", "PAID_ALLOCATION_FAILED", "MERCHANT_SHIPPED", "PROVIDER_LABEL_CREATED":
	default:
		return false
	}
	if !validPickupSource(v.PickupSource) || !validCollection(v) {
		return false
	}
	switch v.PaymentState {
	case "NOT_STARTED", "REVIEW_REQUIRED", "CAPTURED", "AUTHORIZED", "PENDING", "PARTIALLY_REFUNDED", "REFUNDED":
	default:
		return false
	}
	switch v.WorkState {
	case "NONE", "READY", "REVIEW_REQUIRED":
	default:
		return false
	}
	if !money(v.RefundedMinor) || !money(v.RefundPendingMinor) || v.RefundedMinor+v.RefundPendingMinor > v.TotalMinor {
		return false
	}
	// I05: refund amounts and payment_state come from the same SQL facts, so they must agree.
	switch v.PaymentState {
	case "NOT_STARTED", "AUTHORIZED", "PENDING":
		if v.RefundedMinor != 0 || v.RefundPendingMinor != 0 {
			return false
		}
	case "CAPTURED":
		if v.RefundedMinor != 0 {
			return false
		}
	case "PARTIALLY_REFUNDED":
		if v.RefundedMinor == 0 || v.RefundedMinor >= v.TotalMinor {
			return false
		}
	case "REFUNDED":
		if v.RefundedMinor != v.TotalMinor {
			return false
		}
	}
	if v.PaymentState == "NOT_STARTED" && (v.TestMode || v.WorkState != "NONE") {
		return false
	}
	if v.FulfillmentState == "CANCELLED" && v.CommercialState != "CANCELLED" {
		return false
	}
	if v.FulfillmentState == "PAID_ALLOCATION_FAILED" && (v.PaymentState != "REVIEW_REQUIRED" || v.WorkState != "REVIEW_REQUIRED") {
		return false
	}
	// stripe-refund-v1 §7.1 / manual-fulfilment-v1 §5.1 invariants.
	if v.WorkState == "READY" && (v.CommercialState != "CONFIRMED" || !captured(v.PaymentState) ||
		(v.FulfillmentState != "MANUAL_UNASSIGNED" && v.FulfillmentState != "MERCHANT_SHIPPED" && v.FulfillmentState != "PROVIDER_LABEL_CREATED")) {
		return false
	}
	if v.PaymentMode == "pay_at_pickup" {
		// §16.2: never a payment attempt, fact, refund or work item; CONFIRMED at placement, CANCELLED after a release.
		return v.PaymentState == "NOT_STARTED" && v.WorkState == "NONE" && v.RefundedMinor == 0 && v.RefundPendingMinor == 0 &&
			!v.TestMode && (v.CommercialState == "CONFIRMED" || (v.CommercialState == "CANCELLED" && v.FulfillmentState == "CANCELLED")) &&
			v.FulfillmentState != "PAID_ALLOCATION_FAILED"
	}
	if (v.FulfillmentState == "MERCHANT_SHIPPED" || v.FulfillmentState == "PROVIDER_LABEL_CREATED") &&
		(v.WorkState != "READY" || v.CommercialState != "CONFIRMED") {
		return false
	}
	if v.WorkState == "REVIEW_REQUIRED" && v.PaymentState != "REVIEW_REQUIRED" {
		return false
	}
	return v.CommercialState != "CONFIRMED" || (v.WorkState != "NONE" && captured(v.PaymentState))
}

// validPickupSource admits the three §16.1 labels or null (home delivery).
func validPickupSource(s *string) bool {
	if s == nil {
		return true
	}
	return *s == "ecpay_directory" || *s == "buyer_entered" || *s == "merchant_attested"
}

// validCollection: payment_mode card <=> collection_state null (checkout.orders orders_payment_collection CHECK); the
// pay_at_pickup states are the §16.4/§16.8 set.
func validCollection(v Summary) bool {
	switch v.PaymentMode {
	case "card":
		return v.CollectionState == nil
	case "pay_at_pickup":
		if v.CollectionState == nil {
			return false
		}
		switch *v.CollectionState {
		case "PENDING", "COLLECTED", "RETURNED", "REFUNDED_OFFLINE", "CANCELLED", "RESTOCKED":
			return true
		}
	}
	return false
}

// captured is the payment_state set in which a capture fact exists (or its review supersedes it).
func captured(state string) bool {
	switch state {
	case "CAPTURED", "PARTIALLY_REFUNDED", "REFUNDED", "REVIEW_REQUIRED":
		return true
	}
	return false
}

func decodeDetail(raw json.RawMessage) (Detail, error) {
	fields, err := exactNullable(raw, append([]string{"shipment"}, summaryNullable...), append(append([]string{}, summaryKeys...), "country", "service_code", "items", "totals", "destination", "shipment")...)
	if err != nil {
		return Detail{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(fields["shipment"]), []byte("null")) {
		if _, err = exactNullable(fields["shipment"], []string{"carrier_name", "tracking_url"}, shipmentKeys...); err != nil {
			return Detail{}, err
		}
	}
	if _, err = exact(fields["totals"], "subtotal_minor", "discount_minor", "shipping_minor", "shipping_tax_minor", "tax_minor", "total_minor"); err != nil {
		return Detail{}, err
	}
	destination, err := exact(fields["destination"], "kind", "country", "recipient_name", "phone", "home_address", "pickup")
	if err != nil {
		return Detail{}, err
	}
	if _, err = exact(destination["home_address"], "region", "city", "postal_code", "line1", "line2"); err != nil {
		return Detail{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(destination["pickup"]), []byte("null")) {
		if _, err = exact(destination["pickup"], "kind", "namespace", "code", "name", "address", "verification_kind"); err != nil {
			return Detail{}, err
		}
	}
	var itemFields []json.RawMessage
	if json.Unmarshal(fields["items"], &itemFields) != nil || len(itemFields) == 0 || len(itemFields) > 50 {
		return Detail{}, ErrUnavailable
	}
	for _, item := range itemFields {
		values, err := exact(item, "sku_id", "code", "name", "quantity", "unit_price_minor", "amount")
		if err != nil {
			return Detail{}, err
		}
		if _, err = exact(values["amount"], "subtotal_minor", "discount_minor", "tax_minor", "total_minor"); err != nil {
			return Detail{}, err
		}
	}
	var out Detail
	if json.Unmarshal(raw, &out) != nil || !validSummary(out.Summary) || !validDetail(out) {
		return Detail{}, ErrUnavailable
	}
	return out, nil
}

// validShipment holds the head-shipment invariants: a shipment is shown exactly for a
// MERCHANT_SHIPPED order and only while its head is SHIPPED (a voided head is null).
func validShipment(order Summary, s *Shipment) bool {
	if s == nil {
		return order.FulfillmentState != "MERCHANT_SHIPPED"
	}
	return order.FulfillmentState == "MERCHANT_SHIPPED" && s.Status == "SHIPPED" && validShipmentFields(*s)
}

// validPickupVerification admits the three verification kinds of migrations/0072 (§4.1, §16.1).
func validPickupVerification(kind string) bool {
	return kind == "MANUAL_ATTESTED" || kind == "PROVIDER_DIRECTORY_VERIFIED" || kind == "BUYER_ENTERED"
}

// pickupMatchesSource keeps the projection's pickup_source label and the frozen snapshot's verification kind in step.
func pickupMatchesSource(kind string, source *string) bool {
	if source == nil {
		return false
	}
	return (kind == "MANUAL_ATTESTED" && *source == "merchant_attested") ||
		(kind == "PROVIDER_DIRECTORY_VERIFIED" && *source == "ecpay_directory") ||
		(kind == "BUYER_ENTERED" && *source == "buyer_entered")
}

func validDetail(v Detail) bool {
	if !validShipment(v.Summary, v.Shipment) {
		return false
	}
	if !country(v.Country) || !serviceCode.MatchString(v.ServiceCode) || v.Destination.Country != v.Country ||
		!money(v.Totals.SubtotalMinor) || !money(v.Totals.DiscountMinor) || !money(v.Totals.ShippingMinor) ||
		!money(v.Totals.ShippingTaxMinor) || !money(v.Totals.TaxMinor) || v.Totals.TotalMinor != v.TotalMinor {
		return false
	}
	d := v.Destination
	if !country(d.Country) || !textValue(d.RecipientName, 120, true) || !validPhone(d.Phone) ||
		!textValue(d.HomeAddress.Region, 100, false) || !textValue(d.HomeAddress.City, 100, false) ||
		!textValue(d.HomeAddress.PostalCode, 20, false) || !textValue(d.HomeAddress.Line1, 200, false) || !textValue(d.HomeAddress.Line2, 200, false) {
		return false
	}
	if d.Kind == "home" {
		if d.Pickup != nil || d.HomeAddress.City == "" || d.HomeAddress.Line1 == "" {
			return false
		}
	} else if d.Kind == "cvs_711" || d.Kind == "cvs_familymart" || d.Kind == "cvs_hilife" || d.Kind == "cvs_okmart" {
		if d.Country != "TW" || d.Pickup == nil || d.Pickup.Kind != d.Kind || d.HomeAddress != (HomeAddress{}) ||
			!pickupNamespace.MatchString(d.Pickup.Namespace) || !pickupCode.MatchString(d.Pickup.Code) ||
			!textValue(d.Pickup.Name, 120, true) || !textValue(d.Pickup.Address, 400, true) ||
			!validPickupVerification(d.Pickup.VerificationKind) || !pickupMatchesSource(d.Pickup.VerificationKind, v.PickupSource) {
			return false
		}
	} else {
		return false
	}
	var subtotal, discount, tax int64
	inclusive, exclusive := true, true
	for _, item := range v.Items {
		base, err := command.CheckMoney(item.UnitPriceMinor, item.Quantity)
		if err != nil || !command.ValidID(item.SKUID) || !skuCode.MatchString(item.Code) || !textValue(item.Name, 120, true) ||
			item.Amount.SubtotalMinor != base || !money(item.Amount.DiscountMinor) || !money(item.Amount.TaxMinor) ||
			!money(item.Amount.TotalMinor) || item.Amount.DiscountMinor > base {
			return false
		}
		lineBase := base - item.Amount.DiscountMinor
		inclusive = inclusive && item.Amount.TotalMinor == lineBase
		exclusive = exclusive && item.Amount.TotalMinor == lineBase+item.Amount.TaxMinor
		if subtotal, err = add(subtotal, base); err != nil {
			return false
		}
		if discount, err = add(discount, item.Amount.DiscountMinor); err != nil {
			return false
		}
		if tax, err = add(tax, item.Amount.TaxMinor); err != nil {
			return false
		}
	}
	var err error
	if tax, err = add(tax, v.Totals.ShippingTaxMinor); err != nil {
		return false
	}
	if subtotal != v.Totals.SubtotalMinor || discount != v.Totals.DiscountMinor || tax != v.Totals.TaxMinor {
		return false
	}
	base, err := add(subtotal-discount, v.Totals.ShippingMinor)
	if err != nil {
		return false
	}
	return (inclusive && v.TotalMinor == base) || (exclusive && tax <= command.MaxMoney-base && v.TotalMinor == base+tax)
}

func exact(raw json.RawMessage, names ...string) (map[string]json.RawMessage, error) {
	return exactNullable(raw, []string{"pickup"}, names...)
}

// exactNullable is exact with an explicit set of keys whose value may be JSON null (the key itself
// must still be present). Every other key must be non-null.
func exactNullable(raw json.RawMessage, nullable []string, names ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, &fields) != nil || len(fields) != len(names) {
		return nil, ErrUnavailable
	}
	for _, name := range names {
		value, ok := fields[name]
		if !ok || len(value) == 0 || (!slices.Contains(nullable, name) && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return nil, ErrUnavailable
		}
	}
	return fields, nil
}

func canonicalTime(value string) (time.Time, error) {
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil || parsed.Format(timestampLayout) != value {
		return time.Time{}, ErrUnavailable
	}
	return parsed, nil
}

func money(value int64) bool { return value >= 0 && value <= command.MaxMoney }
func add(a, b int64) (int64, error) {
	if !money(a) || !money(b) || a > command.MaxMoney-b {
		return 0, ErrUnavailable
	}
	return a + b, nil
}
func currency(value string) bool {
	return len(value) == 3 && value[0] >= 'A' && value[0] <= 'Z' && value[1] >= 'A' && value[1] <= 'Z' && value[2] >= 'A' && value[2] <= 'Z'
}
func country(value string) bool {
	return len(value) == 2 && value[0] >= 'A' && value[0] <= 'Z' && value[1] >= 'A' && value[1] <= 'Z'
}
func textValue(value string, max int, required bool) bool {
	if (required && value == "") || !utf8.ValidString(value) || utf8.RuneCountInString(value) > max {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validPhone(value string) bool {
	if len(value) < 6 || len(value) > 32 || value != strings.TrimSpace(value) {
		return false
	}
	digits := 0
	for _, c := range value {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '+' || c == '(' || c == ')' || c == ' ' || c == '-':
		default:
			return false
		}
	}
	return digits >= 6 && digits <= 20
}
