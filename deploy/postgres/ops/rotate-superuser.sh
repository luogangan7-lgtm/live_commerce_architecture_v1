#!/usr/bin/env bash
# File: deploy/postgres/ops/rotate-superuser.sh
# Purpose: database half of the pg_superuser_password rotation (docs/runbooks/deploy.md §7):
#   read the NEW password from stdin, ALTER ROLE postgres with every statement-logging path
#   switched off for that session (the live server runs log_statement='ddl', which would
#   otherwise write the new password in cleartext to the postgres log / Docker json-file /
#   diagnostics bundles — review P1), then prove the new password authenticates and the old one
#   no longer does. Idempotent: if the new password already works (a rotation interrupted after
#   the ALTER), it only re-verifies.
# Usage: never by hand; deploy/scripts/pg-ops.sh rotate-superuser pipes the new value in:
#   pg-ops.sh rotate-superuser  ->  run_ops /ops/rotate-superuser.sh < <pending secret file>
# Runs as/in: pg-ops container (postgres image, UID 999, network none, /tmp tmpfs), against the
#   LIVE server over the shared unix socket (/var/run/postgresql, pg_hba `local all postgres
#   scram-sha-256`).
# Reads env: none.
# Reads secrets: /run/secrets/pg_superuser_password = the CURRENT password (session login, via
#   the tmpfs pgpass of ops_auth); the NEW password only from stdin -> 0600 file on the /tmp
#   tmpfs (removed on exit). Neither value is ever printed, put in argv or in the environment.
# Output: value-free log lines (new_password=active, old_password=rejected, result=ok).
# Used by: deploy/scripts/pg-ops.sh rotate-superuser; smoke.sh S41.
# Depends on: ops/lib.sh (ops_auth, ops_psql, ops_alter_superuser_password).
# Status: DESIGN; verified by smoke S41 (no secret in postgres logs with log_statement='ddl'
#   live, old password rejected, migrate/provision/pg-ops/lc_psql keep working afterwards).
# Change rules: the new value must stay hex (the manifest kind hex32): dsn_migrate_owner embeds it
#   unquoted in a keyword/value DSN. Never add `set -x`.
export OPS_SCRIPT=rotate-superuser
# shellcheck source=deploy/postgres/ops/lib.sh
source /ops/lib.sh

(($# == 0)) || ops_die "usage: rotate-superuser.sh < new-password" 2
ops_auth
new_file=$(mktemp /tmp/newpw.XXXXXX)
new_pgpass=$(mktemp /tmp/pgpass-new.XXXXXX)
# ops_auth installed a trap for its own pgpass; replace it so every temp file goes on exit.
trap 'rm -f "$OPS_PGPASS" "$new_file" "$new_pgpass"' EXIT

# At most 200 bytes from stdin, trailing newline stripped; must be the manifest's hex32 format.
head -c 200 | tr -d '\r\n' >"$new_file"
[[ "$(<"$new_file")" =~ ^[0-9a-f]{64}$ ]] || ops_die "new password on stdin is not 64 lowercase hex chars (secrets.manifest.tsv kind hex32)"
cmp -s "$new_file" "$OPS_SECRET" && ops_die "new password equals the current one"
printf '*:*:*:postgres:%s\n' "$(<"$new_file")" >"$new_pgpass"
chmod 0600 "$new_pgpass"

# auth_with PGPASSFILE — true when a superuser session opens with that pgpass (socket, scram).
auth_with() { [[ "$(PGPASSFILE=$1 ops_psql -d postgres -c 'SELECT 1' 2>/dev/null)" == 1 ]]; }

if auth_with "$new_pgpass"; then
  ops_log "new_password=already_active (resuming an interrupted rotation; no ALTER needed)"
else
  auth_with "$OPS_PGPASS" || ops_die "neither the current secret file nor the new value authenticates; stop and escalate (runbook deploy.md §7)"
  ops_alter_superuser_password "$new_file" || ops_die "ALTER ROLE postgres failed (statement text is not logged); nothing changed"
fi
auth_with "$new_pgpass" || ops_die "new password does not authenticate after ALTER ROLE"
ops_log "new_password=active"
if auth_with "$OPS_PGPASS"; then ops_die "old password still authenticates"; fi
ops_log "old_password=rejected result=ok"
