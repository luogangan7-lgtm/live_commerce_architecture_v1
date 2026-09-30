#!/usr/bin/env bash
# File: deploy/scripts/deploy.sh
# Purpose: the three supported release operations (deploy-design §16.1, D11, D12):
#   first [<tag>]       preflight -> images present -> postgres healthy -> migrate ->
#                       provision-logins -> record tag -> up -d (active profiles) -> post checks
#                       -> log. <tag> defaults to compose.env IMAGE_TAG.
#   upgrade <tag>       preflight(new tag) -> MANDATORY backup -> stop app+workers (Caddy keeps
#                       answering 503 + Retry-After) -> migrate(new tag) -> provision -> record
#                       tag -> up -d -> post checks -> log. Never auto-rolls back or restores.
#   app-rollback <tag>  allowed ONLY if the migration ledger count equals the one recorded when
#                       <tag> was last deployed; otherwise refuses ("forward-fix only"). Then
#                       record tag -> up -d (recreates only containers whose image/config
#                       changed) -> post checks -> log. Rolling back to the tag that is already
#                       running is a valid no-op and must pass (post_checks judges the current
#                       lifetime).
#   "record tag" (review P1): compose.env IMAGE_TAG is rewritten atomically to the tag being
#   brought up, immediately BEFORE `up -d`. compose.env is the only tag source for every other
#   Compose entry point (runbook `dc up`/`dc run migrate`, a re-run of `first`, §8 domain change),
#   so a tag that lived only in this process's environment let those silently recreate api/admin/
#   caddy on the previous images (and re-run the previous migrate). Written before `up -d`, not
#   after post_checks: from that moment containers run the new tag, and a failed post-check must
#   not leave compose.env pointing at the old images (a later plain `up -d` would then downgrade
#   over the migrated schema). deployments.log still records only VERIFIED deploys (post_checks
#   passed) and remains the rollback evidence. watchdog.sh W10 alarms on any drift.
# Usage: deploy.sh [--smoke] first [<tag>] | upgrade <tag> | app-rollback <tag>
#   --smoke: used by smoke.sh S37-S39 — public checks use --resolve to 127.0.0.1 and Caddy's
#   local CA instead of real DNS/ACME.
# Runs as/in: deploy host (root), through lib.sh lc_compose. NO production deploy may be run
#   without owner approval (AGENTS.md); this script only automates the approved procedure.
# Reads env: compose.env (IMAGE_TAG, hosts, LC_STATE_DIR, profiles), LC_SMOKE_CACERT (smoke).
# Reads secrets: none directly (postgres/migrate/provision read their own mounted files).
# Writes: $LC_COMPOSE_ENV IMAGE_TAG line (lib.sh lc_env_file_set: temp file + rename, owner/mode
#   kept); ${LC_STATE_DIR:-/var/lib/live-commerce}/deployments.log
#   (UTC ts <TAB> action <TAB> tag <TAB> ledger_count <TAB> operator).
# Used by: operators (docs/runbooks/deploy.md), smoke.sh S37-S39, S42 (stack back after the PITR
#   cut-over), S43 (compose.env follows the deployed tag).
# Depends on: preflight.sh, pg-ops.sh (backup), lib.sh; images from build-images.sh.
# Exit: 0 ok; 1 failed (prints the rollback decision tree); 75 migration lock busy (retry
#   later by hand, never in a loop); 2 usage.
# Status: DESIGN; S37-S39 BLOCKED until cmd/migrate exists (I1). With the I1 proposal applied
#   (VERIFIED_LOCAL 2026-09-28) S37/S38 passed and S39 failed on the no-op rollback (F3, fixed in
#   post_checks); S39 now also rolls back to a genuinely different tag and back.
# Change rules: keep the order migrate -> provision -> start (架构.md §22.1 line 731); never add
#   automatic DB restore; edge-netns must never be targeted alone.
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

smoke=0
if [[ "${1:-}" == --smoke ]]; then
  smoke=1
  shift
fi
action=${1:-}
arg=${2:-}
lc_load_env "$LC_COMPOSE_ENV"
state_dir=${LC_STATE_DIR:-/var/lib/live-commerce}
deploy_log="$state_dir/deployments.log"
operator=${SUDO_USER:-${USER:-unknown}}
APP_SERVICES=(api admin storefront expiry-worker payment-worker-sandbox payment-worker-live meta-worker claims-worker ads-worker)

