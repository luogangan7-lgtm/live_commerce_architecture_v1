// types.go holds the FROZEN customers interface types (docs/delivery/units/customers-core.md), the sentinel
// errors, and the pure logic that needs no database: CD4 current-consent derivation, query and key
// validation, the customer-id keyed idempotency mapping, strict result decoding and the export size cap.
//
// Non-goals: no SQL and no HTTP here (read.go, privacy.go, internal/httpapi and internal/buyerhttp own those);
// no authority decision (the 0078 definers decide it).

package customers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/pagination"
)

// PrivacyPolicyVersion is the deployed privacy notice version (D4/Q8). It is a Go constant, not env: the notice
// text and its version ship together, and the storefront notice carries the same literal. The server writes it
// into every buyer consent event; a request can never name a version (CD4).
const PrivacyPolicyVersion = "lc-2026-10"

// Consent vocabulary (CD4, Q12). Only these two (purpose, channel) pairs exist.
const (
	PurposeMarketingMessages  = "marketing_messages"
	ChannelMetaDM             = "meta_dm"
	PurposeAdsPersonalization = "ads_personalization"
	ChannelMetaAds            = "meta_ads"
)

// Export bounds (§7, I23) and the export document format tag (D8).
const (
	ExportFormat    = "lc.customer-export.v1"
	MaxExportBytes  = 1 << 20
	MaxExportOrders = 200
	// detailOrders is the merchant detail page's order window (contract §5: newest 50).
	detailOrders = 50
	// TimestampLayout is the microsecond UTC layout every timestamp of this package uses; it equals
	// internal/merchantorders' layout and is what pagination.timeKeyed collections require.
	TimestampLayout = "2006-01-02T15:04:05.000000Z"
)

var (
	// ErrIdempotencyConflict: the same key was used for a different request body or another privacy kind (PT409).
	ErrIdempotencyConflict = errors.New("idempotency_conflict")
	// ErrErasureBlocked: an unexpired hold, a live payment session or a refund without a terminal fact (CD7, PT409).
	ErrErasureBlocked = errors.New("erasure_blocked")
	// ErrErased: a buyer retry after a completed erasure (PT410; the capability is revoked).
	ErrErased = errors.New("erased")
	// ErrExportTooLarge: more than MaxExportOrders orders or more than MaxExportBytes after marshal; the
	// transaction rolls back, so no EXPORT row exists.
	ErrExportTooLarge = errors.New("export_too_large")
	// ErrUnavailable is any projection failure (drift, malformed database result, oversize read).
	ErrUnavailable = errors.New("customers unavailable")
)

// Consents is the CD4 current state per purpose (absence = false).
type Consents struct {
	MarketingMessages  bool `json:"marketing_messages"`
	AdsPersonalization bool `json:"ads_personalization"`
}

// Customer is one list row (contract §5). CustomerID is the buyer.owners id (C-1, D1): it is not a credential.
type Customer struct {
	CustomerID      string   `json:"customer_id"`
	FirstSeenAt     string   `json:"first_seen_at"`
	LastActivityAt  string   `json:"last_activity_at"`
	DisplayName     *string  `json:"display_name"`
	PhoneLast3      *string  `json:"phone_last3"`
	OrdersCount     int64    `json:"orders_count"`
	PaidOrdersCount int64    `json:"paid_orders_count"`
	CapturedMinor   int64    `json:"captured_minor"`
	RefundedMinor   int64    `json:"refunded_minor"`
	Currency        *string  `json:"currency"`
	ClaimsCount     int64    `json:"claims_count"`
	Platforms       []string `json:"platforms"`
	Consents        Consents `json:"consents"`
	Active          bool     `json:"active"`
}

// ClaimSummary never carries an actor_key (CD3).
type ClaimSummary struct {
	SessionID string `json:"session_id"`
	Platform  string `json:"platform"`
	BoundAt   string `json:"bound_at"`
	LineCount int64  `json:"line_count"`
}

// ConsentEvent is one row of the append-only consent history.
type ConsentEvent struct {
	Purpose       string `json:"purpose"`
	Channel       string `json:"channel"`
	Granted       bool   `json:"granted"`
	Source        string `json:"source"`
	PolicyVersion string `json:"policy_version"`
	OccurredAt    string `json:"occurred_at"`
}

// PrivacyAction is one EXPORT/ERASURE log row; Summary is counts only (no PII).
type PrivacyAction struct {
	Kind        string          `json:"kind"`
	Via         string          `json:"via"`
	CompletedAt string          `json:"completed_at"`
	Summary     json.RawMessage `json:"summary"`
}

