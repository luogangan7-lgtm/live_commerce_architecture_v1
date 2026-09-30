#!/usr/bin/env bash
# File: deploy/scripts/preflight.sh
# Purpose: validate configuration BEFORE anything starts (deploy-design §16.2). cmd/api logs
#   only "api stopped" on any startup failure (cmd/api/main.go:19-22), so these rules
#   re-implement the apps' config checks statically and (with --online) against the network.
#   Prints only rule id, PASS/FAIL/WARN/SKIP and variable/file NAMES — never values.
# Usage: preflight.sh [--online] [--skip-images]
# Rules: P01 compose.env vars/hosts  P02 secret dir/file modes  P03 secret formats
#   P04 inequalities (incl. custody separation of the four keyrings and K_actor/K_link/replay keys;
#   the ads HPKE public ring matches its private ring)
#   + DSN/password consistency  P05 active key ids  P06 flags, dependencies,
#   profiles, knob-file allowlists  P07 forbidden test/fixture vars  P08 grammars/ranges
#   P09 owner secrets  P10 images present  P11 ACME (production)  P12 DB TLS  P13 disk space
#   --online: P14 DNS -> this host  P15 OIDC discovery from the edge  P16 clock sync
#     P17 admin host resolves only to this host while password login is on (merchant-password-auth-v1 PA15)
# Runs as/in: deploy host (root for real deployments; smoke uses its temp config).
# Reads env: compose.env (via lib.sh, shell env wins), ${LC_ENV_DIR}/*.env,
#   LC_ENVIRONMENT (smoke relaxes host-name and file-owner rules).
# Reads secrets: every file in LC_SECRETS_DIR, in memory only, to check formats/inequalities.
# Used by: deploy.sh (first/upgrade), smoke.sh S10 (positive + 5 negative cases), runbooks.
# Depends on: python3, docker (P10, P13, P15), getent (P14), timedatectl (P16),
#   deploy/secrets.manifest.tsv, deploy/postgres/logins.tsv.
# Status: DESIGN; verified by smoke S10a-e.
# Change rules: when an app adds/changes a config rule, mirror it here and add a smoke S10 case.
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

online=0 skip_images=0
while (($#)); do
  case "$1" in
  --online) online=1 && shift ;;
  --skip-images) skip_images=1 && shift ;;
  *) lc_die "usage: preflight.sh [--online] [--skip-images]" 2 ;;
  esac
done
lc_load_env "$LC_COMPOSE_ENV"
fail=0

# ---- P01-P09, P11, P12: static rules (python, values stay in memory) ---------------------------
python3 - "$LC_DEPLOY_DIR" <<'PY' || fail=1
import base64, json, os, re, stat, sys
from urllib.parse import urlsplit, parse_qs

deploy = sys.argv[1]
E = os.environ
results = []          # (rule, status, name)
def rec(rule, ok, name="", warn=False):
    results.append((rule, "PASS" if ok else ("WARN" if warn else "FAIL"), name))

def parse_env(path):
    out = {}
    with open(path, encoding="utf-8") as fh:
        for n, line in enumerate(fh, 1):
            line = line.rstrip("\r\n")
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            m = re.match(r"^([A-Za-z_][A-Za-z0-9_]*)=(.*)$", line)
            if not m:
                out.setdefault("__bad__", []).append(n)
                continue
            k, v = m.groups()
            if len(v) >= 2 and v[0] == v[-1] and v[0] in "'\"":
                v = v[1:-1]
            elif " #" in v:
                out.setdefault("__inline_comment__", []).append(k)
            out[k] = v
    return out

env_name = E.get("LC_ENVIRONMENT", "")
smoke = env_name == "smoke"
prod = env_name == "production"

# ---- P01 compose.env ------------------------------------------------------------------------
required = ["COMPOSE_PROJECT_NAME", "COMPOSE_PROFILES", "IMAGE_TAG", "LC_IMAGE_PREFIX", "LC_ENVIRONMENT",
            "LC_BIND_ADDR", "LC_HTTP_PORT", "LC_HTTPS_PORT", "LC_ADMIN_HOST", "LC_STORE_HOST",
            "LC_API_HOST", "LC_HOOKS_HOST", "LC_ENV_DIR", "LC_SECRETS_DIR", "LC_SECRETS_GID",
            "LC_BACKUP_DIR", "LC_PG_HOST", "LC_PG_SSLMODE", "LC_IDENTITY_ENABLED", "LC_ONBOARDING_ENABLED",
            "LC_BUYER_ENABLED", "LC_BUYER_SESSION_TTL_SECONDS"]
for k in required:
    rec("P01", bool(E.get(k, "")), k)
rec("P01", env_name in ("production", "staging", "smoke"), "LC_ENVIRONMENT")
rec("P01", re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,62}", E.get("COMPOSE_PROJECT_NAME", "")) is not None, "COMPOSE_PROJECT_NAME")
rec("P01", "CHANGE_ME" not in E.get("IMAGE_TAG", "CHANGE_ME") and re.fullmatch(r"[A-Za-z0-9._-]{1,64}", E.get("IMAGE_TAG", "")) is not None, "IMAGE_TAG")
fqdn = re.compile(r"(?=.{1,253}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]")
hosts = {k: E.get(k, "") for k in ("LC_ADMIN_HOST", "LC_STORE_HOST", "LC_API_HOST", "LC_HOOKS_HOST")}
for k, h in hosts.items():
    ok = fqdn.fullmatch(h) is not None
    if not smoke:
        ok = ok and not h.endswith(".localhost")
    if prod:
        ok = ok and not (h == "example.com" or h.endswith(".example.com") or h.endswith(".example"))
    rec("P01", ok, k)
rec("P01", len(set(hosts.values())) == 4, "LC_*_HOST distinct")

