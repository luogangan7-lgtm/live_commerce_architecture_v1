// webhook.go: POST /v1/platform/stripe/webhook (contract §5 Platform, brief B9). A verified event is only
// a wake-up (stripe-psp D8): the handler resolves the subscription id, RETRIEVES the subscription from
// Stripe with no database transaction open, and mirrors it with billing.apply_subscription on the
// dedicated ingress login (commerce_stripe_ingress, C-4). It never trusts event payload fields as state,
// never applies a livemode event (BD8) and never logs a body, signature or secret.
//
// Status rules: signature error, malformed body or livemode -> 400; a handled event whose retrieve or
// database write fails -> 503 (Stripe retries live webhooks for up to 3 days, F-B9,
// https://docs.stripe.com/billing/subscriptions/webhooks retrieved 2026-09-29); everything else -> 200.

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/httperror"
	"livecommerce/internal/integrations/psp/stripe"
)

const maxWebhookBody = 64 << 10 // §7

// handledEvents are the §5 types: customer.subscription.* carry the subscription as the event object,
// invoice.* carry it under parent.subscription_details (F-B3). Others are acknowledged and ignored.
var handledEvents = map[string]bool{
	"customer.subscription.created": true, "customer.subscription.updated": true,
	"customer.subscription.deleted": true, "customer.subscription.paused": true,
	"customer.subscription.resumed": true, "invoice.paid": true, "invoice.payment_failed": true,
}

// WebhookHandler returns the platform webhook. ingress is the commerce_stripe_ingress pool (the ONLY pool
// that may execute billing.apply_subscription, C-4). A nil/disabled service or nil pool yields a handler
// that answers 503 so Stripe retries rather than dropping events.
func (s *Service) WebhookHandler(ingress *pgxpool.Pool) http.Handler {
	if !s.Enabled() || ingress == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			httperror.Write(w, http.StatusServiceUnavailable, "unavailable")
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveWebhook(w, r, ingress) })
}

func (s *Service) serveWebhook(w http.ResponseWriter, r *http.Request, ingress *pgxpool.Pool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httperror.Write(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	signatures := r.Header.Values("Stripe-Signature")
	if err != nil || len(signatures) != 1 {
		reject(w, "body_or_header")
		return
	}
	// Signature first: nothing below runs on an unauthenticated body.
	ev, err := s.verifier.Verify(raw, signatures[0], s.now())
	switch {
	case err != nil: // stripe.ErrSignature (any header/tolerance/MAC failure) or an unusable verifier
		reject(w, "signature")
		return
	case ev.Malformed:
		reject(w, "malformed")
		return
	case ev.Livemode: // BD8: a live event is never applied by the sandbox-only platform service
		reject(w, "livemode")
		return
	case !handledEvents[ev.Type] || ev.AccountPresent:
		acknowledge(w) // not ours: other types, and Connect events (no platform subscription rides on them)
		return
	}
	subID, ok := subscriptionID(ev, raw)
	if !ok {
		if strings.HasPrefix(ev.Type, "invoice.") {
			// An endpoint on an API version older than stripe.APIVersion has no invoice.parent: without this line the
			// miss is silent and standing would depend on customer.subscription.updated alone (runbook §6.4 pins it).
			slog.Warn("billing_ops_alert", "code", "invoice_without_subscription")
		}
		acknowledge(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*stripeTimeout)
	defer cancel()
	sent := s.now() // retrieved_at = when the GET was sent (see syncCustomer), expressed on the DB clock below
	wire, err := s.stripe.retrieveSubscription(ctx, subID)
	if err != nil {
		slog.Warn("billing_stripe_error", "op", "subscription_retrieve")
		// 503: Stripe retries up to 3 days (F-B9); dropping the event would leave standing stale.
		httperror.Write(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	row, err := wire.project()
	if errors.Is(err, errMultiItem) {
		slog.Warn("billing_multi_item", "subscription", subID)
		acknowledge(w) // retrying returns the same subscription; not applied (B8)
		return
	}
	if err != nil {
		slog.Warn("billing_ops_alert", "code", "unusable", "subscription", subID)
		acknowledge(w) // retrying re-reads the same unusable object
		return
	}
	var result string
	// billing.apply_subscription: single statement = one transaction on the ingress login (C-4); the
	// customer resolves the store, never the event payload. p_retrieved_at is the DB clock minus the time
	// since the GET was sent, so API/DB clock skew can never trip the "future retrieved_at" refusal (PT400)
	// and silently swallow events, and the value is still the GET's send time.
	err = ingress.QueryRow(ctx, `SELECT billing.apply_subscription($1::text,$2::text,$3::text,$4::text,$5::text,
		$6::timestamptz,$7::timestamptz,$8::boolean,$9::timestamptz,clock_timestamp()-make_interval(secs=>$10::float8),
		$11::uuid,$12::boolean)`,
		envSandbox, row.Customer, row.ID, row.Status, row.PriceID, row.PeriodStart, row.PeriodEnd,
		row.CancelAtPeriodEnd, row.Created, s.since(sent), row.MetaStore, row.Livemode).Scan(&result)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "PT400" || pgErr.Code == "23514") {
			// A retry re-applies the same rejected values: alert instead of a 3-day retry loop.
			slog.Warn("billing_ops_alert", "code", "invalid", "subscription", subID)
			acknowledge(w)
			return
		}
		slog.Warn("billing_webhook_apply_failed") // 503: transient database error, Stripe retries
		httperror.Write(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	switch result {
	case "applied", "stale":
	default: // duplicate | mismatch | unknown_customer: mirrored or ignored by SQL, operator must look
		slog.Warn("billing_ops_alert", "code", result, "subscription", subID)
	}
	acknowledge(w)
}

// subscriptionID resolves the subscription an event is about. customer.subscription.* events carry it as
// the object id; invoice.* events carry it at data.object.parent.subscription_details.subscription
// (API >= 2025-03-31.basil, F-B3), re-read from the already verified raw body with stdlib json. An event
// that names no subscription (one-off invoice) is ignored.
func subscriptionID(ev stripe.Event, raw []byte) (string, bool) {
	switch ev.Type {
	case "invoice.paid", "invoice.payment_failed":
		var body struct {
			Data struct {
				Object struct {
					Parent struct {
						SubscriptionDetails struct {
							Subscription string `json:"subscription"`
						} `json:"subscription_details"`
					} `json:"parent"`
				} `json:"object"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &body) != nil {
			return "", false
		}
		id := body.Data.Object.Parent.SubscriptionDetails.Subscription
		return id, subPattern.MatchString(id)
	default:
		return ev.SessionID, ev.ObjectType == "subscription" && subPattern.MatchString(ev.SessionID)
	}
}

func reject(w http.ResponseWriter, code string) {
	slog.Warn("billing_webhook_rejected", "code", code)
	httperror.Write(w, http.StatusBadRequest, "invalid_request")
}

func acknowledge(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"received":true}`))
}