// Detail is the customer detail: the row, the newest orders (merchant-orders-v1 Summary shape), claims,
// consent history (newest first) and privacy actions.
type Detail struct {
	Customer
	Orders         []merchantorders.Summary `json:"orders"`
	Claims         []ClaimSummary           `json:"claims"`
	ConsentHistory []ConsentEvent           `json:"consent_history"`
	PrivacyActions []PrivacyAction          `json:"privacy_actions"`
}

// ListRequest: Page.Cursor carries the opaque `after` cursor (D5); Q is the raw search text.
type ListRequest struct {
	Page pagination.Request
	Q    string
}

// ConsentInput is the strict buyer PUT body (D12). Context selects the server-side source.
type ConsentInput struct {
	Purpose string `json:"purpose"`
	Channel string `json:"channel"`
	Granted bool   `json:"granted"`
	Context string `json:"context"`
}

// WithdrawInput is the merchant withdrawal body: withdrawal only, a merchant can never grant (CD4).
type WithdrawInput struct {
	Purpose string `json:"purpose"`
	Channel string `json:"channel"`
}

// ConsentResult is the stored consent row echoed back (also on an idempotent replay).
type ConsentResult struct {
	Purpose    string `json:"purpose"`
	Channel    string `json:"channel"`
	Granted    bool   `json:"granted"`
	OccurredAt string `json:"occurred_at"`
}

// BuyerPrivacy is GET /v1/buyer/privacy: current consents and whether an erasure exists.
type BuyerPrivacy struct {
	Consents Consents `json:"consents"`
	Erased   bool     `json:"erased"`
}

// ErasureSummary is counts only (D10, <= 1024 bytes as stored).
type ErasureSummary struct {
	ConsentsWithdrawn int64 `json:"consents_withdrawn"`
	SessionsRevoked   int64 `json:"sessions_revoked"`
	SnapshotsRedacted int64 `json:"snapshots_redacted"`
	BundlesRelabelled int64 `json:"bundles_relabelled"`
}

var (
	keyPattern    = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
	phoneLast3    = regexp.MustCompile(`^[0-9]{3}$`)
	currencyCode  = regexp.MustCompile(`^[A-Z]{3}$`)
	platformNames = map[string]bool{"manual": true, "facebook": true, "instagram": true}
	consentSource = map[string]bool{"buyer_checkout": true, "buyer_settings": true, "merchant_recorded": true, "erasure": true}
	policyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)
)

// ValidPair reports whether (purpose, channel) is one of the two consent pairs of CD4.
func ValidPair(purpose, channel string) bool {
	return (purpose == PurposeMarketingMessages && channel == ChannelMetaDM) ||
		(purpose == PurposeAdsPersonalization && channel == ChannelMetaAds)
}

// CurrentConsents derives the CD4 current state from a consent history: per valid (purpose, channel) the
// latest event by OccurredAt decides, absence is false. history is expected newest first (the order this
// package returns); when two events share one timestamp the earlier list entry wins, which is the newest
// under that convention. Unknown pairs are ignored (the database CHECK makes them impossible).
func CurrentConsents(history []ConsentEvent) Consents {
	var out Consents
	latest := map[string]string{}
	for _, e := range history {
		if !ValidPair(e.Purpose, e.Channel) {
			continue
		}
		if seen, found := latest[e.Purpose]; found && e.OccurredAt <= seen {
			continue
		}
		latest[e.Purpose] = e.OccurredAt
		switch e.Purpose {
		case PurposeMarketingMessages:
			out.MarketingMessages = e.Granted
		case PurposeAdsPersonalization:
			out.AdsPersonalization = e.Granted
		}
	}
	return out
}

// NormalizeQuery trims the merchant search text and validates it: 1..40 characters, valid UTF-8, no control
// characters (D6). The empty string means "no filter". A whitespace-only value is invalid, not "no filter":
// the caller sent a q parameter.
func NormalizeQuery(raw string) (string, error) {
	q := strings.TrimSpace(raw)
	if q == "" || utf8.RuneCountInString(q) > 40 || !utf8.ValidString(q) {
		return "", command.ErrInvalid
	}
	for _, r := range q {
		if unicode.IsControl(r) {
			return "", command.ErrInvalid
		}
	}
	return q, nil
}