# ---- P02 secrets dir/file modes ----------------------------------------------------------------
sdir = E.get("LC_SECRETS_DIR", "")
gid = int(E.get("LC_SECRETS_GID", "-1")) if E.get("LC_SECRETS_GID", "").isdigit() else -1
manifest = []
with open(os.path.join(deploy, "secrets.manifest.tsv"), encoding="utf-8") as fh:
    for line in fh:
        if line.startswith("#") or not line.strip():
            continue
        f = line.rstrip("\n").split("\t")
        manifest.append((f[0], f[1], f[2]))
logins = {}
with open(os.path.join(deploy, "postgres", "logins.tsv"), encoding="utf-8") as fh:
    for line in fh:
        if line.startswith("#") or not line.strip():
            continue
        f = line.rstrip("\n").split("\t")
        if f[3] == "core":
            logins[f[0]] = f[4].split(":")[0]
if os.path.isdir(sdir):
    st = os.stat(sdir)
    mode_ok = stat.S_IMODE(st.st_mode) & 0o027 == 0 and st.st_gid == gid
    owner_ok = smoke or st.st_uid == 0
    rec("P02", mode_ok and owner_ok, "LC_SECRETS_DIR")
else:
    rec("P02", False, "LC_SECRETS_DIR")
values = {}
for name, kind, gen in manifest:
    p = os.path.join(sdir, name)
    if not os.path.isfile(p):
        continue
    st = os.stat(p)
    m = stat.S_IMODE(st.st_mode)
    rec("P02", m & 0o337 == 0 and m & 0o040 and st.st_gid == gid and (smoke or st.st_uid == 0), name)
    values[name] = open(p, "rb").read().decode("utf-8", "replace").rstrip("\n")

# ---- P03 existence and formats; P05 key ids -------------------------------------------------------
def b64url32(v):
    try:
        return len(v) == 43 and base64.urlsafe_b64decode(v + "=") and len(base64.urlsafe_b64decode(v + "=")) == 32 \
            and base64.urlsafe_b64encode(base64.urlsafe_b64decode(v + "=")).decode().rstrip("=") == v
    except Exception:
        return False
def b64std32(v):
    try:
        d = base64.b64decode(v, validate=True)
        return len(d) == 32 and base64.b64encode(d).decode() == v
    except Exception:
        return False
acct_id = re.compile(r"[A-Za-z0-9_-]{1,40}")
meta_id = re.compile(r"[A-Za-z0-9_-]{1,64}")
def keyring(v, kind):
    try:
        doc = json.loads(v)
    except ValueError:
        return None
    if kind == "account_keyring":
        entries, idre = doc, acct_id
    else:
        if not isinstance(doc, dict) or set(doc) != {"keys"}:
            return None
        entries, idre = doc["keys"], meta_id
    if not isinstance(entries, list) or not 1 <= len(entries) <= 16 or len(v) > 8192:
        return None
    ids = {}
    for e in entries:
        if not isinstance(e, dict) or set(e) != {"id", "key_base64"} or not isinstance(e["id"], str) \
                or not idre.fullmatch(e["id"]) or e["id"] in ids or not b64std32(e["key_base64"]):
            return None
        ids[e["id"]] = e["key_base64"]
    return ids
def dsn_tcp_ok(v, login):
    try:
        u = urlsplit(v)
        q = parse_qs(u.query)
        return (u.scheme == "postgres" and u.username == login and u.hostname == E.get("LC_PG_HOST", "").lower()
                and u.port == 5432 and u.path == "/live_commerce" and q.get("sslmode") == [E.get("LC_PG_SSLMODE")]
                and q.get("application_name") == [logins.get(login)])
    except ValueError:
        return False
def meta_apps_ok(v):
    try:
        doc = json.loads(v)
    except ValueError:
        return False
    if not isinstance(doc, dict) or set(doc) != {"apps"} or not isinstance(doc["apps"], list) or not 1 <= len(doc["apps"]) <= 16:
        return False
    seen = set()
    for a in doc["apps"]:
        if not isinstance(a, dict) or set(a) != {"app_id", "object", "app_secret", "verify_token"} \
                or not all(isinstance(a[k], str) and a[k] for k in a):
            return False
        key = (a["app_id"], a["object"])
        if key in seen:
            return False
        seen.add(key)
    return len(v) <= 32768
def hpke_ring(v, field):
    # meta-ads-v1 G3: metaads.LoadSealKeys (public) / tokenopen.LoadKeyring (private), 1..16 X25519 keys.
    try:
        doc = json.loads(v)
    except ValueError:
        return None
    if not isinstance(doc, dict) or set(doc) != {"keys"} or not isinstance(doc["keys"], list) \
            or not 1 <= len(doc["keys"]) <= 16 or len(v) > 8192:
        return None
    ids = {}
    for e in doc["keys"]:
        if not isinstance(e, dict) or set(e) != {"id", field} or not isinstance(e["id"], str) \
                or not meta_id.fullmatch(e["id"]) or e["id"] in ids or not b64std32(e[field]):
            return None
        ids[e["id"]] = e[field]
    return ids
