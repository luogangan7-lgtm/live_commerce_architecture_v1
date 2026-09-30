// A-10 dispatcher amendment (meta-ads-v1 §3.1, rulings X2, A10-D1..D4), tier MOCK/UNIT.
//
// Owns: the pure decisions the amendment added to dispatcher.go and jobs.go: claim Mode on the
// callback request, coded policy denials, route validation for ReconcileWithSecret, which
// callback receives a Secret per claim mode, the reconcile-mode loader failure mapping and the
// InsertOperationJobOn lane validation.
//
// Non-goals: nothing here touches PostgreSQL, so a real reconcile claim loading a credential is
// gate MA11 (ads-tests), and the unchanged existing routes are covered by the existing core tests
// plus the focused regression run named in docs/delivery/units/ads-a10.md.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/command"
)

func secretTestRoute() DispatchRoute {
	return DispatchRoute{
		Provider: "synthetic", Action: "meta.ads.pause", Purpose: "transactional",
		Check:      func(context.Context, DispatchRequest) error { return nil },
		LoadSecret: func(context.Context, pgx.Tx, SecretClaim) (Secret, error) { return Secret{}, nil },
		DispatchWithSecret: func(context.Context, DispatchRequest, Secret) (Outcome, error) {
			return Outcome{State: "SUCCEEDED", Code: "dispatch"}, nil
		},
	}
}

func reconcileSecretFn() func(context.Context, DispatchRequest, Secret) (Outcome, error) {
	return func(context.Context, DispatchRequest, Secret) (Outcome, error) {
		return Outcome{State: "SUCCEEDED", Code: "reconcile"}, nil
	}
}

func plainReconcileFn() func(context.Context, DispatchRequest) (Outcome, error) {
	return func(context.Context, DispatchRequest) (Outcome, error) {
		return Outcome{State: "UNKNOWN", Code: "plain"}, nil
	}
}

func TestA10ModeSetPerClaimAndNeverMarshalled(t *testing.T) {
	operation := Operation{ID: "123e4567-e89b-12d3-a456-426614174000", Request: json.RawMessage(`{}`)}
	for _, mode := range []string{"dispatch", "reconcile"} {
		if got := dispatchRequestMode(operation, mode).Mode; got != mode {
			t.Fatalf("mode %q -> %q", mode, got)
		}
	}
	// Check and the callback each build their own request: mutating one must not reach the other.
	a, b := dispatchRequestMode(operation, "reconcile"), dispatchRequestMode(operation, "reconcile")
	a.Mode, a.Request[0] = "dispatch", '['
	if b.Mode != "reconcile" || b.Request[0] != '{' {
		t.Fatal("check request mutation escaped into the callback request")
	}
	// The pre-A-10 constructor is untouched: no mode until the dispatcher sets it.
	if dispatchRequest(operation).Mode != "" {
		t.Fatal("dispatchRequest must not set a mode")
	}
	raw, err := json.Marshal(dispatchRequestMode(operation, "reconcile"))
	if err != nil || strings.Contains(strings.ToLower(string(raw)), "mode") || strings.Contains(string(raw), "reconcile") {
		t.Fatalf("Mode leaked into marshalled bytes: %s (%v)", raw, err)
	}
}

func TestA10CheckNeverReceivesASecret(t *testing.T) {
	secretType, claimType := reflect.TypeOf(Secret{}), reflect.TypeOf(SecretClaim{})
	holdsSecret := func(typ reflect.Type) bool {
		return typ == secretType || typ == claimType || (typ.Kind() == reflect.Pointer && (typ.Elem() == secretType || typ.Elem() == claimType))
	}
	request := reflect.TypeOf(DispatchRequest{})
	for i := 0; i < request.NumField(); i++ {
		if holdsSecret(request.Field(i).Type) {
			t.Fatalf("DispatchRequest.%s can carry a secret", request.Field(i).Name)
		}
	}
	for _, field := range []string{"Check", "Dispatch", "Reconcile"} {
		fn, _ := reflect.TypeOf(DispatchRoute{}).FieldByName(field)
		for i := 0; i < fn.Type.NumIn(); i++ {
			if holdsSecret(fn.Type.In(i)) {
				t.Fatalf("%s takes a secret parameter", field)
			}
		}
	}
	// Only the loader sees the claim fence; only the *WithSecret callbacks see the Secret.
	for _, field := range []string{"DispatchWithSecret", "ReconcileWithSecret"} {
		fn, _ := reflect.TypeOf(DispatchRoute{}).FieldByName(field)
		if fn.Type.NumIn() != 3 || fn.Type.In(2) != secretType {
			t.Fatalf("%s signature changed: %v", field, fn.Type)
		}
	}
}

