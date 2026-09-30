// Pure unit tests for package claims (commerce_worker): error mapping, credentials and
// redaction, manual input validation, window transitions, ingest precedence/§3.1 shape
// against the canonical vectors, and redeem planning. Non-goals: no database; the
// independent test_worker owns TestClaimsKC02Units and the real-PG gates KC03–KC15.

package claims

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

const (
	tenantA  = "11111111-1111-4111-8111-111111111111"
	storeA   = "22222222-2222-4222-8222-222222222222"
	sessionA = "33333333-3333-4333-8333-333333333333"
)

func TestMapErrorTable(t *testing.T) {
	sentinel := errors.New("unchanged")
	for code, want := range map[string]error{
		"22023": command.ErrInvalid, "23514": command.ErrInvalid, "22P02": command.ErrInvalid,
		"23505": command.ErrConflict, "PT409": command.ErrConflict, "23503": command.ErrNotFound,
		"PT401": platform.ErrUnauthorized, "PT403": platform.ErrForbidden, "PT404": platform.ErrScopeNotFound,
	} {
		// DETAIL carries a would-be label; the mapped error must be the bare sentinel (P2(b)).
		pgErr := &pgconn.PgError{Code: code, Detail: "Key (label)=(amy) already exists."}
		got := mapError(fmt.Errorf("wrapped: %w", pgErr))
		if got != want || strings.Contains(got.Error(), "amy") {
			t.Errorf("mapError(%s) = %v, want bare %v", code, got, want)
		}
	}
	if got := mapError(pgx.ErrNoRows); got != command.ErrNotFound {
		t.Errorf("ErrNoRows -> %v", got)
	}
	for _, code := range []string{"40P01", "55P03", "57014", "42501"} {
		pgErr := &pgconn.PgError{Code: code}
		if got := mapError(pgErr); got != error(pgErr) {
			t.Errorf("%s must be returned unchanged, got %v", code, got)
		}
	}
	if mapError(nil) != nil || mapError(sentinel) != sentinel || mapError(command.ErrConflict) != command.ErrConflict {
		t.Error("nil, domain and unknown errors must pass through")
	}
}

func TestLabelKeyAndMAC(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := NewLabelKey(make([]byte, n)); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("NewLabelKey(%d bytes) err=%v", n, err)
		}
	}
	raw := bytes.Repeat([]byte{7}, 32)
	key, err := NewLabelKey(raw)
	if err != nil || !key.set {
		t.Fatalf("NewLabelKey: %v", err)
	}
	raw[0] = 8 // the key is a copy
	other, _ := NewLabelKey(bytes.Repeat([]byte{9}, 32))
	amy, _ := grammar.NormalizeLabel("@Amy ")
	same, _ := grammar.NormalizeLabel("amy")
	base := labelMAC(key, tenantA, storeA, sessionA, amy)
	if len(base) != 64 || base != labelMAC(key, tenantA, storeA, sessionA, same) {
		t.Fatalf("`@Amy ` and `amy` must share one MAC: %s", base)
	}
	if base != labelMAC(LabelKey{key: [32]byte(bytes.Repeat([]byte{7}, 32)), set: true}, tenantA, storeA, sessionA, amy) {
		t.Fatal("NewLabelKey must copy its input")
	}
	for name, mac := range map[string]string{
		"label":   labelMAC(key, tenantA, storeA, sessionA, "bob"),
		"tenant":  labelMAC(key, storeA, storeA, sessionA, amy),
		"store":   labelMAC(key, tenantA, sessionA, sessionA, amy),
		"session": labelMAC(key, tenantA, storeA, tenantA, amy),
		"key":     labelMAC(other, tenantA, storeA, sessionA, amy),
	} {
		if mac == base {
			t.Errorf("MAC does not vary with %s", name)
		}
	}
}

func TestParseLinkToken(t *testing.T) {
	good := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))
	if token, err := ParseLinkToken(good); err != nil || string(token) != good {
		t.Fatalf("canonical token rejected: %v", err)
	}
	fresh := newLinkToken()
	if _, err := ParseLinkToken(string(fresh)); err != nil || fresh == newLinkToken() {
		t.Fatalf("generated token not canonical or not random: %v", err)
	}
	last := good[:42] + "9" // same length, but non-zero trailing bits (index 61 = 0b111101)
	for _, bad := range []string{"", good[:42], good + "A", good + "=", strings.ReplaceAll(good, "-", "+"),
		strings.ReplaceAll(good, "_", "/"), last, " " + good[1:], good[:42] + "\n"} {
		if _, err := ParseLinkToken(bad); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("ParseLinkToken(%q) accepted", bad)
		}
	}
	if len(fresh.hash()) != 32 {
		t.Fatal("token hash must be SHA-256")
	}
}

