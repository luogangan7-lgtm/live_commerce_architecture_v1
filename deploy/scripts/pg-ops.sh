#!/usr/bin/env bash
# File: deploy/scripts/pg-ops.sh
# Purpose: host wrapper that runs the database operation scripts inside the one-shot pg-ops
#   container (profile ops; postgres image = same PG 18 client as the server; no network), plus
#   the two host-side procedures that need both the host and pg-ops: superuser password rotation
#   and the PITR cut-over.
# Usage: pg-ops.sh backup [--tag TAG]
#        pg-ops.sh basebackup
#        pg-ops.sh restore-dump <dump dir name | host path under LC_BACKUP_DIR> [restore options]
#        pg-ops.sh restore-pitr --target-time TS [--base NAME] [--drill | --promote]
#        pg-ops.sh pitr-cutover        (needs LC_CONFIRM_REPLACE_LIVE=I_UNDERSTAND_FORWARD_ONLY)
#        pg-ops.sh rotate-superuser
#        pg-ops.sh list
# Runs as/in: deploy host as root (DB host in the 2-host variant), through lib.sh lc_compose.
# Reads env: compose.env (LC_BACKUP_DIR, LC_SECRETS_DIR, LC_SECRETS_GID), LC_DUMP_RETENTION_DAYS,
#   LC_BASE_KEEP, LC_PITR_TIMEOUT_SECONDS, LC_CONFIRM_REPLACE_LIVE, LC_REQUIRE_MEDIA_GATE
#   (forwarded into the container only if set).
# Reads secrets: pg-ops mounts pg_superuser_password. rotate-superuser writes a NEW value to
#   $LC_SECRETS_DIR/.pg_superuser_password.rotating (0400, durable before the database changes),
#   feeds it to pg-ops on stdin, installs it IN PLACE into pg_superuser_password and removes the
#   pending file; values are never printed, logged or put in argv/env.
# Used by: cron (deploy/host/crontab.example), deploy.sh upgrade (mandatory backup),
#   smoke.sh S28-S31, S41 (rotate-superuser), S42 (restore-pitr --promote + pitr-cutover),
#   docs/runbooks/backup-restore.md, docs/runbooks/deploy.md §7.
# Depends on: deploy/postgres/ops/*.sh, compose services pg-ops/postgres/provision-logins,
#   secrets-init.sh (--rederive after rotation), running postgres (except restore-pitr).
# Status: DESIGN; verified by smoke S28-S31, S41, S42.
# Change rules: keep restore defaults non-destructive (new DB / scratch cluster); every step that
#   replaces live data needs LC_CONFIRM_REPLACE_LIVE and owner approval.
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

lc_load_env "$LC_COMPOSE_ENV"
lc_require_vars LC_BACKUP_DIR
cmd=${1:-}
shift || true

forward=()
for v in LC_DUMP_RETENTION_DAYS LC_BASE_KEEP LC_PITR_TIMEOUT_SECONDS LC_CONFIRM_REPLACE_LIVE LC_REQUIRE_MEDIA_GATE; do
  [[ -n "${!v:-}" ]] && forward+=(-e "$v")
done
# Active profiles + ops (a bare --profile ops would hide the rest of the project from Compose).
run_ops() { lc_compose_with_ops run --rm --no-deps -T "${forward[@]}" pg-ops "$@"; }
# Writers stopped for a cut-over (Caddy and edge-netns keep answering 503 + Retry-After).
WRITER_SERVICES=(api admin storefront expiry-worker payment-worker-sandbox payment-worker-live meta-worker claims-worker ads-worker media-worker)

# wait_postgres_healthy — the container healthcheck (socket ping + listen address) within 180 s.
wait_postgres_healthy() {
  local i status
  for ((i = 0; i < 60; i++)); do
    status=$(docker inspect -f '{{.State.Health.Status}}' "$(lc_compose ps -q postgres)" 2>/dev/null || echo starting)
    [[ "$status" == healthy ]] && return 0
    sleep 3
  done
  lc_die "postgres not healthy after 180 s (docker compose logs postgres)"
}

# archive_switch_ok — force a WAL switch and prove it was archived with no new failure (W4 rule).
archive_switch_ok() {
  local a0 a1 f0 f1 i
  IFS='|' read -r a0 f0 <<<"$(lc_psql <<<"SELECT archived_count, failed_count FROM pg_stat_archiver;")"
  lc_psql <<<"SELECT pg_logical_emit_message(false, 'lc-pg-ops', 'switch'); SELECT pg_switch_wal();" >/dev/null
  for ((i = 0; i < 60; i++)); do
    IFS='|' read -r a1 f1 <<<"$(lc_psql <<<"SELECT archived_count, failed_count FROM pg_stat_archiver;")"
    ((a1 > a0)) && break
    sleep 1
  done
  lc_info "archiver archived $a0->$a1 failed $f0->$f1"
  ((a1 > a0 && f1 == f0))
}

