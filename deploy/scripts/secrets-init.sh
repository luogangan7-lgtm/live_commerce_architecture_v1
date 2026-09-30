#!/usr/bin/env bash
# File: deploy/scripts/secrets-init.sh
# Purpose: create every MISSING secret file listed in deploy/secrets.manifest.tsv (deploy-design
#   §10.3). Never overwrites an existing secret, never prints a value. Derived DSN files are
#   rewritten only when missing or with --rederive (after a password rotation or when
#   LC_PG_HOST/LC_PG_SSLMODE change). Owner-supplied secrets start as the literal __UNSET__,
#   which lcentry treats as "unset" so the app's own validation fails closed.
# Usage: secrets-init.sh [--rederive]
# Runs as/in: deploy host, root (real deployments; files end up 0440 root:${LC_SECRETS_GID}).
#   Smoke runs it as the invoking user with a temp LC_SECRETS_DIR.
# Reads env (compose.env via lib.sh): LC_SECRETS_DIR, LC_SECRETS_GID (group allowed to read),
#   LC_PG_HOST, LC_PG_SSLMODE (DSN host/TLS mode; verify-full adds sslrootcert=/run/secrets/pg_ca_crt).
# Reads secrets: existing pw_* / pg_superuser_password / keyrings (to derive DSNs and key ids
#   and to enforce the inequality rules). Values stay in shell variables and files only.
# Used by: operators (docs/runbooks/deploy.md §配置, §密钥轮换), smoke.sh S09.
# Depends on: openssl (random bytes), python3 (keyring JSON), deploy/postgres/logins.tsv
#   (DSN application_name = consuming service).
# Status: DESIGN; verified by smoke S09 (modes 0440, idempotent re-run keeps checksums).
# Change rules: generators and formats must match internal/identityhttp.ValidSecret (43-char
#   base64url), accounts/env.go (std base64 keys, id ^[A-Za-z0-9_-]{1,40}$, replay key not in
#   the keyring; the Stripe webhook signing keyring reuses this loader under COMMERCE_STRIPE_WEBHOOK_*),
#   meta/env.go and metareply/keyring.go ({"keys":[...]} with 44-char keys) and
#   meta.LoadClaimsActorKey / cmd/claims-worker (44-char std base64, K_actor and K_link distinct) and
#   metaads.LoadSealKeys / tokenopen.LoadKeyring (HPKE X25519 rings, public derived from private). Output lists file names
#   and created|kept|rederived only.
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

rederive=0
case "${1:-}" in
--rederive) rederive=1 ;;
"") ;;
*) lc_die "usage: secrets-init.sh [--rederive]" 2 ;;
esac

lc_load_env "$LC_COMPOSE_ENV"
lc_require_vars LC_SECRETS_DIR LC_SECRETS_GID LC_PG_HOST LC_PG_SSLMODE
[[ -d "$LC_SECRETS_DIR" ]] || lc_die "secrets dir missing: run host-setup.sh first"
[[ "$LC_SECRETS_GID" =~ ^[0-9]+$ ]] || lc_die "LC_SECRETS_GID must be numeric"
[[ "$LC_PG_HOST" =~ ^[A-Za-z0-9.-]{1,253}$ ]] || lc_die "LC_PG_HOST must be a host name or IP"
case "$LC_PG_SSLMODE" in disable | require | verify-ca | verify-full) ;; *) lc_die "invalid LC_PG_SSLMODE" ;; esac
command -v openssl >/dev/null || lc_die "openssl is required"
command -v python3 >/dev/null || lc_die "python3 is required"
umask 077
dir=$LC_SECRETS_DIR

# write_secret NAME VALUE — temp file + rename; group-readable only by LC_SECRETS_GID.
write_secret() {
  local name=$1 value=$2 tmp
  tmp=$(mktemp "$dir/.${name}.XXXXXX")
  printf '%s' "$value" >"$tmp"
  chgrp "$LC_SECRETS_GID" "$tmp"
  chmod 0440 "$tmp"
  mv -f "$tmp" "$dir/$name"
}
read_secret() { cat -- "$dir/$1"; }