decision_tree() {
  cat >&2 <<'EOF'
------------------------------------------------------------------------------------------------
Deploy step failed. Decide (docs/runbooks/deploy.md §回滚决策树):
 1. Nothing changed in the DB ledger since the previous tag?  -> deploy.sh app-rollback <previous tag>
 2. Ledger changed and NO external business fact since the pre-upgrade backup (no payments/
    orders after traffic reopened)?  -> owner decides a DB restore (backup-restore.md), then
    app-rollback. Never restore over real payments.
 3. Otherwise -> forward fix + reconciliation (架构.md §22.1). Never edit applied SQL/checksums.
 compose.env IMAGE_TAG is rewritten just before `up -d`; if the failure came after that point it
 already names the new tag (= what the containers run). app-rollback rewrites it again.
 Diagnostics: deploy/scripts/collect-diagnostics.sh --since 1h
------------------------------------------------------------------------------------------------
EOF
}
trap 'rc=$?; if ((rc != 0 && rc != 2 && rc != 75)); then decision_tree; fi' EXIT

active_app_services() {
  local s
  for s in "${APP_SERVICES[@]}"; do
    if lc_service_active "$s"; then printf '%s\n' "$s"; fi
  done
  return 0
}

run_migrate() {
  local rc=0
  lc_info "migrate (tag $IMAGE_TAG)"
  lc_compose run --rm -T migrate || rc=$?
  if ((rc == 75)); then
    lc_error "migrate exit 75: another migration holds advisory lock 718020260920. Do NOT loop; see docs/runbooks/incident.md §migrate"
    exit 75
  fi
  ((rc == 0)) || lc_die "migrate failed (exit $rc); see: docker compose logs migrate / incident.md §migrate"
}

run_provision() {
  lc_info "provision-logins"
  lc_compose run --rm -T --no-deps provision-logins || lc_die "provision-logins failed (role drift or readiness false; no auto-repair)"
}

wait_postgres() {
  local i status
  lc_compose up -d postgres
  for ((i = 0; i < 60; i++)); do
    status=$(docker inspect -f '{{.State.Health.Status}}' "$(lc_compose ps -q postgres)" 2>/dev/null || echo starting)
    [[ "$status" == healthy ]] && return 0
    sleep 3
  done
  lc_die "postgres not healthy after 180 s"
}

