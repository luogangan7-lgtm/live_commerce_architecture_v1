package foundation_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/platform"
)

type sslStripeAttempt struct {
	p                          psHarness
	registrar, hosted          *pgxpool.Pool
	connection, binding, proof string
	attempt                    string
	tokenHash, requestHash     [32]byte
	configDigest               [32]byte
}

// sslStartStripe uses the existing buyer/order fixture and the frozen SQL
// registrar/start interfaces. The synthetic credential is never decrypted.
func sslStartStripe(t *testing.T) sslStripeAttempt {
	t.Helper()
	p := psSetup(t)
	ctx := context.Background()
	_, registrar := lmaLogin(t, p.f, "commerce_payment_registrar")
	connection, binding, proof := randomUUID(), randomUUID(), randomUUID()
	account := "acct_" + strings.ReplaceAll(randomUUID(), "-", "")
	var registered string
	if err := registrar.QueryRow(ctx, `SELECT integration.register_stripe_account(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'SANDBOX',$6,'fixture_key',$7::bytea,$8::bytea)`,
		p.f.tenantA, p.f.storeA1, p.f.principalA, connection, binding, account, randomBytes(12), randomBytes(48)).Scan(&registered); err != nil || registered == "" {
		t.Fatalf("register synthetic Stripe account: %v", err)
	}
	observed := time.Now().UTC().Add(-time.Second)
	var qualified string
	if err := registrar.QueryRow(ctx, `SELECT payments.qualify_stripe_method(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,1,'PROVIDER_MOCK','synthetic local probe',$6,$7)`,
		p.f.tenantA, p.f.storeA1, p.f.principalA, proof, connection, observed, observed.Add(time.Hour)).Scan(&qualified); err != nil || qualified == "" {
		t.Fatalf("qualify synthetic Stripe method: %v", err)
	}
	var methodVersion int64
	if err := registrar.QueryRow(ctx, `SELECT payments.set_stripe_method(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,'TW',$5::uuid,$6::uuid,0,true,true,1,2500,99999900,'Stripe','Stripe','Stripe')`,
		p.f.tenantA, p.f.storeA1, p.f.principalA, p.market.ID, connection, proof).Scan(&methodVersion); err != nil || methodVersion != 1 {
		t.Fatalf("enable synthetic Stripe method: version=%d err=%v", methodVersion, err)
	}

	hosted, err := platform.OpenHostedPool(ctx, hpRole(t, p.f))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hosted.Close)
	jobs, err := river.NewClient(riverpgxv5.New(hosted), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte(p.cap.Token))
	requestHash := sha256.Sum256([]byte("synthetic stripe start request"))
	configDigest := sha256.Sum256([]byte("synthetic stripe-hosted-v1 config"))
	attempt := randomUUID()
	key := t04Key("stripe-sql-fixture")
	err = buyer.WithScope(ctx, hosted, p.cap.Token, p.f.storeA1, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		if scope.OwnerID != p.cap.Scope.OwnerID {
			return fmt.Errorf("buyer scope changed during Stripe start")
		}
		job, err := jobs.InsertTx(callCtx, tx, pqArgs{OperationID: attempt, Version: 1},
			&river.InsertOpts{Queue: "payment_mock_v1", ScheduledAt: time.Now().Add(5 * time.Second)})
		if err != nil {
			return err
		}
		var raw []byte
		if err = tx.QueryRow(callCtx, `SELECT checkout.start_stripe_payment(
		 $1,$2::uuid,$3,$4,$5::uuid,'stripe_checkout',1,'PROVIDER_MOCK',$6::uuid,$7,'zh-TW',$8,$9)`,
			tokenHash[:], p.f.storeA1, key, requestHash[:], p.hold.OrderID, attempt,
			job.Job.ID, configDigest[:], "https://checkout.example.test/payment/return").Scan(&raw); err != nil {
			return err
		}
		var result struct {
			AttemptID string `json:"attempt_id"`
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if result.AttemptID != attempt {
			return fmt.Errorf("start returned wrong attempt: %q", result.AttemptID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("start synthetic Stripe attempt: %v", err)
	}
	return sslStripeAttempt{p: p, registrar: registrar, hosted: hosted, connection: connection,
		binding: binding, proof: proof, attempt: attempt, tokenHash: tokenHash,
		requestHash: requestHash, configDigest: configDigest}
}

// TestStripeSP21PinnedHandoffAfterKeyRotation exercises the historical attempt,
// not a new-start eligibility check. All account keys and the Checkout URL are
// synthetic; the disposable PG fixture makes no provider request.
func TestStripeSP21PinnedHandoffAfterKeyRotation(t *testing.T) {
	h := sslStartStripe(t)
	p, registrar, hosted := h.p, h.registrar, h.hosted
	connection, binding, proof, attempt := h.connection, h.binding, h.proof, h.attempt
	tokenHash, requestHash, configDigest := h.tokenHash, h.requestHash, h.configDigest
	ctx := context.Background()

	// Owner fixture pinning replaces only the worker observation path. The
	// handoff itself runs through the real hosted role and frozen SQL function.
	sessionID := "cs_test_" + strings.ReplaceAll(randomUUID(), "-", "")
	url := "https://checkout.stripe.com/c/pay/" + sessionID
	mustExec(t, p.f.owner, `UPDATE integration.operations SET provider_reference=$2 WHERE id=$1`, attempt, sessionID)
	mustExec(t, p.f.owner, `UPDATE payments.stripe_sessions SET
	 create_first_sent_at=clock_timestamp(),create_last_sent_at=clock_timestamp(),create_send_count=1,
	 create_body_sha256=$2,session_id=$3,session_url=$4,pinned_at=clock_timestamp()
	 WHERE attempt_id=$1`, attempt, requestHash[:], sessionID, url)
	take := func(hash []byte, profile string, digest []byte) (map[string]any, error) {
		var raw []byte
		err := hosted.QueryRow(ctx, `SELECT checkout.take_stripe_handoff($1,$2::uuid,$3::uuid,$4,$5)`,
			hash, p.f.storeA1, p.hold.OrderID, profile, digest).Scan(&raw)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		err = json.Unmarshal(raw, &out)
		return out, err
	}
	assertRedirect := func(label string) {
		t.Helper()
		out, err := take(tokenHash[:], "PROVIDER_MOCK", configDigest[:])
		if err != nil || out["disposition"] != "REDIRECT" || out["redirect_url"] != url {
			t.Fatalf("%s handoff=%v err=%v", label, out, err)
		}
	}
	assertRedirect("before rotation")
	var firstHandoff time.Time
	if err := p.f.owner.QueryRow(ctx, `SELECT first_handed_out_at FROM payments.stripe_sessions WHERE attempt_id=$1`, attempt).Scan(&firstHandoff); err != nil {
		t.Fatal(err)
	}
	var head int64
	if err := registrar.QueryRow(ctx, `SELECT integration.rotate_stripe_key(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,1,'fixture_key_v2',$5::bytea,$6::bytea)`,
		p.f.tenantA, p.f.storeA1, p.f.principalA, connection, randomBytes(12), randomBytes(48)).Scan(&head); err != nil || head != 2 {
		t.Fatalf("rotate API credential: head=%d err=%v", head, err)
	}
	assertRedirect("after rotation")
	var pinnedVersion, currentVersion int64
	var secondHandoff time.Time
	var boundID string
	if err := p.f.owner.QueryRow(ctx, `SELECT a.credential_version,m.credential_version,m.binding_id::text,s.first_handed_out_at
	 FROM checkout.payment_attempts a JOIN payments.stripe_sessions s ON s.attempt_id=a.id
	 JOIN integration.merchant_accounts m ON m.id=a.connection_id WHERE a.id=$1`, attempt).
		Scan(&pinnedVersion, &currentVersion, &boundID, &secondHandoff); err != nil {
		t.Fatal(err)
	}
	if pinnedVersion != 1 || currentVersion != 2 || boundID != binding || !secondHandoff.Equal(firstHandoff) {
		t.Fatalf("historical pin/head/binding/first handoff changed: pin=%d head=%d binding_same=%t time_same=%t",
			pinnedVersion, currentVersion, boundID == binding, secondHandoff.Equal(firstHandoff))
	}
	for _, v := range []struct {
		name, profile string
		digest        []byte
	}{
		{"profile mismatch", "SANDBOX", configDigest[:]},
		{"config mismatch", "PROVIDER_MOCK", requestHash[:]},
	} {
		t.Run(v.name, func(t *testing.T) {
			out, err := take(tokenHash[:], v.profile, v.digest)
			if err != nil || out["disposition"] != "UNAVAILABLE" || out["redirect_url"] != nil {
				t.Fatalf("mismatched handoff=%v err=%v", out, err)
			}
		})
	}
	if out, err := take(requestHash[:], "PROVIDER_MOCK", configDigest[:]); err == nil && (out["disposition"] == "REDIRECT" || out["redirect_url"] != nil) {
		t.Fatalf("forged owner handoff disclosed URL: %v", out)
	}
	qualExec(t, p.f.owner, `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE id=$1`, proof)
	if out, err := take(tokenHash[:], "PROVIDER_MOCK", configDigest[:]); err != nil || out["disposition"] != "UNAVAILABLE" || out["redirect_url"] != nil {
		t.Fatalf("revoked qualification handoff=%v err=%v", out, err)
	}
}
