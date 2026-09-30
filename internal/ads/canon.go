package ads

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"
)

// canon.go holds the canonical draft string of D10 (AD5). It touches no table: the approval freezes the SHA-256 of
// this string, SQL ads.canonical_draft(uuid) produces the same bytes, and the MA01 vector gate proves they agree.

// DraftCanon is the D10 field list, in D10 order after the constant "v1" tag. IdentityBindingVersion is the identity
// (Page/Instagram) binding's CURRENT semantic version at hashing time, not a stored copy, so re-pointing the identity
// binding invalidates an existing approval.
type DraftCanon struct {
	Template               string
	AdBindingID            string
	AdBindingVersion       int64
	IdentityBindingID      string
	IdentityBindingVersion int64
	SourceRef              string
	Currency               string
	LifetimeBudgetMinor    int64
	StartsAtUnix           int64
	EndsAtUnix             int64
	Countries              []string
	AgeMin                 int
	AgeMax                 int
}

// CanonicalDraft returns the canonical bytes of D10:
// v1|template|ad_binding_id|ad_binding_version|identity_binding_id|identity_binding_version|source_ref|currency|
// lifetime_budget_minor|starts_at unix|ends_at unix|countries sorted, comma|age_min|age_max. Countries are sorted bytewise
// (SQL sorts COLLATE "C"); the input slice is not modified.
func CanonicalDraft(d DraftCanon) []byte {
	countries := append([]string(nil), d.Countries...)
	sort.Strings(countries)
	var b strings.Builder
	b.WriteString("v1|")
	b.WriteString(d.Template)
	b.WriteByte('|')
	b.WriteString(d.AdBindingID)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(d.AdBindingVersion, 10))
	b.WriteByte('|')
	b.WriteString(d.IdentityBindingID)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(d.IdentityBindingVersion, 10))
	b.WriteByte('|')
	b.WriteString(d.SourceRef)
	b.WriteByte('|')
	b.WriteString(d.Currency)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(d.LifetimeBudgetMinor, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(d.StartsAtUnix, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(d.EndsAtUnix, 10))
	b.WriteByte('|')
	b.WriteString(strings.Join(countries, ","))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(d.AgeMin))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(d.AgeMax))
	return []byte(b.String())
}

// DraftSHA256 is the SHA-256 of CanonicalDraft: the value ads.draft_approvals.draft_sha256 freezes.
func DraftSHA256(d DraftCanon) [32]byte { return sha256.Sum256(CanonicalDraft(d)) }