rings = {}
for name, kind, gen in manifest:
    if name not in values:
        rec("P03", False, name + " (missing)")
        continue
    v = values[name]
    if gen == "owner" and v == "__UNSET__":
        rec("P03", True, name)
        continue
    if kind == "hex32":
        ok = re.fullmatch(r"[0-9a-f]{64}", v) is not None
    elif kind == "b64url32":
        ok = b64url32(v)
    elif kind == "b64std32":
        ok = b64std32(v)
    elif kind in ("account_keyring", "meta_keyring"):
        rings[name] = keyring(v, kind)
        ok = rings[name] is not None
    elif kind == "key_id":
        # account + Stripe webhook signing rings use accounts.LoadKeyring (<=40); Meta rings <=64.
        ok = acct_id.fullmatch(v) is not None if ("account" in name or "stripe_webhook" in name) else meta_id.fullmatch(v) is not None
    elif kind == "dsn_tcp":
        ok = dsn_tcp_ok(v, name[len("dsn_"):])
    elif kind == "dsn_socket":
        ok = v.startswith("host=/var/run/postgresql ") and " user=postgres " in v and " dbname=live_commerce " in v
    elif kind == "opaque":
        ok = 0 < len(v) <= 4096 and "\n" not in v
    elif kind == "meta_apps":
        ok = meta_apps_ok(v)
    elif kind in ("hpke_private_ring", "hpke_public_ring"):
        rings[name] = hpke_ring(v, "private_key_base64" if kind == "hpke_private_ring" else "public_key_base64")
        ok = rings[name] is not None
    else:
        ok = False
    rec("P03", ok, name)
for idf, ringf in (("commerce_account_active_key_id", "commerce_account_keys_json"),
                   ("commerce_meta_payload_active_key_id", "commerce_meta_payload_keys_json"),
                   ("commerce_stripe_webhook_active_key_id", "commerce_stripe_webhook_keys_json"),
                   ("commerce_meta_page_token_active_key_id", "commerce_meta_page_token_keys_json"),
                   ("commerce_meta_ads_hpke_active_key_id", "commerce_meta_ads_hpke_public_keys_json")):
    ring = rings.get(ringf)
    rec("P05", ring is not None and values.get(idf) in ring, idf)

# ---- P04 inequalities / DSN consistency --------------------------------------------------------------
bff, buyer, cookie = values.get("commerce_bff_key"), values.get("commerce_buyer_bff_key"), values.get("commerce_buyer_cookie_key")
rec("P04", None not in (bff, buyer) and bff != buyer, "commerce_buyer_bff_key != commerce_bff_key")
rec("P04", None not in (buyer, cookie) and buyer != cookie, "commerce_buyer_cookie_key != commerce_buyer_bff_key")
label = values.get("commerce_claims_label_key")
rec("P04", label is not None and label not in (bff, buyer, cookie), "commerce_claims_label_key differs from the BFF/cookie keys")
acct = rings.get("commerce_account_keys_json") or {}
rec("P04", values.get("commerce_account_replay_key") not in acct.values(), "commerce_account_replay_key not in keyring")
# R1 custody separation (stripe-psp-v1 §12, meta-claims-intake-v1 §3/§7): no key bytes are shared between
# the payment API-key ring, the Stripe webhook signing ring, the Meta payload ring and the Page-token ring,
# and the standalone std-base64 keys (replay keys, K_actor, K_link) are pairwise distinct and in no ring.
ring_names = ("commerce_account_keys_json", "commerce_stripe_webhook_keys_json",
              "commerce_meta_payload_keys_json", "commerce_meta_page_token_keys_json")
ring_keys = {n: set((rings.get(n) or {}).values()) for n in ring_names}
for i, a in enumerate(ring_names):
    for b in ring_names[i + 1:]:
        rec("P04", not (ring_keys[a] & ring_keys[b]), a + " shares no key with " + b)
solo = [n for n, k, g in manifest if k == "b64std32"]
for i, a in enumerate(solo):
    va = values.get(a)
    rec("P04", va is not None and all(va not in ks for ks in ring_keys.values()), a + " not in any keyring")
    for b in solo[i + 1:]:
        rec("P04", va is not None and va != values.get(b), a + " != " + b)
for login in logins:
    dsn, pw = values.get("dsn_" + login), values.get("pw_" + login)
    ok = False
    if dsn and pw:
        try:
            ok = urlsplit(dsn).password == pw
        except ValueError:
            ok = False
    rec("P04", ok, "dsn_" + login + " embeds pw_" + login)
hp, hq = rings.get("commerce_meta_ads_hpke_private_keys_json"), rings.get("commerce_meta_ads_hpke_public_keys_json")
rec("P04", hp is not None and hq is not None and list(hp) == list(hq),
    "commerce_meta_ads_hpke_public_keys_json ids == private ring ids (secrets-init.sh --rederive after a rotation)")
su = values.get("pg_superuser_password")
rec("P04", bool(su) and values.get("dsn_migrate_owner", "").find(" password=" + su + " ") >= 0, "dsn_migrate_owner embeds pg_superuser_password")

# ---- knob files -------------------------------------------------------------------------------------
env_dir = E.get("LC_ENV_DIR", "")
allow = {
    "api.env": {"COMMERCE_ACCOUNTS_ENABLED", "COMMERCE_BUYER_PAYMENT_ENABLED", "COMMERCE_META_WEBHOOK_ENABLED",
                "COMMERCE_STUDIO_ENABLED", "COMMERCE_STUDIO_MEDIA_ENABLED", "COMMERCE_CLAIMS_ENABLED", "COMMERCE_OIDC_CLIENT_ID", "COMMERCE_IDENTITY_PROVIDER_KEY",
                "COMMERCE_SESSION_TTL", "COMMERCE_PAYMENT_PROFILE", "COMMERCE_META_ADS_APP_ID", "COMMERCE_META_ADS_CONFIG_ID",
                "COMMERCE_META_ADS_REDIRECT_URI", "COMMERCE_META_ADS_GRAPH_VERSION", "TZ"},
    "admin.env": {"NODE_OPTIONS", "TZ"},
    "storefront.env": {"NODE_OPTIONS", "TZ"},
    "payment-worker.env": {"COMMERCE_PAYMENT_WORKER_CONCURRENCY", "TZ"},
    "claims-worker.env": {"COMMERCE_META_GRAPH_VERSION", "COMMERCE_META_GRAPH_AUTH_HEADER", "TZ"},
    "ads-worker.env": {"COMMERCE_META_ADS_GRAPH_VERSION", "COMMERCE_META_ADS_PARTNER_AGENT", "TZ"},
    "expiry-worker.env": {"COMMERCE_EXPIRY_WORKER_CONCURRENCY", "TZ"},
    "meta-worker.env": {"COMMERCE_META_WORKER_CONCURRENCY", "TZ"},
    "caddy.env": {"ACME_EMAIL", "LC_ACME_CA", "TZ"},
    "postgres.env": {"TZ"},
}
knobs = {}
for fname in allow:
    p = os.path.join(env_dir, fname)
    if not os.path.isfile(p):
        rec("P06", False, fname + " (missing)")
        knobs[fname] = {}
        continue
    knobs[fname] = parse_env(p)
