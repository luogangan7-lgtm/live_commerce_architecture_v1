<!--
File: docs/runbooks/mail.md
Purpose: 密码登录的发信邮箱（专用 QQ 邮箱 → 之后可升级 Tencent Exmail）的设置、只读探测（PA12）和一次真实发信验证（PA13）。
Runs as/in: 文档；命令由 owner 或在 owner 批准下由运维在部署主机上执行。
Reads env / secrets: LC_SMTP_HOST / LC_SMTP_USERNAME / LC_MAIL_FROM（compose.env）；secret 文件 commerce_smtp_password（SMTP 授权码，owner 以文件提供，绝不经聊天）。
Used by: docs/runbooks/merchant-onboarding.md §0 O2；contracts/merchant-password-auth-v1.md §5、§9 PA12/PA13。
Depends on: cmd/api/identity.go（配置校验）、internal/mail（SMTP 适配器，implicit TLS 465）。
Status: DESIGN。PA12/PA13 均为 NOT_RUN，需要 owner 对“这一次具体发信”的批准。
Change rules: 授权码不得写入仓库、日志、聊天或命令行参数；发信地址必须等于 SMTP 用户名。
-->
# 发信邮箱（密码登录的验证码邮件）

**批准规则**：以下 §3、§4 会连接真实 SMTP 服务器，§4 会给真实邮箱发一封邮件。每一次都要先在聊天里得到 owner 对这一次操作的明确批准（AGENTS.md）。

## 1. 设置（owner）

1. 新建一个**专用**发信邮箱（不要用 owner 个人邮箱）：QQ/Exmail 的授权码同时授予该邮箱的 IMAP/POP 访问，无法收窄范围（契约 §5）。
2. 在邮箱设置里开启 POP3/SMTP，生成 16 位 SMTP **授权码**（不是网页登录密码，F8）。
3. 把授权码写入文件 `${LC_SECRETS_DIR}/commerce_smtp_password`（0440，`root:${LC_SECRETS_GID}`），不要经聊天发送。
4. `compose.env`：`LC_PASSWORD_LOGIN_ENABLED=1`、`LC_SMTP_HOST=smtp.qq.com`、`LC_SMTP_USERNAME=<完整邮箱地址>`、`LC_MAIL_FROM=xgdwm <同一个邮箱地址>`、`LC_MAIL_DAILY_CAP`（默认 200，QQ 的真实日上限**未知**：以首次观察到的 `smtp_rejected` 速率为准，不得调到邮箱持续发送过的数量之上）。
5. 管理域名必须是 DNS-only（不经过 Cloudflare 代理），`preflight.sh --online` 的 P17 规则检查这一点。

## 2. 升级到 Tencent Exmail（推荐，之后）

只改配置与 secret，不改代码：`LC_SMTP_HOST=smtp.exmail.qq.com`，发信地址用 `@xgdwm.com` 邮箱；在 Cloudflare 上按 Exmail 管理后台给出的值加 MX/SPF/DKIM（不要凭本文件手敲记录值，F10），`_dmarc.xgdwm.com` 先 `v=DMARC1; p=none; rua=mailto:<owner 邮箱>` 观察 2–4 周再升到 `p=quarantine`。

## 3. PA12 只读探测（LIVE，不发送任何邮件）

`internal/mail` 的 `Probe` 只做：TLS 握手、AUTH、`MAIL FROM:<用户名>`、`RSET`、`QUIT`，从不进入 `RCPT`/`DATA`；另外用不同的发件地址验证服务器会拒绝（记录回复码）。命令与结果记录到 `output/merchant-password-auth/`。

## 4. PA13 一次真实发信（LIVE，owner 批准）

只发给 owner 自己的邮箱一封验证码邮件：查看邮件头 `spf=pass dkim=pass`（QQ：`qq.com` 对齐；Exmail：`xgdwm.com` 的 `dmarc=pass`），记录到达时间与截图到 `output/merchant-password-auth/`。

## 5. 故障与处置

- 登录/注册收到 503 `mail_unavailable`：全局邮件额度用尽（`global-mail-*`）或发信失败。看 api 日志里的 `password_auth_throttled bucket=…` / `password_auth_mail state=…`（只有状态，没有收件人、验证码或授权码）。
- 用户没收到邮件：让用户点“重新发送”（新验证码，旧的失效）；系统从不自动重发（SMTP 没有幂等键）。
- 账号被锁或额度被滥用：见 `docs/runbooks/merchant-onboarding.md` §7（`#unlock-password`、`#deactivate-principal`）。
- 轮换授权码：在邮箱设置里生成新授权码 → 写文件 → 重启 api → 在邮箱里撤销旧授权码。
