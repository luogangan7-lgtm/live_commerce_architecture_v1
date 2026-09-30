#!/usr/bin/env bash
set -euo pipefail
# stripe-live-enable-v1 §11 harness guard: tests never run with a live Stripe key in the environment
# (value never printed).
if env | grep -qE '=(sk|rk)_live_'; then echo 'refused: live key in test environment' >&2; exit 2; fi
cd "$(dirname "$0")/../.."
command -v docker >/dev/null
command -v go >/dev/null
# PAYUNi protocol tests compare Go forms with independent Node/OpenSSL and the
# public official golden vector; missing Node must fail before starting fixtures.
command -v node >/dev/null
test_mode="${1:-foundation}"
if [[ "$#" -gt 1 ]] || [[ "$test_mode" != foundation && "$test_mode" != --browser-identity && "$test_mode" != --browser-password-auth && "$test_mode" != --browser-admin-legacy && "$test_mode" != --browser-buyer && "$test_mode" != --browser-merchant-buyer && "$test_mode" != --browser-merchant-orders-bff && "$test_mode" != --browser-merchant-orders-ui && "$test_mode" != --browser-input-delivery && "$test_mode" != --browser-studio-bff && "$test_mode" != --browser-studio-ui && "$test_mode" != --browser-live-claims && "$test_mode" != --browser-order && "$test_mode" != --browser-payment && "$test_mode" != --stripe-browser && "$test_mode" != --browser-refund-fulfilment && "$test_mode" != --browser-customers-billing && "$test_mode" != --browser-meta-ads && "$test_mode" != --browser-cvs && "$test_mode" != --browser-webkit && "$test_mode" != --browser-e2e && "$test_mode" != --checkout && "$test_mode" != --payment && "$test_mode" != --payment-worker && "$test_mode" != --expiry-worker && "$test_mode" != --storefront-resolver && "$test_mode" != --buyer-http && "$test_mode" != --purchase-entry && "$test_mode" != --merchant-orders && "$test_mode" != --meta-inbox && "$test_mode" != --meta-consumer && "$test_mode" != --meta-runtime && "$test_mode" != --legacy-isolation && "$test_mode" != --local-recovery && "$test_mode" != --live-planning && "$test_mode" != --live-authority && "$test_mode" != --live-media-plan && "$test_mode" != --live-media-execution && "$test_mode" != --live-browser-input && "$test_mode" != --live-media-input && "$test_mode" != --live-media-crash && "$test_mode" != --live-media-stop && "$test_mode" != --live-media-recovery && "$test_mode" != --live-media-runtime && "$test_mode" != --studio-backend ]]; then
  printf 'Usage: bash scripts/dev/test-local.sh [--browser-identity|--browser-password-auth|--browser-admin-legacy|--browser-buyer|--browser-merchant-buyer|--browser-merchant-orders-bff|--browser-merchant-orders-ui|--browser-input-delivery|--browser-studio-bff|--browser-studio-ui|--browser-live-claims|--browser-order|--browser-payment|--stripe-browser|--browser-refund-fulfilment|--browser-customers-billing|--browser-meta-ads|--browser-cvs|--browser-webkit|--browser-e2e|--checkout|--payment|--payment-worker|--expiry-worker|--storefront-resolver|--buyer-http|--purchase-entry|--merchant-orders|--meta-inbox|--meta-consumer|--meta-runtime|--legacy-isolation|--local-recovery|--live-planning|--live-authority|--live-media-plan|--live-media-execution|--live-browser-input|--live-media-input|--live-media-crash|--live-media-stop|--live-media-recovery|--live-media-runtime|--studio-backend]\n' >&2
  exit 2
fi
if [[ "$test_mode" == --browser-merchant-buyer ]]; then
  # Do not report a pass from an exact Go test selector matching no test.
  test -f tests/foundation/browser_merchant_buyer_chain_test.go
fi
if [[ "$test_mode" == --live-authority ]]; then
  # A selector with no matching test is not an authority gate.
  test -f tests/foundation/live_media_authorization_test.go
fi
if [[ "$test_mode" == --live-media-plan ]]; then
  test -f tests/foundation/live_media_plan_test.go
fi
if [[ "$test_mode" == --live-media-execution ]]; then
  # Refuse a no-test success before provisioning any fixture.
  test -f tests/foundation/live_media_execution_test.go
  grep -q '^func TestLiveMediaExecution' tests/foundation/live_media_execution_test.go
fi
if [[ "$test_mode" == --live-media-input ]]; then
  # A focused feedback loop, never a substitute for BIC05/full regression.
  test -f tests/foundation/live_media_input_custody_test.go
  grep -q '^func TestLiveMediaExecutionBIC' tests/foundation/live_media_input_custody_test.go
fi
if [[ "$test_mode" == --live-browser-input ]]; then
  test -f tests/foundation/live_browser_input_runtime_test.go
  grep -q '^func TestLiveBrowserInputBRW' tests/foundation/live_browser_input_runtime_test.go
fi
if [[ "$test_mode" == --live-media-crash ]]; then
  # Exact LMR05 diagnostic only, not acceptance of the Stop or full suite.
  grep -q '^func TestLiveMediaStopLMR05RealCrashAndCommitAckLoss(t \*testing.T)' tests/foundation/live_media_stop_test.go
fi
if [[ "$test_mode" == --live-media-stop ]]; then
  test -f tests/foundation/live_media_stop_test.go
  grep -q '^func TestLiveMediaStopLMR' tests/foundation/live_media_stop_test.go
fi
if [[ "$test_mode" == --live-media-recovery ]]; then
  test -f tests/foundation/live_media_recovery_test.go
  grep -q '^func TestLiveMediaRecoveryMRR' tests/foundation/live_media_recovery_test.go
fi
if [[ "$test_mode" == --live-media-runtime ]]; then
  test -f tests/foundation/live_media_runtime_test.go
  test -f cmd/media-worker/main_test.go
  test -f internal/integrations/livekit/worker_env_test.go
  grep -q '^func TestLiveMediaRuntimeLMW' tests/foundation/live_media_runtime_test.go
  grep -q '^func TestMediaWorkerLMW' cmd/media-worker/main_test.go
  grep -q '^func TestWorkerEnvironmentLMW' internal/integrations/livekit/worker_env_test.go
