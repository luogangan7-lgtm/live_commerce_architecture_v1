package core

import "fmt"

// Dispatcher secret hook (external-dispatcher-v1 amendment, meta-claims-intake-v1 §6.4, IR-12).
//
// SecretClaim carries the lease fence to LoadSecret and nowhere else: Check, Reconcile and
// DispatchWithSecret never receive it. Secret carries one provider credential from LoadSecret to
// DispatchWithSecret; every formatter redacts it and the dispatcher zeroes it once the callback
// returns, so a callback must copy nothing it wants to keep.

const redactedSecret = "[redacted]"

// SecretClaim identifies the dispatch attempt whose lease fences the credential load.
type SecretClaim struct {
	OperationID string
	Generation  int64
	LeaseToken  []byte
	// Mode is the claim mode, "dispatch" or "reconcile" (R-7a, cvs-ecpay E3), so a loader or Finish
	// can fence on integration.operations.lease_mode.
	Mode string
}

func (SecretClaim) String() string               { return redactedSecret }
func (SecretClaim) GoString() string             { return redactedSecret }
func (SecretClaim) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redactedSecret)) }
func (SecretClaim) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedSecret + `"`), nil }
func (SecretClaim) MarshalText() ([]byte, error) { return []byte(redactedSecret), nil }

// Secret is an opaque credential value.
type Secret struct{ b []byte }

// NewSecret copies b; the caller may zero its own slice afterwards.
func NewSecret(b []byte) Secret { return Secret{b: append([]byte(nil), b...)} }

// Reveal returns the backing bytes (not a copy): the dispatcher zeroes them after
// DispatchWithSecret returns, so callers must neither retain nor log the slice.
func (s Secret) Reveal() []byte { return s.b }

func (Secret) String() string               { return redactedSecret }
func (Secret) GoString() string             { return redactedSecret }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redactedSecret)) }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedSecret + `"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte(redactedSecret), nil }

func (s Secret) zero() { clear(s.b) }
