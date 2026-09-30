package attribution

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/command"
)

// attribution.go holds the pure helpers of the CAPI event (AD8, F16, C3) and the consent-hook entry point (A-3, C5).
// SQL touched: ads.put_capi_context (EXECUTE commerce_buyer_runtime), called only inside the buyer consent transaction.

const (
	eventIDPrefix = "lc-purchase-"
	// maxUserAgentRunes matches the ads.capi_contexts CHECK (1..512 characters).
	maxUserAgentRunes = 512
	// externalIDInfo domain-separates the C3 HMAC input.
	externalIDInfo = "capi-external-id/v1"
)

// EventID is the stable CAPI event_id of a payment attempt (AD8): "lc-purchase-" + the attempt uuid. It never changes
// for an attempt, so a future pixel eventID can dedup against it (F15).
func EventID(attemptID string) string { return eventIDPrefix + attemptID }

// HashPhone returns the lowercase hex SHA-256 of the phone's digits including the country code, the form Meta
// requires for `ph` (F16: "16505551212"; https://developers.facebook.com/docs/marketing-api/conversions-api/parameters/customer-information-parameters,
// retrieved 2026-09-29 via contract F16). Input is E.164 with an optional leading "+" and nothing else; anything that is
// not 8..15 digits starting 1-9 returns ok=false and the caller omits `ph` (never sends a guess).
func HashPhone(e164 string) (string, bool) {
	digits := strings.TrimPrefix(e164, "+")
	if len(digits) < 8 || len(digits) > 15 || digits[0] < '1' || digits[0] > '9' {
		return "", false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return "", false
		}
	}
	sum := sha256.Sum256([]byte(digits))
	return hex.EncodeToString(sum[:]), true
}

// ExternalID is C3: hex(SHA-256(hex(HMAC-SHA256(key, "capi-external-id/v1|<tenant>|<store>|<owner>")))). The key is the
// worker-only COMMERCE_CAPI_EXTERNAL_ID_KEY_FILE secret (O-D), so Meta cannot link the id to the internal owner uuid
// and two stores never share an id for one owner. An empty key returns "" (the route refuses to start without one).
func ExternalID(key []byte, tenantID, storeID, ownerID string) string {
	if len(key) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(externalIDInfo + "|" + tenantID + "|" + storeID + "|" + ownerID))
	inner := hex.EncodeToString(mac.Sum(nil))
	sum := sha256.Sum256([]byte(inner))
	return hex.EncodeToString(sum[:])
}

// cleanUserAgent makes a User-Agent header storable: invalid UTF-8 and control characters are dropped, the result is
// trimmed and cut to 512 runes. Empty result means "no context" (C5: no SQL call), never an error.
func cleanUserAgent(ua string) string {
	ua = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return -1
		}
		return r
	}, ua)
	ua = strings.TrimSpace(ua)
	if utf8.RuneCountInString(ua) > maxUserAgentRunes {
		ua = string([]rune(ua)[:maxUserAgentRunes])
	}
	return ua
}

// PutCAPIContext is the A-3 consent hook (C5): the buyer consent PUT calls it in the SAME transaction, iff the write was
// an ads_personalization grant, so that a later CAPI event can carry the buyer's browser user agent. The definer upserts
// only when customers.consent_allows is already true. sessionHash is the SHA-256 of the buyer capability token the
// caller already resolved; storeID is the server-resolved store. Input is pre-validated here so the SQL call cannot fail
// on input and abort the consent transaction: an empty user agent makes no call at all.
func PutCAPIContext(ctx context.Context, tx pgx.Tx, sessionHash []byte, storeID, userAgent string) error {
	if tx == nil || len(sessionHash) != sha256.Size || !command.ValidID(storeID) {
		return command.ErrInvalid
	}
	ua := cleanUserAgent(userAgent)
	if ua == "" {
		return nil
	}
	// ads.put_capi_context: 0080, EXECUTE commerce_buyer_runtime; resolves the capability and upserts ads.capi_contexts
	// only while consent_allows(ads_personalization, meta_ads) holds; the buyer never gets table access.
	_, err := tx.Exec(ctx, `SELECT ads.put_capi_context($1::bytea,$2::uuid,$3::text)`, sessionHash, storeID, ua)
	return err
}