fi
if [[ "$test_mode" == --studio-backend ]]; then
  test -f tests/foundation/studio_backend_test.go
  test -f tests/foundation/studio_http_test.go
  test -f tests/foundation/studio_process_test.go
  test -f internal/pagination/studio_test.go
  grep -q '^func TestStudioBackend' tests/foundation/studio_backend_test.go
  grep -q '^func TestStudioBackendSTU03' tests/foundation/studio_process_test.go
  grep -q '^func TestStudioCursor' internal/pagination/studio_test.go
  test -f tests/foundation/studio_input_test.go
  grep -q '^func TestStudioInput' tests/foundation/studio_input_test.go
fi
if [[ "$test_mode" == --browser-order ]]; then
  test -f tests/foundation/browser_order_chain_test.go
fi
if [[ "$test_mode" == --browser-payment ]]; then
  test -f tests/foundation/browser_payment_chain_test.go
fi
if [[ "$test_mode" == --stripe-browser ]]; then
  # SP18 / SU05-SU09 (contracts/stripe-buyer-ui-v1.md §9). Steps can be narrowed with
  # LC_STRIPE_BROWSER_STEPS (default: all); only sp18/su07/su09 need the SANDBOX variables.
  test -f tests/foundation/browser_stripe_test.go
  test -f tests/storefront/stripe-browser.mjs
  test -f tests/storefront/payuni-ui-baseline.mjs
  stripe_steps=",${LC_STRIPE_BROWSER_STEPS:-node-env,payuni-baseline,sp18,su07,su09},"
  stripe_key=""
  if [[ "$stripe_steps" == *,sp18,* || "$stripe_steps" == *,su07,* || "$stripe_steps" == *,su09,* || "$stripe_steps" == *,obs,* ]]; then
    if [[ "${STRIPE_BROWSER:-}" != 1 || "${STRIPE_SANDBOX:-}" != 1 ]]; then
      printf 'NOT_RUN: SP18 requires STRIPE_BROWSER=1 and STRIPE_SANDBOX=1 (Stripe test-mode key from secrets.env, account acct_1UJDb0RusP6Wwj7e); nothing was started.\n' >&2
      exit 2
    fi
    # Only STRIPE_SECRET_KEY is read, in a subshell (never `set -a` the file, never echoed).
    stripe_secrets="${LC_SECRETS_FILE:-$HOME/.config/livecommerce/secrets.env}"
    stripe_key="$(set +x; . "$stripe_secrets" 2>/dev/null; printf %s "${STRIPE_SECRET_KEY:-}")"
    if [[ ! "$stripe_key" =~ ^(sk|rk)_test_ ]]; then
      printf 'NOT_RUN: STRIPE_SECRET_KEY in secrets.env is missing or not a Stripe test key (^(sk|rk)_test_); nothing was started.\n' >&2
      exit 2
    fi
    stripe_account="${STRIPE_ACCOUNT_ID:-acct_1UJDb0RusP6Wwj7e}"
    if [[ "$stripe_account" != acct_1UJDb0RusP6Wwj7e ]]; then
      printf 'NOT_RUN: STRIPE_ACCOUNT_ID must be the SANDBOX fixture account (matches SP16).\n' >&2
      exit 2
    fi
    printf 'SANDBOX checkout.stripe.com test mode; no live charge; SP17 webhook NOT_RUN\n'
  fi