func TestRedactedTypes(t *testing.T) {
	const text, label, token = "0912345678", "sentinel-label", "sentinel-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	actor := strings.Repeat("ab", 32)
	key, _ := NewLabelKey(bytes.Repeat([]byte{0x5a}, 32))
	values := []any{
		key, LinkToken(token), IssuedLink{Token: LinkToken(token), Generation: 7},
		ManualClaimInput{ActorLabel: label, Text: text},
		IngestInput{ActorKey: actor, ActorLabel: label, Text: text},
	}
	for _, v := range values {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, out := range []string{fmt.Sprintf("%v|%+v|%#v|%s|%q|%x|%d", v, v, v, v, v, v, v), string(encoded)} {
			for _, secret := range []string{text, label, token, actor, "5a5a5a", "ZZZZ"} {
				if strings.Contains(out, secret) {
					t.Fatalf("%T leaked %q: %s", v, secret, out)
				}
			}
			if !strings.Contains(out, "[redacted]") {
				t.Fatalf("%T not rendered as [redacted]: %s", v, out)
			}
		}
	}
	// ManualClaimInput stays decodable from JSON although it never encodes.
	var in ManualClaimInput
	if err := json.Unmarshal([]byte(`{"actor_label":"amy","text":"A1"}`), &in); err != nil || in.ActorLabel != "amy" || in.Text != "A1" {
		t.Fatalf("decode ManualClaimInput: %+v %v", in, err)
	}
}

