# Unit owner-provisioning — `0065_owner_provisioning.sql` (store creator grants)

Role: commerce_worker (mid tier; small unit). Base = integrator-recorded SHA at dispatch, which must
already contain 0062 (refund-core F1), 0063 (fulfilment-core) and 0064 (meta-claims-intake).
Worktree `.worktrees/owner-provisioning`, branch `unit/owner-provisioning`. No delegation or network.
Contract: `contracts/stripe-refund-v1.md` §12 and ruling "Owner provisioning"; `contracts/
manual-fulfilment-v1.md` MD8 + ruling M-1. Nothing else.

**Goal:** a merchant onboarded through `identity.create_initial_store` can immediately use live
selling, refunds, shipping and export on the store they created. Nobody else gains anything.

## Read
`docs/delivery/PROCESS.md`; the two contract sections above. Code by symbol: the latest
`CREATE OR REPLACE FUNCTION identity.create_initial_store` (today `migrations/0027_merchant_orders.sql`;
grep 0028–0064 for any later successor and use the newest body), its owner/ACL/`COMMENT ON`;
`store_grants_permission_check` as installed after 0064 (`pg_get_constraintdef`, do not assume 0033).
Tests that encode the creator grant set: `tests/foundation/identity_integration_test.go`
(`initialStoreGrants`), `tests/foundation/merchant_orders_test.go` (onboarding cases); grep
`tests/foundation` for `live:manage`, `payments:refund`, `fulfillment:write`, `orders:export` absence
assertions on creators.

## Build
- `migrations/0065_owner_provisioning.sql`: `CREATE OR REPLACE FUNCTION
  identity.create_initial_store(bytea,text,bytea,text,text,text,text)` = the newest body **byte-for-byte**
  except the grant array: the 0027 list (`store:read, audit:read, audit:write, catalog:read,
  catalog:write, inventory:read, inventory:write, inventory:reserve, pricing:read, pricing:write,
  integration:read, integration:manage, orders:read`) + `live:read`, `live:manage`, `payments:refund`,
  `fulfillment:write`, `orders:export`. Same owner (`commerce_identity_writer`), `search_path`, ACL,
  lock order, idempotent replay, audit `merchant.store_created`. Re-issue the ALTER OWNER / REVOKE /
  GRANT / `COMMENT ON` lines unchanged, with the comment amended to name 0065 and "no backfill".
- A guard at the top of 0065: `DO $$ … RAISE EXCEPTION` if any of the five permissions is missing from
  the live `store_grants_permission_check` (fails loudly if applied out of order).
- No `INSERT`/`UPDATE` of `identity.store_grants`, no backfill, no change to `cmd/admin-fixture`.
- Existing tests: update only the expected creator set (e.g. `initialStoreGrants`) and any assertion
  that a *creator* lacks one of the five; keep every assertion that other members, pre-0065
  principals, other stores or other tenants lack them. List each changed line in the return.

## Write paths
`migrations/0065_owner_provisioning.sql`, `tests/foundation/identity_integration_test.go`,
`tests/foundation/merchant_orders_test.go` (expected-set lines only), any other `tests/foundation`
file only for a creator-set expectation found by the grep above (name it in the return),
`output/owner-provisioning/**`. Nothing else. OP01 itself is written by refund-fulfilment-tests.

## Verify
```sh
diff <(sed -n '/FUNCTION identity.create_initial_store/,/\$\$;/p' migrations/<newest>.sql) \
  <(sed -n '/FUNCTION identity.create_initial_store/,/\$\$;/p' migrations/0065_owner_provisioning.sql) \
  > output/owner-provisioning/body.diff   # must show only the array lines
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(Identity|MerchantOrders|<each test you changed>)'
python3 scripts/check_packet.py
```

## Order
Last migration of this batch: 0062 → 0063 → 0064 → **0065**. Dispatch only after all three are merged
(meta-claims-intake owns 0064 and may re-derive the permission CHECK; do not assume its content).

## Non-goals / Return
No new permission names, no per-principal refund limit (owner ruling required before any non-creator
receives `payments:refund`), no admin UI for grants. Return commit SHA, model/reasoning, base, the
body diff path, changed test lines, commands + exit codes + PASS/FAIL/SKIP counts, risks, NOT_RUN.
