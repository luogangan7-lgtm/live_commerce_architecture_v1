#!/usr/bin/env bash
# File: deploy/scripts/watchdog.sh
# Purpose: periodic health checks W1-W10 (deploy-design §16.3). Exit != 0 when any check fails,
#   so cron mails it / systemd marks it; optionally POSTs ONLY the failing check ids to
#   LC_ALERT_WEBHOOK_URL and writes one syslog line via `logger`.
#   W1 services running/healthy + restart counts unchanged   W2 disk > 80 %
#   W3 newest dump > 26 h / newest base > 8 d                  W4 WAL archiving failing
#   W5 TLS certificate expiry < LC_TLS_MIN_DAYS (14)          W6 River backlog per schema
#   W7 transactions / idle-in-transaction older than 5 min     W8 readiness gates false
#   W9 connections > 80 % of max_connections                 W10 image tag drift: a running
#   ${LC_IMAGE_PREFIX}-* container whose tag differs from the IMAGE_TAG line IN compose.env (the
#   file, not the environment: deploy.sh keeps it = deployed tag, review P1; drift means a plain
#   `docker compose up` would change images)
#   W11a-f Stripe LIVE trouble as counts (stripe-live-enable-v1 §8, environment='LIVE' only; zero rows = PASS on
#   SANDBOX): a UNKNOWN operations, b review cases, c REVIEW_REQUIRED work items, d unfinished refunds,
#   e quarantined/ignored webhook receipts, f LIVE endpoint disabled under an enabled+visible method.
#   Thresholds are the §8 defaults, overridable by LC_W11_* (integers only); output is check id + counts.
#   W11 knobs (defaults = §8): LC_W11_UNKNOWN_OP_MINUTES 60, LC_W11_UNKNOWN_OP_MAX 0, LC_W11_REVIEW_HOURS 24,
#   LC_W11_REVIEW_MAX 0, LC_W11_WORK_ITEM_MAX 0, LC_W11_REFUND_HOURS 24, LC_W11_REFUND_MAX 0,
#   LC_W11_RECEIPT_MINUTES 60, LC_W11_RECEIPT_MAX 5, LC_W11_ENDPOINT_MAX 0.
# Usage: watchdog.sh        (cron: */5 * * * *, deploy/host/crontab.example)
# Runs as/in: deploy host (root); DB checks (W3, W4, W6-W9) only where postgres is an active
#   service (single host / DB host). SQL runs via lc_psql inside the postgres container over
#   the unix socket; no query text or row values are printed.
# Reads env: compose.env (hosts, LC_HTTPS_PORT, LC_BACKUP_DIR, LC_STATE_DIR,
#   LC_ALERT_WEBHOOK_URL), LC_TLS_MIN_DAYS (default 14), LC_RETRYABLE_MAX (default 50), LC_W11_* (below).
# Reads secrets: none on the host (the postgres container reads its own password file).
# State: ${LC_STATE_DIR}/watchdog.state (restart counts, last run time for W6 discarded jobs).
# Used by: cron; smoke.sh S35; collect-diagnostics.sh; docs/runbooks/incident.md §分诊.
# Depends on: lib.sh, docker, openssl (W5), df, curl (webhook), logger (optional).
# Status: DESIGN; verified by smoke S35 (healthy -> 0; stopped worker -> W1 failure) and S43
#   (W10 PASS on the deployed tag, FAIL against a compose.env naming another tag).
# Change rules: every new long-running service is covered by W1 automatically; new River
#   schemas must be added to W6.
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
lc_load_env "$LC_COMPOSE_ENV"

