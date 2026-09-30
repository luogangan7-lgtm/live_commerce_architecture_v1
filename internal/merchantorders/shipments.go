// shipments.go owns the merchant-arranged shipment command and history read of
// contracts/manual-fulfilment-v1.md (FROZEN): validation and canonicalization of the carrier,
// tracking and URL rules (§3), the SQL replay/CAS error mapping (§5.1) and the strict decoders.
//
// Non-goals: no carrier API, label, tracking poll, provider operation or River job (MD2); no
// buyer message (MD10); never fetches a tracking URL (§0 rejected alternative: SSRF surface).

package merchantorders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/text/unicode/norm"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// Errors mapped by httpapi to 409 version_changed / 422 codes of contract §5.1.
var (
	ErrVersionChanged      = errors.New("shipment version changed")
	ErrNotShippable        = errors.New("order not shippable")
	ErrInvalidCarrier      = errors.New("invalid carrier")
	ErrInvalidTracking     = errors.New("invalid tracking number")
	ErrInvalidURL          = errors.New("invalid tracking url")
	ErrVoidRequiresShipped = errors.New("void requires a shipped head")
	ErrInvalidVoid         = errors.New("invalid void body")
)

// carrierTrackingTemplates maps carrier_code to a tracking URL template with a {tracking}
// placeholder (path-escaped when used). EMPTY in v1 (ruling M-2): an entry may be added only together
// with its documentation URL and retrieval date (PROCESS §5), and an explicit merchant tracking_url
// always wins over a template.
var carrierTrackingTemplates = map[string]string{}

var carrierCodes = []string{"seven_eleven_cvs", "familymart_cvs", "hilife_cvs", "okmart_cvs", "sf_express", "chunghwa_post", "other"}
var voidReasons = []string{"wrong_order", "wrong_tracking", "not_dispatched", "other"}
var trackingNumber = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 -]{0,63}$`)

// trackingURLBytes: printable ASCII after an LDH dotted host whose last label starts with a letter.
// S6: this is the subset every parser agrees on. Go's url.Parse admits hosts such as a<b.example.com or
// 300.300.300.300 that WHATWG new URL() (admin and storefront parsers) rejects or reads as IPv4, and a
// stored row like that would take down the whole order view. SQL twin: manual_shipment_versions.
// tracking_url CHECK and record_manual_shipment (same pattern, POSIX group syntax).
var trackingURLBytes = regexp.MustCompile(`^https://(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:[/?][!-~]*)?$`)
var shipmentKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// Shipment is the buyer-visible projection of a SHIPPED head, also embedded in the merchant detail.
type Shipment struct {
	Version        int64   `json:"version"`
	Status         string  `json:"status"`
	CarrierCode    string  `json:"carrier_code"`
	CarrierName    *string `json:"carrier_name"`
	TrackingNumber string  `json:"tracking_number"`
	TrackingURL    *string `json:"tracking_url"`
	RecordedAt     string  `json:"recorded_at"`
}

// ShipmentVersion adds the merchant-only fields (never sent to buyers).
type ShipmentVersion struct {
	Shipment
	Note        *string `json:"note"`
	VoidReason  *string `json:"void_reason"`
	PrincipalID string  `json:"principal_id"`
}

// ShipmentInput is the exact PUT body: every key present, nullable ones explicit null.
type ShipmentInput struct {
	ExpectedVersion int64   `json:"expected_version"`
	Status          string  `json:"status"`
	CarrierCode     *string `json:"carrier_code"`
	CarrierName     *string `json:"carrier_name"`
	TrackingNumber  *string `json:"tracking_number"`
	TrackingURL     *string `json:"tracking_url"`
	Note            *string `json:"note"`
	VoidReason      *string `json:"void_reason"`
}

