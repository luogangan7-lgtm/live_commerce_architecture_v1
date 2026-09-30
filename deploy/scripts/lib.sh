#!/usr/bin/env bash
# File: deploy/scripts/lib.sh
# Purpose: shared helpers for every deploy/scripts/*.sh (sourced, never executed):
#   logging that names things but never prints values, a safe KEY=VALUE env-file loader plus a
#   file-only getter/atomic setter (deploy.sh keeps compose.env IMAGE_TAG = deployed tag),
#   the lc_compose wrapper (fixed project dir + env file), a superuser psql helper that runs
#   inside the postgres container over the unix socket, and lc_secret_scan.
# Runs as/in: the deploy host (root for real deployments; any user with docker access for
#   smoke). Requires bash >= 4.4, docker compose >= 2.24, python3 (secret scan).
# Reads env: LC_CONFIG_DIR (default /etc/live-commerce), LC_COMPOSE_ENV (default
#   $LC_CONFIG_DIR/compose.env), LC_TWO_HOST (1 = add compose.two-host-db.yml), and after
#   lc_load_env everything in compose.env (LC_SECRETS_DIR, LC_STATE_DIR, ...).
# Reads secrets: lc_secret_scan reads every file in $LC_SECRETS_DIR (in memory only) to look
#   for leaks; lc_psql makes the postgres container read /run/secrets/pg_superuser_password.
# Used by: host-setup.sh, secrets-init.sh, preflight.sh, build-images.sh, check-pins.sh,
#   deploy.sh, pg-ops.sh, smoke.sh, watchdog.sh, collect-diagnostics.sh.
# Depends on: deploy/compose.yml (service names), deploy/compose.two-host-db.yml.
# Status: DESIGN; exercised by smoke S01 (syntax) and every runtime case.
# Change rules: never add `set -x`; never echo a variable that may hold a secret; keep
#   lc_load_env semantics aligned with Compose (existing environment wins over the file).

if [[ -n "${LC_LIB_LOADED:-}" ]]; then return 0; fi
LC_LIB_LOADED=1

LC_SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LC_DEPLOY_DIR="$(cd "$LC_SCRIPTS_DIR/.." && pwd)"
LC_REPO_ROOT="$(cd "$LC_DEPLOY_DIR/.." && pwd)"
: "${LC_CONFIG_DIR:=/etc/live-commerce}"
: "${LC_COMPOSE_ENV:=$LC_CONFIG_DIR/compose.env}"
LC_SCRIPT_NAME="${LC_SCRIPT_NAME:-$(basename "$0")}"
export LC_SCRIPTS_DIR LC_DEPLOY_DIR LC_REPO_ROOT LC_CONFIG_DIR LC_COMPOSE_ENV

# Core services with no long-running process (excluded from health/watchdog loops).
LC_ONESHOT_SERVICES=(migrate provision-logins pg-ops stripe-admin meta-admin)

lc_ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
lc_log() {
  local level=$1
  shift
  printf '%s %s %s: %s\n' "$(lc_ts)" "$LC_SCRIPT_NAME" "$level" "$*" >&2
}
lc_info() { lc_log INFO "$*"; }
lc_warn() { lc_log WARN "$*"; }
lc_error() { lc_log ERROR "$*"; }
lc_die() {
  lc_log FATAL "$1"
  exit "${2:-1}"
}

