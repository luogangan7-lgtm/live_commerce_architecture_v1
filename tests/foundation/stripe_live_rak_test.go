package foundation_test

// SL08 (contracts/stripe-live-enable-v1.md §11 SL08, §1 L3/L12): the least restricted-key permission set, SANDBOX tier, real
// api.stripe.com in TEST mode only, through the real adapter. Skipped (NOT_RUN, never PASS) unless STRIPE_SANDBOX=1.
//
// Key custody: the owner's `rk_test_` key comes from the environment variable STRIPE_RESTRICTED_TEST_KEY or, when that is
// unset, from that one variable of ~/.config/livecommerce/secrets.env (read only under STRIPE_SANDBOX=1). Anything but an
// rk_test_ key (sk_test_, any live key) is refused before any call. The key is never logged, printed, put in an error or
// written to evidence: the evidence file holds permission NAMES and booleans only.
// Optional STRIPE_HARNESS_KEY (an sk_test_ key that may create test PaymentIntents) is used only to pay the fixture
// PaymentIntent of the refund steps, so the product key stays least-privilege (RF10 pattern); without it those steps are
// NOT_RUN and named as such in the evidence.
//
// What runs with the RAK: VerifyAccount + AccountReadiness (which readiness fields a RAK receives: booleans), the SP16
// probe (checkout session create, expire, retrieve) and list (FindCheckoutSessions), and with a harness PaymentIntent the RF10
// steps (refund create, retrieve, list, PaymentIntent retrieve with latest_charge). Raw read probes (GET, no side effects)
// capture the permission names Stripe reports in a 403 (matched as rak_... tokens only). A 403 on any required step adds one
// permission to the candidate set by hand and reruns (contract): this test reports which step and which names, then fails.
// Evidence: <LC_EVIDENCE_DIR or ../../output/stripe-live>/rak-permissions.txt.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe"
)

const slkAccount = "acct_1UJDb0RusP6Wwj7e" // the SANDBOX fixture account (public id, as SP16/RF10)

func slkKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("STRIPE_RESTRICTED_TEST_KEY")
	if key == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal("refused: no key and no home directory")
		}
		f, err := os.Open(filepath.Join(home, ".config", "livecommerce", "secrets.env"))
		if err != nil {
			t.Fatal("NOT_RUN-blocked: STRIPE_RESTRICTED_TEST_KEY is unset and the owner's secrets.env is not readable")
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if name, value, ok := strings.Cut(line, "="); ok && strings.TrimSpace(name) == "STRIPE_RESTRICTED_TEST_KEY" {
				key = strings.Trim(strings.TrimSpace(value), `"'`)
			}
		}
	}
	if !strings.HasPrefix(key, "rk_test_") {
		t.Fatal("refused: STRIPE_RESTRICTED_TEST_KEY must be an rk_test_ key (sk_test_ and every live key are refused)")
	}
	return key
}

var slkPermission = regexp.MustCompile(`rak_[a-z0-9_]+`)

// slkRawGet reads one path with the RAK and returns the status and the permission names of a 403 body.
func slkRawGet(key, path string) (int, []string) {
	req, err := http.NewRequest(http.MethodGet, "https://api.stripe.com"+path, nil)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Stripe-Version", stripe.APIVersion)
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	var names []string
	if res.StatusCode == http.StatusForbidden {
		seen := map[string]bool{}
		for _, m := range slkPermission.FindAllString(string(raw), -1) {
			if !seen[m] {
				seen[m] = true
				names = append(names, m)
			}
		}
		sort.Strings(names)
	}
	return res.StatusCode, names
}

