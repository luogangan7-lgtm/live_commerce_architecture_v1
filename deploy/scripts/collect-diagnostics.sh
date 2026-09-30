#!/usr/bin/env bash
# File: deploy/scripts/collect-diagnostics.sh
# Purpose: build an incident bundle (deploy-design §16.4): compose ps, image ids/tags, per-service
#   log tails for the window, container state/health/restarts, docker stats, disk, watchdog
#   output, pg_stat_activity grouped by application_name/state/wait_event (NO query text),
#   ledger count, Caddy certificate list. Every file is secret-scanned (lc_secret_scan) BEFORE
#   it is archived; any hit aborts and deletes the bundle.
# Usage: collect-diagnostics.sh [--since 2h]
# Runs as/in: deploy host (root). Output: ${LC_DIAG_DIR:-/var/tmp}/lc-diag-<UTC>.tar.gz (0600).
#   The bundle can contain client IPs and hostnames (access logs): share it only with people
#   handling the incident (owner decision O8 on retention).
# Reads env: compose.env, LC_DIAG_DIR. Reads secrets: LC_SECRETS_DIR values in memory only (scan).
# Used by: docs/runbooks/incident.md §分诊; deploy.sh failure hint; smoke.sh S36.
# Depends on: lib.sh, watchdog.sh, docker, tar, python3.
# Status: DESIGN; verified by smoke S36 (bundle built, secret scan passes).
# Change rules: never add `docker inspect` full output (env/labels), query text or row values.
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

since=2h
while (($#)); do
  case "$1" in
  --since)
    since=${2:?}
    shift 2
    ;;
  *) lc_die "usage: collect-diagnostics.sh [--since 2h]" 2 ;;
  esac
done
[[ "$since" =~ ^[0-9]+[smhd]$ ]] || lc_die "--since must look like 30m, 2h, 1d"
lc_load_env "$LC_COMPOSE_ENV"
umask 077
out_dir=${LC_DIAG_DIR:-/var/tmp}
stamp=$(date -u +%Y%m%dT%H%M%SZ)
work=$(mktemp -d "$out_dir/lc-diag-$stamp.XXXXXX")
trap 'rm -rf "$work"' EXIT

{
  echo "collected_at=$(lc_ts) since=$since project=${COMPOSE_PROJECT_NAME:-} image_tag=${IMAGE_TAG:-}"
  echo "profiles=${COMPOSE_PROFILES:-}"
  docker version --format 'docker={{.Server.Version}}' 2>/dev/null || true
  docker compose version --short 2>/dev/null | sed 's/^/compose=/' || true
} >"$work/summary.txt"
lc_compose ps -a >"$work/compose-ps.txt" 2>&1 || true
lc_compose images >"$work/compose-images.txt" 2>&1 || true
while IFS= read -r s; do
  lc_compose logs --no-color --timestamps --since "$since" "$s" >"$work/logs-$s.txt" 2>&1 || true
  cid=$(lc_compose ps -a -q "$s" 2>/dev/null || true)
  [[ -n "$cid" ]] && docker inspect -f \
    '{{.Name}} image={{.Config.Image}} status={{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}} restarts={{.RestartCount}} started={{.State.StartedAt}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \
    "$cid" >>"$work/containers.txt" 2>&1 || true
done < <(lc_active_services)
docker stats --no-stream --format '{{.Name}} cpu={{.CPUPerc}} mem={{.MemUsage}} pids={{.PIDs}}' >"$work/stats.txt" 2>&1 || true
{
  df -hP "$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /)" "${LC_BACKUP_DIR:-/}" 2>&1 || true
  docker system df 2>&1 || true
} >"$work/disk.txt"
"$LC_SCRIPTS_DIR/watchdog.sh" >"$work/watchdog.txt" 2>&1 || true
if lc_service_active postgres; then
  lc_psql >"$work/pg.txt" 2>&1 <<'SQL' || true
SELECT 'ledger_count', count(*)::text, coalesce(max(version), '') FROM public.lc_schema_migrations;
SELECT 'activity', coalesce(application_name, '') || ' state=' || coalesce(state, '') || ' wait=' || coalesce(wait_event_type || ':' || wait_event, '-'), count(*)::text
  FROM pg_stat_activity GROUP BY application_name, state, wait_event_type, wait_event ORDER BY 3 DESC;
SELECT 'archiver', 'archived=' || archived_count || ' failed=' || failed_count, coalesce(last_failed_time::text, '-') FROM pg_stat_archiver;
SELECT 'db_size', pg_size_pretty(pg_database_size('live_commerce')), '';
SQL
fi
if lc_service_active caddy; then
  lc_compose exec -T caddy find /data/caddy/certificates -name '*.crt' >"$work/caddy-certs.txt" 2>&1 || true
fi

if ! lc_secret_scan "$work"; then
  lc_die "secret scan found a secret value in the bundle; bundle deleted. Investigate the named file/secret."
fi
bundle="$out_dir/lc-diag-$stamp.tar.gz"
tar -C "$work" -czf "$bundle" .
chmod 0600 "$bundle"
lc_info "diagnostics bundle: $bundle"
printf '%s\n' "$bundle"
