package foundation_test

// CB01 (contracts/customers-billing-v1.md §8; tier UNIT, no PG). Written from the contract only:
//   - BD4 standing mapping: all 8 Stripe statuses (F-B1), none, mixed rows, `incomplete`-only -> UNBILLED,
//     canceled + incomplete -> RESTRICTED (the round-1 bypass), permutation independence;
//   - CD4 current-consent derivation: latest event per (purpose, channel), absence = not granted,
//     withdrawal, re-grant, independence of the two purposes, insensitivity to slice order;
//   - the frozen export/privacy constants of customers-core.md.
// Tier note (brief T2): PT412 -> HTTP 402 `billing_restricted` and the export schema/bounds are asserted
// through the real handlers in CB06/CB09 (they need PG). Helper prefix `cbu`.

import (
	"fmt"
	"testing"

	"livecommerce/internal/billing"
	"livecommerce/internal/customers"
)

var cbuStatuses = []string{"incomplete", "incomplete_expired", "trialing", "active", "past_due", "canceled", "unpaid", "paused"}

func cbuPermutations(in []string) [][]string {
	if len(in) <= 1 {
		return [][]string{append([]string(nil), in...)}
	}
	var out [][]string
	for i := range in {
		rest := append(append([]string(nil), in[:i]...), in[i+1:]...)
		for _, p := range cbuPermutations(rest) {
			out = append(out, append([]string{in[i]}, p...))
		}
	}
	return out
}

func cbuEvent(purpose, channel string, granted bool, source, at string) customers.ConsentEvent {
	return customers.ConsentEvent{Purpose: purpose, Channel: channel, Granted: granted, Source: source, PolicyVersion: "lc-2026-10", OccurredAt: at}
}

