// service.go: the billing Service (contract §5, brief B3-B10): startup account check, merchant status
// with refresh-on-read, Checkout, portal and the plan list. Every method that talks to Stripe opens NO
// database transaction while it waits (each SQL step is its own short platform.WithScope transaction), and
// every merchant-visible SQL entry point is a 0079 definer that re-verifies the caller (billing:manage).
// It never charges buyers, blocks anything but a caller's own billing actions, logs a bearer URL or
// retries a Stripe call inside a request.

package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/platform"
)

const (
	billingPermission = "billing:manage"
	refreshAfter      = 10 * time.Minute // B10: refresh-on-read when a row is older
	plansTTL          = 10 * time.Minute // B7 process cache
	plansBudget       = 8 * time.Second  // total time for the plan fetches of one status read
	// checkoutLife: Stripe's minimum is 30 min (F-B4); 30 s of slack keeps a slow request from arriving
	// below it, and record_checkout_session accepts up to 31 min.
	checkoutLife = 30*time.Minute + 30*time.Second
)

// Subscription is one mirrored subscription as the merchant sees it (no Stripe ids beyond the price).
type Subscription struct {
	Status             string  `json:"status"`
	PriceID            string  `json:"price_id"`
	CurrentPeriodStart *string `json:"current_period_start"`
	CurrentPeriodEnd   *string `json:"current_period_end"`
	CancelAtPeriodEnd  bool    `json:"cancel_at_period_end"`
	RetrievedAt        string  `json:"retrieved_at"`
}

// Usage is the BD6 derived count set for the current period.
type Usage struct {
	PeriodStart        string `json:"period_start"`
	PeriodEnd          string `json:"period_end"`
	PaidOrders         int64  `json:"paid_orders"`
	ClaimWindowsOpened int64  `json:"claim_windows_opened"`
	PrivateRepliesSent int64  `json:"private_replies_sent"`
	Members            int64  `json:"members"`
}

