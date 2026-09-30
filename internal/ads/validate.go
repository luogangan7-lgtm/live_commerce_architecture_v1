package ads

import (
	"regexp"
	"sort"
	"time"

	"livecommerce/internal/command"
)

// validate.go is the pure local validation of contract 5.2 for a DraftInput (no database, no clock but the one passed
// in). SQL ads.normalize_input repeats every rule and adds the ones that need rows (bindings, ownership, storefront);
// the Go copy exists so a bad request is refused before a transaction opens and so the rules are unit-tested.

// DraftInput is the merchant's editable draft (frozen HTTP body). Times are RFC 3339 on the wire.
type DraftInput struct {
	AdBindingID         string    `json:"ad_binding_id"`
	IdentityBindingID   string    `json:"identity_binding_id"`
	Template            string    `json:"template"`
	SourceRef           string    `json:"source_ref"`
	Currency            string    `json:"currency"`
	LifetimeBudgetMinor int64     `json:"lifetime_budget_minor"`
	StartsAt            time.Time `json:"starts_at"`
	EndsAt              time.Time `json:"ends_at"`
	Countries           []string  `json:"countries"`
	AgeMin              int       `json:"age_min"`
	AgeMax              int       `json:"age_max"`
}

var (
	sourceBoostPost = regexp.MustCompile(`^[0-9_]{1,80}$`)
	countryPattern  = regexp.MustCompile(`^[A-Z]{2}$`)
)

const (
	// minLeadTime is the earliest start after now (contract 5.2: starts_at >= now + 10 min).
	minLeadTime = 10 * time.Minute
	maxDuration = 30 * 24 * time.Hour
	// minBudgetMinor is one whole currency unit in minor units (hundredths): ponytail: min_daily_budget x days needs an
	// `md=` key in the preflight result grammar, which the frozen grammar lacks; add when MA-S1 shows Meta rejecting a small budget.
	minBudgetMinor = 100
	maxBudgetMinor = 1_000_000_000_000_000 // below 10^15: fits the 15-digit SQL cap and int64 arithmetic
)

// ValidateInput applies the local rules. allowanceCurrency "" skips the currency-equals-allowance rule (the caller does
// not know it; SQL enforces it). It returns a *Refusal with a frozen code, or nil.
func ValidateInput(in DraftInput, allowanceCurrency string, now time.Time) error {
	if !command.ValidID(in.AdBindingID) || !command.ValidID(in.IdentityBindingID) ||
		(in.Template != "BOOST_POST" && in.Template != "PRODUCT_TRAFFIC") {
		return refusal("invalid_request")
	}
	switch in.Template {
	case "BOOST_POST":
		if !sourceBoostPost.MatchString(in.SourceRef) {
			return refusal("source_not_owned")
		}
	default:
		if !command.ValidID(in.SourceRef) {
			return refusal("product_not_published")
		}
	}
	if in.Currency != "TWD" && in.Currency != "USD" && in.Currency != "HKD" {
		return refusal("invalid_request")
	}
	if allowanceCurrency != "" && in.Currency != allowanceCurrency {
		return refusal("currency_mismatch")
	}
	// I05: TWD budgets are whole NT$ (Meta offset 1, F19); the check comes before the size rules so 12345 is not_whole_unit.
	if in.Currency == "TWD" && in.LifetimeBudgetMinor%100 != 0 {
		return refusal("not_whole_unit")
	}
	if in.LifetimeBudgetMinor > maxBudgetMinor || in.LifetimeBudgetMinor < 1 {
		return refusal("invalid_request")
	}
	if in.LifetimeBudgetMinor < minBudgetMinor {
		return refusal("budget_below_minimum")
	}
	// SQL truncates to whole seconds (the canonical hash uses unix seconds); compare the same way.
	starts, ends := in.StartsAt.UTC().Truncate(time.Second), in.EndsAt.UTC().Truncate(time.Second)
	if starts.Before(now.Add(minLeadTime)) {
		return refusal("starts_too_soon")
	}
	if !ends.After(starts) || ends.Sub(starts) > maxDuration {
		return refusal("invalid_request")
	}
	if n := len(in.Countries); n < 1 || n > 10 {
		return refusal("invalid_request")
	}
	seen := map[string]bool{}
	for _, c := range in.Countries {
		if !countryPattern.MatchString(c) || seen[c] {
			return refusal("invalid_request")
		}
		seen[c] = true
	}
	if in.AgeMin < 18 || in.AgeMin > 65 || in.AgeMax < in.AgeMin || in.AgeMax > 65 {
		return refusal("invalid_request")
	}
	return nil
}

// sortedCountries returns the countries in the stored (bytewise) order, as SQL stores and hashes them.
func sortedCountries(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
