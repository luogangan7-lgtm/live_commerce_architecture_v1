#!/usr/bin/env bash
# File: deploy/scripts/ops-admin.sh
# Purpose: the ONLY sanctioned way to run the operator CLIs (stripe-admin, meta-admin) against a
#   deployed stack: a one-shot container of profile `ops` that holds the registrar logins and the
#   operator's own inputs, never one of the app containers (api/admin/storefront/workers hold neither).
#   Prints the CLI's JSON result line (IDs and versions only); never prints a secret.
# Usage: ops-admin.sh stripe-admin register|rotate|webhook|qualify|method [flags]   (stripe-psp-v1 §13)
#        ops-admin.sh stripe-admin live-approve|live-canary|live-revoke [flags]     (stripe-live-enable-v1 §5.2, §7)
#        ops-admin.sh meta-admin page-token|route|route-disable [flags]             (meta-claims-intake-v1 §7, R1 F2)
#   Operator inputs are read from the caller's environment, from a file named by NAME_FILE (O-D, LD3:
#   a live key only ever arrives by file path) or, on a terminal, prompted WITHOUT echo:
#     stripe-admin register|rotate : STRIPE_SECRET_KEY[_FILE] (SANDBOX: sk_test_/rk_test_; LIVE pair set:
#                                    rk_live_ only; sk_live_ always refused), STRIPE_ACCOUNT_ID
#     stripe-admin webhook         : STRIPE_WEBHOOK_SECRET[_FILE] [, STRIPE_WEBHOOK_SECRET_NEXT[_FILE]]  (whsec_...)
#     stripe-admin qualify SANDBOX : STRIPE_SECRET_KEY, STRIPE_ACCOUNT_ID, STRIPE_SANDBOX=1
#     stripe-admin qualify LIVE    : none (stored key; optional STRIPE_SECRET_KEY[_FILE] + STRIPE_ACCOUNT_ID assertions)
#     stripe-admin live-*|method   : none (the keyring is mounted by compose; no secret input)
#     meta-admin page-token        : META_PAGE_ACCESS_TOKEN[_FILE]
#     meta-admin route|route-disable: none (ids, --proof digest and epochs are flags)
#   LIVE gate (stripe-live-enable-v1 §5.2, O3): `--profile LIVE` / `--environment LIVE`, live-approve and
#   live-canary need the pair LC_STRIPE_LIVE_ENABLED=1 + LC_STRIPE_LIVE_APPROVAL_REF in compose.env (mapped to
#   COMMERCE_STRIPE_LIVE_* and forwarded by NAME). live-revoke and `method` never need it (kill switch, LD6).
# Runs as/in: deploy host (root or a docker-group user), through lib.sh lc_compose_with_ops.
# Reads env: compose.env, and the input variables above (forwarded into the one-shot container by
#   NAME only, so the value is never in argv, `ps` or a file; it exists in that container's config
#   until `--rm` removes it, readable only by host root, exactly like the secret files).
# Reads secrets: none directly; the container mounts its own DSN + keyrings (compose.yml).
# Used by: docs/runbooks/deploy.md §Stripe / §Meta, docs/runbooks/merchant-onboarding.md; smoke S44.
# Depends on: compose services stripe-admin / meta-admin, a running postgres with migrations and
#   provisioned logins (deploy.sh first).
# Status: DESIGN; the registrar-login and container wiring is verified by smoke S13 and S44, a real
#   SANDBOX registration is owner-run (NOT_RUN in CI: needs the owner's Stripe test key).
# Change rules: keep the allowlists below equal to the CLIs' subcommands; admit a live key or LIVE only
#   with the valid pair, never `sk_live_` (the CLIs refuse in code too; this is the earlier, clearer
#   failure); a `_FILE` input is read here and never echoed; the kill switch (live-revoke, method) must
#   never depend on the deploy env; never add `set -x`; never echo a forwarded variable or a file path's content.
set -Eeuo pipefail
# LD3/O-D: a caller-exported SHELLOPTS=xtrace must not trace the secret read below.
{ set +x; } 2>/dev/null
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  lc_die "usage: ops-admin.sh stripe-admin register|rotate|webhook|qualify|method|live-approve|live-canary|live-revoke [flags] | meta-admin page-token|route|route-disable [flags]" 2
}
tool=${1:-}
sub=${2:-}
[[ -n "$tool" && -n "$sub" ]] || usage
shift 2
args=("$@")
case "$tool:$sub" in
# §5.2: the three live-* subcommands take no secret input, so none gets a `need` arm below.
stripe-admin:register | stripe-admin:rotate | stripe-admin:webhook | stripe-admin:qualify | stripe-admin:method) ;;
stripe-admin:live-approve | stripe-admin:live-canary | stripe-admin:live-revoke) ;;
meta-admin:page-token | meta-admin:route | meta-admin:route-disable) ;;
*) usage ;;
esac

lc_load_env "$LC_COMPOSE_ENV"