gen_hex32() { openssl rand -hex 32; }
gen_b64url32() { openssl rand 32 | base64 -w0 | tr '+/' '-_' | tr -d '='; }
gen_b64std32() { openssl rand -base64 32; }
gen_keyring() { # $1 = account|signing (list of entries) | meta|pagetoken ({"keys":[...]})
  python3 - "$1" <<'PY'
import base64, datetime, json, os, sys
kind = sys.argv[1]
day = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d")
prefix = {"account": "acct-", "signing": "whk-", "meta": "meta-", "pagetoken": "pt-"}[kind]
entry = {"id": prefix + day, "key_base64": base64.b64encode(os.urandom(32)).decode()}
print(json.dumps([entry] if kind in ("account", "signing") else {"keys": [entry]}, separators=(",", ":")), end="")
PY
}
# meta-ads-v1 G3 (ads-graph): HPKE X25519 token custody. The PRIVATE ring is generated here (raw 32-byte
# keys from openssl's PKCS#8 DER, whose last 32 bytes are the key); the PUBLIC ring is DERIVED from it so
# the api (seal) and ads-worker (open) always agree. Formats: metaads.LoadSealKeys / tokenopen.LoadKeyring.
gen_hpke_private_ring() {
  python3 <<'PY'
import base64, datetime, json, subprocess
der = subprocess.run(["openssl", "genpkey", "-algorithm", "X25519", "-outform", "DER"], check=True, capture_output=True).stdout
assert len(der) == 48, "unexpected X25519 PKCS#8 length"
day = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d")
entry = {"id": "ads-hpke-" + day, "private_key_base64": base64.b64encode(der[-32:]).decode()}
print(json.dumps({"keys": [entry]}, separators=(",", ":")), end="")
PY
}
derive_hpke_public_ring() { # $1 = private ring file
  python3 - "$dir/$1" <<'PY'
import base64, json, subprocess, sys
PKCS8_X25519 = bytes.fromhex("302e020100300506032b656e04220420")  # RFC 8410 PrivateKeyInfo prefix
out = []
for e in json.load(open(sys.argv[1]))["keys"]:
    der = subprocess.run(["openssl", "pkey", "-inform", "DER", "-pubout", "-outform", "DER"], check=True,
                         capture_output=True, input=PKCS8_X25519 + base64.b64decode(e["private_key_base64"])).stdout
    assert len(der) == 44, "unexpected X25519 SPKI length"
    out.append({"id": e["id"], "public_key_base64": base64.b64encode(der[-32:]).decode()})
print(json.dumps({"keys": out}, separators=(",", ":")), end="")
PY
}
last_key_id() { # $1 = keyring file name; newest (last) entry id
  python3 - "$dir/$1" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
entries = doc if isinstance(doc, list) else doc["keys"]
print(entries[-1]["id"], end="")
PY
}
keyring_contains() { # $1 = keyring file, $2 = candidate key (stdin-free comparison in python)
  LC_CANDIDATE=$2 python3 - "$dir/$1" <<'PY'
import json, os, sys
doc = json.load(open(sys.argv[1]))
entries = doc if isinstance(doc, list) else doc["keys"]
sys.exit(0 if any(e["key_base64"] == os.environ["LC_CANDIDATE"] for e in entries) else 1)
PY
}
# Every keyring file and every standalone std-base64 key (replay keys, K_actor, K_link) must be
# pairwise distinct: the account, Stripe-signing, Meta-payload and Page-token custodies never
# share key bytes, and the three claims keys rely on domain separation only (meta-claims-intake-v1
# §4.1). Kept in sync with the manifest kinds; preflight P04 re-checks the same inequalities.
KEYRING_FILES=(commerce_account_keys_json commerce_stripe_webhook_keys_json commerce_meta_payload_keys_json commerce_meta_page_token_keys_json)
b64std_used_elsewhere() { # $1 = candidate, $2 = the file being generated
  local f other
  for f in "${KEYRING_FILES[@]}"; do
    [[ -f "$dir/$f" ]] && keyring_contains "$f" "$1" && return 0
  done
  while IFS=$'\t' read -r other kind _rest; do
    [[ "$kind" == b64std32 && "$other" != "$2" && -f "$dir/$other" && "$(read_secret "$other")" == "$1" ]] && return 0
  done <"$LC_DEPLOY_DIR/secrets.manifest.tsv"
  return 1
}
service_of() { # login -> consuming service from logins.tsv (DSN application_name)
  awk -F'\t' -v l="$1" '$1 == l { split($5, a, ":"); print a[1]; exit }' "$LC_DEPLOY_DIR/postgres/logins.tsv"
}