fi
if [[ "$test_mode" == --browser-merchant-orders-ui ]]; then
  test -f tests/foundation/browser_merchant_orders_ui_test.go
  test -f tests/admin/orders-ui.spec.ts
  # Pure model contract behind the UI (orders/refunds/shipments parsers); was run by no gate before.
  node --test --experimental-strip-types tests/admin/orders-model.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-merchant-orders-bff ]]; then
  test -f tests/foundation/browser_merchant_orders_bff_test.go
  test -f tests/admin/orders-bff.spec.ts
  # Raw URL grammar is shared by Proxy and route; real HTTP below additionally
  # proves that the framework cannot normalize a rejected request past it.
  node --test --experimental-strip-types tests/admin/orders-request.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-studio-ui ]]; then
  test -f tests/admin/studio-ui.spec.ts
  test -f tests/foundation/browser_studio_ui_test.go
  grep -q '^func TestBrowserStudioUIRealChain' tests/foundation/browser_studio_ui_test.go
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-password-auth ]]; then
  # PA11: refuse a no-test success (merchant-password-auth-v1 §9).
  test -f tests/admin/password-auth.spec.ts
  test -f tests/admin/password-bff.test.ts
  test -f tests/foundation/browser_password_auth_test.go
  grep -q '^func TestBrowserPasswordAuth' tests/foundation/browser_password_auth_test.go
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-admin-legacy ]]; then
  # Orphan-spec gate (ledger/production/visual-states + identity-mock + entry-mock): refuse a no-test success.
  test -f tests/foundation/browser_admin_legacy_test.go
  grep -q '^func TestBrowserAdminLedgerFixtureChain' tests/foundation/browser_admin_legacy_test.go
  grep -q '^func TestBrowserAdminIdentityMock' tests/foundation/browser_admin_legacy_test.go
  grep -q '^func TestBrowserAdminEntryMock' tests/foundation/browser_admin_legacy_test.go
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-live-claims ]]; then
  # KC16: refuse a no-test success, and run the pure claims BFF/copy contracts first.
  test -f tests/admin/claims-ui.spec.ts
  test -f tests/foundation/browser_live_claims_test.go
  grep -q '^func TestBrowserLiveClaimsRealChain' tests/foundation/browser_live_claims_test.go
  node --test --experimental-strip-types tests/admin/claims-request.test.ts tests/admin/claim-source.test.ts tests/admin/claims-model.test.ts apps/storefront/tests/claim.test.mjs
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-input-delivery ]]; then
  test -f tests/foundation/browser_input_delivery_test.go
  test -f tests/admin/input-delivery.spec.ts
  grep -q '^func TestBrowserInputDeliveryBRW05RealChain' tests/foundation/browser_input_delivery_test.go
  node --test --experimental-strip-types tests/admin/studio-request.test.ts tests/admin/studio-input.test.ts
  # Runs the actual browser client (including parameter-property syntax) under
  # pinned Node 24, before the separate signed HTTPS/PG transport fixture.
  node --test --experimental-transform-types tests/admin/studio-input-client.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-studio-bff ]]; then
  test -f tests/admin/studio-request.test.ts
  test -f tests/admin/studio-bff.spec.ts
  test -f tests/foundation/browser_studio_bff_test.go
  grep -q '^func TestBrowserStudioBFFRealChain' tests/foundation/browser_studio_bff_test.go
  node --test --experimental-strip-types tests/admin/studio-request.test.ts tests/admin/studio-input.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-refund-fulfilment ]]; then
  # MF07 + RF11 (BROWSER, MOCK Stripe): refuse a no-test success and run the pure BFF model gate first.
  test -f tests/foundation/browser_refund_fulfilment_test.go
  grep -q '^func TestBrowserManualFulfilment' tests/foundation/browser_refund_fulfilment_test.go
  grep -q '^func TestBrowserRefund' tests/foundation/browser_refund_fulfilment_test.go
  test -f tests/admin/manual-fulfilment.spec.ts
  test -f tests/admin/refund.spec.ts
  test -f tests/storefront/shipment-buyer.mjs
  test -f tests/storefront/refund-buyer.mjs
  node --test --experimental-strip-types tests/admin/refund-bff.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-customers-billing ]]; then
  # CB11 (BROWSER, MOCK Stripe): refuse a no-test success and run the pure BFF fence/decoder gate first.
  test -f tests/foundation/browser_customers_billing_test.go
  grep -q '^func TestBrowserCustomersBilling' tests/foundation/browser_customers_billing_test.go
  test -f tests/admin/customers-billing.spec.ts
  test -f tests/storefront/privacy-buyer.mjs
  node --test --experimental-strip-types tests/admin/customers-bff.test.ts tests/admin/customers-model.test.ts \
    tests/admin/customers-request.test.ts tests/admin/billing-model.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-meta-ads ]]; then
  # MA09a (BROWSER, Meta = MOCK): refuse a no-test success and run the pure model/request gates first.
  test -f tests/foundation/browser_meta_ads_test.go
  grep -q '^func TestBrowserMetaAds' tests/foundation/browser_meta_ads_test.go
  grep -q '^func TestBrowserMetaAdsConsent' tests/foundation/browser_meta_ads_test.go
  test -f tests/admin/ads.spec.ts
  test -f tests/storefront/ads-consent.mjs
  node --test --experimental-strip-types tests/admin/ads-model.test.ts tests/admin/ads-request.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-cvs ]]; then
  # TCV08 (BROWSER, MOCK ECPay map/Create + signed status posts): refuse a no-test success before any build.
  test -f tests/foundation/browser_taiwan_cvs_test.go
  grep -q '^func TestBrowserTaiwanCvs' tests/foundation/browser_taiwan_cvs_test.go
  test -f tests/admin/taiwan-cvs.spec.ts
  test -f tests/storefront/cvs-buyer.mjs
  # Pure admin BFF grammar/model gates first (LGR/LGM): no Docker needed, fail before any build.
  node --test --experimental-strip-types tests/admin/logistics-model.test.ts tests/admin/logistics-request.test.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-webkit ]]; then
  # WebKit/iPhone Safari coverage of the buyer-critical flows (MOCK tier; docs/delivery/GATES.md). Refuse before any build when the WebKit
  # browser is absent: that is NOT_RUN (exit 2), never a green run on Chromium.
  test -f tests/storefront/browser-engine.mjs
  command -v node >/dev/null
  if ! node --input-type=module -e 'import { webkit } from "@playwright/test"; import { existsSync } from "node:fs"; process.exit(existsSync(webkit.executablePath()) ? 0 : 1)' 2>/dev/null; then
    printf 'NOT_RUN: Playwright WebKit is not installed (pnpm exec playwright install webkit); nothing was started.\n' >&2
    exit 2
  fi
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-e2e ]]; then
  # T12: refuse a no-test success; the whole deal loop is one Go test driving one Playwright spec.
  test -f tests/foundation/browser_e2e_test.go
  grep -q '^func TestBrowserE2EDealLoop' tests/foundation/browser_e2e_test.go
  test -f tests/e2e/deal-loop.spec.ts
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-buyer || "$test_mode" == --browser-merchant-buyer || "$test_mode" == --browser-order || "$test_mode" == --browser-payment || "$test_mode" == --stripe-browser || "$test_mode" == --browser-refund-fulfilment || "$test_mode" == --browser-customers-billing || "$test_mode" == --browser-meta-ads || "$test_mode" == --browser-cvs || "$test_mode" == --browser-webkit || "$test_mode" == --browser-live-claims || "$test_mode" == --browser-e2e ]]; then
  command -v pnpm >/dev/null
  command -v openssl >/dev/null
  COMMERCE_BUYER_WEB_ENABLED=0 pnpm run build:storefront
  mkdir -p output/playwright
fi
if [[ "$test_mode" == --browser-identity || "$test_mode" == --browser-password-auth || "$test_mode" == --browser-admin-legacy || "$test_mode" == --browser-merchant-buyer || "$test_mode" == --browser-merchant-orders-bff || "$test_mode" == --browser-merchant-orders-ui || "$test_mode" == --browser-input-delivery || "$test_mode" == --browser-studio-bff || "$test_mode" == --browser-studio-ui || "$test_mode" == --browser-live-claims || "$test_mode" == --browser-refund-fulfilment || "$test_mode" == --browser-customers-billing || "$test_mode" == --browser-meta-ads || "$test_mode" == --browser-cvs || "$test_mode" == --browser-webkit || "$test_mode" == --browser-e2e ]]; then
  command -v pnpm >/dev/null
  command -v node >/dev/null
  # Production package, but local-only runtime configuration is injected by the
  # tagged test. Build never needs an IdP or database credential.
  COMMERCE_IDENTITY_ENABLED=0 COMMERCE_FIXTURE_ENABLED=0 pnpm run build:admin