[[ "${LC_REQUIRE_MEDIA_GATE:-0}" =~ ^[01]$ ]] || lc_die "LC_REQUIRE_MEDIA_GATE must be 0 or 1"
state_dir=${LC_STATE_DIR:-/var/lib/live-commerce}
state="$state_dir/watchdog.state"
min_days=${LC_TLS_MIN_DAYS:-14}
retry_max=${LC_RETRYABLE_MAX:-50}
# O4/§8: W11 thresholds are spliced into SQL text, so every override must be a plain integer (no injection surface).
w11_int() { # NAME DEFAULT
  local v=${!1:-$2}
  [[ "$v" =~ ^[0-9]{1,6}$ ]] || lc_die "$1 must be an integer (0..999999)"
  printf '%s' "$v"
}
w11_op_min=$(w11_int LC_W11_UNKNOWN_OP_MINUTES 60)
w11_op_max=$(w11_int LC_W11_UNKNOWN_OP_MAX 0)
w11_rev_h=$(w11_int LC_W11_REVIEW_HOURS 24)
w11_rev_max=$(w11_int LC_W11_REVIEW_MAX 0)
w11_wi_max=$(w11_int LC_W11_WORK_ITEM_MAX 0)
w11_ref_h=$(w11_int LC_W11_REFUND_HOURS 24)
w11_ref_max=$(w11_int LC_W11_REFUND_MAX 0)
w11_rc_min=$(w11_int LC_W11_RECEIPT_MINUTES 60)
w11_rc_max=$(w11_int LC_W11_RECEIPT_MAX 5)
w11_ep_max=$(w11_int LC_W11_ENDPOINT_MAX 0)
failed=()
report() { # id status detail
  printf '%s %s %s\n' "$1" "$2" "$3"
  if [[ "$2" == FAIL && " ${failed[*]} " != *" $1 "* ]]; then failed+=("$1"); fi
  return 0
}
declare -A prev=()
last_run=""
if [[ -r "$state" ]]; then
  while IFS=$'\t' read -r k v; do
    if [[ "$k" == last_run ]]; then last_run=$v; elif [[ -n "$k" ]]; then prev[$k]=$v; fi
  done <"$state"
fi
declare -A now_counts=()
now_ts=$(lc_ts)

# ---- W1 services -----------------------------------------------------------------------------------
w1=0
while IFS= read -r s; do
  cid=$(lc_compose ps -q "$s" 2>/dev/null || true)
  if [[ -z "$cid" ]]; then
    report W1 FAIL "$s not created"
    w1=1
    continue
  fi
  info=$(docker inspect -f '{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}|{{.RestartCount}}' "$cid")
  IFS='|' read -r st health restarts <<<"$info"
  now_counts[$s]=$restarts
  if [[ "$st" != running ]]; then
    report W1 FAIL "$s state=$st"
    w1=1
  elif [[ "$health" != healthy && "$health" != none ]]; then
    report W1 FAIL "$s health=$health"
    w1=1
  elif [[ -n "${prev[$s]:-}" && "$restarts" -gt "${prev[$s]}" ]]; then
    report W1 FAIL "$s restarted $((restarts - prev[$s])) time(s) since last run"
    w1=1
  fi
done < <(lc_long_running_services)
((w1)) || report W1 PASS "services running/healthy"

# ---- W10 image tag drift -------------------------------------------------------------------------------
want_tag=$(lc_env_file_get "$LC_COMPOSE_ENV" IMAGE_TAG || true)
prefix="${LC_IMAGE_PREFIX:-lc}-"
w10=0
while IFS= read -r s; do
  cid=$(lc_compose ps -q "$s" 2>/dev/null || true)
  [[ -n "$cid" ]] || continue # absence is W1's finding
  image=$(docker inspect -f '{{.Config.Image}}' "$cid" 2>/dev/null || true)
  if [[ "$image" == "$prefix"* && "$image" != *":$want_tag" ]]; then
    report W10 FAIL "$s runs tag ${image##*:} but compose.env IMAGE_TAG=${want_tag:-<unset>}"
    w10=1
  fi
done < <(lc_long_running_services)
((w10)) || report W10 PASS "all ${prefix}* containers run compose.env IMAGE_TAG=${want_tag:-<unset>}"

