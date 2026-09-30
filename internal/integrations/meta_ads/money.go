package metaads

// money.go: the two I05 conversions between platform minor units and Meta's wire amounts
// (meta-ads-v1 §3). Both are exact: nothing is rounded, truncated or guessed.

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

var (
	// ErrNotWholeUnit: a TWD amount that is not a whole NT$ (Meta's TWD offset is 1, F19), so
	// dividing would truncate money.
	ErrNotWholeUnit = errors.New("metaads: TWD amount is not a whole unit")
	// ErrUnsupportedCurrency: anything other than TWD, USD, HKD.
	ErrUnsupportedCurrency = errors.New("metaads: unsupported currency")
	// ErrBadSpend: Meta's spend string is not a plain non-negative decimal with <= 2 fraction digits
	// (sign, exponent, more digits or non-digits), or a negative budget was given.
	ErrBadSpend = errors.New("metaads: bad spend value")

	spendPattern = regexp.MustCompile(`^[0-9]{1,15}(\.[0-9]{1,2})?$`)
)

// MetaBudget converts a store-side amount in minor units (hundredths of the currency) into the
// integer Meta expects for that currency. I05: TWD has Meta offset 1 (F19, retrieved 2026-09-29,
// https://developers.facebook.com/docs/marketing-api/currencies) so minor/100 only when exactly
// divisible, else ErrNotWholeUnit (never truncates); USD and HKD have offset 100 and pass through.
// A negative amount is ErrBadSpend.
func MetaBudget(currency string, amountMinor int64) (int64, error) {
	switch currency {
	case "TWD", "USD", "HKD":
	default:
		return 0, ErrUnsupportedCurrency
	}
	if amountMinor < 0 {
		return 0, ErrBadSpend
	}
	if currency == "TWD" {
		if amountMinor%100 != 0 {
			return 0, ErrNotWholeUnit
		}
		return amountMinor / 100, nil
	}
	return amountMinor, nil
}

// SpendMinor parses Meta's decimal `spend` string (in the account currency's major unit) into
// hundredths, exactly. I05: "12.3" is 1230, "12.345" is an error, never rounded. Currency support is
// the same closed set as MetaBudget; every supported currency is treated as 2 decimals here.
func SpendMinor(currency, spend string) (int64, error) {
	switch currency {
	case "TWD", "USD", "HKD":
	default:
		return 0, ErrUnsupportedCurrency
	}
	if !spendPattern.MatchString(spend) {
		return 0, ErrBadSpend
	}
	whole, frac, _ := strings.Cut(spend, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrBadSpend
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	if w > (1<<62)/100 {
		return 0, ErrBadSpend
	}
	return w*100 + f, nil
}
