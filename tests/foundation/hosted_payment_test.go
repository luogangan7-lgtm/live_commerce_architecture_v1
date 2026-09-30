package foundation_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/platform"
)

// These fixtures use a disposable PG database, a synthetic merchant and local
// cryptographic material. No test posts a provider form or logs its contents.
type hpAPI interface {
	BeginHosted(context.Context, string, string, string, checkout.HostedInput) (checkout.PaymentResult, error)
	TakeHosted(context.Context, string, string, string) (checkout.HostedHandoff, error)
}

type hpHarness struct {
	psHarness
	pool      *pgxpool.Pool
	api       hpAPI
	key       []byte
	config    checkout.HostedConfig
	input     checkout.HostedInput
	secretKey string
	secretIV  string
}

func hpSetup(t *testing.T) hpHarness {
	t.Helper()
	p := psSetup(t)
	key := randomBytes(32)
	secretKey := strings.Repeat("K", 32)
	secretIV := strings.Repeat("V", 16)
	// psSetup deliberately seeds an unreadable historical credential for its
	// query-only tests. Replace that row in this disposable fixture with a real
	// envelope using exactly the production AAD shape.
	aad, err := json.Marshal(struct {
		FormatVersion     int    `json:"format_version"`
		TenantID          string `json:"tenant_id"`
		StoreID           string `json:"store_id"`
		ConnectionID      string `json:"connection_id"`
		Provider          string `json:"provider"`
		Environment       string `json:"environment"`
		AccountID         string `json:"account_id"`
		CredentialVersion int64  `json:"credential_version"`
	}{1, p.f.tenantA, p.f.storeA1, p.account, "payuni", "SANDBOX", "mock-account", 1})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := json.Marshal(struct {
		HashKey string `json:"hash_key"`
		HashIV  string `json:"hash_iv"`
	}{secretKey, secretIV})
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := randomBytes(gcm.NonceSize())
	sealed := gcm.Seal(nil, nonce, plain, aad)
	mustExec(t, p.f.owner, `UPDATE integration.account_credentials SET key_id='hosted_fixture',nonce=$1,ciphertext=$2 WHERE connection_id=$3 AND version=1`, nonce, sealed, p.account)
	keys, err := accounts.NewKeyring("hosted_fixture", map[string][]byte{"hosted_fixture": key}, randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := platform.OpenHostedPool(context.Background(), hpRole(t, p.f))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	config := checkout.HostedConfig{ReturnURL: "https://checkout.example.test/payment/return", NotifyURL: "https://checkout.example.test/payment/notify"}
	api := hpStarter(t, pool, "PROVIDER_MOCK", keys, config)
	return hpHarness{psHarness: p, pool: pool, api: api, key: key, config: config,
		input:     checkout.HostedInput{OrderID: p.hold.OrderID, MethodCode: "payuni_credit", MethodVersion: 1, Locale: "zh-TW"},
		secretKey: secretKey, secretIV: secretIV}
}

func hpRole(t *testing.T, f *testFixture) string {
	t.Helper()
	role := "hosted_test_" + hex.EncodeToString(randomBytes(6))
	password := hex.EncodeToString(randomBytes(24))
	quoted := pgx.Identifier{role}.Sanitize()
	mustExec(t, f.owner, fmt.Sprintf(`CREATE ROLE %s LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '%s'`, quoted, password))
	mustExec(t, f.owner, `GRANT commerce_hosted_runtime TO `+quoted+` WITH INHERIT TRUE, SET FALSE`)
	t.Cleanup(func() { mustExec(t, f.owner, `DROP ROLE `+quoted) })
	return roleURL(t, f.databaseURL, role, password)
}

func hpStarter(t *testing.T, pool *pgxpool.Pool, profile string, keys *accounts.Keyring, config checkout.HostedConfig) hpAPI {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := checkout.NewHostedPaymentStarter(context.Background(), pool, jobs, profile, keys, config)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (h hpHarness) begin(key string) (checkout.PaymentResult, error) {
	return h.api.BeginHosted(context.Background(), h.cap.Token, h.f.storeA1, key, h.input)
}

func (h hpHarness) take() (checkout.HostedHandoff, error) {
	return h.api.TakeHosted(context.Background(), h.cap.Token, h.f.storeA1, h.hold.OrderID)
}

func (h hpHarness) counts(t *testing.T) (out [8]int64) {
	t.Helper()
	err := h.f.owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM checkout.payment_attempts WHERE owner_id=$1),
	 (SELECT count(*) FROM integration.operations WHERE buyer_owner_id=$1),
	 (SELECT count(*) FROM checkout.command_results WHERE owner_id=$1),
	 (SELECT count(*) FROM checkout.events WHERE owner_id=$1 AND action='checkout.payment_started'),
	 (SELECT count(*) FROM checkout.hosted_payment_pages p JOIN checkout.payment_attempts a ON a.id=p.attempt_id WHERE a.owner_id=$1),
	 (SELECT count(*) FROM checkout.orders WHERE owner_id=$1 AND commercial_state='AWAITING_PAYMENT'),
	 (SELECT count(*) FROM inventory.reservations WHERE buyer_owner_id=$1 AND state='PAYMENT_PENDING'),
	 (SELECT count(*) FROM river_payment.river_job WHERE kind='payment_query_v1')`, h.cap.Scope.OwnerID).
		Scan(&out[0], &out[1], &out[2], &out[3], &out[4], &out[5], &out[6], &out[7])
	if err != nil {
		t.Fatal(err)
	}
	return
}

func (h hpHarness) page(t *testing.T, attemptID string) (form []byte, preparedAt, expiresAt time.Time, handedOutAt *time.Time) {
	t.Helper()
	err := h.f.owner.QueryRow(context.Background(), `SELECT form,prepared_at,expires_at,handed_out_at FROM checkout.hosted_payment_pages WHERE attempt_id=$1`, attemptID).
		Scan(&form, &preparedAt, &expiresAt, &handedOutAt)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func hpHandoffJSON(t *testing.T, out checkout.HostedHandoff) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func hpDisposition(t *testing.T, out checkout.HostedHandoff) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(hpHandoffJSON(t, out)["disposition"], &value); err != nil {
		t.Fatal("handoff disposition missing")
	}
	return value
}

func hpForm(t *testing.T, out checkout.HostedHandoff) (action string, fields map[string]string) {
	t.Helper()
	var form struct {
		Action string            `json:"action"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(hpHandoffJSON(t, out)["form"], &form); err != nil || form.Action == "" || len(form.Fields) != 4 {
		t.Fatal("issued handoff must contain one bounded scalar provider form")
	}
	return form.Action, form.Fields
}

func hpNoForm(t *testing.T, out checkout.HostedHandoff) {
	t.Helper()
	form := hpHandoffJSON(t, out)["form"]
	if len(form) != 0 && string(form) != "null" {
		t.Fatal("repeated handoff exposed a provider form")
	}
}

func TestBuyerPaymentHostedAtomicPreparationAndIndependentWire(t *testing.T) {
	h := hpSetup(t)
	before := h.counts(t)
	result, err := h.begin(t04Key("hp-atomic"))
	if err != nil {
		t.Fatal(err)
	}
	after := h.counts(t)
	for i := range before {
		if after[i]-before[i] != 1 {
			t.Fatalf("HP01 atomic fact %d: %d -> %d", i, before[i], after[i])
		}
	}
	if result.State != "PAYMENT_PENDING" || result.AmountMinor != 2500 || result.Currency != "TWD" || result.AttemptID == "" || result.OperationID != result.AttemptID {
		t.Fatal("HP01 wrong frozen payment result")
	}
	var kind string
	var rawArgs []byte
	if err := h.f.owner.QueryRow(context.Background(), `SELECT kind,args FROM river_payment.river_job WHERE id=$1`, result.JobID).Scan(&kind, &rawArgs); err != nil {
		t.Fatal("HP01 missing query job")
	}
	var jobArgs map[string]any
	if err := json.Unmarshal(rawArgs, &jobArgs); err != nil || kind != "payment_query_v1" || len(jobArgs) != 2 || jobArgs["operation_id"] != result.OperationID || jobArgs["version"] != float64(1) {
		t.Fatal("HP01 query job does not bind the original attempt")
	}
	stored, prepared, expires, handed := h.page(t, result.AttemptID)
	if handed != nil || !expires.After(prepared) || expires.After(prepared.Add(61*time.Second)) {
		t.Fatal("HP01 wrong immutable preparation deadline")
	}
	var created time.Time
	if err := h.f.owner.QueryRow(context.Background(), `SELECT created_at FROM checkout.payment_attempts WHERE id=$1`, result.AttemptID).Scan(&created); err != nil || !prepared.Equal(created) {
		t.Fatal("HP01 form timestamp is not tied to attempt creation")
	}
	handoff, err := h.take()
	if err != nil || hpDisposition(t, handoff) != "ISSUED" {
		t.Fatalf("HP03 first handoff failed: %v", err)
	}
	action, fields := hpForm(t, handoff)
	if !bytes.Contains(stored, []byte(`"EncryptInfo"`)) {
		t.Fatal("HP01 persisted form missing encrypted wire")
	}
	hpVerifyWire(t, h, result, prepared, action, fields, "zh-tw")
	second, err := h.take()
	if err != nil || hpDisposition(t, second) != "ALREADY_ISSUED" {
		t.Fatalf("HP03 lost-response retry status: %v", err)
	}
	hpNoForm(t, second)
	if h.counts(t) != after {
		t.Fatal("HP03 take changed financial, stock or job facts")
	}
}

// Independent verifier: it does not call PAYUNi Client.open or the hosted
// keyring. It checks both outer authentication and every frozen inner field.
func hpVerifyWire(t *testing.T, h hpHarness, result checkout.PaymentResult, prepared time.Time, action string, fields map[string]string, language string) {
	t.Helper()
	if action != "https://sandbox-api.payuni.com.tw/api/upp" || fields["MerID"] != "mock-account" || fields["Version"] != "2.0" {
		t.Fatal("HP06 wrong outer merchant, version or endpoint")
	}
	if len(fields) != 4 || fields["EncryptInfo"] == "" || len(fields["HashInfo"]) != 64 {
		t.Fatal("HP06 malformed outer form")
	}
	digest := sha256.Sum256([]byte(h.secretKey + fields["EncryptInfo"] + h.secretIV))
	if fields["HashInfo"] != strings.ToUpper(hex.EncodeToString(digest[:])) {
		t.Fatal("HP06 outer form authentication failed")
	}
	wire, err := hex.DecodeString(fields["EncryptInfo"])
	if err != nil {
		t.Fatal("HP06 malformed encrypted wire")
	}
	parts := bytes.Split(wire, []byte(":::"))
	if len(parts) != 2 {
		t.Fatal("HP06 malformed encrypted wire sections")
	}
	ciphertext, err := base64.StdEncoding.Strict().DecodeString(string(parts[0]))
	if err != nil {
		t.Fatal("HP06 malformed wire ciphertext")
	}
	tag, err := base64.StdEncoding.Strict().DecodeString(string(parts[1]))
	if err != nil {
		t.Fatal("HP06 malformed wire tag")
	}
	block, err := aes.NewCipher([]byte(h.secretKey))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, []byte(h.secretIV), append(ciphertext, tag...), nil)
	if err != nil {
		t.Fatal("HP06 independent wire decryption failed")
	}
	values, err := url.ParseQuery(string(plain))
	if err != nil {
		t.Fatal("HP06 decoded wire is not form data")
	}
	want := map[string]string{
		"MerID": "mock-account", "MerTradeNo": result.MerchantTradeNo,
		"TradeAmt":  strconv.FormatInt(result.AmountMinor/100, 10),
		"Timestamp": strconv.FormatInt(prepared.Unix(), 10),
		"ReturnURL": h.config.ReturnURL, "NotifyURL": h.config.NotifyURL,
		"ProdDesc": "Store order", "TradeLExpireSec": "300",
		"Lang": language, "Credit": "1",
	}
	if len(values) != len(want) {
		t.Fatal("HP06 wrong inner form field count")
	}
	for key, expected := range want {
		if got := values[key]; len(got) != 1 || got[0] != expected {
			t.Fatalf("HP06 wrong inner field %s", key)
		}
	}
}

