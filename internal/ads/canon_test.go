package ads

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// canon_test.go: MOCK/UNIT vectors of the D10 canonical draft string (AD5). The SQL twin (ads.canonical_draft) is proved
// byte-identical in the REAL_PG flow test; the frozen MA01 hash vector belongs to the independent test unit.

func vectorDraft() DraftCanon {
	return DraftCanon{Template: "BOOST_POST", AdBindingID: "11111111-1111-4111-8111-111111111111", AdBindingVersion: 3,
		IdentityBindingID: "22222222-2222-4222-8222-222222222222", IdentityBindingVersion: 7, SourceRef: "111_222",
		Currency: "TWD", LifetimeBudgetMinor: 300000, StartsAtUnix: 1790000000, EndsAtUnix: 1790172800,
		Countries: []string{"TW", "HK"}, AgeMin: 18, AgeMax: 65}
}

func TestCanonicalDraftVector(t *testing.T) {
	const want = "v1|BOOST_POST|11111111-1111-4111-8111-111111111111|3|22222222-2222-4222-8222-222222222222|7|111_222|TWD|300000|1790000000|1790172800|HK,TW|18|65"
	if got := string(CanonicalDraft(vectorDraft())); got != want {
		t.Fatalf("canonical draft\n got %s\nwant %s", got, want)
	}
	sum := sha256.Sum256([]byte(want))
	if got := DraftSHA256(vectorDraft()); got != sum || hex.EncodeToString(got[:]) != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash mismatch: %x", got)
	}
}

func TestCanonicalDraftCountriesSortedAndInputUntouched(t *testing.T) {
	d := vectorDraft()
	d.Countries = []string{"US", "TW", "HK"}
	if got := string(CanonicalDraft(d)); got[len(got)-len("|HK,TW,US|18|65"):] != "|HK,TW,US|18|65" {
		t.Fatalf("countries not sorted: %s", got)
	}
	if d.Countries[0] != "US" {
		t.Fatal("CanonicalDraft reordered the caller's slice")
	}
}

func TestCanonicalDraftChangesWithEveryField(t *testing.T) {
	base := DraftSHA256(vectorDraft())
	mutations := map[string]func(*DraftCanon){
		"template":     func(d *DraftCanon) { d.Template = "PRODUCT_TRAFFIC" },
		"ad binding":   func(d *DraftCanon) { d.AdBindingID = "33333333-3333-4333-8333-333333333333" },
		"ad version":   func(d *DraftCanon) { d.AdBindingVersion++ },
		"identity":     func(d *DraftCanon) { d.IdentityBindingID = "44444444-4444-4444-8444-444444444444" },
		"identity ver": func(d *DraftCanon) { d.IdentityBindingVersion++ },
		"source":       func(d *DraftCanon) { d.SourceRef = "111_223" },
		"currency":     func(d *DraftCanon) { d.Currency = "USD" },
		"budget":       func(d *DraftCanon) { d.LifetimeBudgetMinor += 100 },
		"start":        func(d *DraftCanon) { d.StartsAtUnix++ },
		"end":          func(d *DraftCanon) { d.EndsAtUnix++ },
		"countries":    func(d *DraftCanon) { d.Countries = []string{"TW"} },
		"age min":      func(d *DraftCanon) { d.AgeMin++ },
		"age max":      func(d *DraftCanon) { d.AgeMax-- },
	}
	for name, mutate := range mutations {
		d := vectorDraft()
		mutate(&d)
		if DraftSHA256(d) == base {
			t.Fatalf("hash ignores %s", name)
		}
	}
}
