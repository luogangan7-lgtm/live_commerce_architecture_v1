#!/usr/bin/env bash
# File: deploy/postgres/ops/pitr-install.sh
# Purpose: the data-directory half of the PITR cut-over (docs/runbooks/backup-restore.md §6):
#   --check    prove the pitr-scratch volume holds a cluster that `restore-pitr.sh --promote`
#              finished: PROMOTED marker present, pg_control "shut down" (NOT "shut down in
#              recovery", which would make the live server crash-recover past the target on the
#              old timeline — review P1), timeline > 1, no recovery.signal/standby.signal.
#   --install  (live pgdata volume mounted at /live, postgres STOPPED) move the live data dir
#              /live/18/docker aside to /live/18/docker.pre-pitr-<UTC> (same filesystem, instant,
#              the rollback copy) and copy the promoted cluster into /live/18/docker. The PROMOTED
#              marker is removed so the same scratch cluster can never be installed twice.
# Usage: never by hand; deploy/scripts/pg-ops.sh pitr-cutover runs
#   run_ops /ops/pitr-install.sh --check                    (scratch only)
#   pg-ops run -v pgdata:/live ... /ops/pitr-install.sh --install
# Runs as/in: pg-ops container (UID 999 = owner of both data dirs, network none). This is the
#   ONLY place pg-ops ever sees live PGDATA, and only through the explicit extra mount above.
# Reads env / secrets: none.
# Used by: deploy/scripts/pg-ops.sh pitr-cutover; smoke.sh S42.
# Depends on: restore-pitr.sh --promote output in /var/lib/postgresql/pitr (pitr-scratch volume):
#   pgdata/ and the PROMOTED marker (key=value: base, target_time, timeline, promoted_at).
# Status: DESIGN; verified by smoke S42 (promote -> cut-over -> timeline 2, rows == target,
#   archiver failed=0, history file archived, stack back through deploy.sh first).
# Change rules: never delete the previous data dir here; removing it is an owner decision after
#   the new base backup (runbook §6).
export OPS_SCRIPT=pitr-install
# shellcheck source=deploy/postgres/ops/lib.sh
source /ops/lib.sh

mode=${1:-}
[[ "$mode" == --check || "$mode" == --install ]] && (($# == 1)) || ops_die "usage: pitr-install.sh --check | --install" 2
src="$OPS_SCRATCH/pgdata"
marker="$OPS_SCRATCH/PROMOTED"
live_root=/live/18
live="$live_root/docker"

ctl() { pg_controldata "$1" | sed -n "s/^$2: *//p"; }

# ---- scratch cluster must be a finished promotion ------------------------------------------------
[[ -f "$marker" && -d "$src" ]] || ops_die "no promoted PITR cluster in pitr-scratch (run: pg-ops.sh restore-pitr --target-time TS --promote)"
state=$(ctl "$src" "Database cluster state")
tli=$(ctl "$src" "Latest checkpoint's TimeLineID")
want_tli=$(sed -n 's/^timeline=//p' "$marker")
[[ "$state" == "shut down" ]] || ops_die "scratch cluster state is '$state', expected 'shut down' (not promoted or not cleanly stopped)"
[[ "$tli" =~ ^[0-9]+$ ]] && ((tli > 1)) || ops_die "scratch cluster is still on timeline ${tli:-?}; promotion did not happen"
[[ "$tli" == "$want_tli" ]] || ops_die "scratch timeline $tli differs from the PROMOTED marker ($want_tli)"
[[ ! -e "$src/recovery.signal" && ! -e "$src/standby.signal" ]] || ops_die "scratch cluster still has a recovery/standby signal file"
ops_log "scratch ok state='shut down' timeline=$tli $(tr '\n' ' ' <"$marker")"
[[ "$mode" == --check ]] && exit 0

# ---- install into the live pgdata volume ---------------------------------------------------------
[[ -d "$live" ]] || ops_die "live data dir /live/18/docker missing (is the pgdata volume mounted at /live?)"
[[ ! -e "$live/postmaster.pid" ]] || ops_die "live postmaster.pid present: postgres is running or did not stop cleanly; stop it first"
live_state=$(ctl "$live" "Database cluster state")
[[ "$live_state" == "shut down" ]] || ops_die "live cluster state is '$live_state', expected 'shut down' (stop postgres cleanly first)"
need_kb=$(du -sk "$src" | cut -f1)
avail_kb=$(df -Pk "$live_root" | awk 'NR == 2 { print $4 }')
[[ "$need_kb" =~ ^[0-9]+$ && "$avail_kb" =~ ^[0-9]+$ ]] && ((avail_kb > need_kb + need_kb / 10)) ||
  ops_die "not enough space in the pgdata volume: need ${need_kb} KiB (+10 %), have ${avail_kb:-?} KiB"
old="$live.pre-pitr-$(ops_utc)"
mv -- "$live" "$old"
if ! cp -a -- "$src" "$live"; then
  rm -rf -- "$live"
  mv -- "$old" "$live"
  ops_die "copy failed; the previous live data dir was put back unchanged"
fi
chmod 0700 "$live"
sync
rm -f -- "$marker"
ops_log "installed timeline=$tli into 18/docker; previous data dir kept as 18/${old##*/} (remove only after the new base backup and owner sign-off)"
printf 'pitr_previous_datadir=18/%s\n' "${old##*/}"