compose_env = parse_env(E.get("LC_COMPOSE_ENV")) if os.path.isfile(E.get("LC_COMPOSE_ENV", "")) else {}

# ---- P07 forbidden variables ----------------------------------------------------------------------
# COMMERCE_META_GRAPH_BASE_URL is a loopback-MOCK switch (metareply.Config): production always dials graph.facebook.com.
# STRIPE_*/META_PAGE_ACCESS_TOKEN are operator inputs for ops-admin.sh only, never a knob or compose value.
forbidden = re.compile(r"^(COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS|COMMERCE_FIXTURE_.*|LC_TEST_.*|FIXTURE_.*|LC_ADMIN_GUARD_DSN|COMMERCE_META_GRAPH_BASE_URL|STRIPE_[A-Z_]+|META_PAGE_ACCESS_TOKEN)$")
bad = sorted({k for d in list(knobs.values()) + [compose_env] for k in d if forbidden.match(k)})
rec("P07", not bad, "forbidden vars absent" if not bad else ",".join(bad))

# ---- P06 flags, dependencies, profiles, allowlists -----------------------------------------------------
for fname, d in knobs.items():
    for problem in ("__bad__", "__inline_comment__"):
        if problem in d:
            rec("P06", False, fname + " " + problem.strip("_"))
    extra = sorted(k for k in d if not k.startswith("__") and k not in allow[fname] and not forbidden.match(k))
    rec("P06", not extra, fname + (" knobs only" if not extra else " redefines/unknown: " + ",".join(extra)))
api = knobs.get("api.env", {})
def flag(name, value):
    ok = value in ("", "0", "1")
    rec("P06", ok, name)
    return value == "1"
identity = flag("LC_IDENTITY_ENABLED", E.get("LC_IDENTITY_ENABLED", ""))
onboarding = flag("LC_ONBOARDING_ENABLED", E.get("LC_ONBOARDING_ENABLED", ""))
password_login = flag("LC_PASSWORD_LOGIN_ENABLED", E.get("LC_PASSWORD_LOGIN_ENABLED", ""))
rec("P06", not password_login or identity, "LC_PASSWORD_LOGIN_ENABLED requires LC_IDENTITY_ENABLED")
buyer_on = flag("LC_BUYER_ENABLED", E.get("LC_BUYER_ENABLED", ""))
accounts = flag("COMMERCE_ACCOUNTS_ENABLED", api.get("COMMERCE_ACCOUNTS_ENABLED", ""))
payment = flag("COMMERCE_BUYER_PAYMENT_ENABLED", api.get("COMMERCE_BUYER_PAYMENT_ENABLED", ""))
meta = flag("COMMERCE_META_WEBHOOK_ENABLED", api.get("COMMERCE_META_WEBHOOK_ENABLED", ""))
studio = flag("COMMERCE_STUDIO_ENABLED", api.get("COMMERCE_STUDIO_ENABLED", ""))
studio_media = flag("COMMERCE_STUDIO_MEDIA_ENABLED", api.get("COMMERCE_STUDIO_MEDIA_ENABLED", ""))
claims_on = flag("COMMERCE_CLAIMS_ENABLED", api.get("COMMERCE_CLAIMS_ENABLED", ""))
profiles = {p.strip() for p in E.get("COMPOSE_PROFILES", "").split(",") if p.strip()}
stripe_on = flag("LC_STRIPE_ENABLED", E.get("LC_STRIPE_ENABLED", ""))
checkout_on = flag("LC_STRIPE_CHECKOUT_ENABLED", E.get("LC_STRIPE_CHECKOUT_ENABLED", ""))  # LD6: platform kill switch, "" = follow LC_STRIPE_ENABLED
rec("P06", not checkout_on or stripe_on, "LC_STRIPE_CHECKOUT_ENABLED=1 requires LC_STRIPE_ENABLED=1 (checkout without the worker and webhook strands held stock)")
stripe_live_flag = flag("LC_STRIPE_LIVE_ENABLED", E.get("LC_STRIPE_LIVE_ENABLED", ""))
stripe_live_ref = E.get("LC_STRIPE_LIVE_APPROVAL_REF", "")
stripe_live_ref_ok = re.fullmatch(r"[A-Za-z0-9._:-]{8,128}", stripe_live_ref) is not None
stripe_live_pair = stripe_live_flag and stripe_live_ref_ok  # LQ8 reference shape; the owner's message itself is not checkable here
ecpay_on = flag("LC_CVS_ECPAY_ENABLED", E.get("LC_CVS_ECPAY_ENABLED", ""))
# customers-billing-v1 T17: platform-fee billing; unset = off (billing.LoadConfig rejects "0", so only "" or "1").
billing_on = flag("LC_BILLING_ENABLED", E.get("LC_BILLING_ENABLED", ""))
rec("P06", E.get("LC_BILLING_ENABLED", "") != "0", "LC_BILLING_ENABLED unset or 1 (0 stops the api)")
rec("P06", profiles <= {"db", "app", "payments-sandbox", "payments-live", "meta", "claims", "ads", "ops"}, "COMPOSE_PROFILES known")
rec("P06", "ops" not in profiles, "COMPOSE_PROFILES must not list ops (one-shots run through ops-admin.sh / pg-ops.sh)")
rec("P06", not accounts or identity, "COMMERCE_ACCOUNTS_ENABLED requires LC_IDENTITY_ENABLED")
# R1 ruling G2: Studio (planning + claims + claim-source) is deployable; LiveKit media is not (F11).
rec("P06", not studio or identity, "COMMERCE_STUDIO_ENABLED requires LC_IDENTITY_ENABLED")
rec("P06", not claims_on or studio, "COMMERCE_CLAIMS_ENABLED requires COMMERCE_STUDIO_ENABLED")
rec("P06", not studio_media, "COMMERCE_STUDIO_MEDIA_ENABLED must be 0 (media worker not deployable)")
rec("P06", not payment or buyer_on, "COMMERCE_BUYER_PAYMENT_ENABLED requires LC_BUYER_ENABLED")
profile_name = api.get("COMMERCE_PAYMENT_PROFILE", "")
if "app" in profiles and profile_name:
    need = {"SANDBOX": "payments-sandbox", "LIVE": "payments-live"}.get(profile_name)
    rec("P06", need is not None and need in profiles, "COMMERCE_PAYMENT_PROFILE matches payments-* profile")
