<!--
File: docs/runbooks/claims-data-deletion.md
Purpose: 评论认领数据（claims / meta_intake / social.*）的保留期清理与“按人删除”（actor-level erasure）的运维步骤：
  启用保留策略、受理删除请求、执行 retention-admin erase、答复请求人、备份恢复后的 replay。
Runs as/in: 文档（运维人员在部署主机上以 root 执行；数据库语句经 deploy/scripts/lib.sh 的 lc_psql 以 stdin 传入，不进日志）。
Reads env / secrets: 运维登录口令只在本次 shell 会话的变量里；本文不含任何密钥值。
Used by: 运维/owner/集成者；contracts/claims-retention-purge-v1.md §5/§10 引用。
Depends on: migrations/0071_claims_retention.sql、cmd/retention-admin（doc.go 列出子命令与退出码）、cmd/claims-worker（每小时任务）、
  deploy/scripts/deploy.sh retention_check、contracts/customers-billing-v1.md Q9（/data-deletion 说明页）。
Status: DESIGN。生产上从未执行；CRP01–CRP10 由 retention-tests 负责，CRP11 LIVE NOT_RUN。
Change rules: 子命令、退出码必须与 cmd/retention-admin/doc.go 一致；owner 批准项不得改写成“运维可自行决定”。
-->
# 评论认领数据删除与保留期手册（U08）

**生产上的 `policy-set`、`erase`、`replay` 都是删除用户数据：每一次都必须有 owner 在聊天里的明确批准（AGENTS.md，合同 §10(3)、OQ2）。**

## 0. 两个登录，两种权限

| 登录 | 权限 | 在哪里 |
|---|---|---|
| `lc_retention_job` | 只能 `claims.run_retention` + `claims.retention_status` | `deploy/postgres/logins.tsv` 常驻；claims-worker 每小时任务 + 部署后检查 `retention-admin status` |
| `lc_retention_operator` | `erase` / `policy-set` / `replay` / `run` / `status` | **不在 logins.tsv，不在部署主机或 CI 常驻**（ruling B25、合同 §10(5)）；按下面 §1 每次临时开、用完关 |

## 1. 临时开启运维登录（每次操作前，owner 批准后）

```sh
source deploy/scripts/lib.sh && lc_load_env "$LC_COMPOSE_ENV"
pw=$(openssl rand -hex 32)          # 只在本 shell；不写文件
lc_psql <<SQL >/dev/null
SET log_statement='none'; SET log_min_error_statement='panic';
SELECT 'CREATE ROLE lc_retention_operator LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2'
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='lc_retention_operator') \gexec
ALTER ROLE lc_retention_operator LOGIN PASSWORD '$pw';
GRANT commerce_retention_operator TO lc_retention_operator WITH INHERIT TRUE, SET FALSE;
SQL
export COMMERCE_RETENTION_OPERATOR_DATABASE_URL="postgres://lc_retention_operator:$pw@${LC_PG_HOST}:5432/live_commerce?sslmode=${LC_PG_SSLMODE}"
ra() { lc_compose run --rm --no-deps -T -e COMMERCE_RETENTION_OPERATOR_DATABASE_URL meta-worker /app/bin/retention-admin "$@"; }
```

用 `meta-worker` 容器运行是因为 `erase` 按发送者 id 删除需要 `COMMERCE_CLAIMS_ACTOR_KEY`（K_actor，合同 §6 clause 4），只有它挂载该密钥。
**结束后必须关闭：**

```sh
lc_psql <<<"ALTER ROLE lc_retention_operator NOLOGIN PASSWORD NULL;" >/dev/null
unset pw COMMERCE_RETENTION_OPERATOR_DATABASE_URL
```

## 2. 启用保留策略（一次性，owner 批准后）

1. `ra status` → 记下 `version=`（当前为报告模式 `enforced=0`，只计数不删除）。
2. 用 OQ1 默认期限（链接 7 天、intake/回复账本 30 天、认领身份 90 天、social 密文 30 天）或 owner 给的期限：
   `ra policy-set --expected-version <version> --enforced=true --link-days 7 --intake-days 30 --claims-days 90 --social-days 30`