// KeyUUID maps a caller Idempotency-Key (grammar shared with every command receipt) to the uuid the
// request_key columns store: the first 16 bytes of a domain-separated SHA-256 with version/variant bits set.
// Deterministic, so a retry with the same key addresses the same row; the key itself is never stored.
func KeyUUID(key string) (string, error) {
	if !keyPattern.MatchString(key) {
		return "", command.ErrInvalid
	}
	sum := sha256.Sum256([]byte("livecommerce.customers.request-key.v1\x00" + key))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// consentSourceFor maps the buyer request context to the server-side source (D12).
func consentSourceFor(context string) (string, error) {
	switch context {
	case "checkout":
		return "buyer_checkout", nil
	case "settings":
		return "buyer_settings", nil
	}
	return "", command.ErrInvalid
}

func canonicalTime(value string) (time.Time, error) {
	t, err := time.Parse(TimestampLayout, value)
	if err != nil || t.UTC().Format(TimestampLayout) != value {
		return time.Time{}, ErrUnavailable
	}
	return t, nil
}

// exactKeys fails unless raw is a JSON object with exactly the given keys (a missing or extra key is drift).
func exactKeys(raw json.RawMessage, keys ...string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return ErrUnavailable
	}
	for _, k := range keys {
		if _, found := fields[k]; !found {
			return ErrUnavailable
		}
	}
	return nil
}

// strict decodes raw into out rejecting unknown fields and trailing data.
func strict(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil {
		return ErrUnavailable
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrUnavailable
	}
	return nil
}

var customerKeys = []string{"customer_id", "first_seen_at", "last_activity_at", "display_name", "phone_last3", "orders_count",
	"paid_orders_count", "captured_minor", "refunded_minor", "currency", "claims_count", "platforms", "consents", "active"}
var detailKeys = append(append([]string{}, customerKeys...), "order_ids", "claims", "consent_history", "privacy_actions")

func nonNegative(values ...int64) bool {
	for _, v := range values {
		if v < 0 {
			return false
		}
	}
	return true
}

// validCustomer re-checks the database projection (drift is a failure, not data).
func validCustomer(c Customer) bool {
	_, e1 := canonicalTime(c.FirstSeenAt)
	_, e2 := canonicalTime(c.LastActivityAt)
	if !command.ValidID(c.CustomerID) || e1 != nil || e2 != nil ||
		!nonNegative(c.OrdersCount, c.PaidOrdersCount, c.CapturedMinor, c.RefundedMinor, c.ClaimsCount) ||
		c.PaidOrdersCount > c.OrdersCount || c.RefundedMinor > c.CapturedMinor || c.Platforms == nil {
		return false
	}
	if c.PhoneLast3 != nil && !phoneLast3.MatchString(*c.PhoneLast3) {
		return false
	}
	if c.Currency != nil && !currencyCode.MatchString(*c.Currency) {
		return false
	}
	if c.DisplayName != nil && (utf8.RuneCountInString(*c.DisplayName) > 120 || !utf8.ValidString(*c.DisplayName)) {
		return false
	}
	for i, p := range c.Platforms {
		if !platformNames[p] || (i > 0 && c.Platforms[i-1] >= p) { // sorted and distinct (D7)
			return false
		}
	}
	// A customer with money but no currency cannot be derived: currency comes from the latest order.
	if c.Currency == nil && (c.CapturedMinor != 0 || c.RefundedMinor != 0 || c.OrdersCount != 0) {
		return false
	}
	return true
}

func validClaim(c ClaimSummary) bool {
	_, err := canonicalTime(c.BoundAt)
	return command.ValidID(c.SessionID) && platformNames[c.Platform] && err == nil && c.LineCount >= 0 && c.LineCount <= 50
}

func validEvent(e ConsentEvent) bool {
	_, err := canonicalTime(e.OccurredAt)
	return ValidPair(e.Purpose, e.Channel) && consentSource[e.Source] && policyPattern.MatchString(e.PolicyVersion) && err == nil &&
		(!e.Granted || e.Source == "buyer_checkout" || e.Source == "buyer_settings") // only the buyer grants
}

func validAction(a PrivacyAction) bool {
	_, err := canonicalTime(a.CompletedAt)
	var summary map[string]json.RawMessage
	return (a.Kind == "EXPORT" || a.Kind == "ERASURE") && (a.Via == "buyer" || a.Via == "merchant" || a.Via == "restore") &&
		err == nil && len(a.Summary) <= 1024 && json.Unmarshal(a.Summary, &summary) == nil && summary != nil
}

// detailWire is the strict shape of one detail row from identity.read_merchant_customers.
type detailWire struct {
	Customer
	OrderIDs       []string        `json:"order_ids"`
	Claims         []ClaimSummary  `json:"claims"`
	ConsentHistory []ConsentEvent  `json:"consent_history"`
	PrivacyActions []PrivacyAction `json:"privacy_actions"`
}