func TestA10CodedAndBareDenial(t *testing.T) {
	coded := DenyPolicy("over_allowance")
	if !errors.Is(coded, ErrPolicyDenied) {
		t.Fatal("coded denial is not a policy denial")
	}
	if got := coded.Error(); got != "policy denied: over_allowance" {
		t.Fatalf("Error()=%q", got)
	}
	cases := map[string]struct {
		err  error
		want string
	}{
		"coded":         {DenyPolicy("pause_requested"), "pause_requested"},
		"wrapped coded": {fmt.Errorf("check: %w", DenyPolicy("consent_withdrawn")), "consent_withdrawn"},
		"bare":          {ErrPolicyDenied, "policy_denied"},
		"wrapped bare":  {fmt.Errorf("check: %w", ErrPolicyDenied), "policy_denied"},
		"empty code":    {DenyPolicy(""), "policy_denied"},
		"bad chars":     {DenyPolicy("has space"), "policy_denied"},
		"newline":       {DenyPolicy("a\nb"), "policy_denied"},
		"too long":      {DenyPolicy(strings.Repeat("a", 81)), "policy_denied"},
		"max length":    {DenyPolicy(strings.Repeat("a", 80)), strings.Repeat("a", 80)},
	}
	for name, tc := range cases {
		if !errors.Is(tc.err, ErrPolicyDenied) {
			t.Fatalf("%s: not a policy denial", name)
		}
		if got := denialCode(tc.err); got != tc.want {
			t.Fatalf("%s: code %q, want %q", name, got, tc.want)
		}
	}
	// A non-denial error must not be mistaken for one (it stays policy_check_failed upstream).
	if errors.Is(errors.New("boom"), ErrPolicyDenied) || errors.Is(PolicyDenial{Code: "x"}, errors.New("other")) {
		t.Fatal("PolicyDenial matches a foreign sentinel")
	}
}