// Plan is one configured, active, recurring, non-live Stripe price (B7).
type Plan struct {
	PriceID     string `json:"price_id"`
	Name        string `json:"name"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Interval    string `json:"interval"`
}

// Status is the GET billing body (contract §5): identical whether billing is enabled or not, except that
// plans is empty and no refresh happens when it is disabled.
type Status struct {
	Standing       Standing       `json:"standing"`
	PaymentPending bool           `json:"payment_pending"`
	CustomerPinned bool           `json:"customer_pinned"`
	Subscriptions  []Subscription `json:"subscriptions"`
	Usage          Usage          `json:"usage"`
	Plans          []Plan         `json:"plans"`
	Stale          bool           `json:"stale"`
}

// Service holds the platform Stripe client and the startup facts. The zero value and nil are "disabled".
type Service struct {
	cfg      Config
	stripe   *stripeClient
	account  string
	verifier *stripe.WebhookVerifier
	enabled  bool
	now      func() time.Time

	mu      sync.Mutex
	plans   []Plan
	plansAt time.Time
}

// New validates cfg and runs the B4 startup check on the runtime pool: GET /v1/account, then
// billing.platform_account_conflict(id). An unreadable account or a conflict returns a DISABLED service
// (Enabled() false, nil error): standing and GET billing keep working, checkout/portal/webhook answer 503
// for the process lifetime, and exactly one billing_account_conflict line is logged. There is no retry
// loop: an operator fixes the account and restarts. rt nil means http.DefaultTransport.
func New(ctx context.Context, runtime *pgxpool.Pool, cfg Config, rt http.RoundTripper) (*Service, error) {
	if ctx == nil || runtime == nil || !secretKeyPattern.MatchString(cfg.SecretKey) ||
		!webhookSecretPattern.MatchString(cfg.WebhookSecret) || len(cfg.PriceIDs) < 1 || len(cfg.PriceIDs) > maxPriceIDs {
		return nil, ErrConfig
	}
	if origin, ok := normalizeOrigin(cfg.ReturnOrigin); !ok || origin != cfg.ReturnOrigin {
		return nil, ErrConfig
	}
	s := &Service{cfg: cfg, stripe: newStripeClient(cfg.SecretKey, rt), now: time.Now}
	startup, cancel := context.WithTimeout(ctx, stripeTimeout+5*time.Second)
	defer cancel()
	account, err := s.stripe.account(startup)
	if err != nil {
		slog.Error("billing_account_conflict", "reason", "account_unreadable")
		return s, nil
	}
	var conflict bool
	// billing.platform_account_conflict: BD1, true when the id is a registered merchant Stripe account.
	if err := runtime.QueryRow(startup, `SELECT billing.platform_account_conflict($1::text)`, account).Scan(&conflict); err != nil || conflict {
		slog.Error("billing_account_conflict", "reason", "registered_or_unchecked")
		return s, nil
	}
	verifier, err := stripe.NewWebhookVerifier(stripe.WebhookConfig{
		Secrets: []string{cfg.WebhookSecret}, AccountID: account, Environment: envSandbox})
	if err != nil {
		slog.Error("billing_account_conflict", "reason", "webhook_config")
		return s, nil
	}
	s.account, s.verifier, s.enabled = account, verifier, true
	return s, nil
}

// Enabled reports whether checkout, portal and the webhook are live. Nil-safe.
func (s *Service) Enabled() bool { return s != nil && s.enabled }

// billingRead is identity.read_billing's document. stripe_customer_id is internal: Status never exposes it.
type billingRead struct {
	Standing         Standing       `json:"standing"`
	PaymentPending   bool           `json:"payment_pending"`
	CustomerPinned   bool           `json:"customer_pinned"`
	StripeCustomerID *string        `json:"stripe_customer_id"`
	Subscriptions    []Subscription `json:"subscriptions"`
	Usage            Usage          `json:"usage"`
}

// run opens one short merchant transaction (platform.WithScope sets the RLS GUCs the definers verify) and
// requires the freshly resolved scope to equal the caller's, so a token swapped between the route's
// authentication and this call cannot act on another principal's behalf.
func (s *Service) run(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token string, fn func(pgx.Tx, [32]byte) error) error {
	hash := sha256.Sum256([]byte(token))
	err := platform.WithScope(ctx, pool, token, scope.StoreID, billingPermission, func(tx pgx.Tx, got platform.Scope) error {
		if got.TenantID != scope.TenantID || got.PrincipalID != scope.PrincipalID {
			return platform.ErrForbidden
		}
		return fn(tx, hash)
	})
	return mapError(err)
}

func (s *Service) readBilling(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token string) (billingRead, error) {
	var raw []byte
	err := s.run(ctx, pool, scope, token, func(tx pgx.Tx, hash [32]byte) error {
		// identity.read_billing: billing:manage, standing + subscriptions + BD6 usage in one statement.
		return tx.QueryRow(ctx, `SELECT identity.read_billing($1,$2::uuid)`, hash[:], scope.StoreID).Scan(&raw)
	})
	if err != nil {
		return billingRead{}, err
	}
	var out billingRead
	if err := json.Unmarshal(raw, &out); err != nil || !out.Standing.valid() {
		return billingRead{}, ErrUnavailable
	}
	if out.Subscriptions == nil {
		out.Subscriptions = []Subscription{}
	}
	return out, nil
}

// Status is the GET billing read. With a pinned customer and a row older than 10 min it first refreshes
// from Stripe (B10); a Stripe failure returns the database state with Stale=true, never an error.
// Nil receiver and disabled service: plans [] and no Stripe call.
func (s *Service) Status(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token string) (Status, error) {
	r, err := s.readBilling(ctx, pool, scope, token)
	if err != nil {
		return Status{}, err
	}
	stale := false
	if s.Enabled() && r.StripeCustomerID != nil && s.needsRefresh(r.Subscriptions) {
		switch err := s.refresh(ctx, pool, scope, token, *r.StripeCustomerID); {
		case err == nil:
			if r, err = s.readBilling(ctx, pool, scope, token); err != nil {
				return Status{}, err
			}
		case err == errStripe:
			stale = true // Stripe unreachable: show what we have, flagged
		default:
			return Status{}, err
		}
	}
	return Status{Standing: r.Standing, PaymentPending: r.PaymentPending, CustomerPinned: r.CustomerPinned,
		Subscriptions: r.Subscriptions, Usage: r.Usage, Plans: s.planList(ctx), Stale: stale}, nil
}

// since is the non-negative seconds elapsed since t on this process's clock (monotonic when t came from
// time.Now). It is the only clock value that leaves the process toward the database.
func (s *Service) since(t time.Time) float64 {
	if d := s.now().Sub(t); d > 0 {
		return d.Seconds()
	}
	return 0
}

func (s *Service) needsRefresh(subs []Subscription) bool {
	for _, sub := range subs {
		at, err := time.Parse(time.RFC3339Nano, sub.RetrievedAt)
		if err != nil || s.now().Sub(at) > refreshAfter {
			return true
		}
	}
	return false
}

// ReadStanding reads the banner standing inside the caller's own scoped transaction (store:read).
func ReadStanding(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (Standing, error) {
	hash := sha256.Sum256([]byte(token))
	var value string
	// identity.read_billing_standing: store:read, verifies the transaction GUCs against resolve_access.
	if err := tx.QueryRow(ctx, `SELECT identity.read_billing_standing($1,$2::uuid)`, hash[:], scope.StoreID).Scan(&value); err != nil {
		return "", mapError(err)
	}
	if st := Standing(value); st.valid() {
		return st, nil
	}
	return "", ErrUnavailable
}

// refresh lists every subscription of the pinned customer and applies each through
// billing.refresh_subscription (store-scoped, billing:manage). errStripe means Stripe failed (the caller
// decides between stale and 503); any other error is a database error.
func (s *Service) refresh(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token, customer string) error {
	_, err := s.syncCustomer(ctx, pool, scope, token, customer)
	return err
}

// syncCustomer is the shared list-and-apply step of refresh-on-read and checkout step 1. It returns the
// listed statuses (multi-item subscriptions included: they still count as existing).
func (s *Service) syncCustomer(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token, customer string) ([]string, error) {
	// retrieved_at is the time the GET was SENT: of two overlapping retrievals the later-sent one carries
	// the newer data, so the monotone guard keeps the right row even if responses arrive out of order.
	sent := s.now()
	subs, err := s.stripe.listSubscriptions(ctx, customer)
	if err != nil {
		slog.Warn("billing_stripe_error", "op", "subscriptions_list")
		return nil, errStripe
	}
	statuses := make([]string, 0, len(subs))
	for _, w := range subs {
		statuses = append(statuses, w.Status)
		row, perr := w.project()
		if perr == errMultiItem {
			slog.Warn("billing_multi_item", "subscription", w.ID)
			continue
		}
		if perr != nil {
			slog.Warn("billing_subscription_unusable")
			continue
		}
		result, err := s.applyMerchant(ctx, pool, scope, token, row, sent)
		if err != nil {
			return nil, err
		}
		if result != "applied" && result != "stale" {
			slog.Warn("billing_ops_alert", "code", result, "subscription", row.ID)
		}
	}
	return statuses, nil
}

// applyMerchant calls billing.refresh_subscription in its own short transaction.
func (s *Service) applyMerchant(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token string, row subscriptionRow, sent time.Time) (string, error) {
	var result string
	err := s.run(ctx, pool, scope, token, func(tx pgx.Tx, hash [32]byte) error {
		// billing.refresh_subscription: the customer must belong to this store (PT404), then the mirror upsert.
		// p_retrieved_at = DB clock minus the time since the GET was sent (skew-proof, see webhook.go).
		return tx.QueryRow(ctx, `SELECT billing.refresh_subscription($1::bytea,$2::uuid,$3::text,$4::text,$5::text,$6::text,$7::text,
			$8::timestamptz,$9::timestamptz,$10::boolean,$11::timestamptz,clock_timestamp()-make_interval(secs=>$12::float8),
			$13::uuid,$14::boolean)`,
			hash[:], scope.StoreID, envSandbox, row.Customer, row.ID, row.Status, row.PriceID,
			row.PeriodStart, row.PeriodEnd, row.CancelAtPeriodEnd, row.Created, s.since(sent), row.MetaStore, row.Livemode).Scan(&result)
	})
	return result, err
}

// pinCustomer is billing.pin_customer in its own transaction; it returns the id actually pinned (a racing
// pin of the same idempotent customer replays; another id is a conflict).
func (s *Service) pinCustomer(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token, customer string) (string, error) {
	var pinned string
	err := s.run(ctx, pool, scope, token, func(tx pgx.Tx, hash [32]byte) error {
		// billing.pin_customer: set-once per store; re-checks BD1 against integration.merchant_accounts.
		return tx.QueryRow(ctx, `SELECT billing.pin_customer($1::bytea,$2::uuid,$3::text,$4::text,$5::text)`,
			hash[:], scope.StoreID, envSandbox, customer, s.account).Scan(&pinned)
	})
	return pinned, err
}

// StartCheckout runs the contract §5 steps and returns the Stripe-hosted Checkout URL (a bearer link:
// never logged, stored or put in an error). Steps: (1) sync the customer's subscriptions, (2) 409 when a
// non-terminal one exists, (3) create the session (trial only for a never-subscribed store, BD2),
// (4) record it and expire an older open session. No DB transaction is open during any Stripe call.
func (s *Service) StartCheckout(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token, priceID string) (string, error) {
	if !s.Enabled() {
		return "", ErrUnavailable
	}
	if !s.knownPrice(priceID) {
		return "", ErrUnknownPrice
	}
	before, err := s.readBilling(ctx, pool, scope, token)
	if err != nil {
		return "", err
	}
	customer := ""
	if before.StripeCustomerID != nil {
		customer = *before.StripeCustomerID
	} else {
		created, err := s.stripe.createCustomer(ctx, scope.StoreID)
		if err != nil {
			slog.Warn("billing_stripe_error", "op", "customer_create")
			return "", ErrUnavailable
		}
		if customer, err = s.pinCustomer(ctx, pool, scope, token, created); err != nil {
			return "", err
		}
	}
	statuses, err := s.syncCustomer(ctx, pool, scope, token, customer)
	if err == errStripe {
		return "", ErrUnavailable
	}
	if err != nil {
		return "", err
	}
	for _, status := range statuses {
		if status != "canceled" && status != "incomplete_expired" {
			return "", ErrSubscriptionExists
		}
	}
	expires := s.now().Add(checkoutLife)
	id, checkoutURL, err := s.stripe.createCheckout(ctx, checkoutParams{
		Customer: customer, Store: scope.StoreID, PriceID: priceID, ExpiresAt: expires,
		SuccessURL: s.returnURL(scope.StoreID, "done"), CancelURL: s.returnURL(scope.StoreID, "cancel"),
		// BD2: trial once = no local row of any status and Stripe listed no prior subscription.
		Trial: len(statuses) == 0 && len(before.Subscriptions) == 0,
	})
	if err != nil {
		slog.Warn("billing_stripe_error", "op", "checkout_create")
		return "", ErrUnavailable
	}
	var older *string
	if err := s.run(ctx, pool, scope, token, func(tx pgx.Tx, hash [32]byte) error {
		// billing.record_checkout_session: one open session per store; returns an older unexpired id.
		return tx.QueryRow(ctx, `SELECT billing.record_checkout_session($1::bytea,$2::uuid,$3::text,$4::text,$5::timestamptz)`,
			hash[:], scope.StoreID, envSandbox, id, expires).Scan(&older)
	}); err != nil {
		return "", err // the new session was never returned to anyone and expires within 30 min
	}
	if older != nil {
		// Expiring cannot move money and its key is stable per session id (a repeat replays). A failure
		// leaves the older session to expire on its own within 30 min, so it is only logged.
		if err := s.stripe.expireCheckout(ctx, *older); err != nil {
			slog.Warn("billing_stripe_error", "op", "checkout_expire")
		}
	}
	return checkoutURL, nil
}

// OpenPortal returns a Stripe customer-portal URL (a 5-minute bearer link, never logged or stored).
func (s *Service) OpenPortal(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token string) (string, error) {
	if !s.Enabled() {
		return "", ErrUnavailable
	}
	r, err := s.readBilling(ctx, pool, scope, token)
	if err != nil {
		return "", err
	}
	if r.StripeCustomerID == nil {
		return "", ErrNoCustomer
	}
	portalURL, err := s.stripe.createPortal(ctx, *r.StripeCustomerID, s.cfg.ReturnOrigin+"/billing?store="+scope.StoreID)
	if err != nil {
		slog.Warn("billing_stripe_error", "op", "portal_create")
		return "", ErrUnavailable
	}
	return portalURL, nil
}

func (s *Service) knownPrice(id string) bool {
	for _, known := range s.cfg.PriceIDs {
		if known == id {
			return true
		}
	}
	return false
}

// returnURL is B3: <origin>/billing?store=<id>&checkout=done|cancel (the admin proxy adds the locale).
func (s *Service) returnURL(store, outcome string) string {
	return s.cfg.ReturnOrigin + "/billing?store=" + store + "&checkout=" + outcome
}

// planList returns the cached plans, refetching after 10 min (B7). A failed fetch omits that plan, is
// logged, and is not cached, so the next read retries; the result is never nil.
func (s *Service) planList(ctx context.Context) []Plan {
	if !s.Enabled() {
		return []Plan{}
	}
	s.mu.Lock()
	if s.plans != nil && s.now().Sub(s.plansAt) < plansTTL {
		out := append([]Plan{}, s.plans...)
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()
	budget, cancel := context.WithTimeout(ctx, plansBudget)
	defer cancel()
	plans, failed := []Plan{}, false
	for _, id := range s.cfg.PriceIDs {
		plan, ok, err := s.stripe.retrievePrice(budget, id)
		if err != nil {
			failed = true
			slog.Warn("billing_stripe_error", "op", "price_retrieve")
			continue
		}
		if ok {
			plans = append(plans, plan)
		}
	}
	if !failed {
		s.mu.Lock()
		s.plans, s.plansAt = append([]Plan{}, plans...), s.now()
		s.mu.Unlock()
	}
	return plans
}
