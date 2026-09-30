// standing.go: the Go twin of billing.store_standing (contract BD4, gate CB01). The SQL function is
// the authority (the claim-window guard and every reader use it); this pure function exists so the
// mapping is unit-testable without PG and so callers can explain a status list without a query.
// It touches no PG and no network.

package billing

// Standing is a store's derived billing standing. It is never stored (BD4).
type Standing string

const (
	// Good: a trialing or active subscription exists.
	Good Standing = "GOOD"
	// Grace: only past_due subscriptions (banner only, nothing is blocked).
	Grace Standing = "GRACE"
	// Restricted: only unpaid/canceled/paused subscriptions remain; new claim windows are refused (BD5).
	Restricted Standing = "RESTRICTED"
	// Unbilled: no subscription counts (pilot stores, Q4). Unrestricted.
	Unbilled Standing = "UNBILLED"
)

// StandingOf maps subscription statuses to a standing exactly as billing.store_standing does:
// incomplete and incomplete_expired are filtered out first (F-B1: incomplete lasts 23 h and must never
// improve standing), nothing left means UNBILLED, and the best remaining status wins
// (trialing/active > past_due > everything else). An unknown status counts as RESTRICTED, the SQL
// ELSE branch (the column CHECK makes it unreachable in PG).
func StandingOf(statuses []string) Standing {
	counted, grace := 0, false
	for _, status := range statuses {
		switch status {
		case "incomplete", "incomplete_expired":
			continue
		case "trialing", "active":
			return Good
		case "past_due":
			grace = true
		}
		counted++
	}
	switch {
	case counted == 0:
		return Unbilled
	case grace:
		return Grace
	default:
		return Restricted
	}
}

// valid reports whether s is one of the four wire values (guards a database result before it is exposed).
func (s Standing) valid() bool {
	return s == Good || s == Grace || s == Restricted || s == Unbilled
}