func TestValidManual(t *testing.T) {
	key, _ := NewLabelKey(make([]byte, 32))
	bundle := "4444444a-4444-4444-8444-44444444444b"
	if label, err := validManual(key, sessionA, ManualClaimInput{ActorLabel: " @Amy ", Text: "A1+2"}); err != nil || label != "amy" {
		t.Fatalf("new actor: %q %v", label, err)
	}
	if label, err := validManual(key, sessionA, ManualClaimInput{BundleID: bundle, Text: "A1"}); err != nil || label != "" {
		t.Fatalf("existing bundle: %q %v", label, err)
	}
	for name, c := range map[string]struct {
		key     LabelKey
		session string
		in      ManualClaimInput
	}{
		"zero key":        {LabelKey{}, sessionA, ManualClaimInput{ActorLabel: "amy", Text: "A1"}},
		"bad session":     {key, "x", ManualClaimInput{ActorLabel: "amy", Text: "A1"}},
		"both":            {key, sessionA, ManualClaimInput{BundleID: bundle, ActorLabel: "amy", Text: "A1"}},
		"neither":         {key, sessionA, ManualClaimInput{Text: "A1"}},
		"bad bundle":      {key, sessionA, ManualClaimInput{BundleID: strings.ToUpper(bundle), Text: "A1"}},
		"bad label":       {key, sessionA, ManualClaimInput{ActorLabel: "@", Text: "A1"}},
		"empty text":      {key, sessionA, ManualClaimInput{ActorLabel: "amy"}},
		"long text":       {key, sessionA, ManualClaimInput{ActorLabel: "amy", Text: strings.Repeat("A", 257)}},
		"control text":    {key, sessionA, ManualClaimInput{ActorLabel: "amy", Text: "\tA1"}},
		"invalid UTF-8":   {key, sessionA, ManualClaimInput{ActorLabel: "amy", Text: string([]byte{0x41, 0xff})}},
		"C1 control text": {key, sessionA, ManualClaimInput{ActorLabel: "amy", Text: "A1\u0085"}},
	} {
		if _, err := validManual(c.key, c.session, c.in); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

// Contract §4.2 SetWindow table, including §0.1 P2(e): mode changes only while CLOSED and a
// transition carrying a different mode is ErrConflict.
func TestPlanWindowTransitions(t *testing.T) {
	closedExact := Window{State: WindowClosed, MatchMode: MatchExact, Version: 3, Generation: 1}
	openExact := Window{State: WindowOpen, MatchMode: MatchExact, Version: 2, Generation: 1}
	in := func(state string, mode MatchMode) WindowInput { return WindowInput{State: state, MatchMode: mode} }
	for _, c := range []struct {
		current Window
		in      WindowInput
		want    string
	}{
		{Window{}, in(WindowOpen, MatchExact), "opened"},
		{Window{}, in(WindowOpen, MatchKeywordQtyOnly), "opened"},
		{Window{}, in(WindowClosed, MatchKeywordQtyOnly), "mode_set"},
		{closedExact, in(WindowOpen, MatchExact), "opened"},
		{openExact, in(WindowClosed, MatchExact), "closed"},
		{closedExact, in(WindowClosed, MatchKeywordQtyOnly), "mode_set"},
		{openExact, in(WindowOpen, MatchExact), ""},            // OPEN→OPEN
		{openExact, in(WindowOpen, MatchKeywordQtyOnly), ""},   // mode change while OPEN
		{openExact, in(WindowClosed, MatchKeywordQtyOnly), ""}, // mode differs on close
		{closedExact, in(WindowOpen, MatchKeywordQtyOnly), ""}, // mode differs on open
		{closedExact, in(WindowClosed, MatchExact), ""},        // identical CLOSED→CLOSED
	} {
		got, err := planWindow(c.current, c.in)
		if c.want == "" {
			if !errors.Is(err, command.ErrConflict) {
				t.Errorf("%+v -> %+v accepted as %q", c.current, c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%+v -> %+v = %q,%v want %q", c.current, c.in, got, err, c.want)
		}
	}
}

// ingestVectors are the §2.4 Ingest rows of tests/claims/kw-v1-vectors.json.
type ingestVector struct {
	ID          string       `json:"id"`
	Mode        MatchMode    `json:"mode"`
	Text        string       `json:"text"`
	OfferState  string       `json:"offer_state"`
	Window      string       `json:"window"`
	GrammarKind grammar.Kind `json:"grammar_kind"`
	Outcome     string       `json:"outcome"`
	Reason      Reason       `json:"reason"`
	Persisted   *struct {
		Offer            *string `json:"offer"`
		Quantity         *int64  `json:"quantity"`
		ExplicitQuantity *bool   `json:"explicit_quantity"`
		Bundle           bool    `json:"bundle"`
		LineVersion      *int64  `json:"line_version"`
	} `json:"persisted"`
}

func loadIngestVectors(t *testing.T) ([]ingestVector, map[string]offer) {
	t.Helper()
	raw, err := os.ReadFile("../../tests/claims/kw-v1-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Offers []struct {
			Keyword string `json:"keyword"`
			Max     int64  `json:"max_quantity_per_claim"`
			Active  bool   `json:"active"`
		} `json:"ingest_offers"`
		Ingest []ingestVector `json:"ingest"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	offers := map[string]offer{}
	for i, o := range file.Offers {
		offers[o.Keyword] = offer{id: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), keyword: o.Keyword, active: o.Active, maxQuantity: o.Max}
	}
	return file.Ingest, offers
}

// decide replays IngestParsed's pure decisions for one vector on an open window with a
// fresh bundle; the database steps (locks, inserts) are exercised by KC06.
func decide(v ingestVector, offers map[string]offer, occurred time.Time) (event, bool) {
	p := grammar.Parse(v.Text)
	if v.Window == WindowClosed {
		return event{}, false
	}
	if p.Kind == grammar.NoMatch {
		return shape(p, ReasonNoMatch, offer{}), true
	}
	o, ok := offers[p.Keyword]
	if !ok {
		return shape(p, ReasonUnknownKeyword, offer{}), true
	}
	switch v.OfferState {
	case "inactive":
		o.active = false
	case "occurred_before_activated_at":
		o.activatedAt = occurred.Add(time.Second)
	}
	if reason := offerReason(p, v.Mode, o, occurred); reason != "" {
		return shape(p, reason, o), true
	}
	e := shape(p, "", o)
	e.bundleID, e.lineVersion, e.bundleVersion = "b", 1, 1
	return e, true
}

// Every Ingest vector's reason and §3.1 persisted shape, and a duplicate rendering equal to
// the fresh rendering (§0.1 P2(c)).
func TestIngestDecisionVectors(t *testing.T) {
	vectors, offers := loadIngestVectors(t)
	occurred := time.Date(2026, 9, 28, 1, 2, 3, 4000, time.UTC)
	for _, v := range vectors {
		e, persisted := decide(v, offers, occurred)
		if !persisted {
			if v.Reason != ReasonWindowClosed || v.Persisted != nil {
				t.Errorf("%s: only WINDOW_CLOSED is unpersisted", v.ID)
			}
			continue
		}
		r := e.result("event", sessionA, 1, false)
		if r.Outcome != v.Outcome || r.Reason != v.Reason || string(e.kind) != string(v.GrammarKind) {
			t.Errorf("%s %q: outcome=%s reason=%s kind=%s", v.ID, v.Text, r.Outcome, r.Reason, e.kind)
			continue
		}
		want := v.Persisted
		if (want.Offer == nil) != (e.offerID == "") || (want.Offer != nil && *want.Offer != e.keyword) {
			t.Errorf("%s: offer=%q keyword=%q", v.ID, e.offerID, e.keyword)
		}
		if (want.Quantity == nil) != (e.quantity == 0) || (want.Quantity != nil && *want.Quantity != e.quantity) {
			t.Errorf("%s: quantity=%d", v.ID, e.quantity)
		}
		if (want.ExplicitQuantity == nil) != (e.explicit == nil) || (e.explicit != nil && *want.ExplicitQuantity != *e.explicit) {
			t.Errorf("%s: explicit=%v", v.ID, e.explicit)
		}
		if want.Bundle != (e.bundleID != "") {
			t.Errorf("%s: bundle=%q", v.ID, e.bundleID)
		}
		if e.offerID == "" && (r.Keyword != "" || r.Quantity != 0) {
			t.Errorf("%s: unresolved head leaked into the result: %+v", v.ID, r)
		}
		dup := e.result("event", sessionA, 1, true)
		dup.Duplicate = false
		if dup != r {
			t.Errorf("%s: duplicate rendering differs", v.ID)
		}
	}
}

func TestOfferReasonPrecedence(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	active := offer{id: "o", keyword: "A1", active: true, activatedAt: now.Add(-time.Minute), maxQuantity: 3}
	inactive := active
	inactive.active = false
	for _, c := range []struct {
		text string
		mode MatchMode
		o    offer
		want Reason
	}{
		{"A1+0", MatchKeywordQtyOnly, inactive, ReasonOfferInactive}, // inactive before invalid quantity
		{"A1+0", MatchKeywordQtyOnly, active, ReasonInvalidQuantity}, // invalid before required
		{"A1", MatchKeywordQtyOnly, active, ReasonQuantityRequired},
		{"A1+4", MatchKeywordQtyOnly, active, ReasonQuantityOverMax},
		{"A1+3", MatchKeywordQtyOnly, active, ""},
		{"A1", MatchExact, active, ""},
		{"A1+4", MatchExact, active, ReasonQuantityOverMax},
	} {
		if got := offerReason(grammar.Parse(c.text), c.mode, c.o, now); got != c.want {
			t.Errorf("%s/%s: %q want %q", c.text, c.mode, got, c.want)
		}
	}
	if got := offerReason(grammar.Parse("A1"), MatchExact, active, active.activatedAt.Add(-time.Microsecond)); got != ReasonOfferInactive {
		t.Errorf("before activated_at: %q", got)
	}
}

func TestValidIngestShape(t *testing.T) {
	good := IngestInput{TenantID: tenantA, StoreID: storeA, SessionID: sessionA, SourceKind: "manual",
		SourceEventID: sessionA, Platform: "manual", ActorKey: strings.Repeat("0f", 32), ActorLabel: "amy",
		PrincipalID: tenantA, OccurredAt: time.Date(2026, 9, 28, 1, 2, 3, 4000, time.UTC)}
	p := grammar.Parse("A1+2")
	if !validIngest(good, p) {
		t.Fatal("valid ingest rejected")
	}
	for name, mutate := range map[string]func(*IngestInput, *grammar.Result){
		"meta source":      func(in *IngestInput, _ *grammar.Result) { in.SourceKind = "meta" },
		"meta platform":    func(in *IngestInput, _ *grammar.Result) { in.Platform = "facebook" },
		"upper actor key":  func(in *IngestInput, _ *grammar.Result) { in.ActorKey = strings.Repeat("0F", 32) },
		"text present":     func(in *IngestInput, _ *grammar.Result) { in.Text = "A1" },
		"unnormal label":   func(in *IngestInput, _ *grammar.Result) { in.ActorLabel = "@Amy" },
		"nanoseconds":      func(in *IngestInput, _ *grammar.Result) { in.OccurredAt = in.OccurredAt.Add(1) },
		"zero time":        func(in *IngestInput, _ *grammar.Result) { in.OccurredAt = time.Time{} },
		"bad source id":    func(in *IngestInput, _ *grammar.Result) { in.SourceEventID = "x" },
		"forged version":   func(_ *IngestInput, r *grammar.Result) { r.Version = "kw-v2" },
		"forged keyword":   func(_ *IngestInput, r *grammar.Result) { r.Keyword = "a1" },
		"forged quantity":  func(_ *IngestInput, r *grammar.Result) { r.Quantity = 1000 },
		"implicit qty > 1": func(_ *IngestInput, r *grammar.Result) { r.Explicit = false },
		"forged kind":      func(_ *IngestInput, r *grammar.Result) { r.Kind = "MAYBE" },
		"no-match keyword": func(_ *IngestInput, r *grammar.Result) { r.Kind = grammar.NoMatch },
	} {
		in, parsed := good, p
		mutate(&in, &parsed)
		if validIngest(in, parsed) {
			t.Errorf("%s accepted", name)
		}
	}
}

// §0.1 P2(f): unavailable or inactive pending lines are skipped and stay pending; only
// applicable lines are merged, as absolute quantities, into the untouched rest of the cart.
func TestRedeemPlanning(t *testing.T) {
	lines := []claimLine{
		{offerID: "o1", skuID: "sku-b", quantity: 3, version: 2, pending: true, offerActive: true},
		{offerID: "o2", skuID: "sku-a", quantity: 1, version: 1, pending: true, offerActive: true},
		{offerID: "o3", skuID: "sku-c", quantity: 5, version: 1, pending: true, offerActive: false},
		{offerID: "o4", skuID: "sku-d", quantity: 2, version: 1, pending: true, offerActive: true},
		{offerID: "o5", skuID: "sku-e", quantity: 9, version: 4, pending: false, offerActive: true},
	}
	available := map[string]bool{"sku-a": true, "sku-b": true, "sku-c": true, "sku-d": false, "sku-e": true}
	apply, skipped, err := splitPending(lines, available)
	if err != nil {
		t.Fatal(err)
	}
	if len(apply) != 2 || apply[0].skuID != "sku-a" || apply[1].skuID != "sku-b" {
		t.Fatalf("apply=%+v", apply)
	}
	wantSkipped := []Skipped{{SKUID: "sku-c", Reason: "offer_inactive"}, {SKUID: "sku-d", Reason: "unavailable"}}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("skipped=%+v", skipped)
	}
	cart := []storefront.Item{{SKUID: "sku-b", Quantity: 7}, {SKUID: "sku-z", Quantity: 4}, {SKUID: "sku-e", Quantity: 1}}
	merged := mergeCart(cart, apply)
	want := []storefront.Item{{SKUID: "sku-b", Quantity: 3}, {SKUID: "sku-z", Quantity: 4}, {SKUID: "sku-e", Quantity: 1}, {SKUID: "sku-a", Quantity: 1}}
	if !reflect.DeepEqual(merged, want) || cart[0].Quantity != 7 {
		t.Fatalf("merged=%+v (input must stay untouched: %+v)", merged, cart)
	}
	if !cartHas(merged, "sku-b", 3) || cartHas(merged, "sku-b", 7) || cartHas(merged, "sku-x", 1) {
		t.Fatal("cartHas post-condition")
	}
	dupe := append(append([]claimLine{}, lines...), claimLine{offerID: "o6", skuID: "sku-a", quantity: 2, version: 1, pending: true, offerActive: true})
	if _, _, err := splitPending(dupe, available); !errors.Is(err, command.ErrConflict) {
		t.Fatalf("two applicable lines on one SKU must conflict: %v", err)
	}
	if apply, skipped, err := splitPending(lines[4:], available); err != nil || len(apply) != 0 || len(skipped) != 0 {
		t.Fatalf("nothing pending: %+v %+v %v", apply, skipped, err)
	}
}
