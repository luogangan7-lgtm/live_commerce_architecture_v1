// types.go: exported value types and sentinel errors of the ecpay wire package (FROZEN interface of
// docs/delivery/units/cvs-ecpay.md). Credentials and Payload are redacted in every formatter because
// they hold the merchant's HashKey/HashIV and sender data.

package ecpay

import (
	"errors"
	"fmt"
)

// Environment is the ECPay environment of a connection; a deployment runs exactly one (§12, LQ5).
type Environment string

const (
	// EnvSandbox is the public stage host logistics-stage.ecpay.com.tw.
	EnvSandbox Environment = "SANDBOX"
	// EnvLive is the production host logistics.ecpay.com.tw.
	EnvLive Environment = "LIVE"
)

// Sentinel errors. None carries key material, a body or recipient data.
var (
	// ErrInvalid is a malformed input or a malformed provider payload.
	ErrInvalid = errors.New("ecpay: invalid input")
	// ErrMAC is a CheckMacValue mismatch on a provider-originated message.
	ErrMAC = errors.New("ecpay: check mac value mismatch")
	// ErrNoDirectory means the chain has no GetStoreList CvsType (OKMARTC2C, F10/F18; TCV10).
	ErrNoDirectory = errors.New("ecpay: no store directory for this chain")
	// ErrUnavailable is a directory or probe call that did not produce a usable answer.
	ErrUnavailable = errors.New("ecpay: directory unavailable")
	// ErrSecret is every keyring seal/open failure; no detail on purpose.
	ErrSecret = errors.New("ecpay: logistics credential unavailable")
	// ErrConfig is every keyring/env configuration failure; it never echoes a value.
	ErrConfig = errors.New("ecpay: invalid configuration")
)

const redacted = "[redacted]"

// Credentials is one ECPay logistics account's MerchantID and its single HashKey/HashIV pair (F6:
// the same pair signs requests and verifies callbacks).
type Credentials struct{ MerchantID, HashKey, HashIV string }

func (Credentials) String() string   { return redacted }
func (Credentials) GoString() string { return redacted }
func (Credentials) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}
func (Credentials) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Payload is the sealed credential document: ciphertext JSON is
// {hash_key,hash_iv,sender_name,sender_cell_phone}.
type Payload struct{ HashKey, HashIV, SenderName, SenderCellPhone string }

func (Payload) String() string   { return redacted }
func (Payload) GoString() string { return redacted }
func (Payload) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}
func (Payload) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Scope is everything the keyring AAD binds a ciphertext to (E5); Version is the credential version.
type Scope struct {
	TenantID, StoreID, ConnectionID, MerchantID string
	Environment                                 Environment
	Version                                     int64
}

// Store is one directory row (GetStoreList) as ECPay returned it, trailing spaces trimmed (X10).
type Store struct{ ID, Name, Address, Phone string }

// MapReturn is the unsigned e-map return (F3 subset): forgeable by the browser, so only the store id
// is used, and only as a key into the directory (TD3).
type MapReturn struct{ MerchantID, MerchantTradeNo, SubType, StoreID, Outside string }

// CreateRequest is the frozen per-attempt input of Express/Create. CollectionAmount 0 means
// IsCollection=N (§16.3); otherwise it must equal GoodsAmount (F20).
type CreateRequest struct {
	SubType, MerchantTradeNo, MerchantTradeDate, ReceiverStoreID, ReceiverName, ReceiverPhone, GoodsName, ServerReplyURL string
	GoodsAmount, CollectionAmount                                                                                        int
}

// Result is the classified outcome of Create or Query. Outcome is SUCCEEDED, FAILED_FINAL or UNKNOWN;
// Code matches ^[a-z0-9_.]{1,80}$. The code fields are raw provider values (the SQL Finish normalises
// them; the adapter guarantees LogisticsID matches ^[0-9A-Za-z_-]{1,40}$ when SUCCEEDED).
type Result struct{ Outcome, Code, LogisticsID, PaymentNo, ValidationNo, ShipmentNo, StatusCode string }

// StatusReport is a MAC-verified status notification reduced to the F7 fields we persist; recipient
// PII fields are dropped. BodySHA256 is over the raw received bytes (dedupe key).
type StatusReport struct {
	MerchantID, MerchantTradeNo, LogisticsID, RtnCode, RtnMsg, UpdateDate, PaymentNo, ValidationNo string
	BodySHA256                                                                                     [32]byte
}
