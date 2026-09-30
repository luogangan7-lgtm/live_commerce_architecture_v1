package foundation_test

// S4/S5 (r1-final-rulings; contracts/stripe-psp-v1.md §0.2, §13): REAL_PG + MOCK Stripe.
// The registrar binds every credential operation to the connection's REGISTERED account and STORED key:
//   - rotate refuses another account's key (it would seal an envelope that no worker can open);
//   - a SANDBOX probe uses the stored key at expected_version, so an old key or a foreign account's key
//     cannot qualify the head, and the probe evidence never names another account's session.

import (
	"context"
	"encoding/hex"
	"errors"
	"regexp"
	"testing"

	"livecommerce/internal/integrations/psp/stripe/stripetest"
	"livecommerce/internal/payments/stripeadmin"
)

func TestStripeSP21RegistrarBindsStoredCredential(t *testing.T) {
	p := psSetup(t)
	e := sstNewEnv(t, p.f, sstKeyring(t, "sst_api", randomBytes(32)))
	ctx := context.Background()
	s := e.seed(t, p) // credential v1 = s.secret for s.account
	foreign := "acct_T" + t04Tag()
	foreignKey := "sk_test_" + hex.EncodeToString(randomBytes(12))
	if err := e.fake.AddAccount(foreign, foreignKey); err != nil {
		t.Fatal(err)
	}
	head := func() int64 {
		var v int64
		if err := p.f.owner.QueryRow(ctx, `SELECT credential_version FROM integration.merchant_accounts WHERE id=$1`, s.connection).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	probes := func() int {
		return countRows(t, p.f.owner, `SELECT count(*) FROM payments.account_qualifications WHERE connection_id=$1 AND evidence_ref LIKE 'stripe-probe:%'`, s.connection)
	}
	qualify := func(version int64, account, key string) (string, error) {
		return e.reg.Qualify(ctx, s.scope, stripeadmin.QualifyInput{ConnectionID: s.connection, AccountID: account, SecretKey: key,
			Profile: "SANDBOX", Currency: "TWD", ReturnURL: sstReturnURL, ExpectedVersion: version, AmountMinor: 2500})
	}

	t.Run("rotate_with_another_accounts_key_is_rejected_and_the_head_stays", func(t *testing.T) {
		if _, err := e.reg.Rotate(ctx, s.scope, s.connection, 1, foreign, foreignKey); !errors.Is(err, stripeadmin.ErrRejected) {
			t.Fatalf("rotate with a foreign account's key: %v", err)
		}
		if head() != 1 || countRows(t, p.f.owner, `SELECT count(*) FROM integration.account_credentials WHERE connection_id=$1`, s.connection) != 1 {
			t.Fatal("a foreign-account envelope became a credential version")
		}
	})

	next := "sk_test_" + hex.EncodeToString(randomBytes(12))
	if err := e.fake.AddAccount(s.account, next); err != nil { // the account now also accepts the new key
		t.Fatal(err)
	}
	if v, err := e.reg.Rotate(ctx, s.scope, s.connection, 1, s.account, next); err != nil || v != 2 {
		t.Fatalf("rotate to the registered account's new key: %d %v", v, err)
	}

	t.Run("old_key_and_foreign_account_cannot_qualify_the_new_head", func(t *testing.T) {
		for name, c := range map[string][2]string{"old key": {s.account, s.secret}, "foreign account": {foreign, foreignKey}} {
			before := len(e.fake.CreateKeys())
			if _, err := qualify(2, c[0], c[1]); !errors.Is(err, stripeadmin.ErrRejected) {
				t.Fatalf("%s qualified version 2: %v", name, err)
			}
			if probes() != 0 || len(e.fake.CreateKeys()) != before {
				t.Fatalf("%s: evidence written or a probe session created", name)
			}
		}
	})

	t.Run("stored_key_qualifies_and_the_probe_uses_it", func(t *testing.T) {
		mark := len(e.fake.Requests())
		id, err := qualify(2, "", "") // env credentials omitted: the stored credential alone drives the probe
		if err != nil || id == "" {
			t.Fatalf("stored-credential qualify: %v", err)
		}
		var evidence string
		if err := p.f.owner.QueryRow(ctx, `SELECT evidence_ref FROM payments.account_qualifications WHERE id=$1`, id).Scan(&evidence); err != nil ||
			!regexp.MustCompile(`^stripe-probe:cs_test_[A-Za-z0-9_]+$`).MatchString(evidence) {
			t.Fatalf("probe evidence %q %v", evidence, err)
		}
		want := stripetest.KeyFingerprint(next)
		reqs := e.fake.Requests()[mark:]
		if len(reqs) == 0 {
			t.Fatal("no provider traffic recorded")
		}
		for _, r := range reqs {
			if r.KeyFingerprint != want {
				t.Fatalf("probe request %s used a key other than the stored head", r.Path)
			}
		}
	})

	t.Run("stale_expected_version_is_refused_before_any_provider_call", func(t *testing.T) {
		before := len(e.fake.Requests())
		if _, err := qualify(1, "", ""); !errors.Is(err, stripeadmin.ErrRejected) {
			t.Fatalf("stale expected version: %v", err)
		}
		if len(e.fake.Requests()) != before {
			t.Fatal("a stale-version qualify reached the provider")
		}
	})
}