# ---- W2 disk ------------------------------------------------------------------------------------------
docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
for path in "$docker_root" "${LC_BACKUP_DIR:-/}"; do
  used=$(df -P "$path" 2>/dev/null | awk 'NR==2 { gsub("%","",$5); print $5 }')
  if [[ "$used" =~ ^[0-9]+$ ]] && ((used <= 80)); then report W2 PASS "$path used=${used}%"; else report W2 FAIL "$path used=${used:-unknown}%"; fi
done

db=0
lc_service_active postgres && db=1
if ((db)); then
  # ---- W3 backup freshness ------------------------------------------------------------------------------
  newest() { find "$1" -mindepth 1 -maxdepth 1 -type d -name '20*' -printf '%T@\n' 2>/dev/null | sort -n | tail -n1 | cut -d. -f1; }
  now=$(date +%s)
  d=$(newest "$LC_BACKUP_DIR/dumps")
  if [[ -n "$d" ]] && ((now - d <= 26 * 3600)); then report W3 PASS "dump age_h=$(((now - d) / 3600))"; else report W3 FAIL "no dump within 26 h"; fi
  b=$(newest "$LC_BACKUP_DIR/base")
  if [[ -n "$b" ]] && ((now - b <= 8 * 86400)); then report W3 PASS "base age_d=$(((now - b) / 86400))"; else report W3 FAIL "no base backup within 8 d"; fi

  # ---- W4, W6-W9, W11 via one psql session (counts/booleans only) ---------------------------------------------
  since=${last_run:-$now_ts}
  sql=$(
    cat <<SQL
SELECT 'W4', CASE WHEN last_failed_time IS NOT NULL AND (last_archived_time IS NULL OR last_failed_time > last_archived_time)
                  THEN 'FAIL' ELSE 'PASS' END, 'archived=' || archived_count || ' failed=' || failed_count FROM pg_stat_archiver;
SELECT 'W7', CASE WHEN count(*) > 0 THEN 'FAIL' ELSE 'PASS' END, 'old_transactions=' || count(*)
  FROM pg_stat_activity WHERE backend_type = 'client backend' AND xact_start < now() - interval '5 minutes'
   AND coalesce(application_name, '') NOT IN ('pg_dump', 'lc-pg-ops');
SELECT 'W8', CASE WHEN integration.payment_queue_ready() AND checkout.expiry_queue_ready() AND meta_inbox.runtime_ready()
                  AND (live.media_plan_ready() OR '${LC_REQUIRE_MEDIA_GATE:-0}' = '0') THEN 'PASS' ELSE 'FAIL' END,
       'readiness gates (media_plan_ready=' || live.media_plan_ready() || ', required=${LC_REQUIRE_MEDIA_GATE:-0})';
SELECT 'W9', CASE WHEN count(*) > 0.8 * current_setting('max_connections')::int THEN 'FAIL' ELSE 'PASS' END,
       'connections=' || count(*) || '/' || current_setting('max_connections') FROM pg_stat_activity;

-- W11 (stripe-live-enable-v1 §8): LIVE only; counts only, no ids/rows leave the database. Superuser session, so RLS
-- does not hide rows. a: LIVE Stripe operations (session create or refund) stuck UNKNOWN. A settled operation also ends
-- UNKNOWN, with result_code stripe_terminal_observed / stripe_refund_terminal, which integration.finish_stripe_query
-- (0061) / finish_stripe_refund (0062) only accept once the terminal payments.facts / refund_facts row exists: not trouble.
SELECT 'W11a', CASE WHEN c > ${w11_op_max} THEN 'FAIL' ELSE 'PASS' END, 'unknown_ops_over_${w11_op_min}m=' || c || ' max=${w11_op_max}'
  FROM (SELECT count(*) AS c FROM integration.operations o
         WHERE o.provider = 'stripe' AND o.state = 'UNKNOWN' AND o.created_at < now() - interval '${w11_op_min} minutes'
           AND o.result_code NOT IN ('stripe_terminal_observed', 'stripe_refund_terminal')
           AND (EXISTS (SELECT 1 FROM payments.stripe_sessions s WHERE s.attempt_id = o.id AND s.environment = 'LIVE')
             OR EXISTS (SELECT 1 FROM payments.stripe_refunds r WHERE r.id = o.id AND r.environment = 'LIVE'))) x;
-- b: review cases (incl. CLOSURE_CONTRADICTED) opened on LIVE Stripe attempts inside the window.
SELECT 'W11b', CASE WHEN c > ${w11_rev_max} THEN 'FAIL' ELSE 'PASS' END, 'review_cases_${w11_rev_h}h=' || c || ' max=${w11_rev_max}'
  FROM (SELECT count(*) AS c FROM payments.review_cases rc
          JOIN checkout.payment_attempts a ON a.tenant_id = rc.tenant_id AND a.store_id = rc.store_id AND a.id = rc.attempt_id
         WHERE a.environment = 'LIVE' AND a.method_code = 'stripe_checkout'
           AND rc.created_at > now() - interval '${w11_rev_h} hours') x;
-- c: paid orders whose fulfilment hand-off needs a human (REVIEW_REQUIRED), any age.
SELECT 'W11c', CASE WHEN c > ${w11_wi_max} THEN 'FAIL' ELSE 'PASS' END, 'work_items_review_required=' || c || ' max=${w11_wi_max}'
  FROM (SELECT count(*) AS c FROM fulfillment.payment_work_items w
          JOIN checkout.payment_attempts a ON a.tenant_id = w.tenant_id AND a.store_id = w.store_id AND a.id = w.attempt_id
         WHERE a.environment = 'LIVE' AND a.method_code = 'stripe_checkout' AND w.state = 'REVIEW_REQUIRED') x;
-- d: LIVE refunds with no terminal fact after the window, plus open REFUND_UNRESOLVED/REFUND_HISTORY reviews (any age).
SELECT 'W11d', CASE WHEN c > ${w11_ref_max} THEN 'FAIL' ELSE 'PASS' END, 'refunds_unfinished_${w11_ref_h}h_or_review=' || c || ' max=${w11_ref_max}'
  FROM (SELECT (SELECT count(*) FROM payments.stripe_refunds r
                 WHERE r.environment = 'LIVE' AND r.requested_at < now() - interval '${w11_ref_h} hours'
                   AND NOT EXISTS (SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id = r.tenant_id AND f.store_id = r.store_id AND f.refund_id = r.id))
             + (SELECT count(*) FROM payments.review_cases rc
                  JOIN checkout.payment_attempts a ON a.tenant_id = rc.tenant_id AND a.store_id = rc.store_id AND a.id = rc.attempt_id
                 WHERE a.environment = 'LIVE' AND rc.reason IN ('REFUND_UNRESOLVED', 'REFUND_HISTORY')) AS c) x;
-- e: LIVE webhook receipts that were quarantined or ignored in the last window.
SELECT 'W11e', CASE WHEN c > ${w11_rc_max} THEN 'FAIL' ELSE 'PASS' END, 'receipts_quarantined_or_ignored_${w11_rc_min}m=' || c || ' max=${w11_rc_max}'
  FROM (SELECT count(*) AS c FROM payments.stripe_webhook_receipts
         WHERE environment = 'LIVE' AND disposition IN ('QUARANTINED', 'IGNORED')
           AND received_at > now() - interval '${w11_rc_min} minutes') x;
-- f: a LIVE webhook endpoint is disabled while the store still offers Stripe (head method enabled + visible = CANARY/OPEN).
SELECT 'W11f', CASE WHEN c > ${w11_ep_max} THEN 'FAIL' ELSE 'PASS' END, 'endpoints_disabled_under_open_method=' || c || ' max=${w11_ep_max}'
  FROM (SELECT count(DISTINCT e.endpoint_id) AS c FROM payments.stripe_webhook_endpoints e
          JOIN payments.method_versions m ON m.tenant_id = e.tenant_id AND m.store_id = e.store_id AND m.connection_id = e.connection_id
          JOIN payments.method_heads h ON h.tenant_id = m.tenant_id AND h.store_id = m.store_id AND h.market_id = m.market_id
           AND h.country = m.country AND h.code = m.code AND h.current_version = m.version
         WHERE e.environment = 'LIVE' AND NOT e.enabled AND m.provider = 'stripe' AND m.environment = 'LIVE'
           AND m.enabled AND m.visible) x;
SQL
  )
  for schema in river river_payment river_expiry river_meta river_media; do
    sql+="