fi
# A task-owned, temporary PG only. Never use a developer's existing DATABASE_URL.
test_container="lc-foundation-test-$$"
test_owned=0
stripe_lock=""
cleanup() {
  if [[ -n "$stripe_lock" ]]; then rm -rf "$stripe_lock"; fi
  if [[ "$test_owned" == 1 ]] && [[ "$(docker inspect -f '{{index .Config.Labels "livecommerce.fixture"}}' "$test_container" 2>/dev/null || true)" == "$test_container" ]]; then
    docker rm -f "$test_container" >/dev/null
  fi
}
trap cleanup EXIT INT TERM
if [[ "$test_mode" == --stripe-browser ]]; then
  # One PG-holding run at a time machine-wide (Docker Desktop memory); same lock as test-focused.sh.
  # Bounded wait (LC_TEST_LOCK_WAIT, default 300 s): a foreign holder must not deadlock this gate. On
  # timeout the run proceeds WITHOUT exclusivity and says so; it never removes or edits a live lock.
  lock_dir="${LC_TEST_LOCK_DIR:-${TMPDIR:-/tmp}/lc-test-pg.lock}"
  lock_deadline=$(( $(date +%s) + ${LC_TEST_LOCK_WAIT:-300} ))
  until mkdir "$lock_dir" 2>/dev/null; do
    holder="$(cat "$lock_dir/pid" 2>/dev/null || true)"
    if [[ -n "$holder" ]] && ! kill -0 "$holder" 2>/dev/null; then rm -rf "$lock_dir"; continue; fi
    if (( $(date +%s) >= lock_deadline )); then
      printf 'WARNING: PG lock %s still held by pid %s after %ss; continuing without exclusivity.\n' "$lock_dir" "${holder:-?}" "${LC_TEST_LOCK_WAIT:-300}" >&2
      lock_dir=""; break
    fi
    sleep 2
  done
  if [[ -n "$lock_dir" ]]; then echo $$ > "$lock_dir/pid"; stripe_lock="$lock_dir"; fi
fi
export POSTGRES_PASSWORD
POSTGRES_PASSWORD="$(openssl rand -hex 24)"
# Memory: on Linux cgroup v2 the 256 MiB tmpfs data directory is charged to the
# same memcg as the server processes, so 512m OOM-killed postgres mid-suite on
# Linux hosts/CI (observed 2026-09-28: memcg OOM -> "database system is in
# recovery mode"). 1g keeps the same tmpfs/shared_buffers/max_connections gate.
docker run -d --pull=never --name "$test_container" \
  --label "livecommerce.fixture=$test_container" --memory=1g --cpus=1 --pids-limit=128 \
  --tmpfs /var/lib/postgresql:rw,size=268435456 \
  -e POSTGRES_PASSWORD -e POSTGRES_DB=lc_foundation_test \
  -p 127.0.0.1::5432 \
  postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280 \
  -c shared_buffers=32MB -c max_connections=60 >/dev/null
test_owned=1
# The image starts a socket-only temporary server during initdb, then stops it.
# TCP readiness must wait for the final server; socket pg_isready can race createdb.
for ((attempt=0; attempt<40; attempt++)); do
  if docker exec "$test_container" pg_isready -h 127.0.0.1 -U postgres -d lc_foundation_test >/dev/null 2>&1; then break; fi
  sleep 0.5
done
docker exec "$test_container" pg_isready -h 127.0.0.1 -U postgres -d lc_foundation_test >/dev/null
# The fixture safety guard needs a distinct, explicitly disposable database.
# Reuse this owned cluster, not a developer's running UI fixture or credentials.
docker exec "$test_container" createdb -U postgres lc_admin_fixture
test_port="$(docker port "$test_container" 5432/tcp)"
[[ "$test_port" == 127.0.0.1:* ]]
export LC_TEST_DATABASE_URL="postgres://postgres:${POSTGRES_PASSWORD}@${test_port}/lc_foundation_test?sslmode=disable"
export LC_TEST_DATABASE_ALLOWED=1
export COMMERCE_FIXTURE_ALLOWED=1
export LC_ADMIN_GUARD_DSN="postgres://postgres:${POSTGRES_PASSWORD}@${test_port}/lc_admin_fixture?sslmode=disable"
# fresh_pg: recreate the task-owned PG container (same pinned image, limits and loopback binding as above) and re-export the two DSNs.
# Every go test process needs a FRESH cluster: the foundation fixture creates cluster-scoped roles (foundation_api, ...) and refuses a
# cluster that already has them. Used by --stripe-browser and --browser-webkit, one step per go test process.
fresh_pg() {
  if [[ "$(docker inspect -f '{{index .Config.Labels "livecommerce.fixture"}}' "$test_container" 2>/dev/null || true)" == "$test_container" ]]; then
    docker rm -f "$test_container" >/dev/null
  fi
  docker run -d --pull=never --name "$test_container" \
    --label "livecommerce.fixture=$test_container" --memory=1g --cpus=1 --pids-limit=128 \
    --tmpfs /var/lib/postgresql:rw,size=268435456 \
    -e POSTGRES_PASSWORD -e POSTGRES_DB=lc_foundation_test \
    -p 127.0.0.1::5432 \
    postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280 \
    -c shared_buffers=32MB -c max_connections=60 >/dev/null
  for ((attempt=0; attempt<40; attempt++)); do
    if docker exec "$test_container" pg_isready -h 127.0.0.1 -U postgres -d lc_foundation_test >/dev/null 2>&1; then break; fi
    sleep 0.5
  done
  docker exec "$test_container" pg_isready -h 127.0.0.1 -U postgres -d lc_foundation_test >/dev/null
  docker exec "$test_container" createdb -U postgres lc_admin_fixture
  test_port="$(docker port "$test_container" 5432/tcp)"
  [[ "$test_port" == 127.0.0.1:* ]]
  export LC_TEST_DATABASE_URL="postgres://postgres:${POSTGRES_PASSWORD}@${test_port}/lc_foundation_test?sslmode=disable"
  export LC_ADMIN_GUARD_DSN="postgres://postgres:${POSTGRES_PASSWORD}@${test_port}/lc_admin_fixture?sslmode=disable"
}
# go_json_counts <go test -json log> <min leaf cases>: prints "pass=.. fail=.. skip=.. leaf_pass=.. leaf_fail=.. leaf_skip=.. VERDICT=0|1".
# Contract stripe-psp-v1 §14 parsing rule: any SKIP, any FAIL, fewer leaf cases than expected, zero passing tests or a missing log is VERDICT=1.
go_json_counts() {
python3 - "$1" "$2" <<'PY'
import json,sys
t={"pass":0,"fail":0,"skip":0};leaf=dict(t)
try:
    lines=open(sys.argv[1]).read().splitlines()
except OSError:
    print("missing-log VERDICT=1");sys.exit(0)
for line in lines:
    try: e=json.loads(line)
    except ValueError: continue
    a=e.get("Action");n=e.get("Test")
    if n and a in t:
        t[a]+=1
        if "/" in n: leaf[a]+=1
ok=t["fail"]==0 and t["skip"]==0 and t["pass"]>0 and leaf["pass"]>=int(sys.argv[2])
print("pass=%d fail=%d skip=%d leaf_pass=%d leaf_fail=%d leaf_skip=%d VERDICT=%d"%(t["pass"],t["fail"],t["skip"],leaf["pass"],leaf["fail"],leaf["skip"],0 if ok else 1))
PY
}
if [[ "$test_mode" == --browser-identity ]]; then
  LC_BROWSER_IDENTITY_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=180s -run '^(TestBrowserIdentityRealChain|TestBrowserSettingsWizardRealChain|TestMerchantAccountAPIProcessRestart)$' -v ./tests/foundation
  printf 'PASS: isolated PG + signed MOCK IdP browser chain; fixture removed at exit.\n'
