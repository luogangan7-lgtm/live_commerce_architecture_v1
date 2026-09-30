package ads

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"livecommerce/internal/httperror"
)

// validate_test.go: UNIT tests of the pure local validation (contract 5.2) and the refusal/status table.

func goodInput(now time.Time) DraftInput {
	return DraftInput{AdBindingID: "11111111-1111-4111-8111-111111111111", IdentityBindingID: "22222222-2222-4222-8222-222222222222",
		Template: "BOOST_POST", SourceRef: "111_222", Currency: "TWD", LifetimeBudgetMinor: 300000,
		StartsAt: now.Add(11 * time.Minute), EndsAt: now.Add(48 * time.Hour), Countries: []string{"TW"}, AgeMin: 18, AgeMax: 65}
}

func code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err == nil {
		return ""
	}
	return "other:" + err.Error()
}

func TestValidateInputMatrix(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*DraftInput)
		allow  string
		want   string
	}{
		{"ok", func(*DraftInput) {}, "TWD", ""},
		{"ok without allowance knowledge", func(*DraftInput) {}, "", ""},
		{"bad ad binding", func(d *DraftInput) { d.AdBindingID = "x" }, "", "invalid_request"},
		{"bad template", func(d *DraftInput) { d.Template = "DELETE" }, "", "invalid_request"},
		{"boost source shape", func(d *DraftInput) { d.SourceRef = "abc" }, "", "source_not_owned"},
		{"product needs uuid", func(d *DraftInput) { d.Template = "PRODUCT_TRAFFIC"; d.SourceRef = "111_222" }, "", "product_not_published"},
		{"product ok", func(d *DraftInput) {
			d.Template = "PRODUCT_TRAFFIC"
			d.SourceRef = "33333333-3333-4333-8333-333333333333"
		}, "", ""},
		{"unknown currency", func(d *DraftInput) { d.Currency = "EUR" }, "", "invalid_request"},
		{"currency vs allowance", func(d *DraftInput) { d.Currency = "USD" }, "TWD", "currency_mismatch"},
		{"twd not whole unit", func(d *DraftInput) { d.LifetimeBudgetMinor = 12345 }, "", "not_whole_unit"},
		{"twd 99 minor", func(d *DraftInput) { d.LifetimeBudgetMinor = 99 }, "", "not_whole_unit"},
		{"usd 99 minor below minimum", func(d *DraftInput) { d.Currency = "USD"; d.LifetimeBudgetMinor = 99 }, "USD", "budget_below_minimum"},
		{"usd 1 minor (cent) is fine-grained but below one unit", func(d *DraftInput) { d.Currency = "HKD"; d.LifetimeBudgetMinor = 1 }, "", "budget_below_minimum"},
		{"zero budget", func(d *DraftInput) { d.LifetimeBudgetMinor = 0 }, "", "invalid_request"},
		{"starts too soon", func(d *DraftInput) { d.StartsAt = now.Add(9 * time.Minute) }, "", "starts_too_soon"},
		{"ends before start", func(d *DraftInput) { d.EndsAt = d.StartsAt }, "", "invalid_request"},
		{"longer than 30 days", func(d *DraftInput) { d.EndsAt = d.StartsAt.Add(30*24*time.Hour + time.Second) }, "", "invalid_request"},
		{"exactly 30 days", func(d *DraftInput) { d.EndsAt = d.StartsAt.Add(30 * 24 * time.Hour) }, "", ""},
		{"no countries", func(d *DraftInput) { d.Countries = nil }, "", "invalid_request"},
		{"lowercase country", func(d *DraftInput) { d.Countries = []string{"tw"} }, "", "invalid_request"},
		{"duplicate country", func(d *DraftInput) { d.Countries = []string{"TW", "TW"} }, "", "invalid_request"},
		{"eleven countries", func(d *DraftInput) {
			d.Countries = []string{"TW", "HK", "US", "JP", "KR", "SG", "MY", "TH", "VN", "PH", "ID"}
		}, "", "invalid_request"},
		{"age below 18", func(d *DraftInput) { d.AgeMin = 17 }, "", "invalid_request"},
		{"age max above 65", func(d *DraftInput) { d.AgeMax = 66 }, "", "invalid_request"},
		{"age max below min", func(d *DraftInput) { d.AgeMin = 30; d.AgeMax = 29 }, "", "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := goodInput(now)
			tc.mutate(&in)
			if got := code(ValidateInput(in, tc.allow, now)); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// The start check uses whole seconds like SQL and the canonical hash: a start 10 min + 999 ms ahead truncates to exactly the boundary.
func TestValidateInputTruncatesToSeconds(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	in := goodInput(now)
	in.StartsAt = now.Add(10*time.Minute + 999*time.Millisecond)
	in.EndsAt = in.StartsAt.Add(time.Hour)
	if got := code(ValidateInput(in, "", now)); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRefusalStatusesFollowTheFrozenTable(t *testing.T) {
	want := map[string]int{"state_mismatch": 409, "state_expired": 410, "billing_restricted": 402, "over_allowance": 409,
		"attempt_changed": 409, "prior_attempt_not_paused": 409, "draft_approved": 409, "budget_below_minimum": 422,
		"not_whole_unit": 422, "currency_mismatch": 422, "starts_too_soon": 422, "binding_disabled": 409,
		"source_not_owned": 422, "product_not_published": 422, "forbidden": 403, "not_found": 404, "invalid_request": 422,
		"not_in_pick_list": 422, "client_business_changed": 409, "revision_changed": 409, "meta_connect_failed": 502}
	for code, status := range want {
		if got := refusal(code); got.Status != status || got.Code != code {
			t.Fatalf("%s -> %+v want %d", code, got, status)
		}
	}
	if refusal("something_new").Status != 409 {
		t.Fatal("unknown codes must default to 409")
	}
}

// Every frozen refusal code must survive httperror's message allowlist; a code missing there reaches the
// merchant as "internal" (the MA04 finding).
func TestFrozenCodesSurviveHTTPError(t *testing.T) {
	for c, status := range frozenStatus {
		w := httptest.NewRecorder()
		httperror.Write(w, status, c)
		var e httperror.Envelope
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Code != c {
			t.Errorf("code %q is rewritten to %q by httperror (add it to the message table)", c, e.Code)
		}
	}
}
