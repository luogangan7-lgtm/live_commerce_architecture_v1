// Command stripe-admin is the operator-only Stripe registrar CLI (contracts/stripe-psp-v1.md §13,
// contracts/stripe-live-enable-v1.md §5.2). It owns argument/env parsing and one JSON result line; all
// logic lives in internal/payments/stripeadmin. It never prints keys or secrets (stdout carries IDs,
// versions and timestamps only, stderr one fixed code), never runs in the API or worker, and touches LIVE
// only when the owner's COMMERCE_STRIPE_LIVE_ENABLED=1 + COMMERCE_STRIPE_LIVE_APPROVAL_REF pair is set
// (register/rotate --environment LIVE, webhook/qualify --profile LIVE, live-approve, live-canary);
// live-revoke and method never read the pair, so the per-store kill switch does not depend on deploy env.
//
// Environment: COMMERCE_STRIPE_REGISTRAR_DATABASE_URL; STRIPE_SECRET_KEY and STRIPE_ACCOUNT_ID for
// register/rotate (rotate must match the registered account); SANDBOX qualify needs STRIPE_SANDBOX=1
// and the API keyring, probes with the stored credential and treats STRIPE_SECRET_KEY /
// STRIPE_ACCOUNT_ID as optional assertions (S4); LIVE qualify and live-approve use the API keyring and
// the stored key the same way (no key input, no STRIPE_SANDBOX); STRIPE_WEBHOOK_SECRET[_NEXT]
// for webhook, whose AAD account is derived from the registered connection in SQL (§0.2), never
// from STRIPE_ACCOUNT_ID (least privilege; SP15 env sentinel).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/payments/stripeadmin"
	"livecommerce/internal/platform"
)

var (
	errUsage  = errors.New("stripe_admin_usage")
	errConfig = errors.New("stripe_admin_config")
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		// Only fixed sentinel text can reach here: no driver, Stripe or flag-value message.
		_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
		os.Exit(1)
	}
}

type command struct {
	scope stripeadmin.Scope
	fs    *flag.FlagSet
}

func newCommand(name string) *command {
	c := &command{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	c.fs.SetOutput(io.Discard) // flag errors echo the offending value; usage errors are fixed instead
	c.fs.StringVar(&c.scope.TenantID, "tenant", "", "")
	c.fs.StringVar(&c.scope.StoreID, "store", "", "")
	c.fs.StringVar(&c.scope.PrincipalID, "principal", "", "")
	return c
}

func (c *command) parse(args []string) error {
	if c.fs.Parse(args) != nil || c.fs.NArg() != 0 {
		return errUsage
	}
	return nil
}

func emit(stdout io.Writer, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return errConfig
	}
	_, err = stdout.Write(append(b, '\n'))
	return err
}

func apiKeyring(getenv func(string) string) (*accounts.Keyring, error) {
	k, err := accounts.LoadKeyring(getenv)
	if err != nil {
		return nil, errConfig
	}
	return k, nil
}

