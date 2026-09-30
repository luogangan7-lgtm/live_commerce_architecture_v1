<!--
File: docs/runbooks/stripe-live.md
Purpose: Stripe LIVE 激活、canary、停收和监控的运维步骤（stripe-live-enable-v1 §2、§7、§8、§10）：cutover 前置 SQL、逐店审批 → LIVE 探测 → canary → 放开，
  以及 key / webhook 密钥泄露处置。每一步是单独的 `ops-admin.sh` 命令和一条审计行。
Runs as/in: 文档（运维人员在**生产主机**上以 root 执行其中命令；Dashboard 步骤由 owner 在 Stripe 后台完成）。
Reads env / secrets: 无。命令读取 /etc/live-commerce/compose.env（LC_STRIPE_LIVE_* 配对）；live key 与 whsec_ 只经 `<NAME>_FILE` 文件路径进入 ops-admin.sh
  （LD3/O-D），本文只写占位符，不含任何密钥值，agent 从不读取、打印、grep 或复制这些文件，也从不带 `-x` 运行脚本。
Used by: 运维/owner/集成者；deploy.md §2、§6、Phase B 与 merchant-onboarding.md §3 链接到此。
Depends on: contracts/stripe-live-enable-v1.md（v1 FROZEN）、deploy/scripts/{ops-admin,preflight,watchdog}.sh、cmd/stripe-admin 的 live-approve|live-canary|live-revoke 子命令
  （flags 冻结在 docs/delivery/units/stripe-live-core.md）、docs/runbooks/deploy.md。
Status: DESIGN。SL-LIVE01（owner canary）与 SL-LIVE02（owner 停收演练）均为 NOT_RUN（只有 owner 能完成）；没有自动化 LIVE 测试，CI 也从不持有 live key。
  本文所有命令在没有 stripe-live-core 合并前无法运行（子命令与 0077 迁移尚不存在）。
Change rules: 命令必须与 ops-admin.sh 的白名单/配对规则和 cmd/stripe-admin 的 flags 一致；owner-only 步骤不得写成“工程可以完成”；
  任何输出证据的文件只含 id、布尔值、长度和时间，不含卡数据、邮箱、session URL 或密钥。
-->
# Stripe LIVE 运维手册

**铁律（LD3、O-D）**：live key 不经过聊天、仓库、CI、agent 上下文、日志或 URL。owner 把 `rk_live_…` 和 `whsec_…` 各写进服务器上的一个文件；运维只把**路径**交给
`ops-admin.sh`。任何人（包括 agent）都不打开、打印、grep、复制这些文件，也不带 `-x` 运行脚本。命令输出只有 id、版本和布尔值。

**所有 `stripe-admin …` 命令都写成 `deploy/scripts/ops-admin.sh stripe-admin …`——这是唯一的生产入口。** 下文 `<tenant>`、`<store>`、`<owner-principal>` 见 merchant-onboarding.md §2，
`<connection-id>`、`<approval uuid>`、`<endpoint-uuid>` 是前一步的输出或本次新生成的 UUID（`uuidgen | tr A-Z a-z`）。

## 0. 前置（缺一不得开始）

1. owner 书面批准（聊天里的一条消息，记入 Humaux）：**店铺、币种、canary 上限、单笔上限、“注册后删除传输文件”**。它的引用就是 `approval_ref`，格式
   `owner-chat:<YYYY-MM-DD>:stripe-live:<store-slug>`（LQ8）。**这条消息之前不做任何 LIVE 步骤。**
2. owner 在 Stripe Dashboard（live 模式）完成：账户激活/KYC、描述符（5–22 个拉丁字符，卡前缀 2–10）、收款银行、Radar 默认规则 + CVC 规则、
   用 SL08 权限清单创建受限 key `rk_live_…`（Access policy 限定为服务器出口 IP）、创建 live webhook 端点
   （URL `https://<LC_HOOKS_HOST>/v1/stripe/webhook/<endpoint-uuid>`，8 个事件，API 版本 `2026-08-26.dahlia`）。这些是 owner 的事，工程无法代办。
