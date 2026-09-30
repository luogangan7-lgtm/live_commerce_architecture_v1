#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
command -v docker >/dev/null
command -v go >/dev/null
fixture_name="lc-admin-fixture-$$"
fixture_owned=0
fixture_pid=""
fixture_dir="$(mktemp -d /tmp/livecommerce-admin-fixture.XXXXXX)"
fixture_env="$fixture_dir/session.env"
fixture_owner_file="$fixture_dir/owner.dsn"
touch "$fixture_env"
chmod 600 "$fixture_env"
cleanup() {
  if [[ -n "$fixture_pid" ]]; then kill "$fixture_pid" 2>/dev/null || true; wait "$fixture_pid" 2>/dev/null || true; fi
  if [[ "$fixture_owned" == 1 ]] && [[ "$(docker inspect -f '{{index .Config.Labels "livecommerce.fixture"}}' "$fixture_name" 2>/dev/null || true)" == "$fixture_name" ]]; then docker rm -f "$fixture_name" >/dev/null; fi
  # Only files this invocation created; no recursive cleanup or shared caches.
  [[ -f "$fixture_env" ]] && rm "$fixture_env"
  [[ -f "$fixture_owner_file" ]] && rm "$fixture_owner_file"
  [[ -f "$fixture_dir/server" ]] && rm "$fixture_dir/server"
  rmdir "$fixture_dir" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export POSTGRES_PASSWORD
POSTGRES_PASSWORD="$(openssl rand -hex 24)"
docker run -d --pull=never --name "$fixture_name" --label "livecommerce.fixture=$fixture_name" \
  --memory=512m --cpus=1 --pids-limit=128 --tmpfs /var/lib/postgresql:rw,size=268435456 \
  -e POSTGRES_PASSWORD -e POSTGRES_DB=lc_admin_fixture -p 127.0.0.1::5432 \
  postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280 \
  -c shared_buffers=32MB -c max_connections=60 >/dev/null
fixture_owned=1
# Ignore the socket-only initdb server: it shuts down before the final TCP server.
for ((attempt=0;attempt<40;attempt++)); do
  if docker exec "$fixture_name" pg_isready -h 127.0.0.1 -U postgres -d lc_admin_fixture >/dev/null 2>&1; then break; fi
  sleep .5
done
docker exec "$fixture_name" pg_isready -h 127.0.0.1 -U postgres -d lc_admin_fixture >/dev/null
fixture_host="$(docker port "$fixture_name" 5432/tcp)"
[[ "$fixture_host" == 127.0.0.1:* ]]
GOTOOLCHAIN=go1.27.1 go build -o "$fixture_dir/server" ./cmd/admin-fixture
fixture_password="$POSTGRES_PASSWORD"
unset POSTGRES_PASSWORD
# macOS may expose exec-time environment even after Go Unsetenv. Pass only a
# private, one-use file path; the server reads and unlinks it before connecting.
(umask 077; printf '%s' "postgres://postgres:${fixture_password}@${fixture_host}/lc_admin_fixture?sslmode=disable" > "$fixture_owner_file")
COMMERCE_FIXTURE_ALLOWED=1 FIXTURE_OWNER_FILE="$fixture_owner_file" FIXTURE_ENV_PATH="$fixture_env" "$fixture_dir/server" &
fixture_pid=$!
unset fixture_password
wait "$fixture_pid"