# rotate_superuser — docs/runbooks/deploy.md §7. Order matters (crash-safe, resumable):
#   1. new value -> .pg_superuser_password.rotating (0400, fsync) BEFORE the DB changes;
#   2. pg-ops: ALTER ROLE with statement logging off for that session (ops/rotate-superuser.sh);
#   3. install IN PLACE (`cat >`, same inode): Compose mounts file secrets as single-file bind
#      mounts, so the RUNNING postgres container (lc_psql, watchdog, deploy.sh read
#      /run/secrets/pg_superuser_password inside it) sees the new value without a restart. A
#      rename (new inode) would leave it on the old, now invalid value until postgres restarts;
#   4. secrets-init.sh --rederive (dsn_migrate_owner embeds the password);
#   5. prove lc_psql works and scan the postgres log for the new value (lc_secret_scan).
#   Re-running after an interruption resumes from the pending file (step 2 is idempotent).
rotate_superuser() {
  local dir=$LC_SECRETS_DIR cur pending tmp scan
  lc_require_vars LC_SECRETS_DIR LC_SECRETS_GID
  lc_service_active postgres || lc_die "postgres is not an active service here (run on the DB host)"
  command -v openssl >/dev/null || lc_die "openssl is required"
  cur="$dir/pg_superuser_password" pending="$dir/.pg_superuser_password.rotating"
  [[ -f "$cur" ]] || lc_die "secret pg_superuser_password missing"
  umask 077
  if [[ -f "$pending" ]]; then
    lc_warn "resuming an interrupted rotation (pending file present); no new value generated"
  else
    tmp=$(mktemp "$dir/.pg_superuser_password.XXXXXX")
    openssl rand -hex 32 | tr -d '\n' >"$tmp"
    chmod 0400 "$tmp"
    sync -- "$tmp"
    mv -f -- "$tmp" "$pending"
    sync -- "$dir"
  fi
  run_ops /ops/rotate-superuser.sh <"$pending" ||
    lc_die "database step failed; $pending is kept (re-run pg-ops.sh rotate-superuser to resume)"
  cat -- "$pending" >"$cur"
  sync -- "$cur"
  cmp -s -- "$pending" "$cur" || lc_die "installing the new value failed; the valid password is in $pending (copy it over $cur by hand)"
  rm -f -- "$pending"
  lc_info "pg_superuser_password installed in place (same inode; mode/owner unchanged)"
  "$LC_SCRIPTS_DIR/secrets-init.sh" --rederive >/dev/null || lc_die "secrets-init.sh --rederive failed (dsn_migrate_owner still has the old password)"
  lc_info "derived DSNs rewritten (dsn_migrate_owner)"
  [[ "$(lc_psql <<<"SELECT 1;")" == 1 ]] ||
    lc_die "superuser login from the running postgres container fails: its secret mount is stale; run: docker compose restart postgres"
  scan=$(mktemp)
  lc_compose logs --no-color postgres >"$scan" 2>&1 || true
  if ! lc_secret_scan "$scan"; then
    rm -f "$scan"
    lc_die "a secret value was found in the postgres log; treat as a leak (incident.md §5)"
  fi
  rm -f "$scan"
  lc_info "rotate-superuser ok: postgres log scanned clean; update the offline encrypted secrets copy (backup-restore.md §7)"
}