3. owner 在服务器上把 key 和 whsec_ 各写入一个文件（**owner-only 目录，仓库之外**，例如 `/root/lc-transfer/`，目录 0700）：文件属主必须是运行 `ops-admin.sh` 的 uid
   （通常 root；由 owner 用该 uid 写入，或事后 `chown` 给它），权限 0400 或 0600，**只有一行**，无符号链接。不满足时 ops-admin 报 `STRIPE_SECRET_KEY_FILE rejected: …`
   （只报变量名和规则，不报内容）。
4. compose.env 已设置 LIVE 配对（成对，缺一 preflight P06 失败）：
   ```
   COMMERCE_PAYMENT_PROFILE=LIVE            # env/api.env
   COMPOSE_PROFILES=db,app,payments-live,payments-sandbox   # sandbox 保留到 §1 的计数为 0
   LC_STRIPE_ENABLED=1
   LC_STRIPE_LIVE_ENABLED=1
   LC_STRIPE_LIVE_APPROVAL_REF=<approval_ref>
   ```
   `LC_STRIPE_CHECKOUT_ENABLED` 缺省跟随 `LC_STRIPE_ENABLED`；要先接单前对账、不接买家结账时显式设为 0（§3 平台级停收）。
5. 主机侧：生产主机与 `LC_HOOKS_HOST` 的有效 TLS 已就绪（L4），`deploy/scripts/preflight.sh` 全 PASS，`deploy/scripts/watchdog.sh` PASS。
   （目前没有生产主机，SL-LIVE01 在它就绪前是 NOT_RUN。）

## 1. Cutover 前置 SQL（LD1）：必须是 `0|0`

切到 LIVE profile **之前**运行；不是 0 就等它们收敛（`payments-sandbox` 一直保留到此），只读、只输出计数：
```sh
source deploy/scripts/lib.sh; lc_load_env "$LC_COMPOSE_ENV"
# -- cutover-precondition begin
lc_psql <<'SQL'
SELECT
 (SELECT count(*) FROM checkout.payment_attempts a
   WHERE a.method_code = 'stripe_checkout' AND a.environment = 'SANDBOX'
     AND NOT EXISTS (SELECT 1 FROM payments.facts f WHERE f.tenant_id = a.tenant_id AND f.store_id = a.store_id
                      AND f.attempt_id = a.id AND f.kind IN ('CAPTURED', 'CLOSED_UNPAID'))) AS sandbox_open_attempts,
 (SELECT count(*) FROM payments.stripe_refunds r
   WHERE r.environment = 'SANDBOX'
     AND NOT EXISTS (SELECT 1 FROM payments.refund_facts x WHERE x.tenant_id = r.tenant_id AND x.store_id = r.store_id
                      AND x.refund_id = r.id)) AS sandbox_open_refunds;
SQL
# -- cutover-precondition end
```
输出形如 `0|0`（attempt 的终态 = 已有 `CAPTURED` 或 `CLOSED_UNPAID` 事实；退款的终态 = 已有任一退款事实）。

## 2. 顺序（每步一个命令一条审计行；失败最多两次定向修复，然后升级，AGENTS.md）

```
0  §1 计数 = 0|0；部署 LIVE profile（deploy.sh；preflight P06 双向通过）
1  STRIPE_SECRET_KEY_FILE=<路径> … register --environment LIVE      → connection（VerifyAccount，livemode）；owner 删除传输文件
2  STRIPE_WEBHOOK_SECRET_FILE=<路径> … webhook --profile LIVE        → endpoint，whsec 封存；owner 删除传输文件
3  live-approve                                                       → GET /v1/account 就绪读取 + 审批行
4  qualify --profile LIVE                                            → create + expire + retrieve 探测（不扣款），REAL_LIVE 资格
5  method --enabled --visible --max ≤ canary 上限                     → CANARY（对该店所有买家开放，≤ 上限）
6  owner 用自己的卡真实下单支付（≤ 上限），商家后台全额退款 → SUCCEEDED，等退款 webhook
7  live-canary                                                        → SQL 验证，写入 canary_verified_at
8  method --max ≤ max_minor                                           → OPEN
```

