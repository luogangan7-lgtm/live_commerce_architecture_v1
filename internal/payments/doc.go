// Package payments owns merchant payment-method configuration (methods.go) and the payment runtime
// on the profile queues: PAYUNi capture reconciliation and query workers, the Stripe Checkout query,
// signal and refund workers (stripe_*.go; contracts/stripe-psp-v1.md, stripe-refund-v1) and the
// lease-bound provider client material they use.
//
// It never accepts buyer HTTP or starts checkout (internal/checkout does), never infers unpaid from
// a timeout, never re-sends a create or refund with a new idempotency key after UNKNOWN, and never
// writes stock, order or fulfilment state itself: workers record authenticated observations and
// payments.* SQL definers (apply_capture, apply_stripe_refund) decide facts. Stripe keys are read
// per attempt from PG (sealed by internal/integrations/accounts), never from the environment.
// External hosts, only via internal/integrations/psp: api.stripe.com and the PAYUNi endpoint.
package payments