func TestStripeSL08RestrictedKey(t *testing.T) {
	if os.Getenv("STRIPE_SANDBOX") != "1" {
		t.Skip("NOT_RUN: SL08 needs STRIPE_SANDBOX=1 and the owner's rk_test_ restricted test key (STRIPE_RESTRICTED_TEST_KEY); SANDBOX only, LIVE is refused")
	}
	key := slkKey(t)
	client, err := stripe.New(stripe.Config{SecretKey: key, AccountID: slkAccount, Environment: "SANDBOX"})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	var lines []string
	note := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	failed := []string{}
	step := func(name string, status int, err error) {
		note("step %-34s http=%d ok=%v", name, status, err == nil)
		if err != nil {
			failed = append(failed, name)
		}
	}
	defer func() {
		dir := os.Getenv("LC_EVIDENCE_DIR")
		if dir == "" {
			dir = filepath.Join("..", "..", "output", "stripe-live")
		}
		if err := os.MkdirAll(dir, 0o755); err == nil {
			body := "SL08 restricted-key permissions (SANDBOX, names and booleans only; no key, id, amount or PII)\n" + strings.Join(lines, "\n") + "\n"
			_ = os.WriteFile(filepath.Join(dir, "rak-permissions.txt"), []byte(body), 0o644)
		}
	}()
	note("key kind=rk_test api_version=%s account=fixture", stripe.APIVersion)

	// Account read: readiness fields a RAK receives (booleans only) and whether any runtime path needs it.
	meta, err := client.VerifyAccount(ctx)
	step("verify_account (GET /v1/account)", meta.HTTPStatus, err)
	r, meta, err := client.AccountReadiness(ctx)
	step("account_readiness", meta.HTTPStatus, err)
	present := func(name string, ok bool) { note("readiness field %-18s present=%v", name, ok) }
	present("ChargesEnabled", r.ChargesEnabled != nil)
	present("PayoutsEnabled", r.PayoutsEnabled != nil)
	present("DetailsSubmitted", r.DetailsSubmitted != nil)
	present("CurrentlyDueCount", r.CurrentlyDueCount != nil)
	present("DescriptorLength", r.DescriptorLength != nil)
	present("PrefixLength", r.PrefixLength != nil)
	present("CVCRule", r.CVCRule != nil)
	present("AVSRule", r.AVSRule != nil)
	if _, jerr := r.JSON(); jerr != nil {
		note("readiness JSON: a required field is absent for this key (live-approve would fail closed stripe_live_readiness_unknown)")
	} else {
		note("readiness JSON: complete for this key")
	}
	note("account read is used by: VerifyAccount at register/qualify (registrar) and live-approve; the worker/refund runtime paths are asserted below by running them without any account read")

	// SP16: checkout create + expire + retrieve (probe) and list.
	returnURL := "https://example.com/livecommerce/payment/return"
	_, meta, err = client.ProbeCheckout(ctx, randomUUID(), "HKD", 400, returnURL)
	step("probe_checkout create+expire+retrieve", meta.HTTPStatus, err)
	_, meta, err = client.FindCheckoutSessions(ctx, randomUUID(), time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix())
	step("find_checkout_sessions (list)", meta.HTTPStatus, err)

	// Raw read probes name the permissions Stripe wants for each resource family (403 body, rak_ tokens only).
	for _, p := range []struct{ name, path string }{
		{"account", "/v1/account"}, {"checkout_sessions", "/v1/checkout/sessions?limit=1"}, {"payment_intents", "/v1/payment_intents?limit=1"},
		{"charges", "/v1/charges?limit=1"}, {"refunds", "/v1/refunds?limit=1"},
	} {
		status, names := slkRawGet(key, p.path)
		note("read probe %-18s http=%d permissions_named_in_403=%s", p.name, status, strings.Join(names, ","))
	}

	// RF10 steps need a paid test PaymentIntent, which only a harness key may create (the RAK candidate set has no PaymentIntents write).
	harness := os.Getenv("STRIPE_HARNESS_KEY")
	if harness == "" || !strings.HasPrefix(harness, "sk_test_") {
		note("refund steps: NOT_RUN (STRIPE_HARNESS_KEY, an sk_test_ key that may create test PaymentIntents, is absent)")
	} else {
		pi := rf10PaymentIntent(t, harness, 400)
		attempt, ref := randomUUID(), randomUUID()
		refund, meta, err := client.CreateRefund(ctx, stripe.RefundParams{PaymentIntentID: pi, AmountMinor: 100, Currency: "HKD", Reason: "requested_by_customer", RefundRef: ref, AttemptRef: attempt})
		step("refund_create", meta.HTTPStatus, err)
		if err == nil {
			_, meta, err = client.RetrieveRefund(ctx, refund.ID)
			step("refund_retrieve", meta.HTTPStatus, err)
		}
		_, meta, err = client.ListRefunds(ctx, pi, "")
		step("refund_list", meta.HTTPStatus, err)
		_, meta, err = client.RetrievePaymentCharge(ctx, pi)
		step("payment_intent_retrieve_latest_charge", meta.HTTPStatus, err)
	}
	if len(failed) > 0 {
		note("RESULT: candidate permission set is INSUFFICIENT for: %s (add exactly one permission, rerun)", strings.Join(failed, ", "))
		t.Fatalf("restricted key lacks a permission for: %s (see rak-permissions.txt; the key was never printed)", strings.Join(failed, ", "))
	}
	note("RESULT: every step above ran with the restricted key; the least permission names are those the read probes reported")
}