# O3/§5.2: the flag+ref pair comes from compose.env (LC_STRIPE_LIVE_*), is mapped to COMMERCE_STRIPE_LIVE_*
# and forwarded by NAME; a COMMERCE_STRIPE_LIVE_* value in the caller's env is never trusted.
pair_re='^[A-Za-z0-9._:-]{8,128}$'
live_flag=${LC_STRIPE_LIVE_ENABLED:-}
live_ref=${LC_STRIPE_LIVE_APPROVAL_REF:-}
pair_ok=0
if [[ "$live_flag" == 1 && "$live_ref" =~ $pair_re ]]; then pair_ok=1; fi
unset COMMERCE_STRIPE_LIVE_ENABLED COMMERCE_STRIPE_LIVE_APPROVAL_REF

# Which invocations need the pair: LIVE named in the flags (any case), and the two live-* subcommands that
# spend the live key. live-revoke and `method` (any flags) are the kill switch and never need it (LD6, §7).
live_needed=0
case "$tool:$sub" in
stripe-admin:live-approve | stripe-admin:live-canary) live_needed=1 ;;
stripe-admin:register | stripe-admin:rotate | stripe-admin:webhook | stripe-admin:qualify)
  for ((i = 0; i < ${#args[@]}; i++)); do
    case "${args[$i]^^}" in
    --PROFILE=LIVE | -PROFILE=LIVE | --ENVIRONMENT=LIVE | -ENVIRONMENT=LIVE) live_needed=1 ;;
    --PROFILE | -PROFILE | --ENVIRONMENT | -ENVIRONMENT) [[ "${args[$((i + 1))]:-}" == [Ll][Ii][Vv][Ee] ]] && live_needed=1 ;;
    esac
  done
  ;;
esac
# r2 close: register/rotate default to --environment SANDBOX in the CLI, and on a pair host the key check above only
# admits rk_live_; without this the key-compromise rotation would die inside the container (stripe_key_mode_mismatch).
case "$tool:$sub" in
stripe-admin:register | stripe-admin:rotate)
  if ((pair_ok && !live_needed)); then
    lc_die "stripe_live_environment_required: this host has the LIVE pair, so stripe-admin $sub needs --environment LIVE (the default is SANDBOX and refuses an rk_live_ key)"
  fi
  ;;
esac
if ((live_needed && !pair_ok)); then
  lc_die "stripe_live_refused: LIVE is refused without LC_STRIPE_LIVE_ENABLED=1 and LC_STRIPE_LIVE_APPROVAL_REF (8-128 chars of A-Za-z0-9._:-) in compose.env (stripe-live-enable-v1 §5.2)"
fi

