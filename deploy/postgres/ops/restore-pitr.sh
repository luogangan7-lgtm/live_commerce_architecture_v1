#!/usr/bin/env bash
# File: deploy/postgres/ops/restore-pitr.sh
# Purpose: point-in-time recovery into a PRIVATE scratch cluster inside pg-ops (deploy-design
#   §15): untar a base backup into the pitr-scratch volume, replay archived WAL up to
#   --target-time with recovery_target_action=pause, run verify.sql against the paused
#   cluster, then (mode dependent) stop it. Never touches live PGDATA (pg-ops does not mount
#   pgdata) and never archives into the live WAL archive.
# Usage: restore-pitr.sh --target-time '<timestamptz>' [--base <UTC dir name>] [--drill | --promote]
#   --drill    wipes the scratch cluster afterwards (quarterly drill / smoke S31).
#   --promote  (first half of the cut-over, docs/runbooks/backup-restore.md §6; smoke S42) after
#              verify.sql passes at the pause point: pg_wal_replay_resume() ENDS recovery exactly
#              at the target and promotes (new timeline + its .history file), CHECKPOINT, set the
#              superuser password to the CURRENT secret (the cluster otherwise keeps the one it had
#              at the target), clean stop, write the PROMOTED marker for pitr-install.sh.
#              Review P1: the old procedure copied a cluster fast-stopped WHILE PAUSED
#              ("shut down in recovery") and deleted recovery.signal, so the live server
#              crash-recovered through all remaining pg_wal past the target, stayed on timeline 1
#              and then failed archiving forever (segment names already in /backup/wal).
#              The scratch server runs with archive_mode=on + an EMPTY archive_command in this
#              mode (documented "archiving temporarily disabled": WAL is kept and .ready flags
#              queue up). Nothing is archived from pg-ops; the live server archives the new
#              timeline's .history file and segments once pitr-cutover installs the cluster.
#              With archive_mode=off no .ready flag would exist and the history file would never
#              reach the archive, which later PITRs need to follow the new timeline.
#   neither    stopped paused cluster stays for inspection only; pitr-install.sh refuses it.
# Runs as/in: pg-ops container (UID 999, no network) via `deploy/scripts/pg-ops.sh restore-pitr`.
# Reads env: LC_PITR_TIMEOUT_SECONDS (default 1800).
# Reads secrets: pg_superuser_password: pgpass on tmpfs (ops_auth, used by nothing here since the
#   scratch socket uses peer auth) and, with --promote, the value set on the promoted cluster.
#   The scratch hba is `local all postgres peer` (pg-ops runs as OS user postgres, UID 999; the
#   socket dir is 0700 inside a network-less container): the restored cluster has the superuser
#   password of the TARGET time, which differs from the secret file after a rotation.
# Used by: quarterly PITR drill + PITR cut-over (docs/runbooks/backup-restore.md §4, §6),
#   smoke.sh S31 (--drill), S42 (--promote).
# Depends on: basebackup.sh output in /backup/base, archived WAL in /backup/wal, ops/verify.sql,
#   ops/lib.sh ops_alter_superuser_password (--promote).
# Status: DESIGN; verified by smoke S31 (records the drill duration = RTO sample) and S42.
# Change rules: keep listen_addresses='' (socket only); never give the scratch server a real
#   archive_command (archive_mode=off for --drill/inspection, on + empty command for --promote).
export OPS_SCRIPT=restore-pitr
# shellcheck source=deploy/postgres/ops/lib.sh
source /ops/lib.sh

target_time="" base="" drill=0 promote=0
while (($#)); do
  case "$1" in
  --target-time)
    target_time=${2:?--target-time needs a value}
    shift 2
    ;;
  --base)
    base=${2:?--base needs a value}
    shift 2
    ;;
  --drill) drill=1 && shift ;;
  --promote) promote=1 && shift ;;
  *) ops_die "usage: restore-pitr.sh --target-time TS [--base NAME] [--drill | --promote]" 2 ;;
  esac