value_for() { # $1 file, $2 kind -> prints a new value
  local file=$1 kind=$2 v other
  case "$kind" in
  hex32) gen_hex32 ;;
  b64url32)
    # Distinct keys: merchant BFF != buyer BFF != buyer cookie key (C8) != claims label key (G2).
    while :; do
      v=$(gen_b64url32)
      for other in commerce_bff_key commerce_buyer_bff_key commerce_buyer_cookie_key commerce_claims_label_key; do
        [[ "$other" != "$file" && -f "$dir/$other" && "$(read_secret "$other")" == "$v" ]] && continue 2
      done
      printf '%s' "$v"
      return
    done
    ;;
  b64std32)
    # Replay/actor/link keys: distinct from every keyring key and from each other (accounts/crypto.go
    # NewKeyring rejects a replay key inside its keyring; the rest is the deployment rule above).
    while :; do
      v=$(gen_b64std32)
      if b64std_used_elsewhere "$v" "$file"; then continue; fi
      printf '%s' "$v"
      return
    done
    ;;
  account_keyring)
    case "$file" in
    commerce_stripe_webhook_keys_json) gen_keyring signing ;;
    *) gen_keyring account ;;
    esac
    ;;
  meta_keyring)
    case "$file" in
    commerce_meta_page_token_keys_json) gen_keyring pagetoken ;;
    *) gen_keyring meta ;;
    esac
    ;;
  key_id)
    case "$file" in
    commerce_account_active_key_id) last_key_id commerce_account_keys_json ;;
    commerce_meta_payload_active_key_id) last_key_id commerce_meta_payload_keys_json ;;
    commerce_stripe_webhook_active_key_id) last_key_id commerce_stripe_webhook_keys_json ;;
    commerce_meta_page_token_active_key_id) last_key_id commerce_meta_page_token_keys_json ;;
    commerce_meta_ads_hpke_active_key_id) last_key_id commerce_meta_ads_hpke_public_keys_json ;;
    *) lc_die "no key_id rule for $file" ;;
    esac
    ;;
  hpke_private_ring) gen_hpke_private_ring ;;
  *) lc_die "no generator for kind $kind ($file)" ;;
  esac
}

derive_for() { # $1 file, $2 kind -> prints the derived DSN
  local file=$1 kind=$2 login pw svc extra=""
  case "$kind" in
  dsn_socket)
    pw=$(read_secret pg_superuser_password)
    printf 'host=/var/run/postgresql dbname=live_commerce user=postgres password=%s sslmode=disable application_name=lc-migrate' "$pw"
    ;;
  dsn_tcp)
    login=${file#dsn_}
    [[ -f "$dir/pw_$login" ]] || lc_die "pw_$login missing (manifest order)"
    pw=$(read_secret "pw_$login")
    svc=$(service_of "$login")
    [[ -n "$svc" ]] || lc_die "login $login not in logins.tsv"
    [[ "$LC_PG_SSLMODE" == verify-full || "$LC_PG_SSLMODE" == verify-ca ]] && extra="&sslrootcert=/run/secrets/pg_ca_crt"
    printf 'postgres://%s:%s@%s:5432/live_commerce?sslmode=%s&application_name=%s%s' \
      "$login" "$pw" "$LC_PG_HOST" "$LC_PG_SSLMODE" "$svc" "$extra"
    ;;
  hpke_public_ring)
    [[ -f "$dir/commerce_meta_ads_hpke_private_keys_json" ]] || lc_die "commerce_meta_ads_hpke_private_keys_json missing (manifest order)"
    derive_hpke_public_ring commerce_meta_ads_hpke_private_keys_json
    ;;
  *) lc_die "no derivation for kind $kind ($file)" ;;
  esac
}

created=0 kept=0 rederived=0
while IFS=$'\t' read -r file kind generator _consumers _rotation; do
  [[ -z "$file" || "$file" == \#* ]] && continue
  [[ "$file" =~ ^[a-z0-9_]{1,64}$ ]] || lc_die "invalid manifest file name"
  if [[ -f "$dir/$file" ]]; then
    if [[ "$generator" == derive && $rederive == 1 ]]; then
      write_secret "$file" "$(derive_for "$file" "$kind")"
      printf 'secret %-40s rederived\n' "$file"
      rederived=$((rederived + 1))
    else
      printf 'secret %-40s kept\n' "$file"
      kept=$((kept + 1))
    fi
    continue
  fi
  case "$generator" in
  gen) write_secret "$file" "$(value_for "$file" "$kind")" ;;
  derive) write_secret "$file" "$(derive_for "$file" "$kind")" ;;
  owner) write_secret "$file" "__UNSET__" ;;
  *) lc_die "unknown generator $generator ($file)" ;;
  esac
  printf 'secret %-40s created%s\n' "$file" "$([[ $generator == owner ]] && echo ' (__UNSET__: owner must supply)')"
  created=$((created + 1))
done <"$LC_DEPLOY_DIR/secrets.manifest.tsv"
lc_info "secrets dir=${dir} created=$created kept=$kept rederived=$rederived"
