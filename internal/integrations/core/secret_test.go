package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func secretRoute() DispatchRoute {
	r := testDispatchRoute()
	r.Dispatch = nil
	r.LoadSecret = func(context.Context, pgx.Tx, SecretClaim) (Secret, error) { return NewSecret([]byte("x")), nil }
	r.DispatchWithSecret = func(context.Context, DispatchRequest, Secret) (Outcome, error) {
		return Outcome{State: "SUCCEEDED", Code: "ok"}, nil
	}
	return r
}

func TestSecretRedactionAndZero(t *testing.T) {
	raw := []byte("page-token-value")
	s := NewSecret(raw)
	raw[0] = 'X' // NewSecret must have copied
	if string(s.Reveal()) != "page-token-value" {
		t.Fatal("NewSecret aliased the caller slice")
	}
	claim := SecretClaim{OperationID: "op", Generation: 3, LeaseToken: []byte("lease-token-bytes")}
	for _, v := range []any{s, &s, claim, &claim} {
		for _, out := range []string{fmt.Sprint(v), fmt.Sprintf("%v %+v %#v %s %q %x %d", v, v, v, v, v, v, v)} {
			if strings.Contains(out, "page-token-value") || strings.Contains(out, "lease-token") {
				t.Fatalf("formatter leaked: %s", out)
			}
		}
		j, err := json.Marshal(v)
		if err != nil || string(j) != `"[redacted]"` {
			t.Fatalf("json = %s %v", j, err)
		}
	}
	if b, _ := s.MarshalText(); string(b) != "[redacted]" {
		t.Fatalf("text = %s", b)
	}
	backing := s.Reveal()
	s.zero()
	for _, c := range backing {
		if c != 0 {
			t.Fatal("zero left secret bytes in the backing array")
		}
	}
}

func TestSecretRouteShapes(t *testing.T) {
	if _, err := compileDispatchRoutes([]DispatchRoute{secretRoute()}); err != nil {
		t.Fatalf("secret pair rejected: %v", err)
	}
	mutations := map[string]func(*DispatchRoute){
		"dispatch and pair": func(r *DispatchRoute) { r.Dispatch = testDispatchRoute().Dispatch },
		"load only":         func(r *DispatchRoute) { r.DispatchWithSecret = nil },
		"with-secret only":  func(r *DispatchRoute) { r.LoadSecret = nil },
		"nothing":           func(r *DispatchRoute) { r.LoadSecret, r.DispatchWithSecret = nil, nil },
		"dispatch+load": func(r *DispatchRoute) {
			r.DispatchWithSecret = nil
			r.Dispatch = testDispatchRoute().Dispatch
		},
	}
	for name, mutate := range mutations {
		r := secretRoute()
		mutate(&r)
		if _, err := compileDispatchRoutes([]DispatchRoute{r}); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestSecretRouteStricterLease(t *testing.T) {
	// 10s call + 3*2s db + 1s = 17s: needs a lease above it; plain routes only need 15s.
	o := DefaultDispatcherOptions()
	o.LeaseSeconds = 16
	if !validDispatcherOptions(o) {
		t.Fatal("plain inequality must hold at 16s")
	}
	if _, err := NewDispatcher(context.Background(), nil, []DispatchRoute{secretRoute()}, o); !errors.Is(err, errInvalidJob) {
		t.Fatalf("secret route accepted with weak lease: %v", err)
	}
	// A plain route at the same options passes the inequality check and only then needs a pool.
	if _, err := NewDispatcher(context.Background(), nil, []DispatchRoute{testDispatchRoute()}, o); errors.Is(err, errInvalidJob) {
		t.Fatal("plain route wrongly held to the stricter inequality")
	}
}

func TestSecretCallbacksRecoverPanics(t *testing.T) {
	s, err, panicked := invokeLoad(context.Background(), func(context.Context, pgx.Tx, SecretClaim) (Secret, error) {
		panic("boom")
	}, nil, SecretClaim{})
	if !panicked || err != nil || len(s.Reveal()) != 0 {
		t.Fatalf("load panic: %v %v %d", panicked, err, len(s.Reveal()))
	}
	_, err, panicked = invokeSecretOutcome(context.Background(), func(context.Context, DispatchRequest, Secret) (Outcome, error) {
		panic("boom")
	}, DispatchRequest{}, Secret{})
	if !panicked || err != nil {
		t.Fatalf("dispatch panic: %v %v", panicked, err)
	}
}

func TestInsertOperationJobRejectsBadInput(t *testing.T) {
	if _, err := InsertOperationJob(context.Background(), nil, nil, "not-a-uuid"); err == nil {
		t.Fatal("bad input accepted")
	}
}
