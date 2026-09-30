# Unit stripe-live-ops — ops-admin `_FILE` + LIVE pair + allowlist, preflight P06, watchdog W11, runbooks

Role: integration_worker (mid tier). Base `00c1d94`. Worktree `.worktrees/stripe-live-ops`, branch
`unit/stripe-live-ops`. No delegation, no server access, no key of any kind, no `docker` against a real
host (stubs only). Parallel with `stripe-live-core` (disjoint paths; CLI flags are frozen in
`stripe-live-core.md`). Contract: `contracts/stripe-live-enable-v1.md` (v1 FROZEN) incl. §15.

**Goal:** the operator path of §2/§7 runs on the production host exactly as written, a live key only
ever arrives by file path (O-D, LD3), and W11 surfaces LIVE trouble as counts.

## Read (by section)
PROCESS.md; contract §0 (LD1, LD3, LD6, LD9), §2, §5.2 (compose/preflight/ops-admin paragraphs), §5.3
"Deploy" line, §7, §8, §9, §10, §11 SL06/SL09, §13; `stripe-live-core.md` "FROZEN Go interface" (CLI
subcommands/flags) only. By line: `deploy/scripts/ops-admin.sh:1-110` (`usage`, allowlist :41, LIVE
refusal :46-50, `lc_load_env` :54, `need` :57, arms :77-100, :106), `deploy/scripts/preflight.sh:320-345`
(P06), `deploy/scripts/watchdog.sh` (header :3-25, `report`, psql block :112-135), `deploy/scripts/lib.sh`
(`lc_die`, `lc_psql`, `lc_load_env`), `deploy/compose.yml:174-181, 322-356, 420-432`, `deploy/README.md`
rule 20, `deploy/env/api.env.example:30-35`, `docs/runbooks/deploy.md` §5, §6.1, Phase B,
`docs/runbooks/merchant-onboarding.md` O4.

## Defaults adopted
- O1 `_FILE` read happens inside `need` before the TTY prompt: `NAME_FILE` set → absolute, `[[ -f && ! -L ]]`,
  owner = `$(id -u)` (`stat -c %u`, GNU coreutils on the Linux host; macOS is not a target), mode
  `400|600` (`stat -c %a`), exactly one line; `IFS= read -r v <"$file"`; then the existing regex check.
  Errors name the variable only (`lc_die "STRIPE_SECRET_KEY_FILE rejected: mode"`), never the path's
  contents. `set +x` is forced around the read even if the caller exported `SHELLOPTS`.
- O2 Key regex: SANDBOX `^(sk|rk)_test_…`; LIVE (pair set) `^rk_live_…` only; `sk_live_` always refused
  with `stripe_live_key_unrestricted` wording.
- O3 Pair source: compose.env `LC_STRIPE_LIVE_ENABLED` / `LC_STRIPE_LIVE_APPROVAL_REF` (after
  `lc_load_env`), mapped to `COMMERCE_STRIPE_LIVE_ENABLED` / `COMMERCE_STRIPE_LIVE_APPROVAL_REF`, forwarded
  by NAME (`-e NAME`) to every stripe-admin run when set; ref must match `^[A-Za-z0-9._:-]{8,128}$`.
  `live-revoke` and `method` (any flags) never require it.
- O4 W11 runs inside the existing single psql session, one `SELECT 'W11', …` row per §8 signal, id-suffixed
  (`W11a`…`W11f`), thresholds as §8 defaults, overridable by `LC_W11_*` env with integer validation.
  Output = check id + counts only; the alert POST carries ids only (like W1–W10).
- O5 Runbook: new `docs/runbooks/stripe-live.md` = §2 steps 0–8 as copy-paste commands (placeholders
  `<store>`, `<approval uuid>`; never a key), cutover SQL (LD1) as a counts-only query, §7 kill table,
  §10 canary + evidence file shape, key/secret compromise steps. `deploy.md` §5/§6.1/Phase B link to it
  and drop "Stripe 在 LIVE 下被代码拒绝"; README rule 20 → "Two Stripe switches" (§5.2 text).

## FROZEN interface (what core, tests and the integrator rely on)
```sh
# ops-admin.sh
STRIPE_SECRET_KEY_FILE=/abs/path     deploy/scripts/ops-admin.sh stripe-admin register --environment LIVE …
STRIPE_WEBHOOK_SECRET_FILE=/abs/path deploy/scripts/ops-admin.sh stripe-admin webhook  --profile LIVE …
deploy/scripts/ops-admin.sh stripe-admin live-approve|live-canary …   # pair required (ops-admin + CLI)
deploy/scripts/ops-admin.sh stripe-admin live-revoke|method …         # pair never required
# exit 0 = CLI ran; non-zero with one fixed line on stderr naming the variable/rule, never a value
# watchdog.sh: lines "W11<x> PASS|FAIL <name>=<count> …"; exit != 0 when any W11<x> FAIL
# preflight.sh P06: LC_STRIPE_ENABLED=1 requires (SANDBOX ∧ payments-sandbox) ∨ (LIVE ∧ payments-live ∧ pair)
```

