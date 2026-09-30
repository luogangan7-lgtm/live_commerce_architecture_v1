package claims

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
)

// Pure checks of the meta source of the shared ingest core (no database): the frozen constants,
// the persisted reason, the source/platform/principal shape rules and the IngestMetaIntake
// precondition guards that fire before any SQL. The SQL behaviour (lease, window intervals, rate
// bounds, bundle creation) is gated by MCI02-MCI09 in tests/foundation.

func TestMetaIntakeFrozenConstants(t *testing.T) {
	if ReasonRateLimited != "RATE_LIMITED" {
		t.Fatalf("ReasonRateLimited=%q", ReasonRateLimited)
	}
	found := false
	for _, r := range persistedReasons {
		found = found || r == ReasonRateLimited
	}
	if !found {
		t.Fatal("RATE_LIMITED must be persisted (claims.events)")
	}
	if len(boardReasons) != 7 {
		t.Fatalf("merchant board Stats.Rejected must stay the closed 7-key set until the UI unit lands: %d", len(boardReasons))
	}
	for _, r := range boardReasons {
		if r == ReasonRateLimited {
			t.Fatal("RATE_LIMITED leaked into the board key set")
		}
	}
	if metaActorLimit != 10 || metaActorWindow != 60 || metaSessionBundleCap != 5000 {
		t.Fatalf("§4.2 bounds changed: %d/%ds/%d (IR-6: 10 per 60 s per actor, 5000 bundles)", metaActorLimit, metaActorWindow, metaSessionBundleCap)
	}
}

func TestIngestMetaIntakeGuardsBeforeSQL(t *testing.T) {
	// A nil transaction or malformed id can never reach the database.
	if _, err := IngestMetaIntake(context.Background(), nil, sessionA); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil tx: %v", err)
	}
	for _, id := range []string{"", "x", strings.ToUpper(sessionA), sessionA + "0"} {
		if _, err := IngestMetaIntake(context.Background(), nil, id); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("id %q: %v", id, err)
		}
	}
}

func TestMetaShapeRules(t *testing.T) {
	offerID := "11111111-1111-4111-8111-111111111111"
	base := IngestInput{TenantID: tenantA, StoreID: storeA, SessionID: sessionA, SourceKind: "meta", SourceEventID: sessionA,
		Platform: "facebook", ActorKey: strings.Repeat("0f", 32), OccurredAt: time.Date(2026, 9, 29, 1, 2, 3, 4000, time.UTC)}
	known := &metaIngest{sourceID: offerID, receivedAt: base.OccurredAt, offerID: offerID}
	unknown := &metaIngest{sourceID: offerID, receivedAt: base.OccurredAt, unknownKeyword: true}
	none := &metaIngest{sourceID: offerID, receivedAt: base.OccurredAt}
	match, invalid, nomatch := grammar.Parse("A1+2"), grammar.Parse("A1+0"), grammar.Parse("hello")
	if !validShape(base, match, known) || !validShape(base, invalid, known) || !validShape(base, nomatch, none) {
		t.Fatal("valid meta shapes rejected")
	}
	// An unresolved head carries nothing but its grammar kind (claims R5/§8).
	if !validShape(base, grammar.Result{Version: grammar.Version, Kind: grammar.Match}, unknown) ||
		!validShape(base, grammar.Result{Version: grammar.Version, Kind: grammar.InvalidQuantity}, unknown) {
		t.Fatal("unknown-keyword shape rejected")
	}
	if validShape(base, match, unknown) || validShape(base, grammar.Result{Version: grammar.Version, Kind: grammar.NoMatch}, unknown) {
		t.Fatal("unknown keyword may not carry a keyword or NO_MATCH kind")
	}
	instagram := base
	instagram.Platform = "instagram"
	if !validShape(instagram, match, known) {
		t.Fatal("instagram platform rejected")
	}
	for name, mutate := range map[string]func(*IngestInput){
		"manual platform under meta": func(in *IngestInput) { in.Platform = "manual" },
		"other platform":             func(in *IngestInput) { in.Platform = "line" },
		"principal set":              func(in *IngestInput) { in.PrincipalID = tenantA },
		"label set":                  func(in *IngestInput) { in.ActorLabel = "amy" },
		"manual kind":                func(in *IngestInput) { in.SourceKind = "manual" },
		"text present":               func(in *IngestInput) { in.Text = "A1" },
		"upper actor key":            func(in *IngestInput) { in.ActorKey = strings.Repeat("0F", 32) },
	} {
		in := base
		mutate(&in)
		if validShape(in, match, known) {
			t.Errorf("%s accepted", name)
		}
	}
	if validShape(base, match, &metaIngest{sourceID: "x", receivedAt: base.OccurredAt, offerID: offerID}) ||
		validShape(base, match, &metaIngest{sourceID: offerID, offerID: offerID}) ||
		validShape(base, match, &metaIngest{sourceID: offerID, receivedAt: base.OccurredAt, offerID: offerID, unknownKeyword: true}) {
		t.Fatal("malformed intake facts accepted")
	}
	// The manual entry points keep refusing the meta source, platform and a missing principal.
	manual := base
	manual.SourceKind, manual.Platform, manual.PrincipalID = "manual", "manual", tenantA
	if !validIngest(manual, match) {
		t.Fatal("manual shape must stay valid")
	}
	if validIngest(base, match) {
		t.Fatal("IngestParsed must not accept the meta source")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%v %+v %#v", base, base, base)
	if strings.Contains(b.String(), base.ActorKey) {
		t.Fatal("IngestInput leaked the actor key")
	}
}

func TestIngestResultCarriesBundleCreated(t *testing.T) {
	e := event{bundleID: "b", bundleCreated: true}
	if !e.result("e", "s", 1, false).BundleCreated {
		t.Fatal("fresh accepted event must report BundleCreated")
	}
	if e.result("e", "s", 1, true).BundleCreated {
		t.Fatal("a duplicate must never report BundleCreated")
	}
}
