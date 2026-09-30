// R-7a dispatcher extension (taiwan-cvs-logistics-v1 §0.2, rulings B17), tier UNIT.
//
// Owns: the pure parts of Outcome.Detail and DispatchRoute.Finish: Detail never reaches JSON or
// adapter validation, and a panicking Finish becomes an error (so the completion tx rolls back).
//
// Non-goals: the completion transaction itself (Finish before Complete, rollback, SecretClaim.Mode
// on the loader and Finish) is REAL_PG in tests/foundation/dispatcher_r7a_test.go.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestR7aOutcomeDetailIsRoutePrivate(t *testing.T) {
	outcome := Outcome{State: "SUCCEEDED", Code: "created", ProviderReference: "ref-1", Detail: "PRIVATE_DETAIL"}
	raw, err := json.Marshal(outcome)
	if err != nil || strings.Contains(string(raw), "PRIVATE_DETAIL") || strings.Contains(strings.ToLower(string(raw)), "detail") {
		t.Fatalf("Detail leaked into marshalled outcome: %s (%v)", raw, err)
	}
	if !validAdapterOutcome(outcome) || !validOutcome(outcome) {
		t.Fatal("Detail changed adapter/completion validation")
	}
}

func TestR7aFinishPanicBecomesError(t *testing.T) {
	err := invokeFinish(context.Background(), func(context.Context, pgx.Tx, SecretClaim, Outcome) error {
		panic("PRIVATE_FINISH_PANIC")
	}, nil, SecretClaim{}, Outcome{})
	if !errors.Is(err, errFinishPanicked) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("finish panic not contained: %v", err)
	}
	want := errors.New("db fault")
	if err := invokeFinish(context.Background(), func(context.Context, pgx.Tx, SecretClaim, Outcome) error { return want }, nil, SecretClaim{}, Outcome{}); err != want {
		t.Fatalf("finish error not propagated: %v", err)
	}
}
