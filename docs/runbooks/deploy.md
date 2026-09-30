<!--
File: docs/runbooks/deploy.md
Purpose: 部署运行手册 — 主机准备、配置、首次部署、升级、回滚决策、功能上线顺序、密钥轮换、证书、迁移到两台主机/K8s、发布检查清单。
Runs as/in: 文档（运维人员在部署主机上以 root 执行其中命令）。
Reads env / secrets: 无（命令读取 /etc/live-commerce/compose.env 与 secrets 目录，本文不含任何密钥值）。
Used by: 运维/owner/集成者；deploy/README.md 链接到此。
Depends on: deploy/scripts/*.sh, deploy/compose.yml, deploy/env/*.env.example。
Status: DESIGN。未在生产执行过。cmd/migrate 已存在（I1 关闭），smoke full 在 Linux 容器（dind）里对真实构建的四个镜像跑通：
  见 deploy/README.md 状态表；唯一 BLOCKED 是 S29m（I8，逻辑恢复后 media 门禁 t→f，R1 不在关键路径：LiveKit 媒体不部署，`COMMERCE_STUDIO_MEDIA_ENABLED=0`）。
  R1 新增（deploy-release 单元）：Stripe SANDBOX（api 结账 + webhook、sandbox worker 的 Stripe/退款派发）、claims-worker、
  meta-worker 的 K_actor、运维一次性任务 stripe-admin / meta-admin（`deploy/scripts/ops-admin.sh`）、smoke S44。
  2026-09-28 评审 P1 修复：deploy.sh 把部署的 tag 写回 compose.env（§3–§5，smoke S43、看门狗 W10）；
  超级用户口令轮换改为 `pg-ops.sh rotate-superuser`（§7，smoke S41）。
Change rules: 命令必须与脚本保持一致；改脚本行为时同步本文。
-->
# 部署运行手册（live-commerce）

## 0. 范围与状态

- 形态：单机 Docker Compose + Caddy 边缘 + PostgreSQL 18（唯一交易真源）。**非高可用**。
  架构.md §4.2 把 Compose 定位为开发和可重复验收环境，所以生产使用前必须由 owner 通过 ADR（O1）接受这个风险。
- **任何生产部署、LIVE 支付、真实退款、营销重播、破坏性迁移都必须有 owner 明确批准**（AGENTS.md）。
  本手册只描述获批后的操作步骤。
- 以下上线阻塞项关闭前，不得宣称"可生产"：
  - B1：已关闭。`cmd/migrate` 存在，是唯一的生产迁移入口（`deploy.sh` 与 smoke S12 使用它）。
  - B2：未选定 IdP（OIDC），商家无法登录。
  - B3：PAYUNi 商户资质和 LIVE 批准。
  - B4：PAYUNi NotifyURL 没有接收端（`/payuni/notify` 由 Caddy 保留并返回 404）。支付结果只来自 worker 查询。
  - B5：直播/Studio（LiveKit 仅 MOCK）。
  - B6：Meta 未开放给客户：Meta App Review / Access Tier 是 owner 事项；评论 → 认领 → 私信链路的运维缺口见 §6.3 与 merchant-onboarding.md（G1 路由激活已由 `meta-admin route` 关闭；**claim-source 路由未挂载 G2 仍开放**）。
  - B7：T21 独立评审未完成。
  - B8：Compose 作为生产环境与 §4.2 的定位冲突。
- 状态词汇：DESIGN / MODEL_ONLY / MOCK / SANDBOX / LIVE / NOT_RUN / BLOCKED。本地 smoke 通过不等于产品通过。

## 1. 主机准备

1. 主机规格：至少 4 vCPU / 8 GiB / 80 GiB SSD。Docker `data-root` 放在数据盘，`LC_BACKUP_DIR` 放在**另一块盘**（容量 ≥ 2×DB + WAL）。
2. 防火墙：放行 22/tcp（只允许管理来源）、80/tcp、443/tcp、443/udp。
   **注意：Docker 发布的端口会绕过 ufw/firewalld 的 INPUT 规则**，需要用 `DOCKER-USER` 链或 `LC_BIND_ADDR` 限制。
3. 时间同步：启用 chrony 或 systemd-timesyncd，`timedatectl show -p NTPSynchronized` 应输出 `yes`。
4. 以 root 在部署 checkout（例如 `/opt/live-commerce`）执行：
   ```sh
   deploy/scripts/host-setup.sh            # 检查版本，创建 GID 10500 组、目录和权限，安装模板（不覆盖）
   ```
   按输出提示合并推荐的 `/etc/docker/daemon.json`（json-file 日志轮转 + live-restore）。脚本**不会自动修改**这个文件。
5. 安装定时任务：`cp deploy/host/crontab.example /etc/cron.d/live-commerce`，并修改路径。

## 2. 配置

1. 编辑 `/etc/live-commerce/compose.env`：
   - 四个域名：`LC_ADMIN_HOST`、`LC_STORE_HOST`、`LC_API_HOST`、`LC_HOOKS_HOST`。
   - `COMPOSE_PROFILES`。
   - `IMAGE_TAG` **不要手工改**：`deploy.sh first|upgrade|app-rollback` 在 `up -d` 之前把要启动的 tag 原子写入这里（临时文件 + 改名，保留属主和权限）。
     所有其他 Compose 入口（`dc up`、`dc run migrate`、重跑 `first`、§8 换域名）都从这个文件取 tag，所以它必须始终等于正在运行的 tag。
   - 共享开关：`LC_IDENTITY_ENABLED`、`LC_OIDC_ISSUER`、`LC_BUYER_*`。
   跨服务的值**只能**写在这里。
2. 编辑 `/etc/live-commerce/env/*.env`。这些文件只放各服务自己的旋钮；compose.yml 里 `environment:` 已接好的变量不得重复定义（preflight P06 会拦截）：
   - `api.env`：OIDC client id、`COMMERCE_SESSION_TTL`、`COMMERCE_PAYMENT_PROFILE=SANDBOX`、`COMMERCE_STUDIO_ENABLED=1` + `COMMERCE_CLAIMS_ENABLED=1`（场次规划、关键词下单、认领来源；需要 `LC_IDENTITY_ENABLED=1`）、`COMMERCE_STUDIO_MEDIA_ENABLED=0`（必须为 0，P06 强制）。
   - `claims-worker.env`（R1 新增，升级时要从 `deploy/env/claims-worker.env.example` 复制）：`COMMERCE_META_GRAPH_VERSION`（**无默认值**，取 owner 用 MCI11 只读探测确认的 vNN.N；`CHANGE_ME` 会被 preflight P08 拒绝）。
   - `caddy.env`：真实的 `ACME_EMAIL`。`LC_ACME_CA` 要么保持注释，要么填 https URL，**不能留空**。
   - Stripe 有两个开关（stripe-live-enable-v1 LD6）：`compose.env` 的 `LC_STRIPE_ENABLED`（api 的 webhook 路由 + 当前 profile 的 payment worker 的 Stripe/退款派发）
     和 `LC_STRIPE_CHECKOUT_ENABLED`（只管买家结账，缺省跟随 `LC_STRIPE_ENABLED`；设为 0 并重启 api 即平台级停收，webhook 与 worker 继续对账）。
     SANDBOX：`COMMERCE_PAYMENT_PROFILE=SANDBOX` 且启用 `payments-sandbox`；LIVE：`COMMERCE_PAYMENT_PROFILE=LIVE`、启用 `payments-live`，
     并在 compose.env 成对设置 `LC_STRIPE_LIVE_ENABLED=1` 与 `LC_STRIPE_LIVE_APPROVAL_REF`（指向 owner 书面批准，格式 `[A-Za-z0-9._:-]{8,128}`；P06 双向检查）。
     没有配对时 API/worker/`ops-admin.sh` 都拒绝 LIVE。逐店审批、canary 与停收步骤见 `docs/runbooks/stripe-live.md`。
3. 生成密钥（只补缺失的文件，不覆盖、不打印值）：
   ```sh
   deploy/scripts/secrets-init.sh
   ```
4. owner 提供的密钥：用真实值替换文件内容 `__UNSET__`，权限保持 `0440 root:10500`。
   - `commerce_oidc_client_secret`：如果保持 `__UNSET__`，表示使用公共 PKCE 客户端，P09 给出 WARN。
   - `commerce_meta_apps_json`：仅在启用 Meta 时需要。
   - **不属于任何配置文件的运维输入**：`STRIPE_SECRET_KEY`（SANDBOX 只接受 `sk_test_`/`rk_test_`；LIVE 只接受受限 key `rk_live_`，`sk_live_` 一律拒绝）、`STRIPE_ACCOUNT_ID`、`STRIPE_WEBHOOK_SECRET[_NEXT]`、`META_PAGE_ACCESS_TOKEN`。
     它们只在运行 `deploy/scripts/ops-admin.sh` 时由操作者提供：终端无回显提示、调用者环境变量，或（O-D）`<NAME>_FILE=<绝对路径>` 指向 owner 在服务器上写入的一次性传输文件
     （普通文件、非符号链接、属主 = 运行 ops-admin 的 uid、权限 0400/0600、只有一行；用完由 owner 删除，见 stripe-live.md）。
     **不写入仓库、compose.env、env 文件或 secrets 目录**；preflight P07 会拒绝把它们放进任何旋钮文件。
     新增的自动生成密钥（`secrets-init.sh` 补齐）：Stripe webhook 签名 keyring（与支付 API-key keyring 分离）、Meta Page-token keyring、`commerce_claims_actor_key`（K_actor）、`commerce_claims_reply_link_key`（K_link）以及 5 个新登录的 pw_/dsn_。
     preflight P04 检查四个 keyring 互不共享 key、三个独立 b64 密钥两两不同且不在任何 keyring 中。
5. 校验：
   ```sh
   deploy/scripts/preflight.sh --online    # 只输出规则号、PASS/FAIL 和变量名
   ```
   因为 cmd/api 启动失败时只打印 `api stopped`，所以 preflight 必须全绿后才能继续。

## 3. 首次部署

```sh
deploy/scripts/build-images.sh           # 输出 IMAGE_TAG=<sha12>（-dirty 标签禁止用于生产）
deploy/scripts/deploy.sh first <sha12>   # preflight → 镜像检查 → postgres 健康 → migrate → provision-logins → 写 compose.env IMAGE_TAG → up -d → 部署后检查 → 记录
```
- 不带 `<tag>` 时使用 compose.env 里已有的 `IMAGE_TAG`。
- 首次签发 ACME 证书：DNS 必须先指向本机（P14）。演练时在 `caddy.env` 中打开 staging CA，正式签发前再注释掉。
- OIDC 回调地址：`https://<LC_ADMIN_HOST>/api/auth/callback`（`cmd/api/identity.go:59`），需要在 IdP 注册。
- 部署后检查（脚本会自动执行）：
  - 所有常驻服务 running/healthy。
  - 所有 `lc-*` 镜像的容器都运行 `:$IMAGE_TAG`。发现旧容器时直接失败。
  - worker 就绪标记要出现在**该容器本次启动之后**的日志里，也就是 `docker logs --since <.State.StartedAt>`，最多等 60 s。标记包括 `expiry_worker_ready`、`payment_worker_ready`、`meta_worker_ready`、`claims_worker_ready`（仅当对应 profile 启用）。
    `up -d` 不会重建配置没变的容器，所以重复执行 `first`，或回滚到正在运行的 tag，都是合法的空操作，检查会通过。
  - `https://<api>/healthz` 返回 200。
- 构建主机的代理在本机回环地址（`127.0.0.1`/`localhost`/`[::1]`）时，BuildKit 的 RUN 步骤访问不到它。
  `build-images.sh` 默认（`LC_BUILD_NETWORK=auto`）会自动改用 `--network host` 并给出 WARN。只影响构建，不影响镜像。
- `deployments.log`（`/var/lib/live-commerce/`）会追加一行：时间、动作、tag、ledger 行数、操作人。回滚判断依赖这一行。

## 4. 升级（只能前向）

```sh
deploy/scripts/build-images.sh           # 新 tag
deploy/scripts/deploy.sh upgrade <tag>
```
步骤：
1. preflight（新 tag）。
2. **强制备份**（`pg-ops.sh backup --tag pre-upgrade-<tag>`）。备份失败会立即中止，此时没有任何改动。
3. 停止 api/admin/storefront/workers。这段时间 Caddy 返回 503 + `Retry-After: 60`，即维护窗口。
4. 用新镜像运行 migrate。先执行业务 SQL，再执行 River，最后执行 post_river，全部在同一个 advisory lock 下完成。
5. 运行 provision-logins。
6. 把新 tag 写入 compose.env 的 `IMAGE_TAG`（日志：`compose.env IMAGE_TAG <旧> -> <新>`）。
   时机是 `up -d` 之前、迁移成功之后：从这一刻起容器运行新 tag，所以即使部署后检查失败，compose.env 也和实际运行的镜像一致，
   之后任何普通的 `dc up -d` 都不会悄悄换回旧镜像（旧镜像跑在已迁移的 schema 上，旧 migrate 还会报 `database migration unknown to this binary`）。
7. `up -d`。
8. 部署后检查；通过后才写入 `deployments.log`（它只记录验证通过的部署，是回滚判断的依据）。

要点：
- 迁移失败时修复代码后重新执行。**绝不修改已经应用的 SQL 或 checksum**。
- 退出码 75 表示另一个迁移持有锁 718020260920。**不要循环重试**：先确认没有其他迁移在运行，再人工重试一次（见 incident.md §migrate）。
- 已知风险 R3：`migrations.Apply` 内部超时 30 s，大数据量迁移前需要先由集成者调整（I4）。

## 5. 回滚决策树

1. **与该 tag 上次部署时相比，ledger 行数没有变化** → `deploy/scripts/deploy.sh app-rollback <旧tag>`。
   如果 ledger 有变化，脚本会拒绝，并提示 "forward-fix only"（此时 compose.env 不会被改动）。
   允许回滚时，脚本同样在 `up -d` 之前把 `<旧tag>` 写入 compose.env，否则之后的 `dc up` 会把坏版本装回来。
   回滚到正在运行的 tag 是空操作，会通过部署后检查。smoke S39 覆盖四条路径：空操作、换 tag（容器重建）、换回原 tag、ledger 变化时拒绝；
   S43 验证 compose.env 跟随回滚、普通 `up -d` 不改变 tag、看门狗 W10 能发现漂移。
2. ledger 已变化，**且**备份之后**没有任何外部业务事实**（恢复流量后没有新订单或支付）→ 由 owner 决定是否按 backup-restore.md 恢复数据库，再做 app-rollback。
3. 其他情况 → **前向修复 + 对账**（架构.md §22.1）。**禁止在真实支付之上恢复数据库**。
- 应用回滚不等于数据库回滚。脚本永远不会自动回滚或自动恢复。

## 6. 功能上线顺序与进程清单

进程（compose 服务 → profile）：`api`/`admin`/`storefront`/`caddy`/`expiry-worker` → `app`；`payment-worker-sandbox` → `payments-sandbox`；
`payment-worker-live` → `payments-live`；`meta-worker` → `meta`；`claims-worker` → `claims`；`postgres`/`migrate`/`provision-logins` → `db`；
一次性运维任务 `stripe-admin`/`meta-admin`/`pg-ops` → `ops`（**不要写进 `COMPOSE_PROFILES`**，由 `ops-admin.sh`/`pg-ops.sh` 自行追加）。
`media-worker` **不是** compose 服务：LiveKit 全部硬编码 MOCK（`worker_env.go:174`，B5），见 `deploy/env/media-worker.env.example`（仅参考）。

- **Phase A**：identity + accounts + buyer + payments **SANDBOX**（`COMPOSE_PROFILES=db,app,payments-sandbox`），加 Stripe SANDBOX（§6.1）。
- **Phase B**：**LIVE** 需要 owner 批准和 PAYUNi/Stripe 资质。切换 profile 为 `payments-live`，把 `COMMERCE_PAYMENT_PROFILE` 改为 `LIVE`；Stripe 需要 compose.env 的
  LIVE 配对（`LC_STRIPE_LIVE_ENABLED=1` + `LC_STRIPE_LIVE_APPROVAL_REF`，§2），完整顺序、cutover 前置 SQL 与 canary 见 `docs/runbooks/stripe-live.md`。
  PAYUNi 在 LIVE 下仍不可用，直到 PAYUNi B3 LIVE 批准。在历史 sandbox job 处理完之前保留 `payments-sandbox`。preflight 会对 `payments-live` 给出 WARN 提醒。
- Meta 评论入口：`meta` profile + `COMMERCE_META_WEBHOOK_ENABLED=1` + owner 提供的 `commerce_meta_apps_json`（§6.3）。
- Meta 私信发送：`claims` profile。**这是唯一会向买家发送 Meta 消息的进程**，只在 owner 批准真实发送后启用（§6.3）。
- Studio（R1 裁决 G2）：场次规划 + 关键词下单 + 认领来源开启（`COMMERCE_STUDIO_ENABLED=1`、`COMMERCE_CLAIMS_ENABLED=1`，密钥 `commerce_claims_label_key` 由 secrets-init 生成）；LiveKit 媒体保持关闭（P06 强制 `COMMERCE_STUDIO_MEDIA_ENABLED=0`，media 路由不挂载＝404，media worker 不部署）。smoke S45 验证。

- Meta 广告（meta-ads-v1）：`ads` profile（ads-worker）+ api.env `COMMERCE_META_ADS_APP_ID`，见 §6.4。**裁决 B15：0080（ads-capi）合并前保持关闭**（preflight P06 强制）。

### 6.1 Stripe SANDBOX：账户登记与 webhook 端点

前提：`LC_STRIPE_ENABLED=1`、`payments-sandbox` 已运行、owner 提供 Stripe **测试**账户的 `sk_test_`/`rk_test_` key 和 `acct_...`。
全部通过 `deploy/scripts/ops-admin.sh`（一次性容器，持有 `lc_stripe_registrar` 登录，其他常驻容器不挂载它；输出只有 ID 和版本号）：

1. 登记账户（`VerifyAccount` 先核对 key 属于该账户且 `livemode=false`，之后才写库）：
   ```sh
   deploy/scripts/ops-admin.sh stripe-admin register --tenant <tenant> --store <store> --principal <owner-principal>
   # 提示输入 STRIPE_SECRET_KEY（无回显）与 STRIPE_ACCOUNT_ID；输出 {"connection_id":...,"credential_version":1}
   ```
   `<principal>` 必须是该租户已存在的 owner 成员。key 轮换：`ops-admin.sh stripe-admin rotate --tenant ... --connection <id> --expected-version <n>`。
2. 创建 webhook 端点。**先**在本机生成端点 id（UUID，例如 `uuidgen | tr A-Z a-z`），URL 由它决定：
   `https://<LC_HOOKS_HOST>/v1/stripe/webhook/<endpoint-id>`（Caddy 只在 hooks 域名放行这条路径，请求体原样转发）。
   在 Stripe Dashboard（测试模式）或 API 创建端点时：
   - **API 版本固定为 `2026-08-26.dahlia`**（代码常量 `internal/integrations/psp/stripe/config.go`，webhook 与请求版本必须一致）。
   - **订阅事件共 8 个**：`checkout.session.completed`、`checkout.session.async_payment_succeeded`、`checkout.session.async_payment_failed`、`checkout.session.expired`、
     `refund.created`、`refund.updated`、`refund.failed`、`charge.refunded`（后四个是退款；`charge.refund.updated` 已弃用，不订阅）。
   - 记下 Stripe 返回的签名密钥 `whsec_...`（只显示一次）。
3. 登记端点及其签名密钥（用 **Stripe webhook 签名 keyring** 密封，不是支付 API-key keyring）：
   ```sh
   deploy/scripts/ops-admin.sh stripe-admin webhook --tenant ... --store ... --principal ... \
     --connection <connection-id> --endpoint <endpoint-id> --profile SANDBOX --expected-version 0 --enabled
   # 提示输入 STRIPE_WEBHOOK_SECRET（whsec_...，无回显）；输出 {"endpoint_id":...,"key_version":1}
   ```
   端点的 profile 必须等于 `COMMERCE_PAYMENT_PROFILE`，否则 API 返回 404；签名密钥无法解密返回 503 `signing_unavailable`（Stripe 会重试）。
   签名密钥轮换（Stripe 允许 ≤24 h 双密钥）：新密钥设为 `STRIPE_WEBHOOK_SECRET_NEXT` 再执行同一命令，`--expected-version` 用当前版本。
4. 资格探测与支付方式（SANDBOX，会真实创建并立即过期一个 sandbox Checkout Session）：
   ```sh
   STRIPE_SANDBOX=1 deploy/scripts/ops-admin.sh stripe-admin qualify --tenant ... --store ... --principal ... \
     --connection <id> --expected-version <n> --profile SANDBOX --currency <ISO> --amount-minor <method minimum> --return-url https://<LC_STORE_HOST>/payment/return
   deploy/scripts/ops-admin.sh stripe-admin method --tenant ... --store ... --principal ... --market <id> --country <CC> \
     --connection <id> --qualification <qualification-id> --expected-version 0 --enabled --visible --sort 10 --min <minor> --max <minor> \
     --name-hans ... --name-hant ... --name-en ...
   ```
   TWD 最小 2500（Stripe SANDBOX 实测，stripe-psp-v1 D15）。`--profile LIVE` / `--environment LIVE` 只有在 compose.env 设置了 LIVE 配对时才会被 `ops-admin.sh` 放行（否则 `stripe_live_refused`），CLI 再校验一次；LIVE 步骤见 `docs/runbooks/stripe-live.md`。
5. 自检：webhook 配置错误（密钥环、入口登录、profile）会让 api 启动失败，日志只有 `api stopped`（不输出原因），所以以 `docker compose ps api` 为 healthy、
   preflight 全绿为准；smoke S19 证明 hooks 域名的 `/v1/stripe/webhook/*` 到达 Go API，S16 证明启用 Stripe 的 sandbox worker 就绪。
   Stripe Dashboard 里的“发送测试事件”和真实 Checkout 属于 SANDBOX 验证，由 owner 执行；工程侧证据为 NOT_RUN（SP16/SP18/SP17 见 stripe-psp-v1 §14）。

### 6.2 商家 Stripe 与退款

商家在后台发起退款（`payments:refund`，owner 创建店铺时已获得，裁决 24）；退款由 `payment-worker-sandbox` 的 Stripe/退款 worker 发出，结果靠 `refund.*`/`charge.refunded` webhook 与查询对账。
**真实退款需要 owner 明确批准**；SANDBOX 只对测试支付发起，LIVE 下退款不依赖店铺审批状态但仍需 `payments:refund`（stripe-live-enable-v1 LD7；agent/CI 从不调用 LIVE 退款路径，只有 owner 的 canary）。人工发货（承运商 + 运单号）是商家后台操作，无需部署步骤。

### 6.3 Meta：评论入口、Page token、认领来源

1. 前提（owner 事项，工程无法代办）：Meta App（App Review / Access Tier）、Page/IG 资产、`commerce_meta_apps_json`（`{"apps":[{"app_id","object","app_secret","verify_token"}]}`，从 Meta 开发者后台取）。
   Webhook 回调地址：`https://<LC_HOOKS_HOST>/v1/meta/webhooks/<app_id>/<object>`（`object` 为 `page` 或 `instagram`，与 `commerce_meta_apps_json` 的条目一一对应，`internal/integrations/meta/env.go:69`）；hub.verify_token 在 Caddy 访问日志里被替换为 REDACTED（smoke S40）。
2. 启用：`COMMERCE_META_WEBHOOK_ENABLED=1`（api.env）、`COMPOSE_PROFILES` 增加 `meta`；`meta-worker` 同时持有 K_actor（`commerce_claims_actor_key`），没有它评论不会被暂存为认领。
3. **Page access token 登记**（`claims-worker` 发送私信用；`scopes_attested` 由操作者依据 token debug 输出声明：FB 为 `pages_messaging`，IG 为 `instagram_manage_comments`、`pages_read_engagement`）：
   ```sh
   deploy/scripts/ops-admin.sh meta-admin page-token --tenant ... --store ... --principal <有 integration:manage 的成员> \
     --binding <binding-id> --provider facebook --asset <page-id> --expected-version 0 --scopes pages_messaging
   # 提示输入 META_PAGE_ACCESS_TOKEN（无回显）；输出 {"version":1}。明文 token 从不进入 PG、日志或仓库
   ```
4. **认领来源绑定**（**当前部署包内不可用，见已知缺口 G2**）：设计上商家在后台把直播场次绑定到具体的 Facebook 贴文/Instagram media（`PUT .../live-sessions/{session}/claim-source`，需要 `live:manage` + `integration:execute`，owner 已具备，裁决 24）。
   `private_reply=true` 还要求该绑定已登记 Page token（否则 `page_token_missing`）。**这个路由现在不能靠部署配置打开**：在本包构建的 api 里它返回 404（原因见 G2）。
5. 启用真实私信发送（**需要 owner 明确批准**，因为这会向真实买家发消息）：填好 `claims-worker.env` 的 Graph 版本，`COMPOSE_PROFILES` 增加 `claims`，
   `deploy.sh upgrade`；部署后检查会等待 `claims_worker_ready`。日志里的 `claims_worker_routes` 行列出该进程服务的路由（IR-13）。
   R2（U08）：claims-worker 还需要 `dsn_lc_retention_job`（`secrets-init.sh` 生成、provisioning 建登录），每小时跑保留期清理；
   部署后检查用该登录执行 `retention-admin status`，要求 26 小时内有一次运行且 `enforced=1`（`compose.env`
   `LC_REQUIRE_RETENTION_ENFORCED=0` 仅限 W1 试点）。启用保留策略与按人删除见 `docs/runbooks/claims-data-deletion.md`。
6. **Webhook 路由（G1 已关闭，R1 裁决 F2）**：把 Meta app/object/asset 映射到租户与店铺。登录 `lc_meta_registrar`（已在 provisioning 中，
   preflight/provisioning 检查它能执行 `register_meta_binding`、`activate_route`、`disable_route`、`register_meta_page_token` 共 4 个 definer）。
   `--proof` 是 owner 提供的资产所有权证据的 sha256（小写 64 位十六进制），例如把 `GET /{page-id}/subscribed_apps` 的 Graph 读回结果存档后
   `sha256sum`；CLI 不调用 Meta，证据文件由 owner 保管（不进仓库、不进日志）。`--principal` 必须在该店铺持有 `integration:manage`。
   ```sh
   deploy/scripts/ops-admin.sh meta-admin route --tenant ... --store ... --principal ... \
     --app <app_id> --object page --asset <page-id> --proof <sha256hex> --proof-expires 2026-12-31T00:00:00Z --expected-epoch 0
   # 输出 {"binding_id":...,"binding_version":1,"route_id":...,"route_epoch":1}；binding_id 用于第 3 步 page-token 的 --binding
   # 重新激活/换证据：--expected-epoch <当前 route_epoch>；停用：
   deploy/scripts/ops-admin.sh meta-admin route-disable --route <route_id> --expected-epoch <route_epoch>
   ```
   顺序：先第 6 步（route，得到 binding_id）→ 再第 3 步（page-token）→ 第 4 步（认领来源，G2 已关闭）。
   证据：`TestMetaRouteRegistrarF2`（REAL_PG：签名评论在 route 前隔离、route 后恰好一个 job、跨店冲突、停用后再隔离、无 integration:manage 拒绝）、smoke S44。

7. **G2 已关闭（R1 裁决 G2）**：Studio 开关拆分。`COMMERCE_STUDIO_ENABLED=1` 挂载场次规划并允许 `COMMERCE_CLAIMS_ENABLED=1`（关键词下单 + `GET/PUT .../claim-source`）；
   `COMMERCE_STUDIO_MEDIA_ENABLED=0` 时不构建媒体 planner、不要求 `live.media_plan_ready()`，演练/输入路由返回 404，后台 Studio 隐藏演练栏（API 的 `media_enabled=false`）。
   compose 已接入 `COMMERCE_CLAIMS_LABEL_KEY_FILE`（secret `commerce_claims_label_key`）。证据：`TestStudioG2FlagMatrix`、`TestStudioPlanningOnlyG2APIProcess`（真实二进制 + PG）、
   KC16/T12 浏览器门禁（仅规划模式）、smoke S45（部署后 claims/claim-source 未带令牌返回 401/403，媒体路由 404）。

### 6.4 Meta 广告：商家连接广告账户（meta-ads-v1）

前提：`migrations/0080_*`（ads-capi）已合并（裁决 B15，P06 检查）；应用「大梦」4291253377792879 已挂到香港大碗 Business Portfolio 且 owner 完成 App Review / 商业验证（裁决 O-C，owner 步骤）；每店广告上限默认 NT$0＝关闭（裁决 O4），由运营设置。

1. 密钥：`secrets-init.sh` 生成 HPKE X25519 私钥环 `commerce_meta_ads_hpke_private_keys_json`（**只有 ads-worker 挂载**）并派生公钥环 `commerce_meta_ads_hpke_public_keys_json` + `commerce_meta_ads_hpke_active_key_id`（api 挂载，只能加密、不能解密）。owner 以文件提供 `commerce_meta_ads_app_secret`（替换 `__UNSET__`，0440，裁决 O-D；不经过聊天）。
2. 配置：api.env 设 `COMMERCE_META_ADS_APP_ID`、`COMMERCE_META_ADS_CONFIG_ID`（Facebook Login for Business 配置）、`COMMERCE_META_ADS_REDIRECT_URI=https://<LC_ADMIN_HOST>/api/ads/meta/callback`（同时登记到 Meta 应用后台，contract §11）、`COMMERCE_META_ADS_GRAPH_VERSION=v26.0`；ads-worker.env 设同一版本和 `COMMERCE_META_ADS_PARTNER_AGENT`；`COMPOSE_PROFILES` 加 `ads`。`preflight.sh` P03/P05/P06/P08/P09 全绿后 `deploy.sh upgrade`（post-check 等 `ads_worker_ready`）。
3. 回调 URL 的 `code`/`state` 由 Caddy 访问日志改写为 REDACTED（smoke S40 覆盖 `/api/ads/meta/callback`）；admin 对该路径发 `Referrer-Policy: no-referrer`。
4. 关闭：清空 `COMMERCE_META_ADS_APP_ID` 并重启 api（路由 404）。**不要先停 ads-worker**：暂停（pause）只有它能发到 Meta；先在 admin 暂停所有投放，确认 ops 终态后再去掉 `ads` profile。
5. CAPI 与商品 feed（ads-capi，0080）：`secrets-init.sh` 生成 `commerce_capi_external_id_key`（b64std32，**只有 ads-worker 挂载**；缺失则 ads-worker 拒绝启动，已部署环境升级前先重跑 `secrets-init.sh` 补齐）。轮换会改变所有买家的 external_id（匹配率重置，不丢数据）。商品 feed 为 `https://<LC_STORE_HOST>/feeds/meta.csv`（storefront → api `GET /v1/buyer/feeds/meta.csv`，只按已验证 Host 解析店铺，Caddy 无需改动）。买家在隐私页授予 `ads_personalization` 时，同一事务写入 `ads.capi_contexts`（浏览器 UA，8 天后清除）。**生产挂载阻塞项**：`ads.capi_contexts`/`ads.capi_events`/`ads.insights_daily` 尚未登记 customers-billing CD7 保留类（meta-ads-v1 §12），登记前不得在生产开启 CAPI。
6. 广告权限开通（meta-ads-v1 A-1）：0074 只放宽权限 CHECK，`create_initial_store` 不授予 `ads:*`，已部署店铺的 Ads 页在开通前不可用。owner 在聊天批准后，用迁移属主连接运行（幂等、只作用于一个店铺的创建者；需该主体已持有完整创建者权限集，否则整笔回滚）：`psql "$MIGRATION_DATABASE_URL" -v ON_ERROR_STOP=1 -v store_id=<店铺 uuid> -v principal_id=<主体 uuid> -f scripts/ops/grant-ads-permissions.sql`，输出 `granted=<0..3>`。授予权限不等于可投放：每店广告上限默认 NT$0（关闭，裁决 O4），由运营另行设置。
7. 卡住的草稿（无法解绑广告账户）：解绑会被触发器 `bindings_ads_disable_guard` 以 `binding_in_use`（409）拒绝，直到该店所有草稿都不再“可能花费”（AD6）。先在 admin 暂停；暂停 op 需到 SUCCEEDED。若暂停一直到不了：(a) 令牌被 Meta 撤销 → 商家用**同一个**广告账户重新连接（令牌换新、binding 不变），advance sweeper 会再规划下一个 pause seq；(b) 激活 op 停在 UNKNOWN → 等 reconcile（不要手工改 ops 行）；(c) 以上都不成立 → 在 Ads Manager 核实广告已停，草稿在 `ends_at + 1 天` 后自动不再计入，解绑随之放行。没有操作员强制覆盖（有意：覆盖等于允许“看不见的花费”）；如需加速，先记录根因再另立契约。

### 6.5 平台服务费（Stripe Billing，SANDBOX）与 R2 权限补发

1. 前提（owner 事项）：平台自己的 Stripe **测试**账户（与商家 PSP 账户不同，BD1）、计划 price id（Billing Q1/Q2）。
   owner 以文件提供两个密钥（O-D，绝不经聊天）：`commerce_platform_stripe_secret_key`（只接受 `sk_test_`/`rk_test_`，BD8）、
   `commerce_platform_stripe_webhook_secret`（`whsec_`，Stripe 后台为端点 `https://<LC_HOOKS_HOST>/v1/platform/stripe/webhook` 生成；
   这是与商家 PSP webhook 不同的端点）。**创建端点时必须带 `api_version=`<`internal/integrations/psp/stripe.APIVersion` 当前值>**（载荷使用端点的 API 版本而非客户端的 Stripe-Version；旧版本没有 `invoice.parent`，`invoice.*` 事件会被确认但不生效，日志出现 `billing_ops_alert code=invoice_without_subscription` 即为版本不符）。
2. 启用：`compose.env` 设 `LC_BILLING_ENABLED=1`、`LC_BILLING_PRICE_IDS=price_...`（逗号分隔，1–10 个），`deploy.sh upgrade`。
   未设 = 关闭（`0` 会让 api 启动失败，preflight P06 拦截）；preflight P08 校验 price id，P09 校验两个密钥文件格式。
   webhook 复用 `commerce_stripe_ingress` 登录（`COMMERCE_STRIPE_INGRESS_DATABASE_URL`）。
3. **R2 权限补发**（0079 只给新建店铺的创建者 `customers:read`、`customers:privacy`、`billing:manage`；0079 之前建的店铺需手动补发，
   **需 owner 在聊天中批准**）：
   ```sh
   # 以迁移 owner（数据库 owner）连接执行，见脚本头部 HOW；部署包暂无该脚本的 ops 包装（DESIGN）：
   psql "$MIGRATION_DATABASE_URL" -v ON_ERROR_STOP=1 -v store_id=<store uuid> -v principal_id=<创建者 principal uuid> \
     -f scripts/ops/grant-r2-permissions.sql   # 输出 granted=<0..3>，幂等，只作用于该店铺的创建者
   ```
4. 标准（standing）为 RESTRICTED 时只拒绝新开认领窗口（HTTP 402 `billing_restricted`）；退款、履约、结账不受影响（Q6）。

### 6.6 台湾超商取货（taiwan-cvs-logistics-v1 §12/§16）

- **默认（试点，裁决 X11）**：无需部署步骤。`LC_CVS_ECPAY_ENABLED=0`：买家手填门市（`buyer_entered`，附各连锁官方门市查询链接）、
  超商取货付款（pay-at-pickup）、商家在后台设置连锁/取货付款上限、标记已收款/取消/回库均可用；ECPay 路由返回 404。
- **ECPay 物流（可选，台湾主体商家，仅 SANDBOX）**：
  1. owner 在服务器上提供 `ecpay_logistics_keyring` 文件（单行 JSON `{"active":"<id>","keys":[{"id":"<id>","key_base64":"<32 字节>"}]}`，
     只经文件、不经聊天，裁决 O-D）。它密封各商家的 HashKey/HashIV，与其他 keyring 不共用密钥；轮换只追加新 key 并切换 `active`。
  2. `compose.env` 设 `LC_CVS_ECPAY_ENABLED=1`；前提（preflight P06/P09）：api.env `COMMERCE_PAYMENT_PROFILE=SANDBOX`、`COMPOSE_PROFILES` 含 `claims`
     （面单创建路由 `ecpay.cvs_create` 跑在 claims-worker）、keyring 不是 `__UNSET__`。`deploy.sh upgrade`。
  3. Caddy 只在 hooks 域名放行 `/v1/cvs/ecpay/map-return/*` 与 `/v1/cvs/ecpay/status/*`；ECPay 从 `postgate.ecpay.com.tw` 回调，
     若 hooks 域名走 Cloudflare 代理，需确认该来源不被拦截（§0.3 P1）。
  4. 商家在后台「物流设置」连接 ECPay（API 先探测密钥，再密封入库）。**LIVE ECPay 与真实面单购买**需要 owner 批准并同时改 compose
     中 claims-worker 的 `COMMERCE_PAYMENT_PROFILE`/`CVS_ECPAY_LIVE_CREATE` 两行和 api 的 profile（§0.3 P4）；本版本不启用。

## 7. 密钥轮换（按 deploy/secrets.manifest.tsv 的 rotation 列）

| 密钥 | 做法 |
|---|---|
| `pw_<login>` | 写入新值（`openssl rand -hex 32`，不带换行）→ `secrets-init.sh --rederive` → `docker compose run --rm -T --no-deps provision-logins` → 重启该服务 |
| `pg_superuser_password` | **只用** `deploy/scripts/pg-ops.sh rotate-superuser`（见下文）。**禁止**手工执行 `ALTER ROLE postgres PASSWORD ...`：服务器 `log_statement='ddl'`，新口令会以明文进入 postgres 日志（Docker json-file）和诊断包 |
| `commerce_bff_key` | 替换后 api 和 admin **同时**重启 |
| `commerce_buyer_bff_key` | 替换后 api 和 storefront 同时重启（值必须不同于商家 BFF key） |
| `commerce_buyer_cookie_key` | 替换后所有买家会话失效 |
| `commerce_account_keys_json` / `commerce_meta_payload_keys_json` | **只能追加**新 key，再修改 active id 文件并重启相关服务。**禁止删除**仍被存储凭据引用的 key |
| `commerce_account_replay_key` | 只能走 owner 批准的流程 |
| `commerce_stripe_webhook_keys_json` / `commerce_stripe_webhook_active_key_id` | **只能追加**新 key，改 active id，重启 api，再用 `ops-admin.sh stripe-admin webhook ... --expected-version <当前>` 重新密封各端点的签名密钥。**禁止删除**仍被已登记端点引用的 key |
| `commerce_stripe_webhook_replay_key` | 只能走 owner 批准的流程（不得等于任何签名 key 或支付 key） |
| `commerce_meta_page_token_keys_json` / `commerce_meta_page_token_active_key_id` | **只能追加**新 key，改 active id，重启 claims-worker，再用 `ops-admin.sh meta-admin page-token` 重新登记 token（`--expected-version` 用当前版本）|
| `commerce_claims_actor_key`（K_actor） | 轮换会让所有认领 actor 重新映射：只能走 owner 批准的流程，必须不同于其他任何密钥 |
| `commerce_claims_reply_link_key`（K_link） | 计划与发送之间轮换会让在途私信在 Check 被拒（不发送）；轮换后重启 claims-worker，必须不同于其他任何密钥 |
| Stripe API key（`STRIPE_SECRET_KEY`） | 在 Stripe 后台轮换后执行 `ops-admin.sh stripe-admin rotate`（运维输入，不落盘）；`STRIPE_WEBHOOK_SECRET` 见 §6.1 步骤 3 |

所有密钥文件权限保持 `0440 root:10500`。改完后执行 `preflight.sh`。

`pg-ops.sh rotate-superuser`（在 DB 主机上以 root 执行，postgres 必须在运行；smoke S41 验证）：
1. 生成新值（`openssl rand -hex 32`），先持久化到 `secrets/.pg_superuser_password.rotating`（0400），再改数据库。
2. 在 pg-ops 中用**同一个 psql 会话**：`SET log_statement='none'; SET log_min_error_statement='panic'; SET log_min_duration_statement=-1;`，
   psql 自己读取新值（`` \set pw `cat …` ``，新值经 stdin 进入 pg-ops 的 /tmp tmpfs），`SELECT format('ALTER ROLE postgres PASSWORD %L', :'pw') \gexec`。
   然后验证新口令能登录、旧口令被拒绝。做法与 provision-logins.sh 相同（`deploy/postgres/ops/rotate-superuser.sh`）。
3. **原地**写入 `pg_superuser_password`（同一个 inode，属主和权限不变）：Compose 把文件密钥按单文件 bind mount 挂载，
   原地写入后正在运行的 postgres 容器立即看到新值（`lc_psql`、看门狗、deploy.sh 都依赖它）；改名替换会让容器一直看到旧值，直到重启。
4. `secrets-init.sh --rederive` 更新 `dsn_migrate_owner`。
5. 验证 `lc_psql` 可用，并对 `docker compose logs postgres` 做 `lc_secret_scan`，命中即失败。
- 中途失败：`.pg_superuser_password.rotating` 会保留，**重新执行同一命令即可继续**（若新值已生效则跳过 ALTER）。不要删除这个文件。
- 完成后更新离线加密的密钥副本（backup-restore.md §7）。PITR 不受影响：临时集群用 peer 认证，`--promote` 会把超级用户口令设为当前值。
- Meta 广告 HPKE 环：私钥环只**追加**新 key（不得删除仍能打开已存 token 的 key）→ `secrets-init.sh --rederive`（重新派生公钥环）→ 把 `commerce_meta_ads_hpke_active_key_id` 改成新 id → 重启 ads-worker，再重启 api。P04 检查公私钥环 id 一致、P05 检查 active id 在公钥环中。

## 8. 证书与域名

- Caddy 自动申请和续期证书，数据在 `caddy-data` 卷（/data），需要随主机备份。
- 看门狗 W5 在证书剩余不足 14 天时告警。
- 更换域名的步骤：改 compose.env → preflight --online → `dc up -d caddy api admin`（`dc` 见 incident.md 开头）。origin 由域名推导。
  compose.env 的 `IMAGE_TAG` 由 deploy.sh 维护，等于正在运行的 tag，所以这一步不会换镜像；执行前可用 `watchdog.sh` 的 W10 确认没有漂移。

## 9. 迁移到两台主机 / Kubernetes

- 两台主机（NOT_RUN，风险 R6）：
  - DB 主机：`LC_TWO_HOST=1`，`COMPOSE_PROFILES=db,ops`，设置 `LC_PG_BIND_ADDR=<私网IP>`，并放置 `pg_server_crt`/`pg_server_key`。
    启用 pg_hba 中的 `hostssl` 行。migrate、provision 和 pg-ops 都在 DB 主机上运行。
  - 应用主机：`LC_TWO_HOST=1`，`COMPOSE_PROFILES=app,...`，`LC_PG_HOST=<DB IP>`，`LC_PG_SSLMODE=verify-full`，放置 `pg_ca_crt`，然后执行 `secrets-init.sh --rederive`。
  - 代价：两边的 `backend` 网络都变为非 internal，需要用主机防火墙限制出站。
- Kubernetes：暂不提供 manifest（架构.md 规定首发不上 K8s）。映射关系见 deploy-design §19：
  - web Pod = 边缘 sidecar + api + admin + storefront。
  - migrate/provision 改为 Job，pg-ops 改为 CronJob。
  - Secret 挂载到 `/run/secrets`，lcentry 的约定不变。

## 10. 发布检查清单

- [ ] owner 批准记录（范围、tag、窗口）
- [ ] `bash scripts/dev/release-gate.sh --strict` 退出码 0（R1 验收命令：每个 tier 一行 PASS/FAIL/NOT_RUN；NOT_RUN 不是通过），证据在主 checkout 的 `output/release-gate/<UTC>-<sha>/`
- [ ] `deploy/scripts/smoke.sh static` 和 `smoke.sh full`（Linux 部署主机、root）无 FAIL，证据位于 `deploy/.evidence/<run>/result.json`，由集成者复制到 `evidence/release/`（I6）；`S29m` BLOCKED（I8）是已记录的已知限制，不是 PASS
- [ ] 独立验收：test_worker 复跑 smoke；security_reviewer 审查 hba、密钥、加固和 Caddy（作者不能是唯一验收人）
- [ ] `preflight.sh --online` 全部 PASS
- [ ] 升级前备份已经成功，且最近 30 天内做过恢复演练（backup-restore.md）
- [ ] 密钥离线加密副本已经更新（backup-restore.md §密钥备份）
- [ ] `deployments.log` 已经记录本次部署