rec("P06", "payments-live" not in profiles, "payments-live active (REAL MONEY: owner approval required)", warn=True)
if "app" in profiles:
    rec("P06", not meta or "meta" in profiles, "COMMERCE_META_WEBHOOK_ENABLED requires meta profile")
    # stripe-live-enable-v1 §5.2/LD6: LC_STRIPE_ENABLED (webhook + worker) and LC_STRIPE_CHECKOUT_ENABLED (buyer
    # checkout, the platform kill switch) are separate; the profile, the worker profile and the LIVE pair must agree.
    # The pair is read from compose.env keys (same names ops-admin.sh maps to COMMERCE_STRIPE_LIVE_*); both or neither.
    if stripe_on:
        if profile_name == "LIVE":
            rec("P06", "payments-live" in profiles, "LC_STRIPE_ENABLED on COMMERCE_PAYMENT_PROFILE=LIVE requires the payments-live profile")
            rec("P06", stripe_live_pair, "LC_STRIPE_ENABLED on COMMERCE_PAYMENT_PROFILE=LIVE requires LC_STRIPE_LIVE_ENABLED=1 and LC_STRIPE_LIVE_APPROVAL_REF")
        else:
            rec("P06", profile_name == "SANDBOX", "LC_STRIPE_ENABLED requires COMMERCE_PAYMENT_PROFILE=SANDBOX or LIVE (with the live pair)")
            rec("P06", "payments-sandbox" in profiles, "LC_STRIPE_ENABLED on SANDBOX requires the payments-sandbox profile")
    # A LIVE pair without a LIVE deployment is a leftover that would arm the next profile switch: fail both directions.
    rec("P06", stripe_live_flag == bool(stripe_live_ref), "LC_STRIPE_LIVE_ENABLED and LC_STRIPE_LIVE_APPROVAL_REF are set together or not at all")
    rec("P06", not stripe_live_flag or stripe_live_ref_ok, "LC_STRIPE_LIVE_APPROVAL_REF matches [A-Za-z0-9._:-]{8,128}")
    rec("P06", profile_name == "LIVE" or not stripe_live_pair, "LC_STRIPE_LIVE_ENABLED is only meaningful with COMMERCE_PAYMENT_PROFILE=LIVE", warn=True)
    # taiwan-cvs-logistics-v1 §12: ECPay is SANDBOX-only here (compose pins the claims-worker to SANDBOX, LIVE create
    # off), and its label create route runs in claims-worker, so the api profile and the claims profile must agree.
    if ecpay_on:
        rec("P06", profile_name == "SANDBOX", "LC_CVS_ECPAY_ENABLED requires COMMERCE_PAYMENT_PROFILE=SANDBOX")
        rec("P06", "claims" in profiles, "LC_CVS_ECPAY_ENABLED requires the claims profile (ecpay.cvs_create route)")
    # claims-worker sends first private replies through the Meta consumer's data: it needs the meta profile.
    rec("P06", "claims" not in profiles or "meta" in profiles, "claims profile requires meta profile")
    rec("P06", "claims" not in profiles or meta, "claims profile requires COMMERCE_META_WEBHOOK_ENABLED=1 (nothing to intake otherwise)", warn=True)
# meta-ads-v1: the api mounts the merchant ads routes iff COMMERCE_META_ADS_APP_ID is set (cmd/api newMerchantAds).
# ads-worker is the only dispatcher of ads operations: without it a pause would never reach Meta (§6.3 pause always works).
ads_app = api.get("COMMERCE_META_ADS_APP_ID", "")
if ads_app:
    rec("P06", "ads" in profiles, "COMMERCE_META_ADS_APP_ID requires the ads profile (ads-worker dispatches pause)")
    rec("P06", identity, "COMMERCE_META_ADS_APP_ID requires LC_IDENTITY_ENABLED")
    # Ruling B15: the ads mount waits for ads-capi's 0080 (billing.store_standing grant, contract §4.3).
    mig = os.path.join(os.path.dirname(deploy), "migrations")
    rec("P06", os.path.isdir(mig) and any(re.fullmatch(r"0080_.*\.sql", f) for f in os.listdir(mig)),
        "COMMERCE_META_ADS_APP_ID requires migrations/0080 (ruling B15: ads mount waits for ads-capi)")
rec("P06", "ads" not in profiles or bool(ads_app), "ads profile without COMMERCE_META_ADS_APP_ID (nothing to dispatch)", warn=True)