elif [[ "$test_mode" == --browser-password-auth ]]; then
  node --test --experimental-strip-types tests/admin/password-bff.test.ts   # PA10 (Node, no browser, no PG)
  LC_BROWSER_PASSWORD_AUTH_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=900s -run '^TestBrowserPasswordAuth$' -v ./tests/foundation
  printf 'PASS: isolated Next + Go + PG + loopback SMTP fake password-auth browser chain (PA11); no real mailbox, no owner secret.\n'
elif [[ "$test_mode" == --browser-admin-legacy ]]; then
  LC_BROWSER_ADMIN_LEGACY_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=600s -run '^TestBrowserAdmin(LedgerFixtureChain|IdentityMock|EntryMock)$' -v ./tests/foundation
  printf 'PASS: admin ledger (fixture bearer) + production fail-closed + identity-mock + entry-mock browser suites; no signed IdP, not production acceptance.\n'
elif [[ "$test_mode" == --browser-merchant-orders-bff ]]; then
  LC_BROWSER_MERCHANT_ORDERS_BFF_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=180s -run '^TestBrowserMerchantOrdersBFFRealChain$' -v ./tests/foundation
  printf 'PASS: isolated Next + Go + PG merchant-order read transport; not merchant UI or provider acceptance.\n'
elif [[ "$test_mode" == --browser-studio-ui ]]; then
  LC_BROWSER_STUDIO_UI_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=360s -run '^TestBrowserStudioUIRealChain$' -v ./tests/foundation
  printf 'PASS: isolated Studio B UI with signed MOCK IdP and local MOCK Egress; not Cloud or production acceptance.\n'
elif [[ "$test_mode" == --browser-live-claims ]]; then
  LC_BROWSER_LIVE_CLAIMS_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=360s -run '^TestBrowserLiveClaimsRealChain$' -v ./tests/foundation
  printf 'PASS: KC16 isolated admin + storefront Next, Go and PG claims chain; signed MOCK IdP, MOCK manual ingress; no provider or deployment acceptance.\n'
elif [[ "$test_mode" == --browser-input-delivery ]]; then
  LC_BROWSER_INPUT_DELIVERY_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=240s -run '^TestBrowserInputDeliveryBRW05RealChain$' -v ./tests/foundation
  printf 'PASS: isolated HTTPS signed browser + Next + Go + PG input token transport; not decoded SFU media, recovery or production acceptance.\n'
elif [[ "$test_mode" == --browser-studio-bff ]]; then
  LC_BROWSER_STUDIO_BFF_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=240s -run '^TestBrowserStudioBFFRealChain$' -v ./tests/foundation
  printf 'PASS: isolated signed OIDC + Next + Go + PG Studio BFF transport; not Studio page/UI, Cloud or provider acceptance.\n'
elif [[ "$test_mode" == --browser-merchant-orders-ui ]]; then
  LC_BROWSER_MERCHANT_ORDERS_UI_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=300s -run '^TestBrowserMerchantOrdersUIRealChain$' -v ./tests/foundation
  printf 'PASS: isolated merchant C order UI; signed MOCK IdP and local payment fixtures, not production/provider acceptance.\n'
elif [[ "$test_mode" == --browser-buyer ]]; then
  LC_BROWSER_BUYER_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=180s -run '^TestBrowserBuyerRealChain$' -v ./tests/foundation
  printf 'PASS: isolated PG + real buyer browser transport; not UI/PSP/deployment acceptance.\n'
elif [[ "$test_mode" == --browser-merchant-buyer ]]; then
  LC_BROWSER_MERCHANT_BUYER_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=180s -run '^TestBrowserMerchantBuyerRealChain$' -v ./tests/foundation
  printf 'PASS: isolated merchant-to-buyer browser chain; not provider payment or real DNS/TLS deployment proof.\n'
elif [[ "$test_mode" == --browser-order ]]; then
  LC_BROWSER_ORDER_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=180s -run '^TestBrowserBuyerOrderUI$' -v ./tests/foundation
  printf 'PASS: isolated buyer address/order UI gate; not provider payment or deployment acceptance.\n'
elif [[ "$test_mode" == --browser-payment ]]; then
  LC_BROWSER_PAYMENT_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=180s -run '^TestBrowserBuyerPaymentUI$' -v ./tests/foundation
  printf 'PASS: isolated buyer payment UI/native POST gate with a local mock PSP; not provider payment or deployment acceptance.\n'
