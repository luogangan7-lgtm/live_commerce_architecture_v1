#!/usr/bin/env bash
# test-focused.sh — run a focused subset of ./tests/foundation (or any package list)
# against a task-owned, disposable PG 18.6 container.
#
# Why this exists: scripts/dev/test-local.sh runs whole suites (~25 min). Sub-agents
# iterating on one unit need a 10–60 s loop. Provisioning is copied from
# test-local.sh (same pinned image digest, tmpfs, 1g memcg, loopback port, fixture
# guard DSN) so a focused PASS means the same environment as the full suite.
#
# Serialization: Docker Desktop on the dev Mac has ~1.9 GB for all containers and
# each test PG is capped at 1 GB, so only ONE focused run may hold a PG at a time.
# A mkdir lock (atomic on APFS) in $LC_TEST_LOCK_DIR serializes concurrent agents;
# waiters poll every 2 s. A stale lock (holder PID gone) is taken over.
#
# Usage:  bash scripts/dev/test-focused.sh '<go test -run regex>' [packages...]
#   default packages: ./tests/foundation
# Env:    LC_FOCUSED_TIMEOUT (default 900s), LC_FOCUSED_TAGS (e.g. browser)
# Exit:   go test's exit code. Never reports PASS when zero tests ran (checked below).
set -euo pipefail
# stripe-live-enable-v1 §11 harness guard: tests never run with a live Stripe key in the environment
# (value never printed).
if env | grep -qE '=(sk|rk)_live_'; then echo 'refused: live key in test environment' >&2; exit 2; fi

run_regex="${1:?usage: test-focused.sh '<-run regex>' [packages...]}"
shift || true
packages=("${@:-./tests/foundation}")
repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

lock_dir="${LC_TEST_LOCK_DIR:-${TMPDIR:-/tmp}/lc-test-pg.lock}"
until mkdir "$lock_dir" 2>/dev/null; do
  holder="$(cat "$lock_dir/pid" 2>/dev/null || true)"
  # Stale if the holder died, or its PID was reused by an unrelated process (observed
  # 2026-09-29: a dead holder's PID reused by `sleep 3000` blocked every agent).
  if [[ -n "$holder" ]] && { ! kill -0 "$holder" 2>/dev/null ||
      ! ps -o command= -p "$holder" 2>/dev/null | grep -q 'test-focused\.sh'; }; then
    rm -rf "$lock_dir"
    continue
  fi
  # A holder that died between mkdir and writing its pid leaves no pid file.
  if [[ -z "$holder" ]] && [[ -n "$(find "$lock_dir" -maxdepth 0 -mmin +1 2>/dev/null)" ]]; then
    rm -rf "$lock_dir"
    continue
  fi
  sleep 2
done
echo $$ > "$lock_dir/pid"

container="lc-focused-$$"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$lock_dir"
}
trap cleanup EXIT INT TERM

export POSTGRES_PASSWORD
POSTGRES_PASSWORD="$(openssl rand -hex 24)"
docker run -d --pull=never --name "$container" \
  --label "livecommerce.fixture=$container" --memory=1g --cpus=1 --pids-limit=128 \
  --tmpfs /var/lib/postgresql:rw,size=268435456 \
  -e POSTGRES_PASSWORD -e POSTGRES_DB=lc_foundation_test \
  -p 127.0.0.1::5432 \
  postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280 \
  -c shared_buffers=32MB -c max_connections=60 >/dev/null
for ((i = 0; i < 40; i++)); do
  docker exec "$container" pg_isready -h 127.0.0.1 -U postgres -d lc_foundation_test >/dev/null 2>&1 && break
  sleep 0.5
done
docker exec "$container" pg_isready -h 127.0.0.1 -U postgres -d lc_foundation_test >/dev/null
docker exec "$container" createdb -U postgres lc_admin_fixture
port="$(docker port "$container" 5432/tcp)"
[[ "$port" == 127.0.0.1:* ]]

export LC_TEST_DATABASE_URL="postgres://postgres:${POSTGRES_PASSWORD}@${port}/lc_foundation_test?sslmode=disable"
export LC_TEST_DATABASE_ALLOWED=1 COMMERCE_FIXTURE_ALLOWED=1
export LC_ADMIN_GUARD_DSN="postgres://postgres:${POSTGRES_PASSWORD}@${port}/lc_admin_fixture?sslmode=disable"

tags=()
[[ -n "${LC_FOCUSED_TAGS:-}" ]] && tags=(-tags "$LC_FOCUSED_TAGS")
log="$(mktemp)"
set +e
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ${tags[@]+"${tags[@]}"} -timeout="${LC_FOCUSED_TIMEOUT:-900s}" \
  -run "$run_regex" -v "${packages[@]}" 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e
passed="$(grep -c '^--- PASS' "$log" || true)"
failed="$(grep -c '^--- FAIL' "$log" || true)"
skipped="$(grep -c '^--- SKIP' "$log" || true)"
echo "test-focused: top-level PASS=$passed FAIL=$failed SKIP=$skipped exit=$status"
if [[ "$status" == 0 && "$failed" != 0 ]]; then
  # observed 2026-09-30: FAIL=1 printed with exit=0 (a failed top-level test must never end as PASS)
  echo "test-focused: $failed top-level test(s) failed but go test reported exit 0 — treated as FAILURE" >&2
  exit 1
fi
if [[ "$status" == 0 && "$passed" == 0 ]]; then
  echo "test-focused: zero tests matched '$run_regex' — treated as FAILURE, not PASS" >&2
  exit 3
fi
exit "$status"
