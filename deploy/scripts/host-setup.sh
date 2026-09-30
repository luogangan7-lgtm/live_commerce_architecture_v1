#!/usr/bin/env bash
# File: deploy/scripts/host-setup.sh
# Purpose: one-time (idempotent) host preparation (deploy-design §10.2, §14): tool/version
#   checks, the secrets group (GID 10500 by default), config/secrets/state/backup directories
#   with exact owners and modes, env templates copied in when missing (never overwritten),
#   time-sync check, and a PRINTED recommended /etc/docker/daemon.json (never modified).
# Usage: host-setup.sh [--prefix DIR] [--backup-dir DIR] [--gid N] [--check-only]
#   --prefix creates the same tree under DIR (used by smoke and dry runs; skips groupadd and,
#   when not root, chown).
# Runs as/in: deploy host as root (real use). Reads env: none required.
# Reads secrets: none (creates the empty secrets dir only; secrets-init.sh fills it).
# Used by: docs/runbooks/deploy.md §主机准备; smoke.sh (directory logic via --prefix).
# Depends on: coreutils, getent/groupadd (shadow-utils), docker, python3, openssl, curl.
# Status: DESIGN; runbook-reviewed; directory logic exercised by smoke S09.
# Change rules: modes are part of the security model (preflight P02 checks them):
#   secrets dir 0750 root:GID, files 0440; backup dirs 0700 999:999 (postgres UID in the image).
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

prefix="" backup="" gid=10500 check_only=0
while (($#)); do
  case "$1" in
  --prefix)
    prefix=${2:?}
    shift 2
    ;;
  --backup-dir)
    backup=${2:?}
    shift 2
    ;;
  --gid)
    gid=${2:?}
    shift 2
    ;;
  --check-only) check_only=1 && shift ;;
  *) lc_die "usage: host-setup.sh [--prefix DIR] [--backup-dir DIR] [--gid N] [--check-only]" 2 ;;
  esac
done
[[ "$gid" =~ ^[0-9]+$ ]] || lc_die "--gid must be numeric"
is_root=0
[[ $EUID == 0 ]] && is_root=1
[[ -n "$prefix" || $is_root == 1 ]] || lc_die "run as root (or use --prefix for a dry run)"

# ---- tools and versions -----------------------------------------------------------------------
fail=0
check() { if "$@" >/dev/null 2>&1; then printf 'check %-28s PASS\n' "$CHECK"; else printf 'check %-28s FAIL\n' "$CHECK"; fail=1; fi; }
ver_ge() { [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]; }
for tool in docker python3 openssl curl getent sha256sum; do CHECK="tool:$tool" check command -v "$tool"; done
docker_v=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo 0)
compose_v=$(docker compose version --short 2>/dev/null | sed 's/^v//' || echo 0)
CHECK="docker>=25 ($docker_v)" check ver_ge "$docker_v" 25.0.0
CHECK="compose>=2.24 ($compose_v)" check ver_ge "$compose_v" 2.24.0
CHECK="bash>=4.4 ($BASH_VERSION)" check ver_ge "${BASH_VERSION%%(*}" 4.4
if command -v timedatectl >/dev/null 2>&1; then
  sync=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo unknown)
  if [[ "$sync" == yes ]]; then echo "check time-sync                   PASS"; else echo "check time-sync                   WARN ($sync; enable chrony/systemd-timesyncd)"; fi
else
  echo "check time-sync                   WARN (timedatectl not found; verify NTP manually)"
fi
((check_only)) && exit "$fail"

# ---- group, directories, templates ------------------------------------------------------------
if [[ -z "$prefix" ]] && ! getent group "$gid" >/dev/null; then
  groupadd --system --gid "$gid" lcsecrets
  lc_info "created group lcsecrets gid=$gid"
fi
config="$prefix/etc/live-commerce"
state="$prefix/var/lib/live-commerce"
backup=${backup:-$prefix/var/backups/live-commerce}
mkdirp() { # mode owner group path
  mkdir -p "$4"
  chmod "$1" "$4"
  if ((is_root)); then chown "$2:$3" "$4"; fi
}
mkdirp 0750 0 0 "$config"
mkdirp 0750 0 0 "$config/env"
mkdirp 0750 0 "$gid" "$config/secrets"
mkdirp 0750 0 0 "$state"
for d in "$backup" "$backup/dumps" "$backup/base" "$backup/wal"; do mkdirp 0700 999 999 "$d"; done
touch "$state/deployments.log" && chmod 0640 "$state/deployments.log"

install_template() { # src dst
  if [[ -e "$2" ]]; then
    printf 'template %-40s kept\n' "${2#"$prefix"}"
  else
    cp -- "$1" "$2"
    chmod 0640 "$2"
    printf 'template %-40s installed\n' "${2#"$prefix"}"
  fi
}
install_template "$LC_DEPLOY_DIR/env/compose.env.example" "$config/compose.env"
for svc in api admin storefront payment-worker expiry-worker meta-worker claims-worker ads-worker caddy postgres; do
  install_template "$LC_DEPLOY_DIR/env/$svc.env.example" "$config/env/$svc.env"
done

cat <<EOF

Next steps (docs/runbooks/deploy.md):
  1. Edit $config/compose.env and $config/env/*.env (hosts, IMAGE_TAG, flags, OIDC client id).
     Set LC_BACKUP_DIR=$backup if you used --backup-dir.
  2. deploy/scripts/secrets-init.sh, then supply owner secrets (commerce_oidc_client_secret,
     commerce_meta_apps_json) by replacing __UNSET__ files (keep 0440 root:$gid).
  3. deploy/scripts/preflight.sh --online

Recommended /etc/docker/daemon.json (NOT applied automatically; merge by hand, then restart docker):
  {"log-driver":"json-file","log-opts":{"max-size":"20m","max-file":"5","compress":"true"},"live-restore":true}
  Put Docker's data-root on the data disk and LC_BACKUP_DIR on a separate disk.

Firewall: allow 22/tcp (admin source only), 80/tcp, 443/tcp, 443/udp. Docker-published ports
bypass ufw/firewalld INPUT rules: restrict with the DOCKER-USER chain or LC_BIND_ADDR.
EOF
exit "$fail"