elif [[ "$test_mode" == --stripe-browser ]]; then
  # Evidence goes to the MAIN checkout (worktrees are deleted after merge; PROCESS.md §4).
  stripe_main="$(cd "$(git rev-parse --git-common-dir)/.." && pwd)"
  stripe_out="$stripe_main/output/stripe-b2-browser-tests"
  mkdir -p "$stripe_out"
  stripe_sha="$(git rev-parse --short=12 HEAD)"
  stripe_status=0
  # Every go test process needs a FRESH cluster: the foundation fixture creates cluster-scoped roles
  # (foundation_api, ...) and refuses a cluster that already has them. Recreate the task-owned container
  # (same pinned image, limits and loopback binding as above) before each step.
  stripe_fresh_pg() { fresh_pg; }
  # run_stripe_step <label> <go -run regex> <timeout> <min leaf cases> [env assignments...]
  # Parses `go test -json` (contract §14 parsing rule): any SKIP, any FAIL, fewer leaf cases than
  # expected, zero passing tests, a missing log or a non-zero exit is FAIL, never PASS.
  run_stripe_step() {
    local label="$1" regex="$2" tmo="$3" min="$4"; shift 4
    local log="$stripe_out/$stripe_sha-$label.jsonl" started rc=0 verdict=0 counts
    stripe_fresh_pg
    started="$(date +%s)"
    # Only this go test process (never Node) receives the Stripe key; the Go test strips it again.
    env "$@" LC_STRIPE_BROWSER_ACCEPTANCE=1 LC_STRIPE_EVIDENCE_ROOT="$stripe_out" LC_BASELINE_OUT_DIR="$stripe_out" \
      GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout="$tmo" -json -run "$regex" ./tests/foundation >"$log" 2>"$log.stderr" || rc=$?
    counts="$(go_json_counts "$log" "$min")"
    verdict="${counts##*VERDICT=}"; counts="${counts%% VERDICT=*}"
    printf '%s: %s exit=%d verdict=%d duration=%ds log=%s\n' "$label" "$counts" "$rc" "$verdict" "$(( $(date +%s) - started ))" "$log"
    if [[ "$rc" != 0 || "$verdict" != 0 ]]; then stripe_status=1; printf 'FAIL: %s (go test exit=%d, parse verdict=%d)\n' "$label" "$rc" "$verdict" >&2; fi
  }
  sandbox_env=(STRIPE_BROWSER=1 STRIPE_SANDBOX=1 STRIPE_ACCOUNT_ID="${stripe_account:-}" STRIPE_SECRET_KEY="$stripe_key")
  if [[ "$stripe_steps" == *,node-env,* ]]; then
    run_stripe_step node-env '^TestBrowserStripeNodeEnv$' 120s 0
  fi
  if [[ "$stripe_steps" == *,payuni-baseline,* ]]; then
    run_stripe_step payuni-baseline '^TestBrowserPayuniBaseline$' 300s 0 LC_BASELINE_SHA="$stripe_sha" LC_BASELINE_MUTATE="${LC_BASELINE_MUTATE:-}"
  fi
  if [[ "$stripe_steps" == *,sp18,* ]]; then
    if [[ "${STRIPE_BROWSER_SPLIT:-}" == 1 ]]; then
      for scenario in A B C; do run_stripe_step "sp18-$scenario" "^TestBrowserStripeCheckout\$/^SP18$scenario" 900s 2 "${sandbox_env[@]}"; done
    else
      run_stripe_step sp18 '^TestBrowserStripeCheckout$/^SP18' 900s 6 "${sandbox_env[@]}"
    fi
  fi
  if [[ "$stripe_steps" == *,obs,* ]]; then
    # Developer-only (not in the default steps): real hosted page observed without the B2 buyer UI.
    run_stripe_step obs "^TestBrowserStripeCheckout\$/^${LC_STRIPE_OBS:-OBS}" 900s 1 "${sandbox_env[@]}" STRIPE_BROWSER_OBSERVE=1
  fi
  if [[ "$stripe_steps" == *,su07,* ]]; then run_stripe_step su07 '^TestBrowserStripeCheckout$/^SU07' 900s 1 "${sandbox_env[@]}"; fi
  if [[ "$stripe_steps" == *,su09,* ]]; then run_stripe_step su09 '^TestBrowserStripeCheckout$/^SU09' 900s 2 "${sandbox_env[@]}"; fi
  if [[ "$stripe_status" != 0 ]]; then printf 'FAIL: --stripe-browser (see logs in %s)\n' "$stripe_out" >&2; exit 1; fi
  printf 'PASS: stripe-browser steps [%s] (SP18 = SANDBOX, SU07/SU09 = MOCK, SU05 baseline capture).\n' "${stripe_steps//,/ }"
  if [[ -n "$stripe_key" ]]; then printf 'SANDBOX checkout.stripe.com test mode; no live charge; SP17 webhook NOT_RUN\n'; fi
elif [[ "$test_mode" == --browser-e2e ]]; then
  LC_BROWSER_E2E_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=1700s -run '^TestBrowserE2EDealLoop(Sandbox)?$' -v ./tests/foundation
  printf 'PASS: T12 isolated admin + storefront Next, Go, PG, real worker/consumer/poller/dispatcher; Meta = MOCK (signed webhook, fake Graph), Stripe = MOCK (fake + routed hosted page); SANDBOX variant NOT_RUN unless it says otherwise above; not provider or deployment acceptance.\n'
elif [[ "$test_mode" == --browser-refund-fulfilment ]]; then
  LC_BROWSER_REFUND_FULFILMENT_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=1500s -run '^(TestBrowserManualFulfilment|TestBrowserRefund)$' -v ./tests/foundation
  printf 'PASS: MF07 + RF11(a) isolated admin + storefront Next, Go, PG, real worker and the MOCK Stripe fake; RF11(b) SANDBOX is NOT_RUN unless it says otherwise above; not provider or deployment acceptance.\n'
elif [[ "$test_mode" == --browser-customers-billing ]]; then
  LC_BROWSER_CUSTOMERS_BILLING_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=1700s -run '^TestBrowserCustomersBilling$' -v ./tests/foundation
  printf 'PASS: CB11 isolated admin + storefront Next, Go, PG, real worker; platform billing = MOCK (independent billingtest fake, Stripe pages answered in the browser); not provider or deployment acceptance; CB10 SANDBOX and CB12 LIVE are NOT_RUN.\n'
elif [[ "$test_mode" == --browser-meta-ads ]]; then
  # AL1: the frozen ad link / feed link (origin + /products/{id}) must reach a 200 page on the production storefront build.
  node tests/storefront/ad-link.mjs
  node --test --experimental-strip-types apps/storefront/tests/ad-link-route.test.mjs
  LC_BROWSER_META_ADS_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=1700s -run '^TestBrowserMetaAds(Consent)?$' -v ./tests/foundation
  printf 'PASS: MA09a isolated admin Next, Go API + ads worker, PG; Meta = MOCK (fake Graph + the Facebook Login dialog answered by the browser route); not Meta, provider or deployment acceptance; MA09b buyer consent -> CAPI context runs against the production storefront Next build.\n'
elif [[ "$test_mode" == --browser-cvs ]]; then
  LC_BROWSER_CVS_ACCEPTANCE=1 GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=1700s -run '^TestBrowserTaiwanCvs$' -v ./tests/foundation
  printf 'PASS: TCV08 MOCK isolated admin + storefront Next, Go, PG, ecpaytest fake map/Create and signed status posts; SANDBOX and WebKit variants are NOT_RUN unless the go test log says otherwise; not provider or deployment acceptance.\n'