# ---- P08 grammars and ranges -------------------------------------------------------------------------------
ttl = E.get("LC_BUYER_SESSION_TTL_SECONDS", "")
rec("P08", re.fullmatch(r"[1-9][0-9]{1,6}", ttl) is not None and 60 <= int(ttl) <= 2592000, "LC_BUYER_SESSION_TTL_SECONDS")
cur = E.get("LC_ONBOARDING_CURRENCIES", "")
rec("P08", (not cur and not onboarding) or (cur and all(re.fullmatch(r"[A-Z]{3}", c.strip()) for c in cur.split(","))),
    "LC_ONBOARDING_CURRENCIES")
cw = knobs.get("claims-worker.env", {})
if "claims" in profiles:
    rec("P08", re.fullmatch(r"v[0-9]{1,3}\.[0-9]{1,2}", cw.get("COMMERCE_META_GRAPH_VERSION", "")) is not None,
        "COMMERCE_META_GRAPH_VERSION (vNN.N, no default; from probe U5)")
    rec("P08", cw.get("COMMERCE_META_GRAPH_AUTH_HEADER", "") in ("", "0", "1"), "COMMERCE_META_GRAPH_AUTH_HEADER")
aw = knobs.get("ads-worker.env", {})
graph_version = re.compile(r"v[0-9]{1,3}\.[0-9]{1,2}")
if ads_app:
    rec("P08", re.fullmatch(r"[0-9]{1,40}", ads_app) is not None, "COMMERCE_META_ADS_APP_ID (numeric Meta app id)")
    rec("P08", re.fullmatch(r"[0-9]{1,40}", api.get("COMMERCE_META_ADS_CONFIG_ID", "")) is not None, "COMMERCE_META_ADS_CONFIG_ID")
    rec("P08", api.get("COMMERCE_META_ADS_REDIRECT_URI", "") == "https://" + E.get("LC_ADMIN_HOST", "") + "/api/ads/meta/callback",
        "COMMERCE_META_ADS_REDIRECT_URI == https://<LC_ADMIN_HOST>/api/ads/meta/callback")
    rec("P08", graph_version.fullmatch(api.get("COMMERCE_META_ADS_GRAPH_VERSION", "")) is not None, "COMMERCE_META_ADS_GRAPH_VERSION (api)")
if "ads" in profiles:
    rec("P08", graph_version.fullmatch(aw.get("COMMERCE_META_ADS_GRAPH_VERSION", "")) is not None, "COMMERCE_META_ADS_GRAPH_VERSION (ads-worker)")
    rec("P08", not ads_app or aw.get("COMMERCE_META_ADS_GRAPH_VERSION") == api.get("COMMERCE_META_ADS_GRAPH_VERSION"),
        "COMMERCE_META_ADS_GRAPH_VERSION api == ads-worker")
    rec("P08", re.fullmatch(r"[A-Za-z0-9_.-]{1,50}", aw.get("COMMERCE_META_ADS_PARTNER_AGENT", "")) is not None
        and "CHANGE_ME" not in aw.get("COMMERCE_META_ADS_PARTNER_AGENT", ""), "COMMERCE_META_ADS_PARTNER_AGENT")
for fname, key in (("payment-worker.env", "COMMERCE_PAYMENT_WORKER_CONCURRENCY"),
                   ("expiry-worker.env", "COMMERCE_EXPIRY_WORKER_CONCURRENCY"),
                   ("meta-worker.env", "COMMERCE_META_WORKER_CONCURRENCY")):
    v = knobs.get(fname, {}).get(key, "")
    rec("P08", v == "" or (re.fullmatch(r"[1-9][0-9]?", v) is not None and 1 <= int(v) <= 16), key)
def go_duration(v):
    m = re.fullmatch(r"(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?", v or "")
    if not v or not m:
        return None
    h, mi, s = (int(x) if x else 0 for x in m.groups())
    return h * 3600 + mi * 60 + s
if identity:
    d = go_duration(api.get("COMMERCE_SESSION_TTL", ""))
    rec("P08", d is not None and 300 <= d <= 86400, "COMMERCE_SESSION_TTL")
    # merchant-password-auth-v1 R-4: OIDC rules apply only when OIDC is in use (issuer set) or password
    # login is off (OIDC is then the only login and stays fully required).
    if not password_login or E.get("LC_OIDC_ISSUER", ""):
        pk = api.get("COMMERCE_IDENTITY_PROVIDER_KEY", "")
        rec("P08", 1 <= len(pk.encode()) <= 128, "COMMERCE_IDENTITY_PROVIDER_KEY")
        cid = api.get("COMMERCE_OIDC_CLIENT_ID", "")
        rec("P08", bool(cid) and not (prod and cid.startswith("CHANGE_ME")), "COMMERCE_OIDC_CLIENT_ID")
        iss = urlsplit(E.get("LC_OIDC_ISSUER", ""))
        rec("P08", iss.scheme == "https" and bool(iss.hostname) and not iss.query and not iss.fragment
            and not (prod and (iss.hostname or "").endswith("example.com")), "LC_OIDC_ISSUER")
    if password_login and not E.get("LC_OIDC_ISSUER", ""):
        # cmd/api/identity.go loadIdentityConfig: with password login on, OIDC is all-or-nothing, so a leftover
        # client id / provider key (e.g. the example's CHANGE_ME_CLIENT_ID) with no issuer stops the api with only
        # "api stopped" (pilot host, 2026-10-01).
        for k in ("COMMERCE_OIDC_CLIENT_ID", "COMMERCE_IDENTITY_PROVIDER_KEY"):
            rec("P08", api.get(k, "") == "", k + " must be empty when LC_OIDC_ISSUER is empty (password-only login)")
    if password_login:
        # cmd/api/identity.go loadPasswordConfig grammar: DNS name host, From carries exactly the username.
        smtp_host, smtp_user, mail_from = E.get("LC_SMTP_HOST", ""), E.get("LC_SMTP_USERNAME", ""), E.get("LC_MAIL_FROM", "")
        rec("P08", re.fullmatch(r"[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+", smtp_host) is not None, "LC_SMTP_HOST (DNS name)")
        rec("P08", re.fullmatch(r"[^@\s<>]+@[^@\s<>]+\.[^@\s<>]+", smtp_user) is not None and not (prod and "CHANGE_ME" in smtp_user), "LC_SMTP_USERNAME")
        rec("P08", bool(smtp_user) and (mail_from.lower().endswith("<" + smtp_user.lower() + ">") or mail_from.lower() == smtp_user.lower()),
            "LC_MAIL_FROM carries exactly LC_SMTP_USERNAME")
        cap = E.get("LC_MAIL_DAILY_CAP", "200")
        rec("P08", cap.isdigit() and 20 <= int(cap) <= 100000, "LC_MAIL_DAILY_CAP")
        rec("P08", E.get("LC_BREACH_CHECK", "hibp") == "hibp", "LC_BREACH_CHECK must be hibp outside loopback tests")
    rec("P08", not onboarding or bool(cur), "LC_ONBOARDING_CURRENCIES required by onboarding")