# post_checks — proves that the stack running NOW is the one IMAGE_TAG describes, and is ready:
#   1. every long-running service is running (and healthy where it has a healthcheck);
#   2. every container built from our images (${LC_IMAGE_PREFIX:-lc}-*) runs exactly :$IMAGE_TAG,
#      so a stale container can never pass for the target release;
#   3. every active worker logged its ready token during its CURRENT lifetime, i.e. since that
#      container's own .State.StartedAt (floored to the second: the daemon may stamp the first log
#      line a hair before it records StartedAt). NOT "since this script started": `up -d` leaves
#      unchanged containers running (no-op app-rollback to the running tag, a re-run of `first`),
#      and those printed their token when they started (VERIFIED_LOCAL F3: smoke S39 waited 60 s
#      for a fresh token that a no-op `up -d` never produces). A recreated/restarted container
#      has a new StartedAt, so its token must be new as well.
#   4. with claims mounted, the U08 retention purge has run on its own login (retention_check);
#   5. the public HTTPS edge answers /healthz (below).
post_checks() {
  local s cid status i ok bad token image started prefix="${LC_IMAGE_PREFIX:-lc}-" curl_args=()
  for ((i = 0; i < 40; i++)); do
    ok=1 bad=""
    while IFS= read -r s; do
      cid=$(lc_compose ps -q "$s" 2>/dev/null)
      if [[ -z "$cid" ]]; then
        ok=0 bad+=" $s=absent"
        continue
      fi
      status=$(docker inspect -f '{{.State.Status}}/{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid")
      [[ "$status" == running/healthy || "$status" == running/none ]] || { ok=0 bad+=" $s=$status"; }
    done < <(lc_long_running_services)
    ((ok)) && break
    sleep 3
  done
  ((ok)) || lc_die "post-check: not all services running/healthy:$bad (docker compose ps)"
  while IFS= read -r s; do
    image=$(docker inspect -f '{{.Config.Image}}' "$(lc_compose ps -q "$s")")
    if [[ "$image" == "$prefix"* && "$image" != *":$IMAGE_TAG" ]]; then
      lc_die "post-check: $s runs $image, expected tag $IMAGE_TAG (stale container)"
    fi
  done < <(lc_long_running_services)
  lc_info "post-check all ${prefix}* containers run tag $IMAGE_TAG"
  for s in expiry-worker:expiry_worker_ready payment-worker-sandbox:payment_worker_ready \
    payment-worker-live:payment_worker_ready meta-worker:meta_worker_ready \
    claims-worker:claims_worker_ready ads-worker:ads_worker_ready; do
    token=${s#*:} s=${s%%:*}
    lc_service_active "$s" || continue
    for ((i = 0; i < 30; i++)); do
      cid=$(lc_compose ps -q "$s" 2>/dev/null)
      if [[ -n "$cid" ]]; then
        # Re-read every round: a crash-looping worker gets a new StartedAt on each restart.
        started=$(docker inspect -f '{{.State.StartedAt}}' "$cid")
        started=${started%%.*}
        started=${started%Z}Z
        grep -q "$token" < <(docker logs --since "$started" "$cid" 2>&1) && break
      fi
      sleep 2
    done
    ((i < 30)) || lc_die "post-check: $s did not log $token in its current lifetime within 60 s"
    lc_info "post-check $s ready (token since container start $started)"
  done
  if lc_service_active claims-worker; then retention_check; fi
  if lc_service_active caddy; then
    if ((smoke)); then
      # *.localhost sites use Caddy's internal CA; its root appears once Caddy has started.
      LC_SMOKE_CACERT=${LC_SMOKE_CACERT:-$state_dir/smoke-caddy-root.crt}
      for ((i = 0; i < 20; i++)); do
        lc_compose exec -T caddy cat /data/caddy/pki/authorities/local/root.crt >"$LC_SMOKE_CACERT" 2>/dev/null && break
        sleep 2
      done
      [[ -s "$LC_SMOKE_CACERT" ]] || lc_die "post-check: Caddy local root CA not available"
      curl_args=(--resolve "$LC_API_HOST:${LC_HTTPS_PORT}:127.0.0.1" --cacert "$LC_SMOKE_CACERT")
    fi
    for ((i = 0; i < 20; i++)); do
      curl -fsS --max-time 5 "${curl_args[@]}" -o /dev/null "https://$LC_API_HOST:${LC_HTTPS_PORT}/healthz" && break
      sleep 3
    done
    ((i < 20)) || lc_die "post-check: https://$LC_API_HOST/healthz not reachable through the edge"
    lc_info "post-check edge https ok"
  fi
}

# retention_check — claims-retention-purge-v1 §10(5): `retention-admin status` with the claims-worker's own
# retention-job login (lcentry expands its *_FILE; the operator login never exists on this host) must show a
# purge run in the last 26 h and, unless LC_REQUIRE_RETENTION_ENFORCED=0 (waiver W1 only), enforced=1.
# RunOnStart queues the first run at worker start, so a fresh stack is polled for up to 60 s.
retention_check() {
  local out enforced="" last=0 now i
  for ((i = 0; i < 20; i++)); do
    out=$(lc_compose run --rm --no-deps -T claims-worker /app/bin/retention-admin status 2>&1) ||
      lc_die "post-check: retention-admin status failed (${out:0:60})"
    enforced=$(sed -n 's/^enforced=\([01]\)$/\1/p' <<<"$out")
    last=$(sed -n 's/^last_run_unix=\([0-9][0-9]*\)$/\1/p' <<<"$out")
    [[ -n "$enforced" && -n "$last" ]] || lc_die "post-check: retention-admin status output not parsable"
    now=$(date +%s)
    ((last > 0 && now - last < 26 * 3600)) && break
    sleep 3
  done
  ((i < 20)) || lc_die "post-check: no claims retention run in the last 26 h (last_run_unix=$last)"
  if [[ "$enforced" != 1 && "${LC_REQUIRE_RETENTION_ENFORCED:-1}" != 0 ]]; then
    lc_die "post-check: claims retention policy not enforced (claims-retention-purge-v1 §10; docs/runbooks/claims-data-deletion.md)"
  fi
  lc_info "post-check claims retention ok (enforced=$enforced, last run $((now - last)) s ago)"
}

log_deploy() {
  local ledger
  ledger=$(lc_ledger_count)
  mkdir -p "$state_dir"
  printf '%s\t%s\t%s\t%s\t%s\n' "$(lc_ts)" "$1" "$IMAGE_TAG" "$ledger" "$operator" >>"$deploy_log"
  lc_info "recorded $1 tag=$IMAGE_TAG ledger_count=$ledger in deployments.log"
}

# record_tag — compose.env IMAGE_TAG := $IMAGE_TAG (see header "record tag"). Idempotent.
record_tag() {
  local old
  old=$(lc_env_file_get "$LC_COMPOSE_ENV" IMAGE_TAG || true)
  if [[ "$old" == "$IMAGE_TAG" ]]; then
    lc_info "compose.env IMAGE_TAG already $IMAGE_TAG"
    return 0
  fi
  lc_env_file_set "$LC_COMPOSE_ENV" IMAGE_TAG "$IMAGE_TAG"
  lc_info "compose.env IMAGE_TAG ${old:-<unset>} -> $IMAGE_TAG (plain docker compose commands now resolve this tag)"
}

images_present() {
  local img
  for img in go admin storefront caddy; do
    docker image inspect "${LC_IMAGE_PREFIX:-lc}-$img:$IMAGE_TAG" >/dev/null 2>&1 ||
      lc_die "image ${LC_IMAGE_PREFIX:-lc}-$img:$IMAGE_TAG missing (build-images.sh)"
  done
}

case "$action" in
first)
  if [[ -n "$arg" ]]; then
    [[ "$arg" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || lc_die "usage: deploy.sh first [<tag>]" 2
    export IMAGE_TAG=$arg
  fi
  if ((smoke)); then "$LC_SCRIPTS_DIR/preflight.sh"; else "$LC_SCRIPTS_DIR/preflight.sh" --online; fi
  images_present
  wait_postgres
  run_migrate
  run_provision
  record_tag
  lc_compose up -d
  post_checks
  log_deploy first
  ;;
upgrade)
  [[ "$arg" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || lc_die "usage: deploy.sh upgrade <tag>" 2
  export IMAGE_TAG=$arg
  "$LC_SCRIPTS_DIR/preflight.sh"
  images_present
  "$LC_SCRIPTS_DIR/pg-ops.sh" backup --tag "pre-upgrade-$IMAGE_TAG" || lc_die "mandatory pre-upgrade backup failed; nothing was changed"
  before=$(lc_ledger_count)
  mapfile -t svcs < <(active_app_services)
  lc_info "maintenance window: stopping ${svcs[*]} (Caddy answers 503 + Retry-After)"
  ((${#svcs[@]} == 0)) || lc_compose stop "${svcs[@]}"
  run_migrate
  run_provision
  record_tag
  lc_compose up -d
  post_checks
  lc_info "ledger_count before=$before after=$(lc_ledger_count)"
  log_deploy upgrade
  ;;
app-rollback)
  [[ "$arg" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || lc_die "usage: deploy.sh app-rollback <tag>" 2
  [[ -r "$deploy_log" ]] || lc_die "no deployments.log: cannot prove the ledger is unchanged; forward-fix only"
  recorded=$(awk -F'\t' -v t="$arg" '$3 == t { c = $4 } END { print c }' "$deploy_log")
  [[ -n "$recorded" ]] || lc_die "tag $arg was never deployed here; forward-fix only"
  current=$(lc_ledger_count)
  if [[ "$current" != "$recorded" ]]; then
    lc_error "REFUSED: ledger_count now=$current but $arg ran with $recorded. App rollback over a changed schema is unsafe; forward-fix only (runbook §回滚决策树)."
    trap - EXIT
    exit 1
  fi
  export IMAGE_TAG=$arg
  images_present
  record_tag
  lc_compose up -d
  post_checks
  log_deploy app-rollback
  ;;
*)
  lc_die "usage: deploy.sh [--smoke] first [<tag>] | upgrade <tag> | app-rollback <tag>" 2
  ;;
esac
trap - EXIT
lc_info "$action done (tag $IMAGE_TAG)"