elif [[ "$test_mode" == --browser-webkit ]]; then
  # Same production storefront + admin builds and the same go tests as the Chromium modes, with LC_BROWSER_ENGINE=webkit: phone-sized buyer
  # contexts run Playwright's iPhone 15 profile, desktop ones and every admin page run Desktop Safari. One go test process per step on a
  # fresh PG cluster; any SKIP/FAIL, too few leaf cases or a missing log is a failed step (go_json_counts). Evidence goes to the MAIN checkout.
  webkit_main="$(cd "$(git rev-parse --git-common-dir)/.." && pwd)"
  webkit_out="$webkit_main/output/webkit"
  mkdir -p "$webkit_out"
  webkit_sha="$(git rev-parse --short=12 HEAD)"
  webkit_status=0
  # Steps can be narrowed while fixing one flow (LC_WEBKIT_STEPS=payment,cvs); the default is all six and release-gate.sh requires all six.
  webkit_steps=",${LC_WEBKIT_STEPS:-buyer,order,payment,merchant-buyer,cvs,password-auth},"
  # run_webkit_step <label> <acceptance env var> <go -run regex> <timeout> <min leaf cases>
  run_webkit_step() {
    local label="$1" accept="$2" regex="$3" tmo="$4" min="$5"
    [[ "$webkit_steps" == *",$label,"* ]] || return 0
    local log="$webkit_out/$webkit_sha-$label.jsonl" started rc=0 verdict=0 counts
    fresh_pg
    started="$(date +%s)"
    env "$accept=1" LC_BROWSER_ENGINE=webkit GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout="$tmo" -json -run "$regex" ./tests/foundation >"$log" 2>"$log.stderr" || rc=$?
    counts="$(go_json_counts "$log" "$min")"
    verdict="${counts##*VERDICT=}"; counts="${counts%% VERDICT=*}"
    printf '%s: %s exit=%d verdict=%d duration=%ds log=%s\n' "$label" "$counts" "$rc" "$verdict" "$(( $(date +%s) - started ))" "$log"
    if [[ "$rc" != 0 || "$verdict" != 0 ]]; then webkit_status=1; printf 'FAIL: %s (go test exit=%d, parse verdict=%d)\n' "$label" "$rc" "$verdict" >&2; fi
  }
  run_webkit_step buyer LC_BROWSER_BUYER_ACCEPTANCE '^TestBrowserBuyerRealChain$' 900s 0
  run_webkit_step order LC_BROWSER_ORDER_ACCEPTANCE '^TestBrowserBuyerOrderUI$' 900s 0
  run_webkit_step payment LC_BROWSER_PAYMENT_ACCEPTANCE '^TestBrowserBuyerPaymentUI$' 900s 0
  run_webkit_step merchant-buyer LC_BROWSER_MERCHANT_BUYER_ACCEPTANCE '^TestBrowserMerchantBuyerRealChain$' 900s 0
  run_webkit_step cvs LC_BROWSER_CVS_ACCEPTANCE '^TestBrowserTaiwanCvs$/^WebKit$' 1700s 1
  run_webkit_step password-auth LC_BROWSER_PASSWORD_AUTH_ACCEPTANCE '^TestBrowserPasswordAuth$' 1200s 0
  if [[ "$webkit_status" != 0 ]]; then printf 'FAIL: --browser-webkit (see logs in %s)\n' "$webkit_out" >&2; exit 1; fi
  printf 'PASS: --browser-webkit MOCK tier on Playwright WebKit (iPhone 15 buyer, Desktop Safari admin behind a self-signed https front) steps [%s]: buyer, order, payment, merchant-buyer, cvs (TCV08 buyer + merchant), password-auth; Stripe SP18 SANDBOX on WebKit = LC_BROWSER_ENGINE=webkit --stripe-browser (see GATES.md); not provider, real-device or deployment acceptance.\n' "${webkit_steps//,/ }"
elif [[ "$test_mode" == --checkout ]]; then
  # Focused diagnosis uses the same isolated real PG and cleanup guard. It never
  # substitutes for the full foundation/race/vet release gate below.
  # This selector now includes the expiry-worker crash/rescue suites as well as
  # checkout. Their aggregate exceeded 120s; individual SQL/deadline gates stay.
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=240s -run '^TestBuyerCheckout' -v ./tests/foundation
  printf 'PASS: checkout subset only; full regression still required.\n'