func signingKeyring(getenv func(string) string) (*accounts.Keyring, error) {
	k, err := accounts.LoadKeyring(func(name string) string {
		if suffix, ok := strings.CutPrefix(name, "COMMERCE_ACCOUNT_"); ok {
			return getenv("COMMERCE_STRIPE_WEBHOOK_" + suffix)
		}
		return ""
	})
	if err != nil {
		return nil, errConfig
	}
	return k, nil
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	if ctx == nil || getenv == nil || stdout == nil || len(args) < 1 {
		return errUsage
	}
	name, rest := args[0], args[1:]
	if strings.HasPrefix(name, "live-") {
		return runLive(ctx, name, rest, getenv, stdout) // live.go: live-approve, live-canary, live-revoke
	}
	c := newCommand(name)
	var connection, endpoint, profile, currency, returnURL, market, country, qualification, environment string
	var expected, amount, minMinor, maxMinor int64
	var enabled, visible bool
	var sort int
	var nameHans, nameHant, nameEN string
	switch name {
	case "register":
		c.fs.StringVar(&environment, "environment", "SANDBOX", "")
	case "rotate":
		c.fs.StringVar(&environment, "environment", "SANDBOX", "")
		c.fs.StringVar(&connection, "connection", "", "")
		c.fs.Int64Var(&expected, "expected-version", 0, "")
	case "webhook":
		c.fs.StringVar(&connection, "connection", "", "")
		c.fs.StringVar(&endpoint, "endpoint", "", "")
		c.fs.StringVar(&profile, "profile", "", "")
		c.fs.Int64Var(&expected, "expected-version", 0, "")
		c.fs.BoolVar(&enabled, "enabled", false, "")
	case "qualify":
		c.fs.StringVar(&connection, "connection", "", "")
		c.fs.Int64Var(&expected, "expected-version", 0, "")
		c.fs.StringVar(&profile, "profile", "", "")
		c.fs.StringVar(&currency, "currency", "", "")
		c.fs.Int64Var(&amount, "amount-minor", 0, "")
		c.fs.StringVar(&returnURL, "return-url", "", "")
	case "method":
		c.fs.StringVar(&market, "market", "", "")
		c.fs.StringVar(&country, "country", "", "")
		c.fs.StringVar(&connection, "connection", "", "")
		c.fs.StringVar(&qualification, "qualification", "", "")
		c.fs.Int64Var(&expected, "expected-version", 0, "")
		c.fs.BoolVar(&enabled, "enabled", false, "")
		c.fs.BoolVar(&visible, "visible", false, "")
		c.fs.IntVar(&sort, "sort", 0, "")
		c.fs.Int64Var(&minMinor, "min", 0, "")
		c.fs.Int64Var(&maxMinor, "max", 0, "")
		c.fs.StringVar(&nameHans, "name-hans", "", "")
		c.fs.StringVar(&nameHant, "name-hant", "", "")
		c.fs.StringVar(&nameEN, "name-en", "", "")
	default:
		return errUsage
	}
	if err := c.parse(rest); err != nil {
		return err
	}
	if (name == "register" || name == "rotate") && environment != "SANDBOX" && environment != "LIVE" {
		return errUsage
	}
	dsn := getenv("COMMERCE_STRIPE_REGISTRAR_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		return errConfig
	}
	// LIVE (register/rotate --environment LIVE, webhook/qualify --profile LIVE) needs the owner's pair; every
	// other path reads no LIVE variable at all (SP15: each subcommand reads only its own names).
	var live stripe.LiveApproval
	if environment == "LIVE" || ((name == "webhook" || name == "qualify") && profile == "LIVE") {
		var err error
		if live, err = requireLivePair(getenv); err != nil {
			return err
		}
	}

	// Validate everything the operation needs from the environment before any connection opens.
	var apiKeys, signingKeys *accounts.Keyring
	var accountID, secretKey string
	var secrets accounts.StripeWebhookSecrets
	var err error
	switch name {
	case "register", "rotate":
		if apiKeys, err = apiKeyring(getenv); err != nil {
			return err
		}
		accountID, secretKey = getenv("STRIPE_ACCOUNT_ID"), getenv("STRIPE_SECRET_KEY")
	case "webhook":
		if signingKeys, err = signingKeyring(getenv); err != nil {
			return err
		}
		secrets = accounts.StripeWebhookSecrets{CurrentSecret: getenv("STRIPE_WEBHOOK_SECRET"),
			NextSecret: getenv("STRIPE_WEBHOOK_SECRET_NEXT")}
	case "qualify":
		if profile == "SANDBOX" || profile == "LIVE" {
			// The probe creates and expires a real Checkout Session (sandbox: explicit opt-in only; LIVE: the
			// owner's pair, checked above, is the opt-in, and nothing can be charged).
			if profile == "SANDBOX" && getenv("STRIPE_SANDBOX") != "1" {
				return errConfig
			}
			// S4: the probe uses the credential stored at --expected-version, opened with the API
			// keyring; STRIPE_SECRET_KEY / STRIPE_ACCOUNT_ID are optional assertions that must
			// match the stored key and the registered account.
			if apiKeys, err = apiKeyring(getenv); err != nil {
				return err
			}
			accountID, secretKey = getenv("STRIPE_ACCOUNT_ID"), getenv("STRIPE_SECRET_KEY")
		}
	}

	// No mock transport is ever passed here: the deployable CLI can only dial api.stripe.com (SP20).
	reg, err := openRegistrar(ctx, dsn, apiKeys, signingKeys, live)
	if err != nil {
		return err
	}
	defer reg.Close()
	switch name {
	case "register":
		id, err := reg.Register(ctx, c.scope, accountID, secretKey)
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"connection_id": id, "credential_version": 1})
	case "rotate":
		v, err := reg.Rotate(ctx, c.scope, connection, expected, accountID, secretKey)
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"connection_id": connection, "credential_version": v})
	case "webhook":
		id, v, err := reg.SetWebhookEndpoint(ctx, c.scope, stripeadmin.EndpointInput{ConnectionID: connection,
			EndpointID: endpoint, Profile: profile, ExpectedVersion: expected,
			Enabled: enabled, Secrets: secrets})
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"endpoint_id": id, "key_version": v})
	case "qualify":
		id, err := reg.Qualify(ctx, c.scope, stripeadmin.QualifyInput{ConnectionID: connection,
			AccountID: accountID, SecretKey: secretKey, Profile: profile, Currency: currency,
			ReturnURL: returnURL, ExpectedVersion: expected, AmountMinor: amount})
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"qualification_id": id})
	default: // method
		if sort < 0 || sort > 1000 {
			return errUsage
		}
		v, err := reg.SetMethod(ctx, c.scope, stripeadmin.MethodInput{MarketID: market, Country: country,
			ConnectionID: connection, QualificationID: qualification, ExpectedVersion: expected,
			Enabled: enabled, Visible: visible, Sort: int32(sort), MinMinor: minMinor, MaxMinor: maxMinor,
			NameHans: nameHans, NameHant: nameHant, NameEN: nameEN})
		if err != nil {
			return err
		}
		return emit(stdout, map[string]any{"method_version": v})
	}
}

// requireLivePair returns the owner's complete flag+reference pair or errConfig. The fixed sentinel never
// carries the reference. Called only on the paths that need LIVE (see run and runLive).
func requireLivePair(getenv func(string) string) (stripe.LiveApproval, error) {
	live, err := platform.LoadStripeLiveApproval(getenv)
	if err != nil || !live.Valid() {
		return stripe.LiveApproval{}, errConfig
	}
	return live, nil
}

// openRegistrar opens the SANDBOX registrar (no pair) or the LIVE one. No mock transport is ever passed here: the
// deployable CLI can only dial api.stripe.com (SP20).
func openRegistrar(ctx context.Context, dsn string, apiKeys, signingKeys *accounts.Keyring,
	live stripe.LiveApproval) (*stripeadmin.Registrar, error) {
	if live.Enabled {
		return stripeadmin.OpenLive(ctx, dsn, apiKeys, signingKeys, live)
	}
	return stripeadmin.Open(ctx, dsn, apiKeys, signingKeys)
}