func TestA10RouteValidationMatrix(t *testing.T) {
	plain := testDispatchRoute()
	withReconcile := secretTestRoute()
	withReconcile.Reconcile = plainReconcileFn()
	withBoth := secretTestRoute()
	withBoth.ReconcileWithSecret = reconcileSecretFn()
	bothReconcile := withBoth
	bothReconcile.Reconcile = plainReconcileFn()
	neither := secretTestRoute()
	plainWithSecretReconcile := testDispatchRoute()
	plainWithSecretReconcile.Reconcile = nil
	plainWithSecretReconcile.ReconcileWithSecret = reconcileSecretFn()
	plainBothReconcile := testDispatchRoute()
	plainBothReconcile.ReconcileWithSecret = reconcileSecretFn()
	halfPair := withBoth
	halfPair.LoadSecret = nil
	dispatchAndPair := withBoth
	dispatchAndPair.Dispatch = testDispatchRoute().Dispatch

	cases := []struct {
		name  string
		route DispatchRoute
		ok    bool
	}{
		{"plain dispatch + plain reconcile (existing shape)", plain, true},
		{"secret pair + plain reconcile (existing shape)", withReconcile, true},
		{"secret pair + ReconcileWithSecret (A-10)", withBoth, true},
		{"secret pair + both reconcile callbacks", bothReconcile, false},
		{"secret pair + no reconcile callback", neither, false},
		{"plain dispatch + ReconcileWithSecret only", plainWithSecretReconcile, false},
		{"plain dispatch + both reconcile callbacks", plainBothReconcile, false},
		{"ReconcileWithSecret without LoadSecret", halfPair, false},
		{"Dispatch + secret pair", dispatchAndPair, false},
	}
	for _, tc := range cases {
		_, err := compileDispatchRoutes([]DispatchRoute{tc.route})
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestA10SecretCallbackPerMode(t *testing.T) {
	plain := testDispatchRoute()
	optedOut := secretTestRoute()
	optedOut.Reconcile = plainReconcileFn()
	optedIn := secretTestRoute()
	optedIn.ReconcileWithSecret = reconcileSecretFn()
	call := func(fn func(context.Context, DispatchRequest, Secret) (Outcome, error)) string {
		if fn == nil {
			return "<none>"
		}
		out, _ := fn(context.Background(), DispatchRequest{}, Secret{})
		return out.Code
	}
	cases := []struct {
		name  string
		route DispatchRoute
		mode  string
		want  string
	}{
		{"plain route dispatch", plain, "dispatch", "<none>"},
		{"plain route reconcile", plain, "reconcile", "<none>"},
		{"secret route dispatch", optedOut, "dispatch", "dispatch"},
		{"secret route reconcile without opt-in loads nothing", optedOut, "reconcile", "<none>"},
		{"opted-in dispatch", optedIn, "dispatch", "dispatch"},
		{"opted-in reconcile uses ReconcileWithSecret", optedIn, "reconcile", "reconcile"},
	}
	for _, tc := range cases {
		if got := call(secretCallback(tc.route, tc.mode)); got != tc.want {
			t.Fatalf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestA10SecretLoadFailureCodes(t *testing.T) {
	denied := fmt.Errorf("loader: %w", ErrPolicyDenied)
	other := errors.New("no rows")
	cases := []struct {
		name        string
		mode        string
		err         error
		panicked    bool
		wantCode    string
		wantBlocked bool
	}{
		{"dispatch denial is BLOCKED_POLICY (unchanged)", "dispatch", denied, false, "credential_unavailable", true},
		{"dispatch other error stays UNKNOWN (unchanged)", "dispatch", other, false, "secret_load_failed", false},
		{"dispatch panic stays UNKNOWN (unchanged)", "dispatch", nil, true, "secret_load_failed", false},
		{"dispatch denial that panicked is not a denial", "dispatch", denied, true, "secret_load_failed", false},
		{"reconcile denial never BLOCKED_POLICY", "reconcile", denied, false, "credential_unavailable", false},
		{"reconcile other error", "reconcile", other, false, "secret_load_failed", false},
		{"reconcile panic", "reconcile", nil, true, "secret_load_failed", false},
	}
	for _, tc := range cases {
		code, blocked := secretLoadFailure(tc.mode, tc.err, tc.panicked)
		if code != tc.wantCode || blocked != tc.wantBlocked {
			t.Fatalf("%s: got %s/%v, want %s/%v", tc.name, code, blocked, tc.wantCode, tc.wantBlocked)
		}
		if !codePattern.MatchString(code) {
			t.Fatalf("%s: code %q is not recordable", tc.name, code)
		}
	}
}

func TestA10InsertOperationJobOnValidation(t *testing.T) {
	good := []struct {
		queue    string
		priority int
	}{{"ads", 1}, {"ads", 4}, {"default", 2}, {"a", 3}, {"a" + strings.Repeat("b", 39), 1}, {"ads_v2", 1}}
	for _, tc := range good {
		if !validJobLane(tc.queue, tc.priority) {
			t.Fatalf("valid lane rejected: %q/%d", tc.queue, tc.priority)
		}
	}
	bad := []struct {
		queue    string
		priority int
	}{
		{"", 1}, {"Ads", 1}, {"1ads", 1}, {"_ads", 1}, {"ads-x", 1}, {"ads x", 1}, {"ads\n", 1}, {"ads.x", 1},
		{"a" + strings.Repeat("b", 40), 1}, {"ads", 0}, {"ads", 5}, {"ads", -1},
	}
	for _, tc := range bad {
		if validJobLane(tc.queue, tc.priority) {
			t.Fatalf("invalid lane accepted: %q/%d", tc.queue, tc.priority)
		}
	}
	// Argument guards fire before any database access (nil client and tx here).
	id := "123e4567-e89b-12d3-a456-426614174000"
	for _, tc := range []struct {
		name  string
		id    string
		queue string
		prio  int
	}{{"bad id", "not-a-uuid", "ads", 1}, {"bad queue", id, "Ads", 1}, {"bad priority", id, "ads", 9}, {"good lane, nil client", id, "ads", 1}} {
		if _, err := InsertOperationJobOn(context.Background(), nil, nil, tc.id, tc.queue, tc.prio); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
	}
}