elif [[ "$test_mode" == --payment ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestBuyerPayment' -v ./tests/foundation
  printf 'PASS: payment start/query subset only; full regression still required.\n'
elif [[ "$test_mode" == --payment-worker ]]; then
  test -f tests/foundation/payment_runtime_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=120s -run '^TestBuyerPaymentWorker' -v ./tests/foundation
  printf 'PASS: isolated payment worker subset; no real-provider or deployment claim.\n'
elif [[ "$test_mode" == --expiry-worker ]]; then
  test -f tests/foundation/expiry_runtime_test.go
  test -f tests/foundation/expiry_admission_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=150s -run '^TestBuyerCheckoutExpiryRuntime' -v ./tests/foundation
  printf 'PASS: isolated expiry worker subset; no production or recovery-SLO claim.\n'
elif [[ "$test_mode" == --merchant-orders ]]; then
  test -f tests/foundation/merchant_orders_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestMerchantOrders' -v ./tests/foundation
  printf 'PASS: isolated merchant order read subset; no merchant UI/provider/deployment claim.\n'
elif [[ "$test_mode" == --live-planning ]]; then
  test -f tests/foundation/live_planning_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestLivePlanning' -v ./internal/live ./tests/foundation
  printf 'PASS: isolated live draft planning only; no broadcast, HTTP, provider or G06 acceptance.\n'
elif [[ "$test_mode" == --live-authority ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^Test(LivePlanning|LiveMediaAuthorization)' -v ./tests/foundation
  printf 'PASS: isolated MOCK media authority registry and draft planning; no controller, LIVE intake, provider or G06 acceptance.\n'
elif [[ "$test_mode" == --live-media-plan ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=240s -run '^Test(LivePlanning|LiveMediaAuthorization|LiveMediaPlan)' -v ./internal/live ./tests/foundation
  printf 'PASS: isolated MOCK media start intent and native queue; no provider execution, LIVE intake or G06 acceptance.\n'
elif [[ "$test_mode" == --live-media-execution ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=300s -run '^Test(LivePlanning|LiveMediaAuthorization|LiveMediaPlan|LiveMediaExecution)' -v ./internal/live ./tests/foundation
  printf 'PASS: isolated MOCK media execution and recovery; no Stop, LIVE provider, resource reclamation or G06 acceptance.\n'
elif [[ "$test_mode" == --live-media-input ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestLiveMediaExecutionBIC' -v ./tests/foundation
  printf 'PASS: isolated BIC custody subset only; full media and BIC05 regression still required.\n'
elif [[ "$test_mode" == --live-browser-input ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=240s -run '^TestLiveBrowserInputBRW' -v ./tests/foundation
  GOTOOLCHAIN=go1.27.1 go vet ./internal/live ./tests/foundation
  printf 'PASS: isolated BRW SQL/executor and Go HTTP subset; HTTPS browser/SFU and recovery gates remain separate.\n'
elif [[ "$test_mode" == --live-media-crash ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestLiveMediaStopLMR05RealCrashAndCommitAckLoss$' -v ./tests/foundation
  printf 'PASS: isolated LMR05 crash diagnostic only; Stop and full regression still required.\n'
elif [[ "$test_mode" == --live-media-stop ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=300s -run '^Test(LivePlanning|LiveMediaAuthorization|LiveMediaPlan|LiveMediaExecution|LiveMediaStop)' -v ./internal/live ./tests/foundation
  printf 'PASS: isolated bounded MOCK media Stop; no Cloud, LIVE intake, operator escalation recovery or G06 acceptance.\n'
elif [[ "$test_mode" == --live-media-recovery ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -failfast -timeout=540s -run '^TestLiveMediaRecoveryMRR' -v ./tests/foundation
  printf 'PASS: isolated MRR observer SQL/process gates only; no Cloud, human alert delivery, LIVE intake or G06 acceptance.\n'
elif [[ "$test_mode" == --live-media-runtime ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=360s -run '^Test(MediaWorkerLMW|WorkerEnvironmentLMW|LiveMediaRuntimeLMW)' -v ./cmd/media-worker ./internal/integrations/livekit ./tests/foundation
  printf 'PASS: isolated actual media command, PG18 and local TLS runtime; no Cloud, LIVE intake or G06 acceptance.\n'
elif [[ "$test_mode" == --studio-backend ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -tags browser -count=1 -timeout=360s -run '^Test(StudioBackend|StudioCursor|StudioInput)' -v ./internal/pagination ./internal/live ./internal/httpapi ./cmd/api ./tests/foundation
  printf 'PASS: isolated Studio backend/API and local MOCK media gate; not BFF/browser, Cloud, LIVE intake or full Studio acceptance.\n'
elif [[ "$test_mode" == --meta-inbox ]]; then
  test -f tests/foundation/meta_inbox_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestMetaInbox' -v ./tests/foundation
  printf 'PASS: isolated Meta inbox subset only; no public mount/provider qualification claim.\n'
elif [[ "$test_mode" == --meta-consumer ]]; then
  test -f tests/foundation/meta_consumer_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestMetaConsumer' -v ./tests/foundation
  printf 'PASS: isolated Meta social consumer subset only; no public mount/provider qualification claim.\n'
elif [[ "$test_mode" == --legacy-isolation ]]; then
  test -f tests/foundation/legacy_runtime_isolation_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=240s -run '^TestLegacyRuntimeIsolation' -v ./tests/foundation
  printf 'PASS: isolated legacy-family subset only; not full regression/provider/production acceptance.\n'
elif [[ "$test_mode" == --local-recovery ]]; then
  # This bounded gate creates its own source/restore clusters; the parent
  # fixture still enforces explicit local-PG consent. No existing DB is restored.
  test -f tests/foundation/local_recovery_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestLocalRecovery' -v ./tests/foundation
  printf 'PASS: isolated logical restore/cold-start subset only; not PITR, production RPO/RTO or deployment acceptance.\n'
elif [[ "$test_mode" == --meta-runtime ]]; then
  test -f tests/foundation/meta_runtime_test.go
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=180s -run '^TestMetaRuntime' -v ./tests/foundation
  printf 'PASS: isolated Meta API/worker runtime subset only; no public deployment/provider qualification claim.\n'
elif [[ "$test_mode" == --storefront-resolver ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=120s -run '^TestPublishedStorefront' -v ./tests/foundation
  printf 'PASS: published-origin resolver subset only; not public HTTP or provider proof.\n'
elif [[ "$test_mode" == --buyer-http ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=120s -run '^TestBuyerHTTP' -v ./tests/foundation
  printf 'PASS: private buyer HTTP subset only; not public BFF/browser or provider proof.\n'
elif [[ "$test_mode" == --purchase-entry ]]; then
  GOTOOLCHAIN=go1.27.1 go test -race -count=1 -timeout=120s -run '^TestMerchantPurchaseEntry' -v ./tests/foundation
  printf 'PASS: merchant purchase-entry real PG/HTTP subset only; not buyer UI or provider checkout.\n'
else
  # Serial gates include two independent worker crash-rescue suites and fresh
  # migration clusters. Only this additive package envelope grows; individual
  # gate deadlines and production rescue defaults are unchanged.
  # Run package binaries serially: independent author and root parallel runs
  # stalled before checkout's test output on this host. This does not disable
  # -race, in-test concurrency or any test; it is not a product root-cause fix.
  # Isolated Meta cutover/maintenance gates add fresh clusters and real process
  # windows. The aggregate exceeded 360s without an assertion failure; retain
  # every individual SQL/process deadline and allow the complete suite to finish.
  # Root measured 483.549s before LMR; the Stop-inclusive focused suite took
  # 180.126s, adding roughly 113s to the full package. This 900s envelope
  # covers aggregate tests only; no SQL, lease, 5s pacing or fault gate changes.
  # Serial real-clock MRR cases add ~344s to the measured ~715s baseline.
  # This suite envelope does not change recovery's 90s gate or the separately
  # owner-approved LMR05 90s wait; their safety predicates remain unchanged.
  # 2026-09-29: the GitHub runner took ~1449s for this package on bef13f2 (before Stripe B1);
  # adding the SP06-SP21 gates pushed daf08ee past 1500s (panic: test timed out after 25m0s,
  # while TestStripeSP10Deadline was 22s in). 2700s keeps headroom inside the 60 min CI job.
  # 2026-09-30: with the R2 lanes merged the foundation package alone needs ~54 min on the dev Mac (release gate at
  # 57c5aaa: panic "test timed out after 45m0s" with 61 tests not started; those took a further 527 s). -timeout is a
  # hang bound, not a gate: 4500s, and the CI job bound moves to 90 min with it (.github/workflows/foundation.yml).
  # ponytail: one serial package; shard foundation across CI jobs by -run regex when a run nears 70 min.
  GOTOOLCHAIN=go1.27.1 go test -p 1 -race -count=1 -timeout=4500s -v ./...
  GOTOOLCHAIN=go1.27.1 go vet ./...
  printf 'PASS: isolated real PostgreSQL foundation tests; fixture removed at exit.\n'
fi
