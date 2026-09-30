<!--
File: deploy/README.md
Purpose: index of the deployment package, quick start, status matrix, deploy-only dependency
  ledger, verification evidence summary, deviations from the design, and open blockers.
Runs as/in: documentation (read by operators, integrator, reviewers).
Reads env / secrets: none.
Used by: docs/runbooks/*.md link here; integrator copies the dependency ledger into
  docs/implementation/dependencies.md (I5).
Status: DESIGN. Nothing here has been deployed to production (owner approval required).
Change rules: update the ledger + status matrix with every digest bump or new file.
-->
# live-commerce deploy package (T22)

Single-region deployment on **one Linux host running Docker Compose**, with a documented 2-host
variant (DB on its own host). PostgreSQL 18 is the only transactional source of truth, and River
queues live inside it. There is no Kafka, Redis or service mesh (AGENTS.md).

> **Not HA.** 架构.md §4.2 describes Compose as a dev/repeatable-acceptance tool. Running
> production on it needs owner ADR **O1** (blocker B8). **No production deploy has been run.**
> A real deploy needs explicit owner approval.

Runbooks (Chinese): [deploy](../docs/runbooks/deploy.md) ·
[backup/restore](../docs/runbooks/backup-restore.md) · [incident](../docs/runbooks/incident.md) ·
[first merchant onboarding](../docs/runbooks/merchant-onboarding.md) (owner-only prerequisites called out)

R1 acceptance command (whole repo, one PASS/FAIL/NOT_RUN table): `bash scripts/dev/release-gate.sh --strict`.

## Quick start (single host, as root)

```sh
deploy/scripts/host-setup.sh                      # group 10500, dirs/modes, templates -> /etc/live-commerce
vi /etc/live-commerce/compose.env /etc/live-commerce/env/*.env   # hosts, flags, OIDC client id
deploy/scripts/build-images.sh                    # prints IMAGE_TAG (git sha12)
deploy/scripts/secrets-init.sh                    # creates missing secrets only, never prints values
# owner secrets: replace __UNSET__ in secrets/commerce_oidc_client_secret (and meta apps if used)
deploy/scripts/preflight.sh --online              # P01-P16, names only
deploy/scripts/deploy.sh first <sha12>            # postgres -> migrate -> provision -> tag into compose.env -> up -> checks
cp deploy/host/crontab.example /etc/cron.d/live-commerce   # backups + watchdog (edit paths)
# Operator one-shots (own containers, registrar logins, inputs prompted without echo, never stored):
deploy/scripts/ops-admin.sh stripe-admin register|rotate|webhook|qualify|method ...   # docs/runbooks/deploy.md §6.1
deploy/scripts/ops-admin.sh meta-admin page-token ...                                 # docs/runbooks/deploy.md §6.3
```
Upgrade: `deploy.sh upgrade <tag>`. It takes a backup, opens a 503 window, runs migrate and provision, then starts the stack.
Rollback: `deploy.sh app-rollback <tag>` works only when the migration ledger is unchanged. Otherwise the answer is forward-fix.
All three write the tag they bring up into `compose.env` (`IMAGE_TAG`) right before `up -d`, so plain `docker compose`
commands (runbook `dc`) always resolve the running tag; never edit that line by hand (watchdog W10 reports drift).
Superuser password rotation: `pg-ops.sh rotate-superuser` only. PITR restore to live: `pg-ops.sh restore-pitr --promote`
then `pg-ops.sh pitr-cutover` (backup-restore.md §6).

## Topology

```
Internet ─80/443(+udp)─► edge-netns (pause) ── shared 127.0.0.1 ──┬ caddy :80/:443 (TLS, 4 hosts)
                                                                 ├ api :8080 (must be loopback)
                                                                 ├ admin :3100 (Next standalone)
                                                                 └ storefront :3200 (next start)
backend (internal) : postgres :5432 ◄── api, expiry-worker, meta-worker, payment-worker-* (+egress: PAYUNi, api.stripe.com),
                     claims-worker (+egress: graph.facebook.com), ads-worker (+egress: graph.facebook.com)
ops one-shots      : stripe-admin (backend + egress), meta-admin (backend) — profile ops, run only via ops-admin.sh
pgsocket volume    : postgres ◄── migrate (network none), provision-logins, pg-ops (network none)
```
Profiles: `db` (postgres, migrate, provision-logins), `app` (edge-netns, caddy, api, admin,
storefront, expiry-worker), `payments-sandbox` (payment-worker-sandbox: PAYUNi + Stripe/refund dispatch when
`LC_STRIPE_ENABLED=1`), `payments-live` (REAL MONEY; Stripe LIVE only with the pair, rule 20), `meta` (meta-worker, holds K_actor),
`claims` (claims-worker: intake poller + the only sender of Meta private replies; owner approval for real sends),
`ads` (ads-worker: the only Meta ad-account writer and the only holder of the HPKE private ring; with api.env
`COMMERCE_META_ADS_APP_ID`, after 0080 per ruling B15),
`ops` (pg-ops, stripe-admin, meta-admin; never listed in `COMPOSE_PROFILES`).
The media worker is **not deployed**, because it is MOCK-only (`worker_env.go:174`); there is no `media` profile
(deviation 21).

## Files

| Path | What it is |
|---|---|
| `compose.yml` / `compose.two-host-db.yml` | Whole stack; 2-host override (NOT_RUN) |
| `docker/{go,admin,storefront,caddy}.Dockerfile` | `lc-go` (all Go binaries + migrate + lcentry), `lc-admin`, `lc-storefront`, `lc-caddy` |
| `tools/lcentry/` | Stdlib-only launcher: `*_FILE` secret files → env, then `execve`; loopback HTTP probe |
| `caddy/Caddyfile` | Edge routing/TLS: admin, shop, api (default-deny, `/healthz` only), hooks (`/v1/meta/webhooks/*`, `/v1/stripe/webhook/*`); access log with credential query parameters redacted |
| `postgres/postgresql.conf`, `pg_hba.conf` | Server settings + WAL archiving; superuser socket-only, services via scram |
| `postgres/logins.tsv` | Service LOGIN → one authority → grant shape → consumer (single source); 17 core logins incl. Stripe ingress/registrar, claims intake/worker, Meta registrar |
| `postgres/provision-logins.sh` | Idempotent logins + verification matrix + ruling-19 River privileges + registrar EXECUTE + TCP auth + readiness gates |
| `postgres/ops/*` | backup, basebackup, restore-dump, restore-pitr (`--drill`/`--promote`), pitr-install (cut-over), rotate-superuser, verify.sql (run inside `pg-ops`) |
| `secrets.manifest.tsv` | Every secret file: kind, generator, consumers, rotation |
| `env/*.env.example` | `compose.env` (cross-service values, incl. `LC_STRIPE_ENABLED`) + per-service knob templates (`claims-worker.env` added in R1) |
| `scripts/lib.sh` | Shared helpers (`lc_compose`, `lc_psql`, `lc_secret_scan`, env loader) |
| `scripts/host-setup.sh`, `secrets-init.sh`, `preflight.sh` | Host prep, secret generation, config validation |
| `scripts/build-images.sh`, `check-pins.sh` | Image build (sha12 tags, OCI labels); digest-pin guard |
| `scripts/ops-admin.sh` | Operator CLIs (`stripe-admin`, `meta-admin`) as one-shot `ops` containers; prompts inputs without echo; refuses live keys and `--profile LIVE`; audit line without values |
| `scripts/deploy.sh`, `pg-ops.sh` | first / upgrade / app-rollback (keeps compose.env `IMAGE_TAG` = deployed tag); DB operations wrapper incl. `rotate-superuser`, `pitr-cutover` |
| `scripts/watchdog.sh`, `collect-diagnostics.sh` | Cron health checks W1–W10; incident bundle (secret-scanned) |
| `scripts/smoke.sh`, `smoke-browser.mjs` | Acceptance `static` (S01–S06) / `full` (S07–S44, S10f/g, S13n) with evidence |
| `../scripts/dev/release-gate.sh` | R1 acceptance table over every tier (packet, build/vet, TS typecheck, secret grep, depmap, unit, foundation, every browser mode, smoke static/full); NOT_RUN when prerequisites are missing |
| `host/crontab.example` | Backup + watchdog schedule |

## Status matrix (2026-09-29; earlier rows 2026-09-28)

| Area | Status | Evidence |
|---|---|---|
| Static package: syntax, shellcheck, pins, compose config (4 profile sets × 2 files), lcentry tests (96.7 %), Caddyfile validate/fmt, ignore files | **PASS** | `smoke.sh static` (S01–S06) |
| **R1 (unit deploy-release), `smoke.sh full` on 813a980** in a privileged Linux `docker:dind` container (real image builds, root, ports 80/443; the macOS host cannot run `full`) | **LOCAL, 52 PASS / 0 FAIL / 1 BLOCKED (S29m, I8) / 1 NOT_RUN (S34, no Chromium)**, exit 3 | `output/deploy-release/smoke-full-813a980/` (result.json + logs). New cases green: S13 (17 logins, ruling-19 River privileges, registrar EXECUTE), S13n (two injected drifts each fail provisioning by name), S16 (claims-worker + Stripe-enabled sandbox worker ready), S19 (Stripe webhook route reaches the Go API), S44 (operator one-shots + `ops-admin.sh`), S10f/S10g (custody and operator-input negatives). S44b red run first: the wrapper's token regex was invalid (ERE bound > 255), fixed in 813a980 |
| `smoke.sh full` (older rows below) | historical: BLOCKED at S07 before I1 | superseded by the row above |
| DB layer + edge with **scratch** images: postgres (non-root, read-only, checksums, archiving), migrate ×2 over the socket, provision-logins ×2 (12 logins, matrix, TCP auth, readiness), superuser-over-TCP rejected, api/caddy healthy, worker ready tokens, Caddy internal-CA TLS, default-deny, webhook routing, 308 redirect, unknown Host not proxied, 503 + Retry-After window, hardening of 7 containers, no secret in inspect/logs, backup + restore into a new DB, WAL archiving, basebackup + `pg_verifybackup`, PITR drill (2 s), watchdog W2–W9, diagnostics bundle, graceful stop, clean teardown | **VERIFIED_LOCAL (scratch)** | Scratch `lc-go` built on the host from this tree plus the §7 `cmd/migrate` proposal (outside the worktree, not committed); real `lc-caddy` from `docker/caddy.Dockerfile`. This is not product acceptance |
| All 4 image builds + `smoke.sh full` with the I1 proposal: independent test_worker run on 22d5d3f | **FAIL** | 41 PASS / 3 FAIL (S08, S34, S39). S21 was a false PASS. Root causes F1–F3 are in deviations 10–12 |
| Same, author re-run after the fixes (f318632, scratch clone + `cmd/migrate` proposal, sandbox-CA base images via `GO_IMAGE`/`NODE_IMAGE`) | **VERIFIED_LOCAL: 45 PASS / 0 FAIL / 1 BLOCKED**, exit 3 | S07 built through `--network host` (loopback proxy); S08 7 names; S21 `307 -> /zh-CN -> 200`; S34 Chromium PASS; S37–S39 PASS (S39 covers all 4 rollback paths); S33 clean; secret scan hits=0. An independent re-run is still owed, because the author cannot be the only acceptor |
| Review P1 fixes (2026-09-28): S40 Caddy access-log redaction, S41 superuser rotation without log leak, S42 PITR promote + cut-over, S43 compose.env tag persistence + W10 | see `smoke.sh full` evidence of the fix commit | Author run on a scratch clone + I1 proposal; results recorded in the task's deploy-verify-final.md. An independent re-run is still owed |
| Logical-restore media gate (S29m) | **BLOCKED (I8)** | Live `media_plan_ready=t`, restored `f`, PITR `t`. It used to be hidden inside the S29 PASS |
| Real ACME/DNS, OIDC login, PAYUNi sandbox/live, Stripe SANDBOX registration + webhook delivery + checkout, Meta webhooks/private replies, media, 2-host TLS, K8s, load, CVE scan | **NOT_RUN** | Need owner inputs (O2, O3, O11, B3–B6, Stripe test key, Meta App) |

## Deploy-only dependency ledger (integrator: copy into docs/implementation/dependencies.md, I5)

| Dependency | Version / pin | Where used | Why |
|---|---|---|---|
| Docker Engine | ≥ 25 (verified 29.3.1) | host | BuildKit cache mounts, `init`, sysctls |
| Docker Compose plugin | ≥ 2.24 (verified v5.1.1) | host | `depends_on.required: false`, profiles |
| bash, coreutils, openssl, curl, python3 | distro | `deploy/scripts/*` | Key generation, JSON/format checks, secret scan |
| golang | `1.27.1-trixie@sha256:4337…5183` | build stages | go.mod `go 1.27.1` |
| distroless static | `debian13:nonroot@sha256:e2e9…55e3` | `lc-go` runtime | CA certs + tzdata, UID 65532, no shell |
| node | `24.15.0-trixie-slim@sha256:291b…0eb7` | `lc-admin`, `lc-storefront` | Ledger-tested Node (O6: 24.21.0 bump candidate) |
| pnpm | 10.33.0 via corepack | Node builds | root `packageManager` |
| caddy | `2.11.4-alpine@sha256:6aed…cb2b` | `lc-caddy` | Edge TLS; `setcap -r` for non-root |
| pause | `registry.k8s.io/pause:3.10.1@sha256:278f…d24c` | `edge-netns` | Shared netns owner |
| postgres | `@sha256:4ef4…2280` (18.6) | postgres, provision-logins, pg-ops | Same pin as `scripts/dev/test-local.sh` |
| shellcheck (optional) | 0.11.0 | `smoke.sh` S01 | Script lint; NOT_RUN when absent |
| Playwright (repo pin 1.63.0) + Chromium | repo | `smoke-browser.mjs` (S34, optional) | Browser through the edge |

No new Go modules and no new npm packages. `lcentry` uses only the Go standard library.

## Deviations from `deploy-design.md` (all found by local verification or code facts)

1. **Media readiness gate informational by default** (`LC_REQUIRE_MEDIA_GATE=0`). `live.media_plan_ready()`
   pins `md5(pg_get_constraintdef())`, and PostgreSQL flattens the nested `AND` from `BETWEEN` on re-parse.
   So every DB restored from `pg_dump` reports the gate **false** (VERIFIED_LOCAL: `media_stop_budget`,
   `sessions_title_check`, `media_authorization_destinations_external_asset_id_check`). Payment, expiry and
   meta gates stay mandatory in provision-logins, verify.sql and watchdog W8. The migration fix is REQUIRES_INTEGRATOR.
2. PITR scratch server reads `max_connections` etc. from the backup's `pg_control`. Hot standby aborts below
   the primary's values.
3. `archive_command` succeeds on an identical, already-archived segment (`cmp`). This keeps the archiver
   from wedging after a crash.
4. `pitr-scratch` is mounted at `/var/lib/postgresql` **inside pg-ops only**. It inherits UID 999 ownership
   from the image. pg-ops never mounts pgdata.
5. Preflight rejects `LC_ACME_CA=` (empty). Caddy applies `{$VAR:default}` only when the variable is unset.
6. Helper functions are used because `--profile X` **replaces** `COMPOSE_PROFILES`: `lc_compose_with_ops`
   and `lc_compose_all`.
7. `producer | grep -q` is banned under `pipefail`, because SIGPIPE caused false negatives.
8. `lcentry` also rejects a secret that is empty after newline stripping.
9. `host-setup.sh` installs the env templates when they are missing and never overwrites them.
10. **Admin listener is `HOSTNAME=localhost`, not `127.0.0.1`** (F2). Next builds its request origin
    from HOSTNAME, but `request.nextUrl` rewrites `127.x`/`[::1]` to `localhost`. With `127.0.0.1` the two
    origins differ, so Next cannot relativise the locale redirect in `apps/admin/proxy.ts`. `/` then answered
    `307 Location: https://localhost:3100/zh-CN` through Caddy, and browsers got connection refused.
    `admin.Dockerfile` starts node with `--dns-result-order=ipv4first`, so `localhost` still binds 127.0.0.1
    (the address Caddy and the healthcheck dial). Checked locally with the built image: `Location: /zh-CN`.
    App-side hardening remains recommended: send a path-only Location. That fix is outside `deploy/**`.
11. **Post-checks judge each container's current lifetime** (F3). A worker's ready token must appear in
    `docker logs --since <.State.StartedAt>`, not since the script started. Every `lc-*` container must run
    `:$IMAGE_TAG`. A no-op `up -d` (rollback to the running tag, re-running `first`) keeps containers
    running, so no new token was printed, and the rollback died after 60 s.
12. **S08 compares binary names, not a count** (F1). `grep -c '^app/bin/[a-z-]*$'` also counted the `app/bin/`
    directory entry. The expected set is read from `go.Dockerfile` `ARG GO_CMDS` + `lcentry`.
13. **Loopback build proxy** (build-images.sh). BuildKit RUN steps get their own network namespace, where a
    proxy on the host's `127.0.0.1` refuses connections (checked locally). With `LC_BUILD_NETWORK=auto`
    (default), the script builds with `--network host` only when a forwarded proxy variable points at
    loopback. `default` keeps isolation and warns.
14. `go.Dockerfile` checks every `cmd/<name>` before compiling, so I1 fails in seconds instead of after 5 builds.
15. **Caddy access log redacts credential query parameters** (review P1). Caddy logs the full URI; a Meta
    `hub.verify_token` (owner secret) or an OIDC `code`/`state` would have landed in the json-file logs and the
    diagnostics bundle. `format filter` rewrites `hub.verify_token`, `code`, `state`, `token`, `access_token`,
    `id_token` to `REDACTED` in `request>uri`, the `Referer` header and the `Location` response header (S40).
16. **Superuser rotation is a script** (review P1). With `log_statement='ddl'`, a bare `ALTER ROLE postgres
    PASSWORD` is logged in cleartext (reproduced on the pinned image). `pg-ops.sh rotate-superuser` disables
    statement logging for its session, writes the file in place (same inode, so the running postgres
    container's bind-mounted secret follows; a rename would leave `lc_psql` on the old value) and scans the log (S41).
17. **PITR cut-over = `restore-pitr --promote` + `pitr-cutover`** (review P1). The old manual §6 copied a
    cluster stopped while paused (`shut down in recovery`); the live server then replayed past the target on
    timeline 1 and archiving failed forever. `--promote` ends recovery at the target (new timeline; scratch
    runs `archive_mode=on` with an empty command so the history file is queued, not archived from pg-ops);
    `pitr-cutover` installs it with checks (timeline > 1, archiver failed unchanged, history archived) (S42).
    The scratch socket now uses peer auth, so PITR works with base backups older than a password rotation.
18. **compose.env `IMAGE_TAG` follows deploy.sh** (review P1). The tag used to live only in deploy.sh's
    environment, so any later plain `docker compose up/run` recreated services on the previous release.
    deploy.sh now rewrites the line atomically just before `up -d`; watchdog W10 flags drift (S43).

19. **Operator inputs are never repo files, knob files or env-file values.** `STRIPE_SECRET_KEY`, `STRIPE_ACCOUNT_ID`,
    `STRIPE_WEBHOOK_SECRET[_NEXT]` and `META_PAGE_ACCESS_TOKEN` are read by `ops-admin.sh` from the caller's
    environment, a no-echo prompt or (O-D, stripe-live-enable-v1 LD3) a transient transport file named by
    `<NAME>_FILE` (absolute path, regular file, not a symlink, owned by the invoking uid, mode 0400/0600, one line;
    the owner removes it after registration) and forwarded to a one-shot container by NAME (never argv). They are visible in
    that container's config until `--rm` removes it (host root only, like the secret files). `lcentry` deliberately
    still expands only `DATABASE_URL` and `COMMERCE_*` `_FILE` variables. preflight P07 forbids `STRIPE_*` and
    `META_PAGE_ACCESS_TOKEN` in any knob file, and `COMMERCE_META_GRAPH_BASE_URL` (loopback MOCK switch).
20. **Two Stripe switches** (stripe-live-enable-v1 LD6, §5.2). `LC_STRIPE_ENABLED` (compose.env) drives
    `COMMERCE_STRIPE_WEBHOOK_ENABLED` on the api and `COMMERCE_STRIPE_ENABLED` on the payment worker of the deployed profile
    (`payment-worker-sandbox`, or `payment-worker-live` when LIVE). `LC_STRIPE_CHECKOUT_ENABLED` (default: follows
    `LC_STRIPE_ENABLED`) drives only `COMMERCE_STRIPE_CHECKOUT_ENABLED` on the api: setting it to 0 and restarting the api removes
    Stripe from the hosted checkout for every store while webhook and worker keep reconciling (never stop the worker to stop
    sales). LIVE is admitted only with the pair `LC_STRIPE_LIVE_ENABLED=1` + `LC_STRIPE_LIVE_APPROVAL_REF`
    (`[A-Za-z0-9._:-]{8,128}`, the pointer to the owner's written approval), wired into `api` and `payment-worker-live` by
    compose and forwarded by `ops-admin.sh` to stripe-admin by name. preflight P06 requires (SANDBOX and `payments-sandbox`)
    or (LIVE and `payments-live` and the pair); a half-set pair fails. `live-revoke` and `method` never need the pair.
    Operator steps: `docs/runbooks/stripe-live.md`.
21. **No `media-worker` service or `media` profile**, although the unit brief listed one "off by default". Its startup
    contract needs three media logins, a material keyring and a projects JSON that are not in the manifest, and every
    LiveKit project is hard-wired to MOCK (`worker_env.go:174`). A stub that cannot start would make
    `config --profiles media` fail on missing secrets. It stays reference-only in `env/media-worker.env.example`;
    integrator ruling requested (see the unit return).
22. **Operator binaries ship in `lc-go`** (`stripe-admin`, `meta-admin` next to the services) instead of a fifth image:
    the api/worker containers never mount the registrar logins, so the binaries alone grant nothing (S44 asserts no
    long-running service mounts `dsn_lc_stripe_registrar`/`dsn_lc_meta_registrar`). Split the image if a reviewer
    wants defence in depth beyond that.
23. **Custody separation is enforced, not just documented**: `secrets-init.sh` never generates a key equal to any
    keyring key or another standalone b64 key; preflight P04 checks all four keyrings and the replay/K_actor/K_link keys
    pairwise (negative S10f).
24. **`smoke.sh static` and `make_config` are BSD/GNU portable** (`sed -i.bak`), so macOS developer machines and CI run the
    same cases. `smoke.sh full` still needs a Linux host with root (GNU userland, ports 80/443).
25. **U08 claims retention (R2)**: claims-worker runs the hourly `claims_retention_v1` purge on its own login
    `lc_retention_job` (`dsn_lc_retention_job`); `retention-admin` ships in `lc-go`. With the `claims` profile on,
    `deploy.sh` post-checks run `retention-admin status` on that login and require a run in the last 26 h and
    `enforced=1` (`LC_REQUIRE_RETENTION_ENFORCED=0` only under waiver W1). `lc_retention_operator` is never
    provisioned here (`docs/runbooks/claims-data-deletion.md`); smoke S46 asserts the job login cannot erase.

## Known limits recorded by the R1 deploy unit

- **S29m (I8) stays BLOCKED**: not in R1's path (LiveKit media is not deployed, `COMMERCE_STUDIO_MEDIA_ENABLED=0`, `LC_REQUIRE_MEDIA_GATE=0`); owner lane =
  integrator (restore-stable `live.media_plan_ready()` migration). It keeps `smoke.sh full` at exit 3 by design.
- **G1 closed (R1 ruling F2)**: `ops-admin.sh meta-admin route|route-disable` (login `lc_meta_registrar`; definers
  `integration.register_meta_binding` 0066 + `meta_inbox.activate_route`/`disable_route` 0028). No curator login: the curator
  authority is retention/terminal review, not routing. Gate `TestMetaRouteRegistrarF2` (REAL_PG), smoke S44.
- **G2 closed (R1 ruling G2)**: `COMMERCE_STUDIO_ENABLED=1` = planning + claims + claim-source, `COMMERCE_CLAIMS_ENABLED=1`
  (label key secret `commerce_claims_label_key`), `COMMERCE_STUDIO_MEDIA_ENABLED=0` (P06; media routes 404, no MediaPlanner,
  no `live.media_plan_ready()` requirement). Smoke S45 asserts claims/claim-source answer 401/403 on the deployed api; S10e/S10h
  the P06 refusals. Smoke full runs identity=1 against a public OIDC discovery document (`LC_SMOKE_OIDC_ISSUER`).
- SANDBOX Stripe registration/qualification/webhook delivery and every Meta LIVE step need owner inputs: NOT_RUN in CI.

## Blockers and hand-offs

- **REQUIRES_INTEGRATOR** (outside `deploy/**`):
  - I1 `cmd/migrate`: **closed** (exists; S07+ run).
  - I2 `/healthz` route in both Next apps.
  - I3 storefront `output:"standalone"`.
  - I4 `ErrLedgerAhead` + configurable Apply timeout.
  - I5 dependency ledger.
  - I6 evidence copy.
  - I7 postgres digest refresh.
  - I8 restore-stable `live.media_plan_ready()`. It keeps `smoke.sh full` at BLOCKED (S29m) even once I1
    lands. It is not a failure of this release's services, because media is not deployed. The owner decides
    whether a media-less release may ship with S29m BLOCKED.
  - App hardening (not blocking once F2 is deployed): make the `apps/admin/proxy.ts` locale redirect path-only.
- **Owner decisions:**
  - O1 Compose/non-HA ADR.
  - O2 IdP.
  - O3 off-host encrypted backups.
  - O4 host.
  - O5 feature phases.
  - O6 Node bump.
  - O7 HSTS preload.
  - O8 log/IP retention.
  - O9 WAF/rate limiting.
  - O10 off-host logs.
  - O11 CVE scanner.
- **Launch blockers:** B1–B8 (see `docs/runbooks/deploy.md` §0).