func decodeCustomer(raw json.RawMessage) (Customer, error) {
	if err := exactKeys(raw, customerKeys...); err != nil {
		return Customer{}, err
	}
	var c Customer
	if strict(raw, &c) != nil || !validCustomer(c) {
		return Customer{}, ErrUnavailable
	}
	return c, nil
}

func decodeDetail(raw json.RawMessage) (detailWire, error) {
	if err := exactKeys(raw, detailKeys...); err != nil {
		return detailWire{}, err
	}
	var d detailWire
	if strict(raw, &d) != nil || !validCustomer(d.Customer) || d.OrderIDs == nil || len(d.OrderIDs) > MaxExportOrders+1 ||
		d.Claims == nil || d.ConsentHistory == nil || d.PrivacyActions == nil {
		return detailWire{}, ErrUnavailable
	}
	for _, id := range d.OrderIDs {
		if !command.ValidID(id) {
			return detailWire{}, ErrUnavailable
		}
	}
	for _, c := range d.Claims {
		if !validClaim(c) {
			return detailWire{}, ErrUnavailable
		}
	}
	for _, e := range d.ConsentHistory {
		if !validEvent(e) {
			return detailWire{}, ErrUnavailable
		}
	}
	for _, a := range d.PrivacyActions {
		if !validAction(a) {
			return detailWire{}, ErrUnavailable
		}
	}
	// CD4: the SQL row's consents must equal the derivation from the history it returned with them.
	if CurrentConsents(d.ConsentHistory) != d.Consents {
		return detailWire{}, ErrUnavailable
	}
	return d, nil
}

func decodeConsentResult(raw []byte, purpose, channel string) (ConsentResult, error) {
	var out ConsentResult
	if err := exactKeys(raw, "purpose", "channel", "granted", "occurred_at"); err != nil || strict(raw, &out) != nil {
		return ConsentResult{}, ErrUnavailable
	}
	if _, err := canonicalTime(out.OccurredAt); err != nil || out.Purpose != purpose || out.Channel != channel {
		return ConsentResult{}, ErrUnavailable
	}
	return out, nil
}

func decodeErasure(raw []byte) (ErasureSummary, error) {
	var out ErasureSummary
	if err := exactKeys(raw, "consents_withdrawn", "sessions_revoked", "snapshots_redacted", "bundles_relabelled"); err != nil ||
		strict(raw, &out) != nil || !nonNegative(out.ConsentsWithdrawn, out.SessionsRevoked, out.SnapshotsRedacted, out.BundlesRelabelled) ||
		len(raw) > 1024 {
		return ErasureSummary{}, ErrUnavailable
	}
	return out, nil
}

// exportDoc is the merchant export envelope (D8). Orders are the merchant order Detail shape produced by the
// existing projection, so an export always equals the order page.
type exportDoc struct {
	Format         string                  `json:"format"`
	GeneratedAt    string                  `json:"generated_at"`
	Store          exportStore             `json:"store"`
	CustomerID     string                  `json:"customer_id"`
	Orders         []merchantorders.Detail `json:"orders"`
	Consents       []ConsentEvent          `json:"consents"`
	Claims         []ClaimSummary          `json:"claims"`
	PrivacyActions []PrivacyAction         `json:"privacy_actions"`
}

// buyerExportDoc is the same envelope without customer_id and principal ids (D8); orders are the buyer's own
// order documents as the buyer_export_orders definer returned them.
type buyerExportDoc struct {
	Format         string            `json:"format"`
	GeneratedAt    string            `json:"generated_at"`
	Store          exportStore       `json:"store"`
	Orders         []json.RawMessage `json:"orders"`
	Consents       []ConsentEvent    `json:"consents"`
	Claims         []ClaimSummary    `json:"claims"`
	PrivacyActions []PrivacyAction   `json:"privacy_actions"`
}

type exportStore struct {
	Name string `json:"name"`
}

// marshalBounded marshals doc and enforces the export caps (D8): more than MaxExportOrders orders or more than
// MaxExportBytes after marshal is ErrExportTooLarge (the caller's transaction then rolls back with no row).
func marshalBounded(doc any, orders int) ([]byte, error) {
	if orders > MaxExportOrders {
		return nil, ErrExportTooLarge
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(body) > MaxExportBytes {
		return nil, ErrExportTooLarge
	}
	return body, nil
}

// exportSummary is the counts-only summary stored with the EXPORT row (no PII).
func exportSummary(orders, consents, claims, actions int) string {
	return fmt.Sprintf(`{"orders":%d,"consents":%d,"claims":%d,"privacy_actions":%d}`, orders, consents, claims, actions)
}