### 步骤 1 登记账户（`rk_live_` 只经文件路径）
```sh
export STRIPE_ACCOUNT_ID=<acct_...>          # 不是密钥
STRIPE_SECRET_KEY_FILE=<owner 写的文件路径> \
  deploy/scripts/ops-admin.sh stripe-admin register --environment LIVE \
  --tenant <tenant> --store <store> --principal <owner-principal>
# 输出 {"connection_id":...,"credential_version":1}；只接受 rk_live_，sk_live_ 一律拒绝（stripe_live_key_unrestricted）
```
输出打印后由 owner 删除传输文件（批准消息里已写明）。key 轮换见 §6。

### 步骤 2 登记 live webhook 端点
```sh
STRIPE_WEBHOOK_SECRET_FILE=<owner 写的文件路径> \
  deploy/scripts/ops-admin.sh stripe-admin webhook --tenant <tenant> --store <store> --principal <owner-principal> \
  --connection <connection-id> --endpoint <endpoint-uuid> --profile LIVE --expected-version 0 --enabled
# 输出 {"endpoint_id":...,"key_version":1}
```
端点 uuid 必须与 owner 在 Dashboard 里注册的 URL 一致。

### 步骤 3 审批（把 owner 的书面批准落成 DB 行）
**前置（`--attest` 里的两个码要有证据才能写）：**
- `policy_pages`：`LC_LEGAL_REQUIRE_FINAL=1 node tests/storefront/legal-pages.mjs` 必须全绿（LG01；现状 BLOCKED：页脚在 customers-billing-ui 合并前未挂载 B8，且政策文本仍有 owner/draft 占位）。
- `rak_live`：`output/stripe-live/rak-permissions.txt` 必须存在（SL08 在 owner 的 `rk_test_` 上跑过；现状 NOT_RUN）。
缺任一项就不得把对应码写进 `--attest`，也就不能批准。
需要配对（ops-admin 与 CLI 都先拒绝没有配对的调用）。`--attest` 必须列全 §9 的 11 个码，缺一个被拒绝：
```sh
deploy/scripts/ops-admin.sh stripe-admin live-approve --tenant <tenant> --store <store> --principal <owner-principal> \
  --approval <approval uuid> --connection <connection-id> --expected-version <credential_version> --currency TWD \
  --approval-ref <approval_ref> --approved-at <owner 批准时间 RFC3339> --canary-max 5000 --max 2000000 \
  --attest account_active,canary_private,descriptor,dispute_notice,managed_off,payout_bank,policy_pages,radar_default,rak_live,three_ds,webhook_live
```
- `--principal` 必须同时有 `integration:manage` 和 `payments:refund`（店铺创建者）。
- TWD：canary ≤ 2 × 最小额（NT$50 = 5000 minor），单笔上限 ≤ NT$20,000（2,000,000 minor，前 30 天，LQ3）。其他币种在 owner 定单笔上限前被拒绝。
- 账户就绪（`charges_enabled`/`payouts_enabled`/`details_submitted`/无 `currently_due`/描述符长度）由代码向 Stripe 读取；读不到字段 = `stripe_live_readiness_unknown`（失败即关闭），不是通过。
- 重复执行同一 `--approval` 且参数完全相同 → 返回同一 id；参数不同 → 拒绝。

### 步骤 4 LIVE 资格探测（会真实创建并立即过期一个 live Checkout Session，不扣款）
```sh
deploy/scripts/ops-admin.sh stripe-admin qualify --tenant <tenant> --store <store> --principal <owner-principal> \
  --connection <connection-id> --expected-version <credential_version> --profile LIVE --currency TWD --amount-minor 2500 \
  --return-url https://<LC_STORE_HOST>/payment/return
# 输出 {"qualification_id":...}；使用已封存的 key（不需要再传 key），资格 30 天有效，过期只需重跑本步（不用重新 canary）
```

### 步骤 5 打开 CANARY 支付方式
```sh
deploy/scripts/ops-admin.sh stripe-admin method --tenant <tenant> --store <store> --principal <owner-principal> \
  --market <market-id> --country TW --connection <connection-id> --qualification <qualification-id> --expected-version 0 \
  --enabled --visible --sort 10 --min 2500 --max 5000 --name-hans ... --name-hant ... --name-en ...
```
**CANARY 对该店的所有买家开放（≤ 上限）**，靠 owner 声明的 `canary_private`（店铺未对外宣传）限制暴露面，不靠买家身份。