func TestBuyerPaymentHostedReplayConflictsAndSingleAttempt(t *testing.T) {
	h := hpSetup(t)
	key := t04Key("hp-replay")
	result, err := h.begin(key)
	if err != nil {
		t.Fatal(err)
	}
	stored, prepared, expires, _ := h.page(t, result.AttemptID)
	counts := h.counts(t)
	replay, err := h.begin(key)
	if err != nil || !reflect.DeepEqual(replay, result) || h.counts(t) != counts {
		t.Fatal("HP02 identical key/input did not replay one receipt")
	}
	stored2, prepared2, expires2, _ := h.page(t, result.AttemptID)
	if !bytes.Equal(stored, stored2) || !prepared.Equal(prepared2) || !expires.Equal(expires2) {
		t.Fatal("HP02 replay changed the stored form or deadline")
	}
	for _, mutate := range []func(*checkout.HostedInput){
		func(in *checkout.HostedInput) { in.Locale = "en" },
		func(in *checkout.HostedInput) { in.MethodVersion++ },
		func(in *checkout.HostedInput) { in.OrderID = randomUUID() },
	} {
		in := h.input
		mutate(&in)
		if _, err := h.api.BeginHosted(context.Background(), h.cap.Token, h.f.storeA1, key, in); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("HP02 changed request accepted: %v", err)
		}
	}
	keys, err := accounts.NewKeyring("hosted_fixture", map[string][]byte{"hosted_fixture": h.key}, randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	config := h.config
	config.NotifyURL = "https://checkout.example.test/payment/notify-v2"
	for _, api := range []hpAPI{hpStarter(t, h.pool, "PROVIDER_MOCK", keys, config), hpStarter(t, h.pool, "SANDBOX", keys, h.config)} {
		if _, err := api.BeginHosted(context.Background(), h.cap.Token, h.f.storeA1, key, h.input); !errors.Is(err, command.ErrConflict) {
			t.Fatalf("HP02 changed profile/config accepted: %v", err)
		}
	}
	if _, err := h.begin(t04Key("hp-second-key")); !errors.Is(err, command.ErrConflict) {
		t.Fatalf("HP02 second key created a new attempt: %v", err)
	}
	if h.counts(t) != counts {
		t.Fatal("HP02 conflict mutated payment aggregate")
	}
}

