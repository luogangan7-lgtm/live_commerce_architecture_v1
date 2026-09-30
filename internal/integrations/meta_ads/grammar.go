package metaads

// grammar.go: read results ride in integration.operations.provider_reference (meta-ads-v1 §3, no
// schema change). ads.put_insights_day and the ads sweeper parse these strings, so encode and parse
// are strict inverses and both refuse anything outside the closed grammar.
//
// ponytail: results ride in provider_reference (no schema change); add an operation result column
// if a read ever needs more than the 200-character cap (ads-core D2: 0008 CHECK + core.validReference).

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// maxReference is ads-core D2: the operation CHECK and core.validReference cap at 200, not §3's 255.
const maxReference = 200

var (
	// ErrGrammar is every encode/parse refusal of a read result.
	ErrGrammar = errors.New("metaads: result outside the read-result grammar")

	// referencePattern is contract §3's grammar with the key length widened from {2,3} to {2,4}: the
	// contract's own preflight key `fund` has four letters, so its printed regex rejects its own example.
	referencePattern = regexp.MustCompile(`^v1(;[a-z]{2,4}=[A-Za-z0-9_./+-]{1,40}){1,9}$`)
	tzPattern        = regexp.MustCompile(`^[A-Za-z0-9_./+-]{1,40}$`)
	currencyPattern  = regexp.MustCompile(`^[A-Z]{3}$`)
	statusEnum       = regexp.MustCompile(`^[A-Z_]{1,40}$`)
	minorPattern     = regexp.MustCompile(`^[0-9]{1,15}\.[0-9]{2}$`)
	countPattern     = regexp.MustCompile(`^[0-9]{1,15}$`)
)

// Preflight is one account readiness read: `v1;st=<account_status>;cur=<ISO>;fund=<0|1>;tz=<tz>`.
type Preflight struct {
	Status   int
	Currency string
	Timezone string
	Funded   bool
}

// InsightsDay is one campaign-day read:
// `v1;es=<effective_status>;sp=<dec>;im=<int>;cl=<int>;pu=<int|na>;pv=<dec|na>;cur=<ISO>;tz=<tz>`.
// Money is in hundredths of Currency (I05); nil Purchases/PurchaseValueMinor mean Meta reported none ("na").
type InsightsDay struct {
	EffectiveStatus, Currency, Timezone string
	SpendMinor, Impressions, Clicks     int64
	Purchases, PurchaseValueMinor       *int64
}

func minorString(v int64) string { return fmt.Sprintf("%d.%02d", v/100, v%100) }

// EncodePreflight renders p; an out-of-range field or a result over 200 characters is ErrGrammar.
func EncodePreflight(p Preflight) (string, error) {
	if p.Status < 0 || p.Status > 999 || !currencyPattern.MatchString(p.Currency) || !tzPattern.MatchString(p.Timezone) {
		return "", ErrGrammar
	}
	fund := "0"
	if p.Funded {
		fund = "1"
	}
	return checked(fmt.Sprintf("v1;st=%d;cur=%s;fund=%s;tz=%s", p.Status, p.Currency, fund, p.Timezone))
}

// ParsePreflight is the strict inverse of EncodePreflight.
func ParsePreflight(ref string) (Preflight, error) {
	f, err := fields(ref, "st", "cur", "fund", "tz")
	if err != nil {
		return Preflight{}, err
	}
	st, e1 := strconv.Atoi(f[0])
	if e1 != nil || !countPattern.MatchString(f[0]) || st > 999 || !currencyPattern.MatchString(f[1]) ||
		(f[2] != "0" && f[2] != "1") || !tzPattern.MatchString(f[3]) {
		return Preflight{}, ErrGrammar
	}
	return Preflight{Status: st, Currency: f[1], Funded: f[2] == "1", Timezone: f[3]}, nil
}

// EncodeInsights renders d; negative numbers, a bad enum/currency/tz or > 200 characters is ErrGrammar.
func EncodeInsights(d InsightsDay) (string, error) {
	if !statusEnum.MatchString(d.EffectiveStatus) || !currencyPattern.MatchString(d.Currency) || !tzPattern.MatchString(d.Timezone) ||
		d.SpendMinor < 0 || d.Impressions < 0 || d.Clicks < 0 ||
		(d.Purchases != nil && *d.Purchases < 0) || (d.PurchaseValueMinor != nil && *d.PurchaseValueMinor < 0) {
		return "", ErrGrammar
	}
	pu, pv := "na", "na"
	if d.Purchases != nil {
		pu = strconv.FormatInt(*d.Purchases, 10)
	}
	if d.PurchaseValueMinor != nil {
		pv = minorString(*d.PurchaseValueMinor)
	}
	return checked(fmt.Sprintf("v1;es=%s;sp=%s;im=%d;cl=%d;pu=%s;pv=%s;cur=%s;tz=%s",
		d.EffectiveStatus, minorString(d.SpendMinor), d.Impressions, d.Clicks, pu, pv, d.Currency, d.Timezone))
}

// ParseInsights is the strict inverse of EncodeInsights.
func ParseInsights(ref string) (InsightsDay, error) {
	f, err := fields(ref, "es", "sp", "im", "cl", "pu", "pv", "cur", "tz")
	if err != nil {
		return InsightsDay{}, err
	}
	d := InsightsDay{EffectiveStatus: f[0], Currency: f[6], Timezone: f[7]}
	if !statusEnum.MatchString(d.EffectiveStatus) || !currencyPattern.MatchString(d.Currency) || !tzPattern.MatchString(d.Timezone) ||
		!minorPattern.MatchString(f[1]) || !countPattern.MatchString(f[2]) || !countPattern.MatchString(f[3]) {
		return InsightsDay{}, ErrGrammar
	}
	d.SpendMinor = mustMinor(f[1])
	d.Impressions, _ = strconv.ParseInt(f[2], 10, 64)
	d.Clicks, _ = strconv.ParseInt(f[3], 10, 64)
	if f[4] != "na" {
		if !countPattern.MatchString(f[4]) {
			return InsightsDay{}, ErrGrammar
		}
		v, _ := strconv.ParseInt(f[4], 10, 64)
		d.Purchases = &v
	}
	if f[5] != "na" {
		if !minorPattern.MatchString(f[5]) {
			return InsightsDay{}, ErrGrammar
		}
		v := mustMinor(f[5])
		d.PurchaseValueMinor = &v
	}
	return d, nil
}

// mustMinor converts a string already matched by minorPattern (15 digits max, so no overflow).
func mustMinor(s string) int64 {
	whole, frac, _ := strings.Cut(s, ".")
	w, _ := strconv.ParseInt(whole, 10, 64)
	f, _ := strconv.ParseInt(frac, 10, 64)
	return w*100 + f
}

func checked(ref string) (string, error) {
	if len(ref) > maxReference || !referencePattern.MatchString(ref) {
		return "", ErrGrammar
	}
	return ref, nil
}

// fields splits ref into the values of exactly the given keys, in order.
func fields(ref string, keys ...string) ([]string, error) {
	if len(ref) > maxReference || !referencePattern.MatchString(ref) {
		return nil, ErrGrammar
	}
	parts := strings.Split(ref, ";")[1:]
	if len(parts) != len(keys) {
		return nil, ErrGrammar
	}
	out := make([]string, len(keys))
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k != keys[i] {
			return nil, ErrGrammar
		}
		out[i] = v
	}
	return out, nil
}