done
((drill + promote < 2)) || ops_die "--drill and --promote are mutually exclusive" 2
[[ -n "$target_time" ]] || ops_die "--target-time is required" 2
[[ "$target_time" =~ ^[0-9A-Za-z:.+\ _-]{8,40}$ ]] || ops_die "invalid --target-time" 2
timeout=${LC_PITR_TIMEOUT_SECONDS:-1800}
[[ "$timeout" =~ ^[1-9][0-9]{0,5}$ ]] || ops_die "invalid LC_PITR_TIMEOUT_SECONDS" 2
if [[ -z "$base" ]]; then
  base=$(find "$OPS_BACKUP/base" -mindepth 1 -maxdepth 1 -type d -name '20*T*Z' -printf '%f\n' 2>/dev/null | sort | tail -n1)
fi
[[ "$base" =~ ^20[0-9]{6}T[0-9]{6}Z$ && -f "$OPS_BACKUP/base/$base/base.tar.gz" ]] || ops_die "no usable base backup (run pg-ops.sh basebackup)"

ops_auth
start=$(date +%s)
data="$OPS_SCRATCH/pgdata"
sock="$OPS_SCRATCH/sock"
rm -rf -- "${OPS_SCRATCH:?}"
mkdir -m 0700 "$OPS_SCRATCH" "$data" "$sock"
cleanup() {
  pg_ctl -D "$data" -m fast stop >/dev/null 2>&1 || true
  rm -f "$OPS_PGPASS"
  if ((drill)); then rm -rf -- "${OPS_SCRATCH:?}"; fi
}
trap cleanup EXIT

tar -xzf "$OPS_BACKUP/base/$base/base.tar.gz" -C "$data"
tar -xzf "$OPS_BACKUP/base/$base/pg_wal.tar.gz" -C "$data/pg_wal"
touch "$data/recovery.signal"
# Peer auth (see header): works whatever superuser password the restored cluster has.
printf 'local all postgres peer\n' >"$OPS_SCRATCH/pg_hba.conf"
if ((promote)); then
  archive_opts="-c archive_mode=on -c archive_command='' -c archive_library=''"
else
  archive_opts="-c archive_mode=off"
fi
# Hot standby refuses to replay with any of these below the primary's values ("recovery
# aborted because of insufficient parameter settings"), so take them from the backup's
# pg_control instead of hardcoding (VERIFIED_LOCAL: max_connections=20 aborted recovery).
ctl() { pg_controldata "$data" | sed -n "s/^$1 setting: *//p"; }
mc=$(ctl max_connections) mwp=$(ctl max_worker_processes) mws=$(ctl max_wal_senders)
mpx=$(ctl max_prepared_xacts) mlx=$(ctl max_locks_per_xact)
[[ "$mc$mwp$mws$mpx$mlx" =~ ^[0-9]+$ ]] || ops_die "cannot read primary settings from pg_control"
ops_log "base=$base target_time=$target_time scratch=$data drill=$drill promote=$promote start"

pg_ctl -D "$data" -l "$OPS_SCRATCH/pitr.log" -w -t 600 -o "\
 -c listen_addresses='' -c unix_socket_directories='$sock' -c hba_file='$OPS_SCRATCH/pg_hba.conf' \
 $archive_opts -c shared_buffers=128MB -c logging_collector=off \
 -c max_connections=$mc -c max_worker_processes=$mwp -c max_wal_senders=$mws \
 -c max_prepared_transactions=$mpx -c max_locks_per_transaction=$mlx \
 -c restore_command='cp /backup/wal/%f %p' -c recovery_target_time='$target_time' \
 -c recovery_target_action=pause" start >/dev/null ||
  ops_die "scratch server did not start (see $OPS_SCRATCH/pitr.log inside pg-ops)"

state=""
for ((i = 0; i < timeout; i += 2)); do
  state=$(PGHOST=$sock ops_psql -d postgres -c "SELECT pg_get_wal_replay_pause_state();" 2>/dev/null || true)
  [[ "$state" == paused ]] && break
  if ! pg_ctl -D "$data" status >/dev/null 2>&1; then
    ops_die "recovery ended before the target was reached (target later than the newest archived WAL?)"
  fi
  sleep 2
