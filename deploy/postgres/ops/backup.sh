#!/usr/bin/env bash
# File: deploy/postgres/ops/backup.sh
# Purpose: nightly logical backup (deploy-design §15): pg_dump -Fc of live_commerce plus a
#   roles-only globals file (no passwords, bootstrap superuser lines removed), a value-free
#   manifest.json, SHA256SUMS, then retention pruning. The directory only becomes visible under
#   its final name after everything is written (watchdog W3 never sees a partial backup).
# Runs as/in: pg-ops container (UID 999, no network) via `deploy/scripts/pg-ops.sh backup
#   [--tag TAG]`; cron 02:17 daily (deploy/host/crontab.example); deploy.sh upgrade (mandatory).
# Reads env: LC_DUMP_RETENTION_DAYS (default 14).
# Reads secrets: pg_superuser_password (through ops/lib.sh pgpass on tmpfs).
# Output: /backup/dumps/<UTC>_<tag>/{db.dump,globals.sql,manifest.json,SHA256SUMS}, mode 0600/0700.
#   Backups CONTAIN BUYER PII: keep the backup disk access-controlled; the off-host copy must be
#   encrypted (owner decision O3). Secrets are NOT in any dump (backup them separately).
# Used by: restore-dump.sh, watchdog W3, docs/runbooks/backup-restore.md.
# Depends on: ops/lib.sh; running postgres reachable over the pgsocket volume.
# Status: DESIGN; verified by smoke S28 (files, SHA256SUMS, pg_restore --list).
# Change rules: manifest keys are one per line (restore-dump.sh parses ledger_count with sed).
export OPS_SCRIPT=backup
# shellcheck source=deploy/postgres/ops/lib.sh
source /ops/lib.sh

tag=manual
while (($#)); do
  case "$1" in
  --tag)
    tag=${2:?--tag needs a value}
    shift 2
    ;;
  *) ops_die "unknown argument (usage: backup.sh [--tag TAG])" 2 ;;
  esac
done
[[ "$tag" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || ops_die "invalid tag" 2
retention=${LC_DUMP_RETENTION_DAYS:-14}
[[ "$retention" =~ ^[1-9][0-9]{0,3}$ ]] || ops_die "invalid LC_DUMP_RETENTION_DAYS" 2

ops_auth
run_id=$(ops_utc)
start=$(date +%s)
mkdir -p "$OPS_BACKUP/dumps"
work="$OPS_BACKUP/dumps/.inprogress-${run_id}_$tag"
final="$OPS_BACKUP/dumps/${run_id}_$tag"
mkdir -m 0700 "$work"
trap 'rm -f "$OPS_PGPASS"; rm -rf "$work"' EXIT

ops_log "run_id=$run_id tag=$tag start"
pg_dump -Fc -Z 6 --no-password -d "$OPS_DB" -f "$work/db.dump"
# Roles and memberships only; passwords are never dumped (logins are re-provisioned by
# provision-logins.sh). The bootstrap superuser already exists in any target cluster.
pg_dumpall --roles-only --no-role-passwords --no-password |
  grep -Ev '^(CREATE|ALTER) ROLE postgres( |;)' >"$work/globals.sql"

sha_dump=$(ops_sha256 "$work/db.dump")
sha_globals=$(ops_sha256 "$work/globals.sql")
duration=$(($(date +%s) - start))
ops_psql -v run_id="$run_id" -v tag="$tag" -v sha_dump="$sha_dump" -v sha_globals="$sha_globals" \
  -v duration="$duration" >"$work/manifest.json" <<'SQL'
SELECT concat_ws(E'\n',
  '{',
  '  "run_id": ' || to_json(:'run_id'::text) || ',',
  '  "tag": ' || to_json(:'tag'::text) || ',',
  '  "database": "live_commerce",',
  '  "server_version": ' || to_json(current_setting('server_version')) || ',',
  '  "ledger_count": ' || (SELECT count(*) FROM public.lc_schema_migrations) || ',',
  '  "ledger_max_version": ' || to_json((SELECT max(version) FROM public.lc_schema_migrations)) || ',',
  '  "database_size_bytes": ' || pg_database_size(current_database()) || ',',
  '  "schemas": ' || (SELECT json_agg(nspname ORDER BY nspname) FROM pg_namespace
                       WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema') || ',',
  '  "sha256": {"db.dump": ' || to_json(:'sha_dump'::text) || ', "globals.sql": ' || to_json(:'sha_globals'::text) || '},',
  '  "duration_seconds": ' || :'duration'::int,
  '}');
SQL
(cd "$work" && sha256sum db.dump globals.sql manifest.json >SHA256SUMS)
chmod 0600 "$work"/*
mv "$work" "$final"
trap 'rm -f "$OPS_PGPASS"' EXIT
size=$(du -sb "$final" | cut -f1)
ops_log "run_id=$run_id tag=$tag dir=$final bytes=$size duration_s=$duration result=ok"

# Retention: only after a successful backup, only our own directory names.
find "$OPS_BACKUP/dumps" -mindepth 1 -maxdepth 1 -type d -name '20*T*Z_*' -mtime "+$retention" -print0 |
  while IFS= read -r -d '' old; do
    rm -rf -- "$old"
    ops_log "retention removed=${old##*/}"
  done
find "$OPS_BACKUP/dumps" -mindepth 1 -maxdepth 1 -type d -name '.inprogress-*' -mmin +1440 -exec rm -rf -- {} +
printf '%s\n' "$final"