3. `ra status` 应显示 `enforced=1`。之后把 `compose.env` 的 `LC_REQUIRE_RETENTION_ENFORCED` 设回 `1`（W1 期间可为 0）。
4. 部署后检查（`deploy.sh` retention_check）从此要求 `enforced=1` 且最近 26 小时内有一次清理；失败 = 部署未验证。
   `last_run_more=1` 连续超过 24 小时（仅 enforced 模式；report-only 时 more 恒为 0，积压看 run 行的各 counts） → 清理跟不上（合同 §9 上限：每小时 20×500 行/类），升级给集成者。

**第二个商家接入之前必须完成本节**（ruling X4：W1 到期）。

## 3. 受理删除请求

1. 入口：店面 `/data-deletion` 说明页（customers-billing Q9）或店家转达。记录请求时间（PDPA 第 11/13 条：30 天内答复）。
2. 核实身份（OQ3）：只接受 (a) 店家在自己的 Messenger/IG 对话里确认此人并给出认领单（bundle）id，或 (b) 请求人给出**自己**评论的链接；**绝不接受只有名字的请求**。
3. 为本请求生成一个 UUID 作为请求号（也是给请求人的确认码）：`uuidgen | tr A-Z a-z`。
4. 把请求号、选择器类型（不含任何 id 值）、owner 批准的聊天引用记入工单。

## 4. 执行 erase（每次都要 owner 批准）

选择器的 id 只从 stdin 传入，绝不出现在命令行、日志或工单里：

```sh
# 选择器 id 一律用 read -rs 读入（不回显、不进 shell history）；绝不以赋值或命令行参数键入
# (c) 店家给出的认领单
ra erase --request <uuid> --tenant <uuid> --store <uuid> --bundle <uuid> </dev/null
# (b) 请求人自己的评论 id
read -rs COMMENT_ID
printf '{"comment_ref":"%s"}' "$COMMENT_ID" | ra erase --request <uuid> --object page --asset <page id>
# (a) Meta 发送者 id（--app 可重复 0..8 次，用于删除私信/评论 social.* 行）
read -rs SENDER_ID
printf '{"sender_id":"%s"}' "$SENDER_ID" | ra erase --request <uuid> --object page --asset <page id> --app <app id>
unset COMMENT_ID SENDER_ID
```

| 退出码 | 含义 | 处理 |
|---|---|---|
| 0 | 完成（或同一请求重放，输出 `replayed=1`） | 记录输出的计数 |
| 3 | held：该人仍有 8 天内的回复窗口或未决操作；输出 `retry_after=` | **什么都没写**；到时间后用同一请求号重试 |
| 4 | not_found | 答复请求人“没有找到与您相关的数据” |
| 5 | conflict（同一请求号用于不同选择器）或 busy（每小时任务正在跑） | conflict 换新请求号；busy 稍后重试 |
| 2 / 1 | 用法错误 / 其他（stderr 只有固定代码） | 检查参数；不要重试到成功为止，升级给集成者 |

## 5. 答复请求人

30 天内答复，内容：已删除、确认码 = 请求号。订单、付款/退款事实、发货记录按法定保留不删除（customers-billing CD7）。

## 6. 备份恢复之后（§21.4）

恢复的数据库可能带回已删除的数据。**在重新开放营销/回复之前**：`ra replay`（按日志重放全部 `actor_erased` 请求）；
若日志本身也被回滚，用 T20 导出的完整墓碑元组：`ra replay --tombstones-file <path>`。按 peer key 删除的 social 行不可重放，
由 C5 在 `social_days` 内清理（合同 §9）。

## 7. 退回 W1（仅 owner 自营试点店）

W1 期间 `compose.env` 可设 `LC_REQUIRE_RETENTION_ENFORCED=0`，部署检查仍要求清理任务在跑（报告模式）。
接入第二个商家前必须完成 §2 并改回 `1`。
