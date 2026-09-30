package ads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
)

// service_test.go: UNIT tests of the service pieces that need no database (OAuth state, dialog URL, redaction, error
// mapping, checker gating). The REAL_PG walk is pg_flow_test.go.

func testService(t *testing.T) *Service {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(jobs, nil, DialogConfig{AppID: "4291253377792879", ConfigID: "123456789",
		RedirectURI: "https://admin.example.test/api/admin/ads/meta/callback", GraphVersion: "v26.0", StateKey: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewServiceValidatesDialogConfig(t *testing.T) {
	jobs, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	ok := DialogConfig{AppID: "1", ConfigID: "2", RedirectURI: "https://a.example.test/cb", GraphVersion: "v26.0", StateKey: bytes.Repeat([]byte{7}, 32)}
	if _, err = NewService(jobs, nil, ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]DialogConfig{
		"http redirect":     {AppID: "1", ConfigID: "2", RedirectURI: "http://a.example.test/cb", GraphVersion: "v26.0", StateKey: bytes.Repeat([]byte{7}, 32)},
		"userinfo redirect": {AppID: "1", ConfigID: "2", RedirectURI: "https://u:p@a.example.test/cb", GraphVersion: "v26.0", StateKey: bytes.Repeat([]byte{7}, 32)},
		"fragment redirect": {AppID: "1", ConfigID: "2", RedirectURI: "https://a.example.test/cb#x", GraphVersion: "v26.0", StateKey: bytes.Repeat([]byte{7}, 32)},
		"app id":            {AppID: "x", ConfigID: "2", RedirectURI: "https://a.example.test/cb", GraphVersion: "v26.0"},
		"config id":         {AppID: "1", ConfigID: "", RedirectURI: "https://a.example.test/cb", GraphVersion: "v26.0"},
		"no state key":      {AppID: "1", ConfigID: "2", RedirectURI: "https://a.example.test/cb", GraphVersion: "v26.0"},
		"short state key":   {AppID: "1", ConfigID: "2", RedirectURI: "https://a.example.test/cb", GraphVersion: "v26.0", StateKey: []byte("short")},
		"version":           {AppID: "1", ConfigID: "2", RedirectURI: "https://a.example.test/cb", GraphVersion: "26", StateKey: bytes.Repeat([]byte{7}, 32)},
	} {
		if _, err = NewService(jobs, nil, bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err = NewService(nil, nil, ok); err == nil {
		t.Fatal("nil River client accepted")
	}
}

func TestOAuthStateIsDeterministicScopedKeyedAndHashed(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	scope := platform.Scope{TenantID: "t1", StoreID: "s1", PrincipalID: "p1"}
	a, ha := stateParam(key, scope, "key-aaaaaaaa")
	b, hb := stateParam(key, scope, "key-aaaaaaaa")
	if a != b || string(ha) != string(hb) {
		t.Fatal("state must be reproducible for a replay")
	}
	if !statePattern.MatchString(a) {
		t.Fatalf("state shape %q", a)
	}
	for _, other := range []platform.Scope{{TenantID: "t2", StoreID: "s1", PrincipalID: "p1"}, {TenantID: "t1", StoreID: "s2", PrincipalID: "p1"}, {TenantID: "t1", StoreID: "s1", PrincipalID: "p2"}} {
		if c, _ := stateParam(key, other, "key-aaaaaaaa"); c == a {
			t.Fatal("state must differ per scope")
		}
	}
	if c, _ := stateParam(key, scope, "key-bbbbbbbb"); c == a {
		t.Fatal("state must differ per key")
	}
	if string(stateHash(a)) != string(ha) || len(ha) != 32 {
		t.Fatal("stored hash must be SHA-256 of the state text")
	}
	if strings.Contains(string(ha), a) {
		t.Fatal("hash leaks state")
	}
	// r3 P2: a reader of ops.command_results knows scope and Idempotency-Key but not the server key, so the
	// unkeyed derivation of the first version (and any other key) must not reproduce a live state.
	if c, _ := stateParam(bytes.Repeat([]byte{8}, 32), scope, "key-aaaaaaaa"); c == a {
		t.Fatal("state must depend on the server-side key")
	}
	raw := sha256.Sum256([]byte("livecommerce/ads-oauth-state/v1|t1|s1|p1|key-aaaaaaaa"))
	if old := base64.RawURLEncoding.EncodeToString(raw[:]); old == a {
		t.Fatal("state equals the unkeyed v1 derivation")
	}
}

func TestDialogURLCarriesOnlyTheFrozenParameters(t *testing.T) {
	s := testService(t)
	raw := s.dialogURL("STATE")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "www.facebook.com" || u.Path != "/v26.0/dialog/oauth" {
		t.Fatalf("url %q %v", raw, err)
	}
	q := u.Query()
	want := map[string]string{"client_id": "4291253377792879", "config_id": "123456789", "response_type": "code",
		"override_default_response_type": "true", "redirect_uri": "https://admin.example.test/api/admin/ads/meta/callback", "state": "STATE"}
	if len(q) != len(want) {
		t.Fatalf("unexpected parameters: %v", q)
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("%s=%q", k, q.Get(k))
		}
	}
}

func TestSealedTokenNeverFormats(t *testing.T) {
	tok := SealedToken{KeyID: "key-id-marker", Enc: []byte("enc-marker-bytes-0123456789abcd"), Ciphertext: []byte("cipher-marker-bytes")}
	res := ConnectResult{ClientBusinessID: "1", Token: tok}
	outputs := []string{fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s %q %x", tok, tok, tok, tok, tok, tok), fmt.Sprintf("%+v %#v", res, res), fmt.Sprint(&tok)}
	j, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(j))
	for _, out := range outputs {
		for _, marker := range []string{"key-id-marker", "enc-marker", "cipher-marker"} {
			if strings.Contains(out, marker) {
				t.Fatalf("formatter leaked %s in %q", marker, out)
			}
		}
	}
}

func TestValidConnectResult(t *testing.T) {
	good := ConnectResult{ClientBusinessID: "555", Scopes: []string{"ads_management"}, Picks: []Pick{{Kind: "ad_account", ID: "9"}},
		Token: SealedToken{KeyID: "k", Enc: make([]byte, 32), Ciphertext: make([]byte, 40)}}
	if !validConnectResult(good) {
		t.Fatal("good result refused")
	}
	for name, mutate := range map[string]func(*ConnectResult){
		"business":   func(r *ConnectResult) { r.ClientBusinessID = "x" },
		"no scopes":  func(r *ConnectResult) { r.Scopes = nil },
		"bad scope":  func(r *ConnectResult) { r.Scopes = []string{"Ads-Management"} },
		"bad pick":   func(r *ConnectResult) { r.Picks = []Pick{{Kind: "page", ID: "9"}} },
		"pick id":    func(r *ConnectResult) { r.Picks = []Pick{{Kind: "dataset", ID: "a"}} },
		"enc length": func(r *ConnectResult) { r.Token.Enc = make([]byte, 12) },
		"short ct":   func(r *ConnectResult) { r.Token.Ciphertext = make([]byte, 3) },
		"no key id":  func(r *ConnectResult) { r.Token.KeyID = "" },
	} {
		r := good
		r.Scopes = append([]string(nil), good.Scopes...)
		r.Picks = append([]Pick(nil), good.Picks...)
		mutate(&r)
		if validConnectResult(r) {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestMapErrorCodes(t *testing.T) {
	pg := func(state, msg string) error { return &pgconn.PgError{Code: state, Message: msg} }
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{pg("AD402", "billing_restricted"), 402, "billing_restricted"},
		{pg("AD409", "over_allowance"), 409, "over_allowance"},
		{pg("AD410", "state_expired"), 410, "state_expired"},
		{pg("AD422", "not_whole_unit"), 422, "not_whole_unit"},
		{pg("AD409", "driver detail that must never be echoed"), 409, "conflict"},
	}
	for _, tc := range cases {
		var r *Refusal
		if err := mapError(tc.err); !errors.As(err, &r) || r.Status != tc.status || r.Code != tc.code {
			t.Fatalf("%v -> %v", tc.err, err)
		}
	}
	if !errors.Is(mapError(pg("22023", "x")), command.ErrInvalid) || !errors.Is(mapError(pg("40001", "x")), command.ErrConflict) ||
		!errors.Is(mapError(pg("P0002", "x")), command.ErrNotFound) || !errors.Is(mapError(pgx.ErrNoRows), command.ErrNotFound) {
		t.Fatal("standard SQLSTATE mapping changed")
	}
	other := pg("XX000", "secret internal")
	if mapError(other) != other {
		t.Fatal("unknown errors must pass through for the 503 classification")
	}
	if mapError(nil) != nil {
		t.Fatal("nil")
	}
}

func TestCheckerGatesBeforeAnyDatabaseCall(t *testing.T) {
	c := NewChecker(nil)
	// Reconcile mode is query-only and always passes, even without a pool (X2).
	if err := c.Check(context.Background(), core.DispatchRequest{Provider: "meta_ads", Action: "meta.ads.activate", Mode: "reconcile"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, req := range []core.DispatchRequest{
		{Provider: "facebook", Action: "meta.private_reply", Mode: "dispatch"},
		{Provider: "meta_ads", Action: "meta.ads.delete_campaign", Mode: "dispatch"},
		{Provider: "meta_ads", Action: "", Mode: "dispatch"},
	} {
		err := c.Check(context.Background(), req)
		var d core.PolicyDenial
		if !errors.As(err, &d) || !errors.Is(err, core.ErrPolicyDenied) || (d.Code != "unknown_action" && d.Code != "check_unavailable") {
			t.Fatalf("%+v -> %v", req, err)
		}
	}
	// A known action with no pool fails closed as a denial, never as permission.
	if err := c.Check(context.Background(), core.DispatchRequest{Provider: "meta_ads", Action: "meta.ads.activate", Mode: "dispatch"}); !errors.Is(err, core.ErrPolicyDenied) {
		t.Fatalf("no pool: %v", err)
	}
}
