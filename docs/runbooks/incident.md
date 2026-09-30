<!--
File: docs/runbooks/incident.md
Purpose: 故障处置运行手册 — 日志位置、分诊流程、按服务诊断、支付 UNKNOWN、安全事件、升级规则与禁止事项。
Runs as/in: 文档（命令在部署主机上以 root 执行）。
Reads env / secrets: 无（本文不含密钥；诊断包会做密钥扫描）。
Used by: 值班/运维/owner；deploy.sh 失败提示与 watchdog 告警指向此处。
Depends on: deploy/scripts/{watchdog,collect-diagnostics,preflight,pg-ops}.sh, deploy/compose.yml。
Status: DESIGN；日志标记与检查项已在本地 scratch 运行中出现过（VERIFIED_LOCAL），生产未执行。
Change rules: 新增服务或新日志标记时同步 §1 表格。
-->
# 故障处置运行手册

所有 `docker compose` 命令都要通过部署脚本使用的相同参数执行。建议在 shell 中定义：
```sh
dc() { docker compose --project-directory /opt/live-commerce/deploy --env-file /etc/live-commerce/compose.env -f /opt/live-commerce/deploy/compose.yml "$@"; }
```
`dc` 从 compose.env 读取 `IMAGE_TAG`。deploy.sh 在每次 `up -d` 之前把要运行的 tag 写进去，所以 `dc up`/`dc run migrate` 使用的就是当前部署的镜像。
**不要在 shell 里 `export IMAGE_TAG=…`**（环境变量优先于文件），也不要手工改这一行；看门狗 W10 会报告容器 tag 与文件不一致。

## 1. 日志在哪里

| 服务 | 命令 | 内容 | 就绪/错误标记 |
|---|---|---|---|
| caddy | `dc logs --since 30m caddy` | JSON 访问日志和运行日志（含客户端 IP）。URI、Referer、Location 中的 `hub.verify_token`、`code`、`state`、`token`、`access_token`、`id_token` 记为 `REDACTED`（smoke S40） | ACME 错误、`dial tcp 127.0.0.1:...` 表示上游挂掉 |
| api | `dc logs --since 30m api` | slog 文本 | 启动失败**只会**打印 `api stopped`（设计如此，见 §3 api） |
| admin / storefront | `dc logs admin` / `dc logs storefront` | Next 服务日志 | 模块加载阶段的配置错误，例如 `missing COMMERCE_BFF_KEY`、`invalid keys` |
| expiry-worker | `dc logs expiry-worker` | slog | 正常：`expiry_worker_ready`；异常：`*_invalid_config`、`*_database*`、`*_start_failed` |
| payment-worker-* | `dc logs payment-worker-sandbox` | slog | 正常：`payment_worker_ready`；其他标记同上 |
| meta-worker | `dc logs meta-worker` | slog | 正常：`meta_worker_ready`；异常：`worker_start_diagnostic` |
| claims-worker（profile claims） | `dc logs claims-worker` | slog | 正常：`claims_worker_ready` 和一行 `claims_worker_routes`；异常：`claims_worker_invalid_config`、`claims_worker_database_unavailable`、`claims_worker_routes_unavailable`、`claims_worker_start_failed`。**它是唯一向 Meta 用户发送私信的进程**：怀疑误发时先 `dc stop claims-worker` |
| stripe-admin / meta-admin（一次性，profile ops） | `deploy/scripts/ops-admin.sh ...`，审计行在 `$LC_STATE_DIR/ops-admin.log` | 只有 JSON 结果行 | 失败只输出固定码：`stripeadmin: config\|database\|rejected\|provider`、`meta_admin_usage\|config\|database\|register_failed\|version_conflict`；`database` = 登录/连接问题（先看 provision-logins 的 DRIFT）
| postgres | `dc logs postgres` | stderr（`%m [%p] user@db/app`） | 慢 SQL > 500 ms、锁等待、DDL、检查点；**不记录绑定参数** |
| migrate | `dc logs migrate` | 固定标记 | 成功：`migrate_applied`；异常：`migrate_busy`、`migrate_failed sqlstate=…`、`migrate_invalid_config` |
| provision-logins | `dc logs provision-logins` | 每个登录角色一行 | `auth=ok`、`DRIFT ...`、`ready.*=t/f`、`summary ... result=ok` |

- 文件位置：`/var/lib/docker/containers/<id>/<id>-json.log`。json-file 驱动，每份 20 MB × 5 份，并压缩。
- 这个版本没有集中日志系统（O10）。