// NormalizeShipment applies contract §3 plus the §5.1 void-body rule and returns the canonical
// input the database receives. It is pure (MF01) and stricter than nothing: SQL re-checks every rule.
func NormalizeShipment(in ShipmentInput) (ShipmentInput, error) {
	if in.ExpectedVersion < 0 {
		return in, command.ErrInvalid
	}
	switch in.Status {
	case "VOIDED":
		if in.CarrierCode != nil || in.CarrierName != nil || in.TrackingNumber != nil || in.TrackingURL != nil ||
			in.Note != nil || in.VoidReason == nil || !slices.Contains(voidReasons, *in.VoidReason) {
			return in, ErrInvalidVoid
		}
		return in, nil
	case "SHIPPED":
	default:
		return in, command.ErrInvalid
	}
	if in.VoidReason != nil {
		return in, ErrInvalidVoid
	}
	if in.CarrierCode == nil || !slices.Contains(carrierCodes, *in.CarrierCode) {
		return in, ErrInvalidCarrier
	}
	out := in
	if in.CarrierName != nil {
		name := strings.TrimSpace(norm.NFC.String(*in.CarrierName))
		if !validLabel(name, 80, 1) {
			return in, ErrInvalidCarrier
		}
		out.CarrierName = &name
	} else if *in.CarrierCode == "other" {
		return in, ErrInvalidCarrier
	}
	if in.TrackingNumber == nil {
		return in, ErrInvalidTracking
	}
	number := strings.TrimSpace(*in.TrackingNumber)
	if !trackingNumber.MatchString(number) {
		return in, ErrInvalidTracking
	}
	out.TrackingNumber = &number
	if in.TrackingURL != nil {
		link, err := canonicalTrackingURL(*in.TrackingURL)
		if err != nil {
			return in, err
		}
		out.TrackingURL = &link
	}
	if in.Note != nil {
		note := norm.NFC.String(*in.Note)
		if !validLabel(note, 200, 0) {
			return in, command.ErrInvalid
		}
		out.Note = &note
	}
	return out, nil
}

// validLabel: valid UTF-8, min..max runes, no control characters (SQL twin: [[:cntrl:]]).
func validLabel(v string, max, min int) bool {
	n := utf8.RuneCountInString(v)
	if !utf8.ValidString(v) || n < min || n > max {
		return false
	}
	return !strings.ContainsFunc(v, unicode.IsControl)
}

