// Command api owns the API process assembly: it loads each feature's configuration (identity,
// accounts, buyer and hosted payment, Meta webhooks, Stripe webhooks, Studio, claims, merchant
// refunds, Taiwan CVS), opens the scoped DB pools, builds the handlers and mounts them on one listener. Feature
// files (buyer.go, buyer_payment.go, stripe_webhook.go, merchant_refund.go, ...) each own one
// feature's config and wiring only.
//
// It never starts a worker or dispatches a provider call (workers are cmd/*-worker; the API only
// inserts River jobs and admits signed webhooks), never reads STRIPE_* secrets, and holds no
// business rule: routes and rules live in internal/httpapi and the domain packages. External
// services: none called at request time except the OIDC issuer during identity login
// (internal/oidclogin), graph.facebook.com during the merchant Meta-ads connect callback
// (merchant_ads.go via internal/integrations/meta_ads: code exchange + pick list; never a Graph write,
// never a token read-back: the API process holds HPKE public keys only) and, only with
// CVS_ECPAY_ENABLED=1, logistics(-stage).ecpay.com.tw for the merchant's connect probe, the
// store-directory check of a map selection and the abandon-time query
// (internal/integrations/shipping/ecpay; the label create itself is cmd/claims-worker's dispatcher route).
package main