if payment or accounts:
    rec("P08", profile_name in ("SANDBOX", "LIVE"), "COMMERCE_PAYMENT_PROFILE")
for k in ("LC_HTTP_PORT", "LC_HTTPS_PORT"):
    v = E.get(k, "")
    rec("P08", v.isdigit() and 1 <= int(v) <= 65535, k)
rec("P08", E.get("LC_PG_SSLMODE") in ("disable", "require", "verify-ca", "verify-full"), "LC_PG_SSLMODE")
rec("P08", E.get("LC_SECRETS_GID", "").isdigit(), "LC_SECRETS_GID")
rec("P08", E.get("LC_REQUIRE_MEDIA_GATE", "0") in ("0", "1"), "LC_REQUIRE_MEDIA_GATE")
rec("P08", E.get("LC_REQUIRE_RETENTION_ENFORCED", "1") in ("0", "1"), "LC_REQUIRE_RETENTION_ENFORCED")

if billing_on:
    # Same grammar as internal/billing.LoadConfig (1..10 distinct price ids); ids are not secrets.
    ids = [i.strip() for i in E.get("LC_BILLING_PRICE_IDS", "").split(",")]
    rec("P08", 1 <= len(ids) <= 10 and len(set(ids)) == len(ids)
        and all(re.fullmatch(r"price_[A-Za-z0-9]{1,64}", i) for i in ids), "LC_BILLING_PRICE_IDS")
# ---- P09 owner-supplied secrets ------------------------------------------------------------------------------
if billing_on and "app" in profiles:
    # customers-billing-v1 BD8: SANDBOX platform keys only; owner-supplied files (O-D). Same patterns as
    # internal/billing.LoadConfig, so a bad file fails here instead of stopping the api at start.
    rec("P09", re.fullmatch(r"(sk|rk)_test_[A-Za-z0-9]{16,240}", values.get("commerce_platform_stripe_secret_key", "")) is not None,
        "commerce_platform_stripe_secret_key (sk_test_/rk_test_ only; LC_BILLING_ENABLED=1)")
    rec("P09", re.fullmatch(r"whsec_[!-~]{16,249}", values.get("commerce_platform_stripe_webhook_secret", "")) is not None,
        "commerce_platform_stripe_webhook_secret (whsec_; LC_BILLING_ENABLED=1)")
if meta and "app" in profiles:
    rec("P09", values.get("commerce_meta_apps_json", "__UNSET__") != "__UNSET__", "commerce_meta_apps_json")
if ads_app:
    rec("P09", values.get("commerce_meta_ads_app_secret", "__UNSET__") != "__UNSET__", "commerce_meta_ads_app_secret")
if ecpay_on:
    rec("P09", values.get("ecpay_logistics_keyring", "__UNSET__") != "__UNSET__", "ecpay_logistics_keyring")
if identity and (not password_login or E.get("LC_OIDC_ISSUER", "")):
    rec("P09", values.get("commerce_oidc_client_secret", "__UNSET__") != "__UNSET__",
        "commerce_oidc_client_secret (__UNSET__ = public PKCE client)", warn=True)
if identity and password_login:
    rec("P09", values.get("commerce_smtp_password", "__UNSET__") != "__UNSET__",
        "commerce_smtp_password (owner-supplied SMTP authorization code of a dedicated sending mailbox)")

# ---- P11 ACME (production) ------------------------------------------------------------------------------------
caddy = knobs.get("caddy.env", {})
email = caddy.get("ACME_EMAIL", "")
rec("P11", re.fullmatch(r"[^@\s]+@[^@\s]+\.[^@\s]+", email) is not None, "ACME_EMAIL")
# Caddy applies {$LC_ACME_CA:default} only when the variable is UNSET; an empty value is a
# Caddyfile parse error ("wrong argument count ... acme_ca"), so present-but-empty fails here.
if "LC_ACME_CA" in caddy:
    rec("P11", caddy["LC_ACME_CA"].startswith("https://"), "LC_ACME_CA (unset it or give an https URL)")
if prod:
    rec("P11", "example." not in email and "CHANGE_ME" not in email, "ACME_EMAIL not a placeholder")
    rec("P11", "staging" not in caddy.get("LC_ACME_CA", ""), "LC_ACME_CA not staging")

# ---- P12 DB TLS -------------------------------------------------------------------------------------------------
if E.get("LC_PG_HOST", "postgres") != "postgres":
    rec("P12", E.get("LC_PG_SSLMODE") == "verify-full", "LC_PG_SSLMODE=verify-full for remote LC_PG_HOST")