done
[[ "$state" == paused ]] || ops_die "recovery did not reach the pause point within ${timeout}s"
replay=$(PGHOST=$sock ops_psql -d postgres -c "SELECT pg_last_xact_replay_timestamp();")
ops_log "paused at_target=yes last_replayed_xact=$replay"
[[ "${LC_REQUIRE_MEDIA_GATE:-0}" =~ ^[01]$ ]] || ops_die "LC_REQUIRE_MEDIA_GATE must be 0 or 1" 2
PGHOST=$sock ops_psql -d "$OPS_DB" -v require_media="${LC_REQUIRE_MEDIA_GATE:-0}" -f /ops/verify.sql >&2 ||
  ops_die "verify.sql failed on the PITR cluster"

if ((promote)); then
  # End recovery exactly here: with recovery_target_action=pause, resuming at the target ends
  # recovery and promotes (PostgreSQL docs, recovery_target_action). Poll until writable.
  PGHOST=$sock ops_psql -d postgres -c "SELECT pg_wal_replay_resume();" >/dev/null
  in_recovery=t
  for ((i = 0; i < 120; i++)); do
    in_recovery=$(PGHOST=$sock ops_psql -d postgres -c "SELECT pg_is_in_recovery();" 2>/dev/null || echo t)
    [[ "$in_recovery" == f ]] && break
    sleep 1
  done
  [[ "$in_recovery" == f ]] || ops_die "promotion did not finish within 120 s (see $OPS_SCRATCH/pitr.log inside pg-ops)"
  # The end-of-recovery checkpoint is asynchronous; force one so pg_control names the new timeline.
  PGHOST=$sock ops_psql -d postgres -c "CHECKPOINT;" >/dev/null
  tli=$(PGHOST=$sock ops_psql -d postgres -c "SELECT timeline_id FROM pg_control_checkpoint();")
  [[ "$tli" =~ ^[0-9]+$ ]] && ((tli > 1)) || ops_die "promoted cluster is on timeline ${tli:-?}, expected > 1"
  # Live services authenticate with the CURRENT secret files; the cluster has the target-time
  # password. provision-logins (run by pitr-cutover) re-sets the service logins the same way.
  PGHOST=$sock ops_alter_superuser_password "$OPS_SECRET" || ops_die "could not set the current superuser password on the promoted cluster"
  pg_ctl -D "$data" -m fast stop >/dev/null
  state=$(pg_controldata "$data" | sed -n 's/^Database cluster state: *//p')
  [[ "$state" == "shut down" ]] || ops_die "promoted cluster stopped in state '$state', expected 'shut down'"
  hist=$(printf '%08X.history' "$tli")
  [[ -f "$data/pg_wal/$hist" && -f "$data/pg_wal/archive_status/$hist.ready" ]] ||
    ops_die "timeline history $hist or its .ready flag missing: the live archive would never receive it"
  [[ ! -e "$data/recovery.signal" ]] || ops_die "recovery.signal still present after promotion"
  printf 'base=%s\ntarget_time=%s\ntimeline=%s\npromoted_at=%s\n' "$base" "$target_time" "$tli" "$(ops_utc)" >"$OPS_SCRATCH/PROMOTED"
  duration=$(($(date +%s) - start))
  ops_log "base=$base duration_s=$duration verify=PASS promoted timeline=$tli state='shut down' result=ok"
  ops_log "next: LC_CONFIRM_REPLACE_LIVE=I_UNDERSTAND_FORWARD_ONLY deploy/scripts/pg-ops.sh pitr-cutover (owner approval)"
  printf 'pitr_duration_seconds=%s\npitr_promoted_timeline=%s\n' "$duration" "$tli"
  exit 0
fi
pg_ctl -D "$data" -m fast stop >/dev/null
duration=$(($(date +%s) - start))
ops_log "base=$base duration_s=$duration verify=PASS drill=$drill result=ok"
if ((!drill)); then
  ops_log "stopped PAUSED cluster kept for inspection only (state 'shut down in recovery'); it can NOT be cut over. Use --promote for that."
fi
printf 'pitr_duration_seconds=%s\n' "$duration"