SELECT 'W6', CASE WHEN to_regclass('$schema.river_job') IS NULL THEN 'PASS' ELSE 'CHECK' END, '$schema';"
  done
  if out=$(lc_psql <<<"$sql" 2>/dev/null); then
    while IFS='|' read -r id st detail; do
      [[ -z "$id" ]] && continue
      if [[ "$id" == W6 && "$st" == CHECK ]]; then
        q="SELECT count(*) FILTER (WHERE state = 'available' AND scheduled_at < now() - interval '10 minutes'),
                  count(*) FILTER (WHERE state = 'retryable'),
                  count(*) FILTER (WHERE state = 'discarded' AND finalized_at > '$since'::timestamptz)
           FROM $detail.river_job;"
        IFS='|' read -r stale retry discarded <<<"$(lc_psql <<<"$q" 2>/dev/null || echo 'x|x|x')"
        if [[ "$stale" == 0 && "$retry" =~ ^[0-9]+$ && "$retry" -le "$retry_max" && "$discarded" == 0 ]]; then
          report W6 PASS "$detail stale_available=0 retryable=$retry new_discarded=0"
        else
          report W6 FAIL "$detail stale_available=$stale retryable=$retry new_discarded=$discarded"
        fi
      elif [[ "$id" == W6 ]]; then
        report W6 PASS "$detail (schema absent)"
      else
        report "$id" "$st" "$detail"
      fi
    done <<<"$out"
  else
    report W4 FAIL "postgres query failed (container down or not ready)"
  fi
