package metaads

// money_grammar_test.go: unit gates for the I05 conversions and the read-result grammar. Independent
// gate MA01 (ads-tests) re-proves these from the contract; these are the implementer's own vectors.

import (
	"errors"
	"strings"
	"testing"
)

func TestMetaBudgetVectors(t *testing.T) {
	for _, c := range []struct {
		cur   string
		minor int64
		want  int64
		err   error
	}{
		{"TWD", 1234500, 12345, nil},
		{"TWD", 100, 1, nil},
		{"TWD", 0, 0, nil},
		{"TWD", 12345, 0, ErrNotWholeUnit}, // never truncates money
		{"TWD", 199, 0, ErrNotWholeUnit},
		{"USD", 12345, 12345, nil},
		{"HKD", 9900, 9900, nil},
		{"JPY", 100, 0, ErrUnsupportedCurrency},
		{"", 100, 0, ErrUnsupportedCurrency},
		{"USD", -1, 0, ErrBadSpend},
	} {
		got, err := MetaBudget(c.cur, c.minor)
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("MetaBudget(%q,%d) = %d,%v want %d,%v", c.cur, c.minor, got, err, c.want, c.err)
		}
	}
}

func TestSpendMinorVectors(t *testing.T) {
	for _, c := range []struct {
		s    string
		want int64
		err  error
	}{
		{"0", 0, nil}, {"12.3", 1230, nil}, {"12.34", 1234, nil}, {"0.05", 5, nil}, {"1000", 100000, nil},
		{"12.345", 0, ErrBadSpend}, {"-1", 0, ErrBadSpend}, {"1e3", 0, ErrBadSpend}, {"+1", 0, ErrBadSpend},
		{"", 0, ErrBadSpend}, {".5", 0, ErrBadSpend}, {"5.", 0, ErrBadSpend}, {" 1", 0, ErrBadSpend},
		{"12.340", 0, ErrBadSpend}, {"1,5", 0, ErrBadSpend}, {strings.Repeat("9", 16), 0, ErrBadSpend},
	} {
		got, err := SpendMinor("USD", c.s)
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("SpendMinor(%q) = %d,%v want %d,%v", c.s, got, err, c.want, c.err)
		}
	}
	if _, err := SpendMinor("JPY", "1"); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Fatalf("JPY = %v", err)
	}
	// 15 nines is the largest accepted whole part and must not overflow int64.
	if got, err := SpendMinor("TWD", strings.Repeat("9", 15)); err != nil || got != 99999999999999900 {
		t.Fatalf("max = %d,%v", got, err)
	}
}

func ptr(v int64) *int64 { return &v }

func TestReadResultGrammarRoundTrip(t *testing.T) {
	p := Preflight{Status: 1, Currency: "TWD", Timezone: "Asia/Taipei", Funded: true}
	ref, err := EncodePreflight(p)
	if err != nil || ref != "v1;st=1;cur=TWD;fund=1;tz=Asia/Taipei" {
		t.Fatalf("preflight = %q,%v", ref, err)
	}
	if back, err := ParsePreflight(ref); err != nil || back != p {
		t.Fatalf("parse = %+v,%v", back, err)
	}
	for _, d := range []InsightsDay{
		{EffectiveStatus: "CAMPAIGN_PAUSED", Currency: "TWD", Timezone: "Asia/Taipei"},
		{EffectiveStatus: "ACTIVE", Currency: "USD", Timezone: "America/Los_Angeles", SpendMinor: 123456, Impressions: 9000, Clicks: 12,
			Purchases: ptr(3), PurchaseValueMinor: ptr(99900)},
		{EffectiveStatus: "ACTIVE", Currency: "HKD", Timezone: "Asia/Hong_Kong", Purchases: ptr(0)},
	} {
		ref, err := EncodeInsights(d)
		if err != nil || len(ref) > 200 {
			t.Fatalf("encode %+v = %q,%v", d, ref, err)
		}
		back, err := ParseInsights(ref)
		if err != nil || back.EffectiveStatus != d.EffectiveStatus || back.SpendMinor != d.SpendMinor || back.Impressions != d.Impressions ||
			back.Clicks != d.Clicks || back.Currency != d.Currency || back.Timezone != d.Timezone ||
			(back.Purchases == nil) != (d.Purchases == nil) || (back.PurchaseValueMinor == nil) != (d.PurchaseValueMinor == nil) {
			t.Fatalf("round trip %q -> %+v,%v", ref, back, err)
		}
		if d.Purchases != nil && *back.Purchases != *d.Purchases {
			t.Fatalf("purchases %d", *back.Purchases)
		}
		if d.PurchaseValueMinor != nil && *back.PurchaseValueMinor != *d.PurchaseValueMinor {
			t.Fatalf("value %d", *back.PurchaseValueMinor)
		}
	}
	// The worst-case insights string still fits the 200 cap (D2); anything longer is refused.
	worst := InsightsDay{EffectiveStatus: strings.Repeat("A", 40), Currency: "TWD", Timezone: strings.Repeat("a", 40),
		SpendMinor: 99999999999999999, Impressions: 999999999999999, Clicks: 999999999999999, Purchases: ptr(999999999999999), PurchaseValueMinor: ptr(99999999999999999)}
	if ref, err := EncodeInsights(worst); err != nil || len(ref) > 200 {
		t.Fatalf("worst case must fit the 200 cap: %d,%v", len(ref), err)
	}
	if _, err := checked("v1;es=" + strings.Repeat("A", 40) + ";sp=" + strings.Repeat("1", 40) + ";im=" + strings.Repeat("1", 40) + ";cl=" + strings.Repeat("1", 40) + ";pu=" + strings.Repeat("1", 40)); !errors.Is(err, ErrGrammar) {
		t.Fatalf("over-long result must be refused, got %v", err)
	}
}

