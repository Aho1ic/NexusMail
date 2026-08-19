# NexusMail

NexusMail 是一个轻量、自托管、单用户的统一邮件网关和客户端。它在一个 Go 进程中聚合 QQ、163、Gmail 与 Outlook，提供实时 IMAP 同步、SQLite FTS5 搜索、远端草稿、SMTP outbox、REST/WebSocket API 和响应式 React UI。

## 快速开始

要求：Go 1.26、带 CGO 工具链的 SQLite FTS5 构建环境，以及 Node.js 24（仅构建前端需要）。

```bash
cp .env.example .env
openssl rand -base64 32
```

把输出填写到 `.env` 的 `NEXUSMAIL_MASTER_KEY`，同时设置至少 32 字符的随机 `NEXUSMAIL_API_KEY`。随后可直接运行：

```bash
docker compose up --build
```

打开 `http://localhost:13737`，输入 API Key 换取 HttpOnly 浏览器会话。QQ/163 账户应填写服务商生成的客户端授权码；Gmail/Outlook 还需在 `.env` 配置 OAuth Client，并把回调地址设为：

- `http://localhost:13737/api/v1/oauth/gmail/callback`
- `http://localhost:13737/api/v1/oauth/outlook/callback`

生产环境必须把 `NEXUSMAIL_PUBLIC_URL` 改为最终 HTTPS 地址。所有 secret 均支持同名 `_FILE` 变量，例如 `NEXUSMAIL_MASTER_KEY_FILE=/run/secrets/master_key`。

## 本地开发

```bash
cd web && npm ci
cd ..
make test
make build
NEXUSMAIL_API_KEY='replace-with-a-random-32-char-key' \
NEXUSMAIL_MASTER_KEY='base64-encoded-32-byte-key' \
./bin/nexusmail
```

常用命令：

```bash
make dev        # 运行 Go 服务
make test       # Go 测试（启用 sqlite_fts5）
make test-race  # race detector
make web-build  # 类型检查并构建 SPA，同时更新嵌入资源
make test-e2e    # Playwright Chromium 冒烟
```

## 架构与 API

- [架构与实施基线](docs/architecture-plan.md)
- [OpenAPI 3.1 契约](api/openapi.yaml)
- [显式 SQLite migration](migrations/000001_init.up.sql)

浏览器使用 HttpOnly session 和 `X-CSRF-Token`；外部客户端可直接发送 `X-API-Key`。服务暴露 `/healthz` 与 `/readyz`，数据及 blob 默认保存在 `/data`。

## 安全说明

- 数据库只保存 AES-256-GCM 加密后的邮箱凭据；主密钥不落库。
- Access Token 仅驻留内存，Refresh Token 加密存储。
- 邮件 HTML 经服务端清洗后在 sandbox iframe 内显示，默认不加载远程图片。
- 日志设计为不输出 API Key、OAuth token、授权码、正文或附件名。
- `/data` 包含邮件元数据、可缓存正文和耐久草稿附件，应纳入加密备份与权限控制。