## Build
1. `ops-admin.sh`: header/`usage()` text (:7, :33), allowlist (:41) + `live-approve|live-canary|live-revoke`,
   LIVE refusal (:46-50) → admitted only with the pair (`--profile LIVE`, `--environment LIVE`, and the two
   live-* subcommands), `_FILE` in `need` (O1), key regex (O2), pair mapping + forwarding (O3). No `need`
   arm for the three live-* subcommands (they take no secret; the keyring is already mounted,
   compose.yml:426-428).
2. `preflight.sh` P06 (:336-338) both directions, reading the pair from compose.env keys.
3. `watchdog.sh` W11 (O4): §8 six signals, `environment='LIVE'` only; header comment lists W11.
4. Docs (O5): `deploy/README.md` rule 20, `deploy/env/api.env.example:33` (LIVE admits Stripe only with the
   pair), `docs/runbooks/{deploy.md,merchant-onboarding.md}` edits, new `docs/runbooks/stripe-live.md`.
Shell style: `set -euo pipefail` as today, `shellcheck` clean; every changed block has a one-line why
comment citing the contract section (`# LD3/O-D: key by file path; never echoed`).

## Write paths
`deploy/scripts/{ops-admin.sh,preflight.sh,watchdog.sh}`, `deploy/README.md`, `deploy/env/api.env.example`,
`docs/runbooks/{deploy.md,merchant-onboarding.md,stripe-live.md}`, `output/stripe-live-ops/**`.
Forbidden: `deploy/compose.yml` (integrator), `deploy/caddy/**`, Go, SQL, `tests/**`, contracts,
`deploy/scripts/lib.sh` (ask the integrator if a helper is missing).

## Gates
Implementer: `shellcheck` + a local self-check log per script (stubbed `docker`, temp compose.env) showing one
refusal and one admit for `_FILE`, pair, allowlist, P06, W11 — evidence only, not the gate.
Independent (stripe-live-tests): **SL06** shell half (ops-admin `_FILE`/allowlist/pair, P06 both
directions) and **SL09** (W11 red/green on seeded PG). Owner: SL-LIVE02 kill drill uses §7 as written here.

## Verify
```sh
shellcheck deploy/scripts/ops-admin.sh deploy/scripts/preflight.sh deploy/scripts/watchdog.sh
bash -n deploy/scripts/ops-admin.sh deploy/scripts/preflight.sh deploy/scripts/watchdog.sh
python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/stripe-live-ops/`.

## Integrator hooks (only the integrator edits these)
`deploy/compose.yml` (§5.2), exactly:
- `api` (:179-180): `COMMERCE_STRIPE_CHECKOUT_ENABLED: ${LC_STRIPE_CHECKOUT_ENABLED:-${LC_STRIPE_ENABLED:-0}}`;
  add `COMMERCE_STRIPE_LIVE_ENABLED: ${LC_STRIPE_LIVE_ENABLED:-0}`,
  `COMMERCE_STRIPE_LIVE_APPROVAL_REF: ${LC_STRIPE_LIVE_APPROVAL_REF:-}`; comment "two switches (LD6)".
- `payment-worker-live` (:347-353): add `COMMERCE_STRIPE_ENABLED: ${LC_STRIPE_ENABLED:-0}` and the same two
  pair keys; fix the stale comment at :327-328.
- `stripe-admin` service: unchanged (ops-admin forwards the pair by name).
Shared-file order: auth-core's preflight P08/P15/PA rule and merchant-onboarding sections are integrator
hooks applied after this unit merges; this unit edits only P06 and onboarding O4.
Also: `deploy/secrets.manifest.tsv` unchanged (no new secret: the live key is sealed into PG, O-D);
cron entry for watchdog unchanged (W11 rides the existing run).

## Order / Non-goals / Return
Author at dispatch; SL06/SL09 run after merge. No compose edit, no host changes, no new service or table,
no Stripe Dashboard steps (owner). Return commit SHA, model/reasoning, base, paths, commands + exits,
self-check logs, O1–O5 as applied, risks (e.g. `stat` flavour), NOT_RUN.