# lc_load_env FILE — parse KEY=VALUE lines (Compose env-file subset) and export them.
# Variables already present in the environment win, exactly like Compose interpolation.
lc_load_env() {
  local file=$1 line key val
  [[ -r "$file" ]] || lc_die "env file not readable: $file (run host-setup.sh / copy the template)"
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
    if [[ ! "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
      lc_die "unparsable line in ${file##*/} (expected KEY=VALUE)"
    fi
    key=${BASH_REMATCH[1]}
    val=${BASH_REMATCH[2]}
    if [[ "$val" =~ ^\"(.*)\"$ || "$val" =~ ^\'(.*)\'$ ]]; then val=${BASH_REMATCH[1]}; fi
    if [[ -z "${!key+x}" ]]; then export "$key=$val"; fi
  done <"$file"
}

# lc_env_file_get FILE KEY — print the value of the first KEY= line of FILE (quotes stripped like
# lc_load_env); exit 1 when absent. Reads ONLY the file, never the process environment: used to
# compare what compose.env says with what runs (deploy.sh, watchdog W10, smoke S43).
lc_env_file_get() {
  local file=$1 key=$2 line val
  [[ -r "$file" ]] || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ "$line" == "$key="* ]] || continue
    val=${line#*=}
    if [[ "$val" =~ ^\"(.*)\"$ || "$val" =~ ^\'(.*)\'$ ]]; then val=${BASH_REMATCH[1]}; fi
    printf '%s\n' "$val"
    return 0
  done <"$file"
  return 1
}

# lc_env_file_set FILE KEY VALUE — atomically make FILE contain exactly one KEY=VALUE line: the
# first KEY= line is rewritten in place, later duplicates are dropped, and the line is appended
# when absent. Temp file in the same directory (same filesystem), owner and mode copied from
# FILE, fsync, rename; a symlinked FILE is resolved first. VALUE must be a plain token (no
# newline); never use it for secrets. Used by deploy.sh (IMAGE_TAG), smoke.sh S43.
lc_env_file_set() {
  local file key=$2 value=$3 tmp line found=0
  file=$(readlink -f -- "$1") || lc_die "lc_env_file_set: cannot resolve $1"
  [[ -f "$file" ]] || lc_die "lc_env_file_set: $file is not a regular file"
  [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || lc_die "lc_env_file_set: invalid key"
  [[ "$value" =~ ^[A-Za-z0-9._:/@+-]*$ ]] || lc_die "lc_env_file_set: $key value must be a plain token"
  tmp=$(mktemp "$(dirname -- "$file")/.${file##*/}.XXXXXX")
  {
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "${line%$'\r'}" == "$key="* ]]; then
        ((found)) && continue
        printf '%s=%s\n' "$key" "$value"
        found=1
      else
        printf '%s\n' "$line"
      fi
    done <"$file"
    ((found)) || printf '%s=%s\n' "$key" "$value"
  } >"$tmp"
  chown --reference="$file" -- "$tmp" 2>/dev/null || lc_warn "could not copy the owner of ${file##*/} (not root?)"
  chmod --reference="$file" -- "$tmp"
  sync -- "$tmp"
  mv -f -- "$tmp" "$file"
}

# lc_require_vars NAME... — fail naming the first unset/empty variable.
lc_require_vars() {
  local name
  for name in "$@"; do
    [[ -n "${!name:-}" ]] || lc_die "required variable $name is not set (compose.env)"
  done
}

# lc_compose ARGS... — the only way scripts talk to Compose.
lc_compose() {
  local files=(-f "$LC_DEPLOY_DIR/compose.yml")
  if [[ "${LC_TWO_HOST:-0}" == 1 ]]; then files+=(-f "$LC_DEPLOY_DIR/compose.two-host-db.yml"); fi
  docker compose --project-directory "$LC_DEPLOY_DIR" --env-file "$LC_COMPOSE_ENV" "${files[@]}" "$@"
}

# NOTE: a CLI `--profile X` REPLACES COMPOSE_PROFILES instead of adding to it. Use these
# helpers instead of passing --profile directly, or `down` would only see profile X services.
# lc_compose_with_ops ARGS... — current profiles + ops (pg-ops runs next to the live stack).
lc_compose_with_ops() { COMPOSE_PROFILES="${COMPOSE_PROFILES:+$COMPOSE_PROFILES,}ops" lc_compose "$@"; }
# lc_compose_all ARGS... — every profile (teardown of a whole project, e.g. smoke).
lc_compose_all() { COMPOSE_PROFILES="db,app,payments-sandbox,payments-live,meta,claims,ads,ops" lc_compose "$@"; }

# lc_active_services — services enabled by COMPOSE_PROFILES (one per line).
lc_active_services() { lc_compose config --services 2>/dev/null; }

# lc_long_running_services — active services minus the one-shot jobs.
lc_long_running_services() {
  local s o skip
  while IFS= read -r s; do
    skip=0
    for o in "${LC_ONESHOT_SERVICES[@]}"; do [[ "$s" == "$o" ]] && skip=1; done
    [[ $skip == 0 && -n "$s" ]] && printf '%s\n' "$s"
  done < <(lc_active_services)
}

# lc_service_active NAME — true if NAME is enabled by the current profiles.
# Never `producer | grep -q` under pipefail: grep -q exits at the first match, the producer gets
# SIGPIPE (141) and the whole pipeline reports failure although it matched. Read via < <(...)
# so only grep's status counts (VERIFIED_LOCAL: watchdog skipped DB checks because of this).
lc_service_active() { grep -qx -- "$1" < <(lc_active_services); }

# lc_psql — run SQL from stdin as the superuser inside the running postgres container, over
# the unix socket. Output: unaligned, tuples only, '|' separated. The SQL text is never logged.
lc_psql() {
  lc_compose exec -T postgres bash -c \
    'PGPASSWORD="$(< /run/secrets/pg_superuser_password)" exec psql -X -q -At -F "|" -v ON_ERROR_STOP=1 -h /var/run/postgresql -U postgres -d live_commerce'
}

# lc_ledger_count — rows in the checksummed migration ledger (migrations/migrate.go).
lc_ledger_count() {
  lc_psql <<<"SELECT CASE WHEN to_regclass('public.lc_schema_migrations') IS NULL THEN 0 ELSE (SELECT count(*) FROM public.lc_schema_migrations) END;"
}

# lc_git_tag — image tag: 12-char commit + "-dirty" when the checkout has local changes.
lc_git_tag() {
  local sha
  sha="$(git -C "$LC_REPO_ROOT" rev-parse --short=12 HEAD)"
  if [[ -n "$(git -C "$LC_REPO_ROOT" status --porcelain 2>/dev/null)" ]]; then sha+="-dirty"; fi
  printf '%s\n' "$sha"
}

# lc_secret_scan TARGET... — exit 1 (and name secret FILE + target file, never the value) if
# any secret value of >= 16 chars (or any >= 16-char string inside a JSON secret, or the
# password part of a DSN) appears in the target files/directories.
lc_secret_scan() {
  lc_require_vars LC_SECRETS_DIR
  python3 - "$LC_SECRETS_DIR" "$@" <<'PY'
import json, os, re, sys
from urllib.parse import urlsplit
sdir, targets = sys.argv[1], sys.argv[2:]
needles = {}
def add(value, name):
    if isinstance(value, str):
        value = value.encode()
    value = value.strip()
    if len(value) >= 16 and value != b"__UNSET__":
        needles[value] = name
for name in sorted(os.listdir(sdir)):
    path = os.path.join(sdir, name)
    if not os.path.isfile(path) or name.startswith("."):
        continue
    raw = open(path, "rb").read()
    add(raw, name)
    text = raw.decode("utf-8", "replace").strip()
    try:
        stack = [json.loads(text)]
        while stack:
            node = stack.pop()
            if isinstance(node, dict):
                stack.extend(node.values())
            elif isinstance(node, list):
                stack.extend(node)
            elif isinstance(node, str):
                add(node, name)
    except ValueError:
        pass
    if text.startswith("postgres://"):
        pw = urlsplit(text).password
        if pw:
            add(pw, name)
    m = re.search(r"password=(\S+)", text)
    if m:
        add(m.group(1), name)
hits = 0
def scan(path):
    global hits
    try:
        data = open(path, "rb").read()
    except OSError:
        return
    for needle, name in needles.items():
        if needle in data:
            hits += 1
            print(f"secret-scan HIT secret={name} file={path}", file=sys.stderr)
for target in targets:
    if os.path.isdir(target):
        for root, _, files in os.walk(target):
            for f in files:
                scan(os.path.join(root, f))
    else:
        scan(target)
print(f"secret-scan needles={len(needles)} hits={hits}", file=sys.stderr)
sys.exit(1 if hits else 0)
PY
}