// canonicalTrackingURL implements contract §3.2: absolute https, <=512 bytes, dotted host, no userinfo,
// port or fragment, printable ASCII only (SQL CHECK twin), re-serialized by Go so the stored bytes are
// one canonical form. The URL is data for the buyer's browser; no code here ever dials it.
func canonicalTrackingURL(raw string) (string, error) {
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x21 || raw[i] > 0x7e || raw[i] == '#' {
			return "", ErrInvalidURL
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" ||
		u.Host == "" || u.Port() != "" || strings.ContainsAny(u.Host, "[]") || !strings.Contains(u.Hostname(), ".") ||
		strings.HasSuffix(u.Hostname(), ".") || strings.HasPrefix(u.Hostname(), ".") || strings.Contains(u.Hostname(), "..") {
		return "", ErrInvalidURL
	}
	u.Host = strings.ToLower(u.Host)
	out := u.String()
	if len(out) > 512 || !trackingURLBytes.MatchString(out) {
		return "", ErrInvalidURL
	}
	return out, nil
}

// RecordShipment is the single PUT command: record (expected_version 0 or after a void), correct and
// void are distinct status/version transitions of the same call (contract §5.1). key is the
// Idempotency-Key. Atomic in SQL under the order lock; no provider call, job or operation is created.
func RecordShipment(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, orderID string, in ShipmentInput) (ShipmentVersion, error) {
	if tx == nil || !validAuthorityInput(scope, token) || !command.ValidID(orderID) || !shipmentKey.MatchString(key) {
		return ShipmentVersion{}, command.ErrInvalid
	}
	normalized, err := NormalizeShipment(in)
	if err != nil {
		return ShipmentVersion{}, err
	}
	// The hash covers the exact submitted body and the order, so one key cannot replay onto another
	// order and a void replay is byte-stable (contract §5.1).
	body, err := json.Marshal(in)
	if err != nil {
		return ShipmentVersion{}, command.ErrInvalid
	}
	digest := sha256.Sum256(append([]byte("fulfillment.manual_shipment.record.v1\n"+orderID+"\n"), body...))
	auth := sha256.Sum256([]byte(token))
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT fulfillment.record_manual_shipment($1,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		auth[:], scope.StoreID, orderID, key, digest[:], normalized.ExpectedVersion, normalized.Status,
		normalized.CarrierCode, normalized.CarrierName, normalized.TrackingNumber, normalized.TrackingURL,
		normalized.Note, normalized.VoidReason).Scan(&raw)
	if err != nil {
		return ShipmentVersion{}, mapShipmentError(err)
	}
	// SQL already re-authorized after its last write; keep the original Scope as a second fence.
	if err := platform.RequirePermission(ctx, tx, scope, token, "fulfillment:write"); err != nil {
		return ShipmentVersion{}, mapError(err)
	}
	out, err := decodeVersion(raw)
	if err != nil || out.Status != normalized.Status {
		return ShipmentVersion{}, ErrUnavailable
	}
	return out, nil
}

// ShipmentHistory returns every version ascending, merchant-only fields included (orders:read).
func ShipmentHistory(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, orderID string) ([]ShipmentVersion, error) {
	if tx == nil || !validAuthorityInput(scope, token) || !command.ValidID(orderID) {
		return nil, command.ErrInvalid
	}
	auth := sha256.Sum256([]byte(token))
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT fulfillment.read_manual_shipment_history($1,$2::uuid,$3::uuid)`,
		auth[:], scope.StoreID, orderID).Scan(&raw); err != nil {
		return nil, mapError(err)
	}
	if err := platform.RequirePermission(ctx, tx, scope, token, "orders:read"); err != nil {
		return nil, mapError(err)
	}
	var items []json.RawMessage
	if len(raw) > 512<<10 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &items) != nil || len(items) > 1000 {
		return nil, ErrUnavailable
	}
	out := make([]ShipmentVersion, 0, len(items))
	for i, item := range items {
		v, err := decodeVersion(item)
		if err != nil || v.Version != int64(i+1) {
			return nil, ErrUnavailable
		}
		out = append(out, v)
	}
	return out, nil
}

var shipmentKeys = []string{"version", "status", "carrier_code", "carrier_name", "tracking_number", "tracking_url", "recorded_at"}

func decodeVersion(raw json.RawMessage) (ShipmentVersion, error) {
	if _, err := exactNullable(raw, []string{"carrier_name", "tracking_url", "note", "void_reason"},
		append(append([]string{}, shipmentKeys...), "note", "void_reason", "principal_id")...); err != nil {
		return ShipmentVersion{}, err
	}
	var v ShipmentVersion
	if json.Unmarshal(raw, &v) != nil || !command.ValidID(v.PrincipalID) || !validShipmentFields(v.Shipment) ||
		(v.Status != "SHIPPED" && v.Status != "VOIDED") || (v.Status == "VOIDED") != (v.VoidReason != nil) ||
		(v.VoidReason != nil && !slices.Contains(voidReasons, *v.VoidReason)) ||
		(v.Note != nil && !validLabel(*v.Note, 200, 0)) {
		return ShipmentVersion{}, ErrUnavailable
	}
	return v, nil
}

// validShipmentFields checks what the SQL CHECKs already enforce, so a database/Go drift is a
// projection failure and never displayed data. Status is checked by the caller (SHIPPED or VOIDED).
func validShipmentFields(s Shipment) bool {
	if _, err := canonicalTime(s.RecordedAt); err != nil {
		return false
	}
	return s.Version > 0 && slices.Contains(carrierCodes, s.CarrierCode) &&
		trackingNumber.MatchString(s.TrackingNumber) && !strings.HasSuffix(s.TrackingNumber, " ") &&
		(s.CarrierCode != "other" || s.CarrierName != nil) &&
		(s.CarrierName == nil || validLabel(*s.CarrierName, 80, 1)) &&
		(s.TrackingURL == nil || (len(*s.TrackingURL) <= 512 && trackingURLBytes.MatchString(*s.TrackingURL)))
}

// mapShipmentError maps the fixed SQL error messages of record_manual_shipment (a closed set this
// migration owns; no driver text ever leaves) and delegates authority codes to mapError.
func mapShipmentError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "PT400", "22023", "23514", "22003", "22P02":
			return command.ErrInvalid
		case "23505":
			// Another principal already used this Idempotency-Key for the operation: a key conflict.
			return command.ErrConflict
		case "PT409":
			if pg.Message == "version_changed" {
				return ErrVersionChanged
			}
			return command.ErrConflict
		case "PT422":
			switch pg.Message {
			case "not_shippable":
				return ErrNotShippable
			case "invalid_carrier":
				return ErrInvalidCarrier
			case "invalid_tracking":
				return ErrInvalidTracking
			case "invalid_url":
				return ErrInvalidURL
			case "void_requires_shipped":
				return ErrVoidRequiresShipped
			case "invalid_void":
				return ErrInvalidVoid
			}
			return command.ErrInvalid
		}
	}
	return mapError(err)
}
