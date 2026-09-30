// amount.go: currency allowlist and minor-unit mapping (contracts/stripe-psp-v1.md §4, §0.1).
// It never rounds, converts or re-prices; any mismatch is a rejection.
//
// Ownership: integration_worker. The same table must exist as the SQL functions
// payments.stripe_amount_ok / payments.stripe_unit_amount (integrator, migration 0061);
// SP02 asserts Go↔SQL parity once that migration exists.
// Callers: EncodeCreateBody (unit_amount check), the registrar's method bounds, SP02.

package stripe

// currencyRule is one row of the §4 table. Stripe's unit equals the repository's
// ISO 4217 minor unit for every admitted currency, so UnitAmount is an identity
// guarded by min/max/step.
type currencyRule struct {
	min, max, step int64
}

// currencyRules is the closed v1 allowlist from the §0.1 integrator ruling
// (TWD, HKD, SGD, MYR, USD). It supersedes the D15/§4 draft that listed JPY; JPY and
// every other currency (ISK/UGX ×100, HUF, 3-decimal currencies) are rejected.
//
// Facts: https://docs.stripe.com/currencies (retrieved 2026-09-28, F7): amounts are in
// the minor unit; TWD charges are two-decimal but payouts are zero-decimal and
// divisible by 100; HKD minimum 4.00 and USD minimum 0.50. The SGD 0.50 and MYR 2.00
// minimums come from the same page's minimum-charge table, which rendered only
// partially (SOURCE_PARTIAL). All minimums are local prefilters, not Stripe's
// guarantee: the real minimum depends on the settlement currency, and a first-send
// 400 is the definitive rejection (§10). The 8-digit maximum is conservative.
var currencyRules = map[string]currencyRule{
	"HKD": {min: 400, max: 99_999_999, step: 1},
	"USD": {min: 50, max: 99_999_999, step: 1},
	"SGD": {min: 50, max: 99_999_999, step: 1},
	"MYR": {min: 200, max: 99_999_999, step: 1},
	// TWD: whole NT$ only (step 100) because Stripe pays TWD out as zero-decimal.
	// TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)
	"TWD": {min: 2500, max: 99_999_900, step: 100},
}

// UnitAmount maps an ISO 4217 amount in minor units to Stripe's unit_amount for an
// uppercase currency code. It returns ErrInvalid for a currency outside the v1
// allowlist (including lowercase input), an amount outside [min,max], or an amount
// that is not a multiple of the currency step. It never rounds.
func UnitAmount(currency string, amountMinor int64) (int64, error) {
	rule, ok := currencyRules[currency]
	if !ok {
		return 0, ErrInvalid
	}
	// I05: the charged amount must equal the order amount exactly; out-of-band or
	// non-step amounts are rejected, never rounded.
	if amountMinor < rule.min || amountMinor > rule.max || amountMinor%rule.step != 0 {
		return 0, ErrInvalid
	}
	return amountMinor, nil
}