## 2. 分诊流程

1. 看看门狗：`deploy/scripts/watchdog.sh`。它会输出 W1–W10 的 PASS/FAIL，可以快速定位问题类别。
   - W10（镜像 tag 漂移）：某个 `lc-*` 容器运行的 tag 与 compose.env 的 `IMAGE_TAG` 不同，说明有人绕过 deploy.sh 起了容器，
     或者部署中途失败。**不要**直接 `dc up -d`（它会按文件里的 tag 重建容器）：先看 `deployments.log` 最后一行和 `dc ps`，
     再按 deploy.md §5 用 `deploy.sh upgrade|app-rollback <tag>` 收敛。
2. `dc ps`：查看状态、健康状况和重启次数。
3. 健康检查：`curl -fsS https://<api>/healthz`；`docker inspect -f '{{.State.Health.Status}}' <容器>`。
4. 查看对应服务的日志（§1）。
5. 打包现场：
   ```sh
   deploy/scripts/collect-diagnostics.sh --since 2h
   ```
   诊断包路径为 `/var/tmp/lc-diag-*.tar.gz`（0600），已经做过密钥扫描，但仍可能含 IP 和域名。只发给处理这次事故的人。

## 3. 按服务诊断

### caddy
- 证书申请失败：检查 DNS 是否指向本机（`preflight.sh --online` 的 P14）、80/443 是否对外可达、是否触发了 ACME 频率限制（演练时改用 staging CA）。
- 502/503：上游挂掉。Caddy 会对 502/503/504 统一返回 503 并带 `Retry-After: 60`。先检查 api/admin/storefront。
- 配置改动后热加载：`dc exec caddy caddy reload --config /etc/caddy/Caddyfile --address unix//run/caddy/admin.sock`。

### api（启动失败时只打印 `api stopped`）
1. `deploy/scripts/preflight.sh --online`：它离线复刻了 api 的配置规则，会指出具体的规则号和变量名。
2. `dc run --rm -T --no-deps provision-logins`：检查登录角色、成员矩阵和就绪门禁。
3. 如果启用了 identity，在边缘 netns 中测试 OIDC 发现：
   `dc exec caddy wget -q -O- "$LC_OIDC_ISSUER/.well-known/openid-configuration"`。
4. 仍然无法定位时，在 **staging 副本**中逐个关闭开关（`COMMERCE_*_ENABLED`）做二分。**不要在生产上试错**。
- 常见原因：
  - `LISTEN_ADDR` 不是回环地址。compose 中已经固定为回环。
  - buyer 的 BFF key 与商家 BFF key 相同。
  - `COMMERCE_BUYER_SESSION_TTL` 越界。
  - DSN 对应的登录角色拥有多个权限角色（provision 会报 DRIFT）。

### admin / storefront
- 配置错误会在模块加载时抛出，日志中能看到。
- `__Host-` cookie 只能在 HTTPS 下工作。确认访问经过 Caddy，并且 `COMMERCE_PUBLIC_ORIGIN` 使用 https。
- storefront 会校验 Host，所以只能通过 `LC_STORE_HOST` 访问。
- 打开 `https://<admin>/` 后浏览器被跳到 `https://localhost:3100/...`（连接被拒绝）：检查 admin 容器的 `HOSTNAME`，它必须是 `localhost`，不能是 `127.0.0.1`。
  原因见 compose.yml admin 的注释和 deploy/README.md 偏差第 10 条。
  自检命令：`curl -sI --resolve <admin>:443:127.0.0.1 https://<admin>/ | grep -i location`，应该返回 `location: /zh-CN`（相对路径）。

### workers
- 没有出现就绪标记：先查 `*_invalid_config`（例如并发数不是 1–16 的规范十进制数），再查 `*_database*`（DSN、角色、网络）。
- `*_queue_ready()` 为 false，表示迁移没有应用或只应用了一部分。**不会自动修复**：用 `dc run --rm -T migrate` 确认 ledger，然后联系集成者。
- 看门狗 W6：
  - `stale_available`：job 超过 10 分钟没人处理，说明 worker 停了或卡住。
  - `retryable` 过多：外部依赖异常。
  - `new_discarded`：job 被放弃，必须人工查看。支付 job 要走对账流程，见 §4。