else
  report W3 SKIP "postgres not an active service on this host"
fi

# ---- W5 TLS expiry --------------------------------------------------------------------------------------
if lc_service_active caddy; then
  for h in "$LC_ADMIN_HOST" "$LC_STORE_HOST" "$LC_API_HOST" "$LC_HOOKS_HOST"; do
    end=$(openssl s_client -connect "127.0.0.1:${LC_HTTPS_PORT:-443}" -servername "$h" </dev/null 2>/dev/null |
      openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)
    if [[ -z "$end" ]]; then
      report W5 FAIL "$h no certificate served"
      continue
    fi
    left=$((($(date -d "$end" +%s) - $(date +%s)) / 86400))
    if (($(date -d "$end" +%s) > $(date +%s))) && ((left >= min_days)); then report W5 PASS "$h days_left=$left"; else report W5 FAIL "$h days_left=$left"; fi
  done
fi

# ---- persist state ------------------------------------------------------------------------------------
if mkdir -p "$state_dir" 2>/dev/null; then
  {
    printf 'last_run\t%s\n' "$now_ts"
    for s in "${!now_counts[@]}"; do printf '%s\t%s\n' "$s" "${now_counts[$s]}"; done
  } >"$state.tmp" && mv -f "$state.tmp" "$state"
fi

if ((${#failed[@]})); then
  msg="live-commerce watchdog FAIL: ${failed[*]}"
  command -v logger >/dev/null 2>&1 && logger -t live-commerce-watchdog -- "$msg" || true
  if [[ -n "${LC_ALERT_WEBHOOK_URL:-}" ]]; then
    curl -fsS --max-time 10 -H 'Content-Type: application/json' \
      -d "{\"source\":\"live-commerce-watchdog\",\"failed\":\"${failed[*]}\"}" "$LC_ALERT_WEBHOOK_URL" >/dev/null || true
  fi
  echo "$msg" >&2
  exit 1
fi
echo "watchdog PASS"