func TestBuyerPaymentHostedConcurrentTakeOneForm(t *testing.T) {
	h := hpSetup(t)
	if _, err := h.begin(t04Key("hp-take-race")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan checkout.HostedHandoff, 2)
	errorsCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := h.take()
			results <- out
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("HP03 concurrent take: %v", err)
		}
	}
	issued, replay := 0, 0
	for out := range results {
		switch hpDisposition(t, out) {
		case "ISSUED":
			issued++
			hpForm(t, out)
		case "ALREADY_ISSUED":
			replay++
			hpNoForm(t, out)
		default:
			t.Fatal("HP03 unknown take disposition")
		}
	}
	if issued != 1 || replay != 1 {
		t.Fatal("HP03 concurrent handoff was not one-shot")
	}
}

func TestBuyerPaymentHostedCryptoFailureRollsBack(t *testing.T) {
	h := hpSetup(t)
	before := h.counts(t)
	wrong, err := accounts.NewKeyring("unknown_fixture", map[string][]byte{"unknown_fixture": randomBytes(32)}, randomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	api := hpStarter(t, h.pool, "PROVIDER_MOCK", wrong, h.config)
	if _, err := api.BeginHosted(context.Background(), h.cap.Token, h.f.storeA1, t04Key("hp-crypto-fail"), h.input); err == nil {
		t.Fatal("HP01 unreadable credential prepared a form")
	}
	if h.counts(t) != before {
		t.Fatal("HP01 crypto failure committed partial state")
	}
	if _, err := h.begin(t04Key("hp-crypto-retry")); err != nil {
		t.Fatal("HP01 rollback did not permit a clean retry")
	}
}

func TestBuyerPaymentHostedThreeFrozenLocales(t *testing.T) {
	for locale, language := range map[string]string{"zh-CN": "zh-tw", "zh-TW": "zh-tw", "en": "en"} {
		t.Run(locale, func(t *testing.T) {
			h := hpSetup(t)
			h.input.Locale = locale
			result, err := h.begin(t04Key("hp-locale"))
			if err != nil {
				t.Fatal(err)
			}
			_, prepared, _, _ := h.page(t, result.AttemptID)
			var persistedLocale string
			if err := h.f.owner.QueryRow(context.Background(), `SELECT locale FROM checkout.hosted_payment_pages WHERE attempt_id=$1`, result.AttemptID).Scan(&persistedLocale); err != nil || persistedLocale != locale {
				t.Fatal("HP02 requested locale was not frozen")
			}
			handoff, err := h.take()
			if err != nil || hpDisposition(t, handoff) != "ISSUED" {
				t.Fatal("HP06 locale handoff failed")
			}
			action, fields := hpForm(t, handoff)
			hpVerifyWire(t, h, result, prepared, action, fields, language)
		})
	}
}

func TestBuyerPaymentHostedAdmissionDriftBeforePrepare(t *testing.T) {
	for name, change := range map[string]func(*testing.T, hpHarness){
		"binding-disabled": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE integration.bindings SET enabled=false WHERE id=$1`, h.binding)
		},
		"method-hidden": func(t *testing.T, h hpHarness) {
			mustExec(t, h.f.owner, `UPDATE payments.method_versions SET visible=false WHERE connection_id=$1`, h.account)
		},
		"qualification-revoked": func(t *testing.T, h hpHarness) {
			qualExec(t, h.f.owner, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, h.proof)
		},
		"credential-rotated": func(t *testing.T, h hpHarness) {
			hpRotateFixtureHead(t, h)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := hpSetup(t)
			before := h.counts(t)
			change(t, h)
			if _, err := h.begin(t04Key("hp-admission-drift")); err == nil {
				t.Fatal("HP05 drift before preparation accepted")
			}
			if h.counts(t) != before {
				t.Fatal("HP05 denied preparation committed partial facts")
			}
		})
	}
	for _, profile := range []string{"SANDBOX", "LIVE"} {
		t.Run("mock-not-"+profile, func(t *testing.T) {
			h := hpSetup(t)
			before := h.counts(t)
			keys, err := accounts.NewKeyring("hosted_fixture", map[string][]byte{"hosted_fixture": h.key}, randomBytes(32))
			if err != nil {
				t.Fatal(err)
			}
			api := hpStarter(t, h.pool, profile, keys, h.config)
			if _, err := api.BeginHosted(context.Background(), h.cap.Token, h.f.storeA1, t04Key("hp-real-profile"), h.input); err == nil || h.counts(t) != before {
				t.Fatal("HP05 mock evidence qualified a real execution profile")
			}
		})
	}
}

func hpRotateFixtureHead(t *testing.T, h hpHarness) {
	t.Helper()
	mustExec(t, h.f.owner, `INSERT INTO integration.account_credentials(tenant_id,store_id,connection_id,version,key_id,nonce,ciphertext,principal_id)
	 SELECT tenant_id,store_id,connection_id,2,key_id,nonce,ciphertext,principal_id
	 FROM integration.account_credentials WHERE connection_id=$1 AND version=1`, h.account)
	mustExec(t, h.f.owner, `UPDATE integration.merchant_accounts SET credential_version=2 WHERE id=$1`, h.account)
}