# pitr_cutover — docs/runbooks/backup-restore.md §6 (owner approval). Replaces the live cluster
#   with the one `restore-pitr --promote` left in pitr-scratch: check it -> stop writers +
#   postgres -> pitr-install.sh --install (live pgdata mounted ONLY for that run; old data dir
#   kept as 18/docker.pre-pitr-<UTC>) -> start postgres -> prove: not in recovery, timeline > 1,
#   WAL switch archived with failed unchanged, timeline history file in the archive ->
#   provision-logins (service passwords = current secrets) -> new base backup. Writers stay
#   STOPPED: reopen step by step (§5 4-7), e.g. with deploy.sh first.
pitr_cutover() {
  local s svcs=() tli hist in_rec pgvol
  [[ "${LC_CONFIRM_REPLACE_LIVE:-}" == I_UNDERSTAND_FORWARD_ONLY ]] ||
    lc_die "pitr-cutover replaces the live database: set LC_CONFIRM_REPLACE_LIVE=I_UNDERSTAND_FORWARD_ONLY (owner approval)" 2
  lc_service_active postgres || lc_die "postgres is not an active service here (run on the DB host)"
  run_ops /ops/pitr-install.sh --check || lc_die "no promoted PITR cluster to install (pg-ops.sh restore-pitr --target-time TS --promote)"
  # The docker volume the postgres container really uses for /var/lib/postgresql. `run -v pgdata:..`
  # would NOT resolve to the project volume: Compose passes it through and Docker creates a new,
  # empty global volume literally named "pgdata" (VERIFIED_LOCAL).
  pgvol=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/postgresql"}}{{.Name}}{{end}}{{end}}' \
    "$(lc_compose ps -a -q postgres)" 2>/dev/null || true)
  [[ "$pgvol" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || lc_die "cannot resolve the postgres data volume (container missing?)"
  for s in "${WRITER_SERVICES[@]}"; do
    if lc_service_active "$s"; then svcs+=("$s"); fi
  done
  lc_info "stopping writers: ${svcs[*]:-none}; then postgres"
  ((${#svcs[@]} == 0)) || lc_compose stop "${svcs[@]}"
  lc_compose stop postgres
  lc_info "installing into volume $pgvol (mounted at /live for this one pg-ops run)"
  lc_compose_with_ops run --rm --no-deps -T -v "$pgvol:/live" pg-ops /ops/pitr-install.sh --install ||
    lc_die "install failed (live data dir unchanged or restored); postgres is STOPPED: docker compose up -d postgres"
  lc_compose up -d --no-deps postgres
  wait_postgres_healthy
  in_rec=$(lc_psql <<<"SELECT pg_is_in_recovery();")
  tli=$(lc_psql <<<"SELECT timeline_id FROM pg_control_checkpoint();")
  [[ "$in_rec" == f ]] || lc_die "cut-over server is still in recovery"
  [[ "$tli" =~ ^[0-9]+$ ]] && ((tli > 1)) || lc_die "cut-over server is on timeline ${tli:-?}, expected > 1"
  archive_switch_ok || lc_die "WAL archiving fails after the cut-over (watchdog W4; incident.md §postgres)"
  hist=$(printf '%08X.history' "$tli")
  [[ -f "$LC_BACKUP_DIR/wal/$hist" ]] || lc_die "timeline history $hist did not reach $LC_BACKUP_DIR/wal"
  lc_info "cut-over ok: in_recovery=f timeline=$tli history=$hist archived"
  lc_compose run --rm -T --no-deps provision-logins || lc_die "provision-logins failed after the cut-over (service logins not usable)"
  run_ops /ops/basebackup.sh || lc_die "base backup after the cut-over failed; take one before reopening (new timeline)"
  lc_info "pitr-cutover done. Writers are STOPPED; reopen per backup-restore.md §5 steps 4-7 (deploy/scripts/deploy.sh first brings the stack back with post-checks)"
}

case "$cmd" in
backup) run_ops /ops/backup.sh "$@" ;;
basebackup) run_ops /ops/basebackup.sh "$@" ;;
restore-dump)
  src=${1:?usage: pg-ops.sh restore-dump <dump dir> [options]}
  shift
  # Accept a host path under LC_BACKUP_DIR and translate it to the container mount /backup.
  if [[ "$src" == "$LC_BACKUP_DIR"/* ]]; then src="/backup/${src#"$LC_BACKUP_DIR"/}"; fi
  run_ops /ops/restore-dump.sh "$src" "$@"
  ;;
restore-pitr) run_ops /ops/restore-pitr.sh "$@" ;;
pitr-cutover)
  (($# == 0)) || lc_die "usage: pg-ops.sh pitr-cutover" 2
  pitr_cutover
  ;;
rotate-superuser)
  (($# == 0)) || lc_die "usage: pg-ops.sh rotate-superuser" 2
  rotate_superuser
  ;;
list)
  for d in dumps base; do
    echo "== $d ($LC_BACKUP_DIR/$d)"
    if [[ -d "$LC_BACKUP_DIR/$d" ]]; then
      find "$LC_BACKUP_DIR/$d" -mindepth 1 -maxdepth 1 -type d -name '20*' -printf '%f\n' | sort |
        while IFS= read -r n; do printf '%-40s %s\n' "$n" "$(du -sh "$LC_BACKUP_DIR/$d/$n" | cut -f1)"; done
    fi
  done
  echo "== wal segments: $(find "$LC_BACKUP_DIR/wal" -maxdepth 1 -type f 2>/dev/null | wc -l)"
  ;;
*) lc_die "usage: pg-ops.sh backup [--tag T] | basebackup | restore-dump <dir> [...] | restore-pitr --target-time TS [--drill|--promote] | pitr-cutover | rotate-superuser | list" 2 ;;
esac
