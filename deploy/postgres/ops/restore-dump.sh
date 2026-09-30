#!/usr/bin/env bash
# File: deploy/postgres/ops/restore-dump.sh
# Purpose: restore a logical backup into a NEW database (default) and verify it (deploy-design
#   §15). Replacing the live database is possible only with --replace-live AND
#   LC_CONFIRM_REPLACE_LIVE=I_UNDERSTAND_FORWARD_ONLY AND no client sessions on live_commerce;
#   it then renames live_commerce -> live_commerce_replaced_<UTC> (nothing is dropped) and the
#   verified restore -> live_commerce.
# Usage: restore-dump.sh <dir> [--target-db NAME] [--fresh-cluster] [--drop-after-verify]
#        [--replace-live]      (<dir> = /backup/dumps/<name> or just <name>)
# Runs as/in: pg-ops container (UID 999, socket only) via `deploy/scripts/pg-ops.sh restore-dump`.
# Reads env: LC_CONFIRM_REPLACE_LIVE (only with --replace-live).
# Reads secrets: pg_superuser_password (pgpass on tmpfs).
# Used by: monthly restore drill + production restore (docs/runbooks/backup-restore.md), smoke S29.
# Depends on: backup.sh output (SHA256SUMS, manifest.json ledger_count), ops/verify.sql.
# Status: DESIGN; verified by smoke S29 (restore into scratch DB, verify, drop).
# Change rules: restoring over real payments is forbidden (AGENTS.md; runbook §恢复顺序). Keep
#   owners on restore (SECURITY DEFINER ownership matters). --fresh-cluster applies roles
#   first; logins then need `provision-logins` (dumps carry no passwords).
export OPS_SCRIPT=restore-dump
# shellcheck source=deploy/postgres/ops/lib.sh
source /ops/lib.sh

src="" target="" fresh=0 drop_after=0 replace_live=0
while (($#)); do
  case "$1" in
  --target-db)
    target=${2:?--target-db needs a value}
    shift 2
    ;;
  --fresh-cluster) fresh=1 && shift ;;
  --drop-after-verify) drop_after=1 && shift ;;
  --replace-live) replace_live=1 && shift ;;
  -*) ops_die "unknown option $1" 2 ;;
  *)
    src=$1
    shift
    ;;
  esac
done
[[ -n "$src" ]] || ops_die "usage: restore-dump.sh <dir> [--target-db NAME] [--fresh-cluster] [--drop-after-verify] [--replace-live]" 2
[[ "$src" == /* ]] || src="$OPS_BACKUP/dumps/$src"
[[ -f "$src/db.dump" && -f "$src/SHA256SUMS" && -f "$src/manifest.json" ]] || ops_die "not a backup directory: ${src##*/}" 2
run_id=$(ops_utc)
target=${target:-live_commerce_restore_${run_id,,}}
[[ "$target" =~ ^[a-z_][a-z0-9_]{0,62}$ ]] || ops_die "invalid target database name" 2
[[ "$target" != "$OPS_DB" ]] || ops_die "target must be a NEW database; use --replace-live for the swap" 2
if ((replace_live)); then
  [[ "${LC_CONFIRM_REPLACE_LIVE:-}" == I_UNDERSTAND_FORWARD_ONLY ]] ||
    ops_die "--replace-live requires LC_CONFIRM_REPLACE_LIVE=I_UNDERSTAND_FORWARD_ONLY" 2
fi

ops_auth
(cd "$src" && sha256sum --quiet -c SHA256SUMS) || ops_die "SHA256SUMS mismatch in ${src##*/}"
expected=$(sed -n 's/^ *"ledger_count": *\([0-9][0-9]*\),*$/\1/p' "$src/manifest.json")
[[ "$expected" =~ ^[0-9]+$ ]] || ops_die "manifest.json has no ledger_count"
ops_log "source=${src##*/} checksums=ok expected_ledger=$expected target=$target"

if ((replace_live)); then
  others=$(ops_psql -d postgres -c "SELECT count(*) FROM pg_stat_activity WHERE datname='$OPS_DB' AND backend_type='client backend' AND pid <> pg_backend_pid();")
  [[ "$others" == 0 ]] || ops_die "live_commerce still has $others client sessions: stop all app/worker containers first"
fi
exists=$(ops_psql -d postgres -c "SELECT count(*) FROM pg_database WHERE datname='$target';")
[[ "$exists" == 0 ]] || ops_die "target database already exists: $target"
if ((fresh)); then
  ops_psql -d postgres -f "$src/globals.sql" >/dev/null
  ops_log "globals applied (fresh cluster); run provision-logins afterwards to set login passwords"
fi
start=$(date +%s)
ops_psql -d postgres -c "CREATE DATABASE $target TEMPLATE template0;"
pg_restore --exit-on-error --no-password -d "$target" "$src/db.dump"
[[ "${LC_REQUIRE_MEDIA_GATE:-0}" =~ ^[01]$ ]] || ops_die "LC_REQUIRE_MEDIA_GATE must be 0 or 1" 2
ops_psql -d "$target" -v expected_ledger="$expected" -v require_media="${LC_REQUIRE_MEDIA_GATE:-0}" -f /ops/verify.sql >&2 ||
  ops_die "verify.sql failed on $target (left in place for inspection)"
ops_log "restored target=$target duration_s=$(($(date +%s) - start)) verify=PASS"

if ((replace_live)); then
  others=$(ops_psql -d postgres -c "SELECT count(*) FROM pg_stat_activity WHERE datname='$OPS_DB' AND pid <> pg_backend_pid();")
  [[ "$others" == 0 ]] || ops_die "live_commerce gained sessions during restore; aborting swap (restored copy kept as $target)"
  old="live_commerce_replaced_${run_id,,}"
  ops_psql -d postgres -c "ALTER DATABASE $OPS_DB RENAME TO $old;"
  ops_psql -d postgres -c "ALTER DATABASE $target RENAME TO $OPS_DB;"
  ops_log "swap done: previous live database kept as $old (drop it only after owner sign-off)"
  printf '%s\n' "$OPS_DB"
  exit 0
fi
if ((drop_after)); then
  ops_psql -d postgres -c "DROP DATABASE $target;"
  ops_log "drill cleanup dropped=$target"
fi
printf '%s\n' "$target"