# need NAME secret|plain REGEX HINT — require the variable: from NAME, else from the file named by NAME_FILE
# (secret kind only), else prompt (no echo) when on a terminal.
# LD3/O-D: the file value is never echoed; every error names the variable only.
read_secret_file() { # NAME — sets v from ${NAME}_FILE
  local fvar="${1}_FILE" file owner mode extra fd
  file=${!fvar}
  [[ "$file" == /* ]] || lc_die "$fvar rejected: path must be absolute"
  [[ -f "$file" && ! -L "$file" ]] || lc_die "$fvar rejected: not a regular file (symlinks are refused)"
  owner=$(stat -c %u -- "$file" 2>/dev/null) || lc_die "$fvar rejected: cannot stat"
  [[ "$owner" == "$(id -u)" ]] || lc_die "$fvar rejected: owner is not the invoking uid"
  mode=$(stat -c %a -- "$file" 2>/dev/null) || lc_die "$fvar rejected: cannot stat"
  [[ "$mode" == 400 || "$mode" == 600 ]] || lc_die "$fvar rejected: mode must be 0400 or 0600"
  exec {fd}<"$file" || lc_die "$fvar rejected: not readable"
  v=
  IFS= read -r v <&"$fd" || [[ -n "$v" ]] || lc_die "$fvar rejected: empty"
  # exactly one line: any further byte (a second line, even a blank one) is refused
  # shellcheck disable=SC2034 # the byte itself is irrelevant; only whether one exists
  if IFS= read -r -N 1 extra <&"$fd"; then lc_die "$fvar rejected: must contain exactly one line"; fi
  exec {fd}<&-
}
need() {
  local name=$1 kind=$2 re=$3 hint=$4 v fvar="${1}_FILE"
  if [[ "$kind" == secret && -n "${!fvar:-}" ]]; then
    [[ -z "${!name:-}" ]] || lc_die "$name and $fvar are both set; give one"
    read_secret_file "$name"
    export "$name=$v"
  elif [[ -z "${!name:-}" ]]; then
    [[ -t 0 ]] || lc_die "$name is required ($hint); export it, set $fvar, or run from a terminal to be prompted"
    if [[ "$kind" == secret ]]; then
      read -r -s -p "$name ($hint): " v
      echo >&2
    else
      read -r -p "$name ($hint): " v
    fi
    export "$name=$v"
  fi
  v=${!name}
  # LR-3: an unrestricted live key is never admitted, with or without the pair (LD3).
  if [[ "$name" == STRIPE_SECRET_KEY && "$v" == sk_live_* ]]; then
    lc_die "$name rejected: stripe_live_key_unrestricted (only a restricted rk_live_ key is admitted)"
  fi
  # ERE bounds stop at 255 (RE_DUP_MAX), so the upper length limit is checked separately.
  [[ "$v" =~ $re && ${#v} -le 4096 ]] || lc_die "$name has an unexpected format ($hint)"
  unset v
  forward+=(-e "$name")
}

# O2: the key mode follows the pair; SANDBOX admits sk_/rk_test_, a LIVE host (pair set) admits rk_live_ only.
if ((pair_ok)); then
  key_re='^rk_live_[A-Za-z0-9_]{8,}$'
  key_hint="restricted Stripe LIVE key rk_live_...; sk_live_ and test keys are refused here"
else
  key_re='^(sk|rk)_test_[A-Za-z0-9_]{8,}$'
  key_hint="Stripe TEST key sk_test_/rk_test_; live keys are refused"
fi

forward=()
# O3: the pair reaches every stripe-admin run by NAME when valid (compose.yml does not wire it into the
# stripe-admin container); an invalid or absent pair is simply not forwarded, so the kill switch works either way.
if [[ "$tool" == stripe-admin ]] && ((pair_ok)); then
  export COMMERCE_STRIPE_LIVE_ENABLED=1 COMMERCE_STRIPE_LIVE_APPROVAL_REF="$live_ref"
  forward+=(-e COMMERCE_STRIPE_LIVE_ENABLED -e COMMERCE_STRIPE_LIVE_APPROVAL_REF)
fi
case "$tool:$sub" in
stripe-admin:register | stripe-admin:rotate)
  need STRIPE_SECRET_KEY secret "$key_re" "$key_hint"
  need STRIPE_ACCOUNT_ID plain '^acct_[A-Za-z0-9]{6,64}$' "acct_..."
  ;;
stripe-admin:webhook)
  need STRIPE_WEBHOOK_SECRET secret '^whsec_[A-Za-z0-9_+/=-]{8,}$' "signing secret whsec_... of the endpoint"
  if [[ -n "${STRIPE_WEBHOOK_SECRET_NEXT:-}" || -n "${STRIPE_WEBHOOK_SECRET_NEXT_FILE:-}" ]]; then
    need STRIPE_WEBHOOK_SECRET_NEXT secret '^whsec_[A-Za-z0-9_+/=-]{8,}$' "next signing secret whsec_..."
  fi
  ;;
stripe-admin:qualify)
  # The CLI decides SANDBOX/LIVE from --profile. LIVE uses the stored key (no input needed); SANDBOX
  # opts in with STRIPE_SANDBOX=1. A key given here is only an assertion against the stored one.
  if [[ -n "${STRIPE_SECRET_KEY:-}" || -n "${STRIPE_SECRET_KEY_FILE:-}" ]]; then
    need STRIPE_SECRET_KEY secret "$key_re" "$key_hint"
    need STRIPE_ACCOUNT_ID plain '^acct_[A-Za-z0-9]{6,64}$' "acct_..."
    if ((!live_needed)); then
      [[ "${STRIPE_SANDBOX:-}" == 1 ]] || lc_die "STRIPE_SANDBOX=1 is required (qualify creates and expires a real sandbox Checkout Session)"
      forward+=(-e STRIPE_SANDBOX)
    fi
  fi
  ;;
meta-admin:page-token)
  need META_PAGE_ACCESS_TOKEN secret '^[^[:space:]]{16,}$' "Page access token; scopes are attested by --scopes"
  ;;
esac

# `run` REPLACES the service command, so the binary is named again. --no-deps: never start
# postgres/migrate from here (deploy.sh owns ordering). -T: no TTY, the prompt above already ran.
lc_info "ops-admin $tool $sub (one-shot container, profile ops)"
rc=0
lc_compose_with_ops run --rm --no-deps -T "${forward[@]}" "$tool" "/app/bin/$tool" "$sub" "${args[@]}" || rc=$?
unset STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET STRIPE_WEBHOOK_SECRET_NEXT META_PAGE_ACCESS_TOKEN COMMERCE_STRIPE_LIVE_APPROVAL_REF
# Audit trail: who ran which subcommand and how it ended. No flag values (they may name tenants).
if [[ -n "${LC_STATE_DIR:-}" && -d "${LC_STATE_DIR:-}" && -w "${LC_STATE_DIR:-}" ]]; then
  printf '%s ops-admin operator=%s tool=%s sub=%s live_pair=%s exit=%s\n' "$(lc_ts)" "${SUDO_USER:-${USER:-unknown}}" "$tool" "$sub" "$pair_ok" "$rc" \
    >>"$LC_STATE_DIR/ops-admin.log" || true
fi
exit "$rc"
