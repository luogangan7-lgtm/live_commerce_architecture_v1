// cvs_rules.go holds the pure rules of taiwan-cvs-logistics-v1 that Go and SQL must agree on: the four CVS kinds, the
// buyer-entered store code format (§16.1), the deterministic ecpay_logistics connection id and the error type the CVS
// routes map SQL SQLSTATEs to (unit default C7).
//
// Non-goals: no SQL, no network, no state. The SQL twins are migrations/0073 record_buyer_cvs_store (store code regex,
// checked again in the database) and fulfillment.ecpay_connection_id; a shared golden table (testdata) pins both.

package fulfillment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
)

// isCVSKind reports the four Taiwan convenience-store kinds (TD6); SQL twin: the widened kind CHECKs of migrations/0072.
func isCVSKind(kind string) bool {
	return kind == "cvs_711" || kind == "cvs_familymart" || kind == "cvs_hilife" || kind == "cvs_okmart"
}

// Store code formats of §16.1 (F21, retrieved 2026-09-30): 7-ELEVEN https://emap.pcsc.com.tw/ (6 digits),
// FamilyMart https://family.map.com.tw/famiport/storeNumberFreeze.aspx (6 digits), OK mart
// https://www.okmart.com.tw/convenient_shopSearch (4 digits). Hi-Life length is UNKNOWN (3..8 accepted until evidence).
var (
	storeCode6   = regexp.MustCompile(`^[0-9]{6}$`)
	storeCode4   = regexp.MustCompile(`^[0-9]{4}$`)
	storeCodeAny = regexp.MustCompile(`^[0-9]{3,8}$`)
)

// ValidBuyerStoreCode is the §16.1 table; it is the twin of the SQL check in record_buyer_cvs_store. Codes are strings:
// leading zeros are data and are never trimmed or numerically converted.
func ValidBuyerStoreCode(kind, code string) bool {
	switch kind {
	case "cvs_711", "cvs_familymart":
		return storeCode6.MatchString(code)
	case "cvs_okmart":
		return storeCode4.MatchString(code)
	case "cvs_hilife":
		return storeCodeAny.MatchString(code)
	}
	return false
}

// ConnectionID is the deterministic id of the one ecpay_logistics account of (tenant, store, environment). It lets
// Connect bind the AEAD AAD (tenant, store, connection, environment, merchant, version) before the first insert. Twin:
// SQL fulfillment.ecpay_connection_id (sha256 prefix with the version-4 and variant bits set).
func ConnectionID(tenantID, storeID, environment string) string {
	sum := sha256.Sum256([]byte("ecpay-logistics-connection|" + tenantID + "|" + storeID + "|" + environment))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// CVSError is a coded refusal of the CVS routes (unit default C7): Status is the HTTP status, Code the contract's error
// code (the SQL message), RetryAfter seconds for 429. It carries no driver text.
type CVSError struct {
	Status     int
	Code       string
	RetryAfter int
}

func (e *CVSError) Error() string { return "cvs refused: " + e.Code }

// Sentinels for conditions the routes classify without a SQLSTATE.
var (
	// ErrECPayDisabled: CVS_ECPAY_ENABLED is off, so no ECPay call or label can be made (mapped to 503 unavailable).
	ErrECPayDisabled = errors.New("ecpay logistics disabled")
	// errCVSReplay marks PT2RP: the idempotency key was already used, the caller must roll back and read the stored result.
	errCVSReplay = errors.New("cvs replay")
)