func TestCustomersBillingCB01Unit(t *testing.T) {
	t.Run("BD4 each of the 8 statuses alone", func(t *testing.T) {
		want := map[string]billing.Standing{
			"trialing": billing.Good, "active": billing.Good,
			"past_due": billing.Grace,
			"unpaid":   billing.Restricted, "canceled": billing.Restricted, "paused": billing.Restricted,
			"incomplete": billing.Unbilled, "incomplete_expired": billing.Unbilled, // never improve, never restrict (BD4)
		}
		if len(want) != len(cbuStatuses) {
			t.Fatal("the table must cover every status of F-B1")
		}
		for _, s := range cbuStatuses {
			if got := billing.StandingOf([]string{s}); got != want[s] {
				t.Errorf("StandingOf([%s]) = %s, want %s", s, got, want[s])
			}
		}
	})

	t.Run("BD4 none, nil and the wire values", func(t *testing.T) {
		for name, in := range map[string][]string{"nil": nil, "empty": {}} {
			if got := billing.StandingOf(in); got != billing.Unbilled {
				t.Errorf("%s: %s, want UNBILLED (Q4 pilot stores are unrestricted)", name, got)
			}
		}
		for got, want := range map[billing.Standing]string{billing.Good: "GOOD", billing.Grace: "GRACE", billing.Restricted: "RESTRICTED", billing.Unbilled: "UNBILLED"} {
			if string(got) != want {
				t.Errorf("wire value %q, want %q", got, want)
			}
		}
	})

	t.Run("BD4 mixed rows: best remaining status wins, incomplete filtered first", func(t *testing.T) {
		cases := []struct {
			name string
			in   []string
			want billing.Standing
		}{
			{"canceled + incomplete (the 23 h lift, round-1 P1)", []string{"canceled", "incomplete"}, billing.Restricted},
			{"canceled + incomplete_expired", []string{"canceled", "incomplete_expired"}, billing.Restricted},
			{"unpaid + incomplete", []string{"unpaid", "incomplete"}, billing.Restricted},
			{"incomplete + incomplete_expired only", []string{"incomplete", "incomplete_expired"}, billing.Unbilled},
			{"canceled + active", []string{"canceled", "active"}, billing.Good},
			{"canceled + trialing", []string{"canceled", "trialing"}, billing.Good},
			{"past_due + unpaid", []string{"past_due", "unpaid"}, billing.Grace},
			{"past_due + canceled + paused", []string{"past_due", "canceled", "paused"}, billing.Grace},
			{"past_due + trialing", []string{"past_due", "trialing"}, billing.Good},
			{"incomplete + past_due", []string{"incomplete", "past_due"}, billing.Grace},
			{"unpaid + canceled + paused", []string{"unpaid", "canceled", "paused"}, billing.Restricted},
			{"all eight", cbuStatuses, billing.Good},
			{"all but the two good ones", []string{"incomplete", "incomplete_expired", "past_due", "canceled", "unpaid", "paused"}, billing.Grace},
		}
		for _, c := range cases {
			for i, p := range cbuPermutations(c.in) {
				if len(c.in) > 6 && i%97 != 0 { // 40320 permutations of 8: sample, the small sets are exhaustive
					continue
				}
				if got := billing.StandingOf(p); got != c.want {
					t.Fatalf("%s: StandingOf(%v) = %s, want %s (order must not matter)", c.name, p, got, c.want)
				}
			}
		}
	})

	t.Run("CD4 consent derivation", func(t *testing.T) {
		const mm, dm, ap, ads = customers.PurposeMarketingMessages, customers.ChannelMetaDM, customers.PurposeAdsPersonalization, customers.ChannelMetaAds
		day := func(n int) string { return fmt.Sprintf("2026-09-%02dT10:00:00.000000Z", n) }
		cases := []struct {
			name string
			hist []customers.ConsentEvent
			want customers.Consents
		}{
			{"absence is not granted", nil, customers.Consents{}},
			{"grant marketing", []customers.ConsentEvent{cbuEvent(mm, dm, true, "buyer_checkout", day(1))}, customers.Consents{MarketingMessages: true}},
			{"grant ads only", []customers.ConsentEvent{cbuEvent(ap, ads, true, "buyer_settings", day(1))}, customers.Consents{AdsPersonalization: true}},
			{"grant then buyer withdraw", []customers.ConsentEvent{cbuEvent(mm, dm, true, "buyer_checkout", day(1)), cbuEvent(mm, dm, false, "buyer_settings", day(2))}, customers.Consents{}},
			{"grant then merchant withdraw", []customers.ConsentEvent{cbuEvent(mm, dm, true, "buyer_checkout", day(1)), cbuEvent(mm, dm, false, "merchant_recorded", day(2))}, customers.Consents{}},
			{"grant then erasure withdraws both", []customers.ConsentEvent{
				cbuEvent(mm, dm, true, "buyer_checkout", day(1)), cbuEvent(ap, ads, true, "buyer_checkout", day(1)),
				cbuEvent(mm, dm, false, "erasure", day(3)), cbuEvent(ap, ads, false, "erasure", day(3))}, customers.Consents{}},
			{"withdraw then re-grant", []customers.ConsentEvent{
				cbuEvent(mm, dm, true, "buyer_checkout", day(1)), cbuEvent(mm, dm, false, "buyer_settings", day(2)), cbuEvent(mm, dm, true, "buyer_settings", day(3))}, customers.Consents{MarketingMessages: true}},
			{"purposes are independent", []customers.ConsentEvent{
				cbuEvent(mm, dm, true, "buyer_checkout", day(1)), cbuEvent(ap, ads, true, "buyer_checkout", day(1)), cbuEvent(ap, ads, false, "buyer_settings", day(2))}, customers.Consents{MarketingMessages: true}},
			{"withdrawal of a never-granted pair", []customers.ConsentEvent{cbuEvent(ap, ads, false, "merchant_recorded", day(1))}, customers.Consents{}},
			{"an invalid pair is never a grant", []customers.ConsentEvent{cbuEvent(mm, ads, true, "buyer_checkout", day(1)), cbuEvent(ap, dm, true, "buyer_checkout", day(1))}, customers.Consents{}},
		}
		for _, c := range cases {
			// the result depends on occurred_at only, never on where the event sits in the slice
			rev := make([]customers.ConsentEvent, len(c.hist))
			for i, e := range c.hist {
				rev[len(rev)-1-i] = e
			}
			if got := customers.CurrentConsents(c.hist); got != c.want {
				t.Errorf("%s (oldest first): %+v, want %+v", c.name, got, c.want)
			}
			if got := customers.CurrentConsents(rev); got != c.want {
				t.Errorf("%s (newest first): %+v, want %+v", c.name, got, c.want)
			}
		}
	})

	t.Run("frozen constants", func(t *testing.T) {
		if customers.ExportFormat != "lc.customer-export.v1" || customers.MaxExportBytes != 1<<20 || customers.MaxExportOrders != 200 {
			t.Errorf("export constants: %q %d %d", customers.ExportFormat, customers.MaxExportBytes, customers.MaxExportOrders)
		}
		if customers.PrivacyPolicyVersion != "lc-2026-10" {
			t.Errorf("PrivacyPolicyVersion %q (Q8: lc-2026-10)", customers.PrivacyPolicyVersion)
		}
		if customers.PurposeMarketingMessages != "marketing_messages" || customers.ChannelMetaDM != "meta_dm" ||
			customers.PurposeAdsPersonalization != "ads_personalization" || customers.ChannelMetaAds != "meta_ads" {
			t.Error("purpose/channel constants drifted from CD4")
		}
	})
}