else:
    rec("P12", True, "LC_PG_HOST=postgres (internal network)")

failed = False
for rule, status, name in results:
    if status == "FAIL":
        failed = True
    if status != "PASS" or os.environ.get("LC_PREFLIGHT_VERBOSE") == "1":
        print(f"{rule} {status} {name}")
summary = {}
for rule, status, _ in results:
    summary.setdefault(rule, set()).add(status)
for rule in sorted(summary):
    st = "FAIL" if "FAIL" in summary[rule] else ("WARN" if "WARN" in summary[rule] else "PASS")
    print(f"{rule} {st} (rule summary)")
sys.exit(1 if failed else 0)
PY

# ---- P10 images present locally ------------------------------------------------------------------
if ((skip_images)); then
  echo "P10 SKIP images (--skip-images)"
else
  for img in go admin storefront caddy; do
    if docker image inspect "${LC_IMAGE_PREFIX:-lc}-$img:$IMAGE_TAG" >/dev/null 2>&1; then
      echo "P10 PASS ${LC_IMAGE_PREFIX:-lc}-$img:IMAGE_TAG"
    else
      echo "P10 FAIL ${LC_IMAGE_PREFIX:-lc}-$img:IMAGE_TAG (run build-images.sh)"
      fail=1
    fi
  done
fi

# ---- P13 disk space (>= 20 % free) -------------------------------------------------------------------
free_pct() { df -P "$1" 2>/dev/null | awk 'NR==2 { gsub("%","",$5); print 100-$5 }'; }
docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
for pair in "docker_root:$docker_root" "LC_BACKUP_DIR:${LC_BACKUP_DIR:-/}"; do
  name=${pair%%:*} path=${pair#*:}
  pct=$(free_pct "$path" || true)
  if [[ "$pct" =~ ^[0-9]+$ ]] && ((pct >= 20)); then
    echo "P13 PASS $name free=${pct}%"
  else
    echo "P13 FAIL $name free=${pct:-unknown}% (< 20 %)"
    fail=1
  fi
done

# ---- online checks --------------------------------------------------------------------------------
if ((online)); then
  if [[ "${LC_ENVIRONMENT:-}" == smoke ]]; then
    echo "P14 SKIP smoke uses *.localhost"
  else
    mine=" $(hostname -I 2>/dev/null) ${LC_PUBLIC_IP:-} "
    for h in LC_ADMIN_HOST LC_STORE_HOST LC_API_HOST LC_HOOKS_HOST; do
      addrs=$(getent ahosts "${!h}" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')
      hit=0
      for a in $addrs; do [[ "$mine" == *" $a "* ]] && hit=1; done
      if ((hit)); then echo "P14 PASS $h"; else
        echo "P14 FAIL $h does not resolve to this host (set LC_PUBLIC_IP behind NAT)"
        fail=1
      fi
    done
  fi
  if [[ "${LC_IDENTITY_ENABLED:-0}" == 1 && -n "${LC_OIDC_ISSUER:-}" ]]; then
    url="${LC_OIDC_ISSUER%/}/.well-known/openid-configuration"
    if grep -qx caddy < <(lc_compose ps --status running --services 2>/dev/null); then
      if lc_compose exec -T caddy wget -q -T 10 -O /dev/null "$url" >/dev/null 2>&1; then echo "P15 PASS LC_OIDC_ISSUER discovery (edge netns)"; else
        echo "P15 FAIL LC_OIDC_ISSUER discovery unreachable from the edge netns"
        fail=1
      fi
    elif curl -fsS --max-time 10 -o /dev/null "$url"; then
      echo "P15 WARN LC_OIDC_ISSUER reachable from the host (caddy not running; edge netns not checked)"
    else
      echo "P15 FAIL LC_OIDC_ISSUER discovery unreachable"
      fail=1
    fi
  else
    echo "P15 SKIP identity disabled or no OIDC issuer (password-only login)"
  fi
  # merchant-password-auth-v1 ruling Q6 (contract gate PA15; rule P17 — final number, ruling B4, confirmed at the R2 integration merge: every other R2 rule lives under P06/P08/P09):
  # per-IP limits trust the edge's X-Forwarded-For, so with password login on the admin host must resolve
  # ONLY to this host. A Cloudflare-proxied name resolves to Cloudflare anycast addresses, none of which
  # are this host's, so "every address is ours" also proves "no Cloudflare range" without a hard-coded list.
  if [[ "${LC_PASSWORD_LOGIN_ENABLED:-0}" == 1 && "${LC_ENVIRONMENT:-}" != smoke ]]; then
    mine=" $(hostname -I 2>/dev/null) ${LC_PUBLIC_IP:-} "
    addrs=$(getent ahosts "$LC_ADMIN_HOST" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')
    foreign=0
    [[ -n "$addrs" ]] || foreign=1
    for a in $addrs; do [[ "$mine" == *" $a "* ]] || foreign=1; done
    if ((foreign)); then
      echo "P17 FAIL LC_ADMIN_HOST must resolve only to this host (DNS-only, no Cloudflare proxy) while LC_PASSWORD_LOGIN_ENABLED=1"
      fail=1
    else
      echo "P17 PASS LC_ADMIN_HOST resolves only to this host"
    fi
  fi
  if command -v timedatectl >/dev/null 2>&1 && [[ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" == yes ]]; then
    echo "P16 PASS clock synchronized"
  elif [[ "${LC_ENVIRONMENT:-}" == smoke ]]; then
    echo "P16 WARN clock sync not verifiable (smoke)"
  else
    echo "P16 FAIL clock not NTP-synchronized"
    fail=1
  fi
fi

if ((fail)); then
  lc_error "preflight FAILED (fix the FAIL lines above; values are never printed)"
  exit 1
fi
lc_info "preflight PASS"
