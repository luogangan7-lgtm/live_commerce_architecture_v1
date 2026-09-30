# Unit deploy-release — deployable package for R1 + release gate script

1. deploy/compose.yml + deploy/env/*.example + provisioning scripts cover every R1 process and variable:
   api (COMMERCE_STRIPE_CHECKOUT_ENABLED, COMMERCE_STRIPE_WEBHOOK_* keyring, claim-source), payment-worker
   (Stripe runtime + refund worker), claims-worker (new), meta-worker, expiry-worker, media-worker (off by
   default), admin, storefront, caddy; one-shot jobs migrate, stripe-admin, meta-admin (operator CLIs,
   profile `ops`). Login roles provisioned exactly as the pool validators require (incl. ruling 19 runtime
   River privileges, Stripe ingress/registrar, claims intake). Names only, never values.
2. Runbooks: docs/runbooks/deploy.md updated (B1 closed by cmd/migrate; new processes; Stripe account
   registration + webhook endpoint creation with the pinned API version and the event list incl. refund
   events; Meta Page-token registration via meta-admin; claim-source binding; secret rotation), plus
   docs/runbooks/merchant-onboarding.md (operator steps to onboard the first real merchant: store,
   Stripe account, Meta Page, domain) with every owner-only prerequisite called out.
3. `deploy/scripts/smoke.sh static` and `full` green on this branch (fix S29m/I8 only if it is in R1's
   path; otherwise record it as a known limit with the owning lane). Evidence under deploy/.evidence/.
4. `scripts/dev/release-gate.sh`: runs, in order, check_packet, build/vet, secret grep, depmap check,
   unit tests, the whole foundation package, every browser mode in docs/delivery/GATES.md (SANDBOX modes
   only when the Stripe test key is present, else NOT_RUN), deploy smoke static; prints a table
   PASS/FAIL/NOT_RUN and exits non-zero on any FAIL. This is the R1 acceptance command.