### 步骤 6 owner 真实支付与全额退款（owner 亲自做）
owner 以买家身份用自己的卡在真实店铺下单一件 ≤ 上限的商品，在 Stripe 页面支付，等订单 CONFIRMED；再以有 `payments:refund` 的商家在后台**全额**退款，
等 SUCCEEDED 和退款 webhook 收据。**agent、CI 和测试从不调用 LIVE 退款路径**（LD7）；Stripe 对 canary 的手续费不退（金额 UNKNOWN，owner 接受）。

### 步骤 7 记录 canary
```sh
deploy/scripts/ops-admin.sh stripe-admin live-canary --tenant <tenant> --store <store> --principal <owner-principal> \
  --approval <approval uuid> --attempt <canary attempt id> --refund <canary refund id>
# 输出验证时间；SQL 校验币种、金额 ≤ 上限、全额退款、无 review case、支付和退款各至少一条 ACCEPTED 的 LIVE 收据
```
证据文件 `output/stripe-live/<store>/canary.txt`（gitignored，只含）：approval id、attempt id、refund id、verified 时间、commit SHA、
`calculated_statement_descriptor` 的**长度**和是否等于配置的前缀/描述符（布尔）。**不含**卡数据、邮箱、session URL。

### 步骤 8 放开
```sh
deploy/scripts/ops-admin.sh stripe-admin method --tenant <tenant> --store <store> --principal <owner-principal> \
  --market <market-id> --country TW --connection <connection-id> --qualification <qualification-id> --expected-version <当前版本> \
  --enabled --visible --sort 10 --min 2500 --max <max_minor> --name-hans ... --name-hant ... --name-en ...
```
`--max` 不能高于审批里的 `max_minor`；canary 未验证前 SQL 拒绝高于 canary 上限的值。

失败处理：任一步失败 → 先 `method --enabled=false`（§3），店铺保持 CANARY，诊断；同一阻塞最多两次定向修复，然后升级。

## 3. 停收（LD6、§7）：不停止对账

| 范围 | 命令 | 效果 | 不受影响 |
| --- | --- | --- | --- |
| 单店，可恢复 | `deploy/scripts/ops-admin.sh stripe-admin method --tenant <tenant> --store <store> --principal <p> --market <m> --country TW --connection <c> --qualification <q> --expected-version <v> --enabled=false`（**不需要配对**） | 下一次开始支付返回 PT409（method），页面显示不可用；在任何状态下都成功（含 revoke/过期/轮换之后） | 已打开会话的交接（到期为止）、worker、webhook、退款 |
| 单店，强制 | `deploy/scripts/ops-admin.sh stripe-admin live-revoke --tenant <tenant> --store <store> --principal <p> --approval <approval uuid> --revoke-ref <ref>`（**不需要配对**） | 审批及其 REAL_LIVE 资格被撤销 → 开始支付 PT409；重新启用要新审批 + 探测 + canary | 同上 |
| 全平台 | compose.env 设 `LC_STRIPE_CHECKOUT_ENABLED=0`，然后 `deploy.sh`（重启 api） | Stripe 从所有店铺的托管结账中移除 | worker、webhook、退款继续 |

**不要用停止 `payment-worker-live` 或 webhook 来止损**：会让 PAYMENT_PENDING 的库存和 UNKNOWN 的退款悬空（stripe-psp D5，refund RD3/RD4）。
已打开的会话不会被强制过期，它们在 `expires_at`（≤ 40 分钟）自然关闭。owner 的 SL-LIVE02 演练：`method --enabled=false` → 店铺页显示 Stripe 不可用 → 重新打开。

## 4. 泄露处置