### postgres
- 磁盘满（W2）：先清理 Docker 镜像和日志。**不要删除 `wal/`**，它由 basebackup 负责清理。
- 归档失败（W4）：检查 `${LC_BACKUP_DIR}/wal` 的权限（`0700 999:999`）和剩余空间；`dc logs postgres | grep archive`。
  已存在的同名同内容段会被视为成功；**不同内容的同名段**会持续失败，需要人工比对。
  PITR 切换后出现这种情况，说明数据目录不是用 `restore-pitr --promote` + `pitr-cutover` 放进来的（backup-restore.md §6）：
  `SELECT timeline_id FROM pg_control_checkpoint()` 应大于 1。
- 连接数过高（W9）：执行 `SELECT application_name, state, count(*) FROM pg_stat_activity GROUP BY 1,2`，看是哪个服务的连接池过大。
- 长事务（W7）：找到对应的 `application_name`。只能在确认影响后，经批准再执行 `pg_terminate_backend`。

### DB 角色（provision-logins 报 DRIFT）
- DRIFT 会指出登录角色名和问题，例如 `membership=commerce_runtime,commerce_auth` 或 `set_role_allowed`。
- 修复必须人工完成并经过评审：在 pg-ops 中以超级用户执行精确的 `REVOKE <多余角色> FROM <login>`，然后重跑 provision-logins。
  脚本**永远不会自动撤销权限**。

### migrate
- 退出码 75（`migrate_busy`）：另一个会话持有 advisory lock 718020260920。
  用 `SELECT pid, application_name FROM pg_locks JOIN pg_stat_activity USING(pid) WHERE locktype='advisory'` 查出持有者，确认它是正在执行的迁移后等待完成。**禁止循环重试**。
- `migrate_failed sqlstate=XXXXX`：按 SQLSTATE 定位。checksum 不匹配表示有人修改了已经应用的 SQL，**只能前向修复**。
- 如果库中的 ledger 版本比当前镜像新（例如回滚到旧镜像后），脚本会拒绝 app-rollback。只能前向修复（I4 会增加专门的哨兵错误）。

### 备份
- 备份失败会导致 upgrade 中止，这是预期行为。检查 `${LC_BACKUP_DIR}` 的空间和权限，以及 pg-ops 的输出。
- W3 告警：检查 cron（`/etc/cron.d/live-commerce`）以及 `/var/log/live-commerce-backup.log`。

## 4. 支付 UNKNOWN / 外部不确定状态

- **绝不盲目重试**（AGENTS.md）。先到 PAYUNi 商户后台核对交易的真实状态，再由 worker 的查询路径收敛。
- 不要人工改库去"修复"支付状态。

## 5. 安全事件

| 事件 | 处置 |
|---|---|
| `commerce_account_keys_json` 或 replay key 泄露（涉及真实资金） | 立即 `dc stop payment-worker-live`，通知 owner，按 deploy.md §7 轮换（追加新 key），评估商户 PSP 凭据是否需要在 PAYUNi 侧重置 |
| Stripe webhook 签名 keyring 或某端点的 `whsec_` 泄露 | 在 Stripe 后台滚动该端点的签名密钥，用 `ops-admin.sh stripe-admin webhook ... STRIPE_WEBHOOK_SECRET_NEXT` 登记新密钥（deploy.md §6.1 步骤 3），必要时追加 keyring key（deploy.md §7）；`STRIPE_SECRET_KEY` 泄露 → 在 Stripe 后台撤销并 `stripe-admin rotate` |
| Meta Page token 泄露 | 在 Meta 后台撤销 token，`dc stop claims-worker`，用 `ops-admin.sh meta-admin page-token` 登记新 token（`--expected-version` = 当前版本），确认后再启动 |
| BFF key 泄露 | 轮换这一对 key（api + admin 或 api + storefront），同时重启 |
| 数据库口令泄露 | 轮换对应的 `pw_<login>`，执行 `--rederive`，重跑 provision 并重启相关服务；超级用户口令用 `deploy/scripts/pg-ops.sh rotate-superuser`（deploy.md §7，**禁止**手工 `ALTER ROLE`：`log_statement='ddl'` 会把新口令写进日志） |
| 诊断包、日志或备份外泄 | 视为 PII 事件，通知 owner（O8） |

## 6. 升级规则与禁止事项

- 同一个问题最多做两次定向修复，仍然失败就升级给 owner 裁决，不要无限重跑（AGENTS.md）。
- 禁止：
  - 在真实支付之上恢复数据库。
  - 重放外部副作用。
  - 删除失败的测试、证据或诊断包。
  - 为了通过检查去放宽 preflight 或 smoke 的阈值。