func TestReadResultGrammarRejects(t *testing.T) {
	for _, bad := range []Preflight{
		{Status: -1, Currency: "TWD", Timezone: "Asia/Taipei"}, {Status: 1000, Currency: "TWD", Timezone: "Asia/Taipei"},
		{Status: 1, Currency: "twd", Timezone: "Asia/Taipei"}, {Status: 1, Currency: "TWD", Timezone: "Asia Taipei"},
		{Status: 1, Currency: "TWD", Timezone: ""}, {Status: 1, Currency: "TWD", Timezone: "a;b=c"},
	} {
		if _, err := EncodePreflight(bad); !errors.Is(err, ErrGrammar) {
			t.Errorf("EncodePreflight(%+v) accepted", bad)
		}
	}
	for _, bad := range []string{
		"", "v2;st=1;cur=TWD;fund=1;tz=Asia/Taipei", "v1;cur=TWD;st=1;fund=1;tz=Asia/Taipei", // wrong order
		"v1;st=1;cur=TWD;fund=2;tz=Asia/Taipei", "v1;st=01x;cur=TWD;fund=1;tz=Asia/Taipei", "v1;st=1;cur=TWD;fund=1",
		"v1;st=1;cur=TWD;fund=1;tz=Asia/Taipei;x=y",
	} {
		if _, err := ParsePreflight(bad); !errors.Is(err, ErrGrammar) {
			t.Errorf("ParsePreflight(%q) accepted", bad)
		}
	}
	for _, bad := range []string{
		"v1;es=ACTIVE;sp=1.5;im=1;cl=1;pu=na;pv=na;cur=TWD;tz=Asia/Taipei",   // sp needs 2 fraction digits
		"v1;es=active;sp=1.50;im=1;cl=1;pu=na;pv=na;cur=TWD;tz=Asia/Taipei",  // enum is upper case
		"v1;es=ACTIVE;sp=1.50;im=-1;cl=1;pu=na;pv=na;cur=TWD;tz=Asia/Taipei", // negative
		"v1;es=ACTIVE;sp=1.50;im=1;cl=1;pu=x;pv=na;cur=TWD;tz=Asia/Taipei",   // pu
		"v1;es=ACTIVE;sp=1.50;im=1;cl=1;pu=na;pv=1;cur=TWD;tz=Asia/Taipei",   // pv
		"v1;es=ACTIVE;sp=1.50;im=1;cl=1;pu=na;pv=na;cur=TWD",                 // missing tz
	} {
		if _, err := ParseInsights(bad); !errors.Is(err, ErrGrammar) {
			t.Errorf("ParseInsights(%q) accepted", bad)
		}
	}
	if _, err := EncodeInsights(InsightsDay{EffectiveStatus: "ACTIVE", Currency: "TWD", Timezone: "Asia/Taipei", SpendMinor: -1}); !errors.Is(err, ErrGrammar) {
		t.Fatal("negative spend accepted")
	}
}