| 事件 | 做法 | 后果 |
| --- | --- | --- |
| API key 泄露 | Stripe Dashboard → API keys → 该受限 key → **Expire key**（或 **Rotate key**，Expiration 选 **Now**；泄露时**绝不**用 7 天宽限期）。然后 owner 把新 `rk_live_` 写进文件，运行 `STRIPE_SECRET_KEY_FILE=<路径> STRIPE_ACCOUNT_ID=<acct_...> deploy/scripts/ops-admin.sh stripe-admin rotate --environment LIVE --tenant <tenant> --store <store> --principal <owner-principal> --connection <connection-id> --expected-version <n>`（配对必须仍在 compose.env；缺 `--environment LIVE` 时 ops-admin 直接拒绝 `stripe_live_environment_required`） | 该账户所有 Stripe I/O 认证失败，attempt 保持 UNKNOWN（库存保留）直到新 key 登记并重新 `qualify`；审批保留 |
| webhook 签名密钥泄露 | Dashboard → live 端点 → **Roll secret**，选立即过期；然后 `STRIPE_WEBHOOK_SECRET_FILE=<路径> deploy/scripts/ops-admin.sh stripe-admin webhook … --profile LIVE --expected-version <当前版本> --enabled` | 旧密钥签名的事件被拒绝；Stripe 最多重试 3 天（L4）；worker 轮询不受影响 |
| 传输文件在仓库、备份或日志里被发现 | 视同 key 泄露：先按上一行处置，再删除文件，再 `lc_secret_scan` 检查日志 | — |

## 5. 监控：watchdog W11a–f（只输出检查 id 和计数）

`deploy/scripts/watchdog.sh` 每次运行（cron `*/5`）在同一个 psql 会话里输出六行 `W11x PASS|FAIL <name>=<count> max=<n>`；只针对 `environment='LIVE'`，SANDBOX 部署恒为 PASS。
告警 webhook 只带失败的检查 id。阈值是 §8 默认值，可用 `LC_W11_*`（整数）覆盖，见 watchdog.sh 头部。

| 检查 | 含义 | 默认阈值 | 先做什么 |
| --- | --- | --- | --- |
| W11a | LIVE Stripe 操作（建 session / 退款）UNKNOWN 超过 60 分钟 | > 0 | 查 worker 日志与 Stripe 可达性；**不要手动重发**（UNKNOWN 走对账，不盲重试） |
| W11b | 24 小时内 LIVE 支付 attempt 出现 review case（含 CLOSURE_CONTRADICTED） | > 0 | 人工核对 Stripe Dashboard 与订单；case 只读不改 |
| W11c | 已付订单的履约交接 `REVIEW_REQUIRED` | > 0 | 同 W11b，决定发货或退款 |
| W11d | LIVE 退款超过 24 小时无终态事实，或存在 `REFUND_UNRESOLVED`/`REFUND_HISTORY` review | > 0 | 查 refund webhook 收据与退款 worker；确认 Stripe 侧退款状态 |
| W11e | 最近 60 分钟被隔离/忽略的 LIVE webhook 收据 | > 5 | 查端点 URL、签名密钥版本、`livemode` 是否错配（qualify 探测会产生少量 ignored 事件，阈值已留余量） |
| W11f | LIVE 端点被禁用而该店仍有 enabled+visible 的支付方式 | > 0 | 重新启用端点，或对该店 `method --enabled=false`（§3） |

Stripe 侧的队列**不在**我们的系统里：Radar 审核队列、争议（dispute）、payout 失败由 owner 在 Stripe Dashboard/邮件里盯（争议窗口 7–21 天，L10）。
只有退款前置检查看到 `Disputed=true` 时，争议才会以 `CONFLICTING_REPORT` 形式出现在我们这里。

## 6. 验收与证据标签

| 门 | 层级 | 状态 |
| --- | --- | --- |
| SL06 shell 半边（ops-admin `_FILE`/白名单/配对、preflight P06 双向） | process + shell | 由 stripe-live-tests 独立编写；实现者自检日志只是证据，不是门 |
| SL09（watchdog W11 红/绿，种子化 PG） | REAL_PG + shell | 同上 |
| SL-LIVE01（owner canary，§2 步骤 6–7） | **LIVE** | NOT_RUN，owner 完成后才成立；之后该店标签为 `LIVE (owner canary)`，不是 `production_supported` |
| SL-LIVE02（owner 停收演练，§3） | **LIVE** | NOT_RUN，owner 完成 |
