<!--
File: docs/runbooks/backup-restore.md
Purpose: 备份与恢复运行手册 — 备份范围、计划与保留、诚实的 RPO/RTO、手工备份与校验、恢复演练、生产恢复顺序、PITR 提升、密钥备份、异地副本。
Runs as/in: 文档（命令在部署主机/DB 主机上以 root 执行，数据库操作在一次性 pg-ops 容器内完成）。
Reads env / secrets: 无（本文不含任何密钥值）。
Used by: 运维/owner；deploy/README.md 与 deploy.md 链接到此。
Depends on: deploy/scripts/pg-ops.sh, deploy/postgres/ops/*, deploy/postgres/postgresql.conf（WAL 归档）。
Status: DESIGN；备份/恢复/PITR 已用 scratch 镜像本地验证（VERIFIED_LOCAL），未在真实数据上执行。
  2026-09-28 评审 P1 修复：§6 旧流程（复制"暂停中被快速停止"的集群并删除 recovery.signal）会让在线库越过目标时间做崩溃恢复、
  停留在时间线 1，并让归档永久失败。现改为 `restore-pitr --promote` + `pitr-cutover` 两个脚本（smoke S42）。
Change rules: 恢复顺序遵循 架构.md §22.3；修改脚本默认行为（新库/临时集群）必须经过评审。
-->
# 备份与恢复运行手册

## 1. 备份范围

| 内容 | 是否备份 | 方式 |
|---|---|---|
| 业务库 `live_commerce`（含 River 队列表） | 是 | 每日 `pg_dump -Fc`；每周 base backup；WAL 连续归档 |
| 角色与成员关系 | 是（**不含密码**） | `globals.sql`（`pg_dumpall --roles-only --no-role-passwords`） |
| 密钥 `/etc/live-commerce/secrets` | **否**，需单独做离线加密备份 | 见 §7 |
| Caddy 证书（caddy-data 卷） | 否，可重新签发 | 丢失后 ACME 会重新签发，注意频率限制 |
| 应用镜像 | 否 | 按 git tag 重新构建 |

**备份含买家个人信息（PII）**：备份盘必须做访问控制，异地副本必须加密（O3）。

## 2. 计划、保留与诚实的 RPO/RTO

- 逻辑备份每天 02:17，保留 14 天（`LC_DUMP_RETENTION_DAYS`）。
- base backup 每周日 03:23，保留最近 2 份（`LC_BASE_KEEP`），同时清理最旧 base 之前的 WAL。
- WAL 连续归档，`archive_timeout=60s`，目标目录为 `${LC_BACKUP_DIR}/wal`。
- 看门狗 W3（备份过旧）和 W4（归档失败）负责告警。
- RPO（丢失窗口）：
  - WAL 所在盘独立时，DB 盘损坏的 RPO 约 1 分钟。
  - 没有可用 WAL 时，RPO 为 24 小时。
  - **整机丢失时，RPO 等于最后一次异地副本的时间**。异地副本尚未建立（O3）。
- 架构.md §22.3 的目标（RPO ≤ 5 min、RTO ≤ 60 min）在定时演练（S31）和异地副本都落地之前**未被证明**。
  本地 scratch 演练中 PITR 用时约 2 s（数据量极小，不具代表性）。

## 3. 手工备份与校验

```sh
deploy/scripts/pg-ops.sh backup --tag manual        # 输出目录：${LC_BACKUP_DIR}/dumps/<UTC>_manual/
deploy/scripts/pg-ops.sh basebackup                 # 同时执行 pg_verifybackup 和 WAL 清理
deploy/scripts/pg-ops.sh list
cd ${LC_BACKUP_DIR}/dumps/<目录> && sha256sum -c SHA256SUMS
```
- `manifest.json` 只记录元数据：tag、服务器版本、ledger 行数和最大版本、库大小、schema 列表、sha256、耗时。不含任何数据值。

## 4. 恢复演练

- **每月**：恢复到一个新库，校验通过后删除。
  ```sh
  deploy/scripts/pg-ops.sh restore-dump <UTC>_nightly --drop-after-verify
  ```
  `verify.sql` 检查三项：ledger 行数与 manifest 一致；支付、过期、Meta 三个就绪门禁为真；各 schema 的表数量。
- **每季度**：PITR 演练。在 pg-ops 内的临时集群执行，完全不接触生产 PGDATA，并且关闭归档。
  临时集群的 socket 使用 peer 认证，所以超级用户口令轮换（deploy.md §7）之后，用更早的 base backup 也能演练。
  ```sh
  deploy/scripts/pg-ops.sh restore-pitr --target-time '2026-09-28 03:00:00+00' --drill
  ```
  把输出中的 `pitr_duration_seconds` 记为 RTO 样本。
- **已知现象（I8）**：逻辑恢复后的库中 `live.media_plan_ready()` 一定为 false。
  原因是该函数固定了约束定义的 md5，而 PostgreSQL 在重新解析时会展开 BETWEEN 产生的嵌套 AND。
  物理备份/PITR 不受影响。Studio/媒体当前未部署，所以默认只作提示；设置 `LC_REQUIRE_MEDIA_GATE=1` 会把它变成硬性要求。
  独立验证（2026-09-28）确认：在线库 `t`，逻辑恢复库 `f`。为了不让这个问题藏在 S29 PASS 里，smoke 新增 **S29m**：
  - 在线 `t`、恢复 `f` → BLOCKED（REQUIRES_INTEGRATOR I8），`smoke.sh full` 整体结果为 BLOCKED；
  - 两边都是 `t` → PASS，表示 I8 已修复，此时应把 `LC_REQUIRE_MEDIA_GATE` 默认改为 1。
  S31（PITR）的说明里会记录 `media_plan=` 的值，作为物理恢复不受影响的证据。

## 5. 生产恢复流程（架构.md §22.3 顺序；需要 owner 批准）

1. **确定干净的版本和恢复点**：选择代码 tag 和恢复时间点，确认目标之后没有**不可重放的外部事实**。
2. **停止所有写入方**：`docker compose stop api admin storefront expiry-worker payment-worker-sandbox payment-worker-live meta-worker claims-worker`（没启用的服务会被忽略）。
   **支付 worker 必须保持停止**，直到 §5 第 5 步完成。
3. **数据库 + 密钥**：
   - 先恢复密钥（§7）。
   - 方案 A：逻辑恢复（先恢复到新库，再原子改名。旧库保留，不会被删除）。
     ```sh
     LC_CONFIRM_REPLACE_LIVE=I_UNDERSTAND_FORWARD_ONLY deploy/scripts/pg-ops.sh restore-dump <目录> --replace-live
     ```
     如果目标是全新集群，加 `--fresh-cluster`（先导入角色），再执行 `docker compose run --rm -T --no-deps provision-logins` 为登录角色设置密码。
   - 方案 B：PITR，见 §6（`pitr-cutover` 已包含 provision-logins 和新的 base backup）。
4. **先开放身份和只读订单**，验证数据正确。
5. **处理删除/撤回的墓碑记录**（K23），然后对账 PSP：对照 PAYUNi 商户后台逐笔核对 `river_payment` 中的 job 和支付状态。
   **历史外部副作用绝不自动重放**，UNKNOWN 状态不能盲目重试。
6. 恢复**有限写入**（关闭营销），观察看门狗。
7. 最后开放营销和媒体。

禁止事项：
- 在已经有真实支付的数据之上直接恢复。
- 自动重放外部副作用。
- 删除失败证据。

## 6. PITR 切换（需要 owner 批准；两个命令，不需要手工改数据目录）

为什么不能再用旧流程：`restore-pitr`（不带参数）在**暂停状态**下快速停止集群，pg_control 状态是 `shut down in recovery`。
删除 `recovery.signal` 后把它当在线库启动，PostgreSQL 只做普通崩溃恢复：重放 pg_wal 里剩下的全部 WAL（越过目标时间，
把目标之后的事务带回来），并停留在时间线 1；接着同名段已在 `/backup/wal` 中且内容不同，`archive_command` 的 `cmp` 分支永久拒绝，
W4 一直失败，pg_wal 持续增长直到磁盘写满（评审用固定镜像复现：目标时 200 行，切换后 400 行，timeline_id=1，failed=3）。

1. **在临时集群里恢复并提升**（在线库不受影响，可以提前做）：
   ```sh
   deploy/scripts/pg-ops.sh restore-pitr --target-time '<T>' --promote
   ```
   - 暂停在目标 → `verify.sql` → `pg_wal_replay_resume()`，恢复**恰好在目标处结束**并提升到新时间线（写出 `0000000N.history`）
     → `CHECKPOINT` → 把超级用户口令设为**当前**密钥文件的值（语句日志已在会话内关闭）→ 干净停止（状态 `shut down`）。
   - 临时服务器此时用 `archive_mode=on` + **空的** `archive_command`：不向归档写任何东西，但 history 文件和新段的 `.ready` 标记会保留，
     切换后由在线库归档。
   - 输出 `pitr_promoted_timeline=N`（N ≥ 2）。失败时不会留下 `PROMOTED` 标记，第 2 步会拒绝。
2. **切换**（停机窗口；Caddy 在此期间返回 503 + `Retry-After`）：
   ```sh
   LC_CONFIRM_REPLACE_LIVE=I_UNDERSTAND_FORWARD_ONLY deploy/scripts/pg-ops.sh pitr-cutover
   ```
   脚本依次执行：
   - 检查临时集群：有 `PROMOTED` 标记、状态 `shut down`（不是 `shut down in recovery`）、时间线 > 1、没有 `recovery.signal`/`standby.signal`；
   - 停止写入方（api、admin、storefront、各 worker），再停止 postgres；
   - 只在这一次 pg-ops 运行中挂载在线 pgdata 卷：确认在线库已干净停止、空间足够，把 `18/docker` 改名为 `18/docker.pre-pitr-<UTC>`（回退副本），
     再把提升后的集群复制为 `18/docker`；
   - 启动 postgres，并验证：`pg_is_in_recovery()` = `f`；`SELECT timeline_id FROM pg_control_checkpoint()` > 1；
     强制一次 WAL 切换后 `pg_stat_archiver` 的 archived 增加、failed **不变**；`${LC_BACKUP_DIR}/wal/0000000N.history` 已归档；
   - `provision-logins`（登录角色口令恢复为当前密钥值，并检查就绪门禁）；
   - 立即做新的 base backup（新时间线的起点）。
   - **写入方保持停止**。
3. 按 §5 第 4–7 步逐步开放；`deploy/scripts/deploy.sh first` 会拉起全部服务并执行部署后检查。之后 `watchdog.sh` 应全部 PASS（W4 failed=0）。
4. 清理回退副本（owner 签字、新 base backup 已完成后）：
   ```sh
   dc --profile ops run --rm --no-deps -T -v <项目>_pgdata:/live pg-ops -c 'rm -rf -- /live/18/docker.pre-pitr-<UTC>'
   ```
   `<项目>_pgdata` 是 postgres 容器实际使用的卷名（`docker inspect -f '{{range .Mounts}}{{.Name}} {{end}}' <postgres 容器>`）。
   注意：`-v pgdata:/live` **不会**解析成项目卷，Docker 会新建一个名为 `pgdata` 的空卷。

回退（仅限切换后还没有新的业务写入，owner 决定）：停止 postgres，用同样的 pg-ops 挂载执行
`mv /live/18/docker /live/18/docker.rejected-pitr-<UTC> && mv /live/18/docker.pre-pitr-<UTC> /live/18/docker`，再启动 postgres。
回退后必须把 `${LC_BACKUP_DIR}/wal/0000000N.history` 移出归档目录（例如移到 `${LC_BACKUP_DIR}/wal-rejected/`），
否则以后的 PITR 会跟随这个被放弃的时间线（`requested timeline N is not a child of this server's history`）；然后立即做新的 base backup。

## 7. 密钥备份与恢复

- owner 维护 `/etc/live-commerce/secrets` 的**离线加密副本**（例如 age/gpg 加密后离线保存，工具属于新依赖，由 owner 决定）。
- 每次轮换后都要更新这个副本。
- **丢失 `commerce_account_keys_json` 会导致已存储的商户 PSP 凭据永久无法解密。**
- 恢复步骤：还原文件，设置权限 `0440 root:10500`、目录 `0750`，然后执行 `preflight.sh`。

## 8. 异地副本（O3）

- 使用 rclone/restic 等工具把 `${LC_BACKUP_DIR}`（dumps、base、wal）加密推送到异地。工具属于新依赖，需要 owner 批准。
- 建议频率：WAL 实时或每 5 分钟，dumps 每日。
- 在异地副本落地之前，整机丢失的 RPO 没有保证。
