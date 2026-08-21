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

若在应用前面部署了反向代理（Nginx、Traefik 等），需要把代理地址填入 `NEXUSMAIL_TRUSTED_PROXIES`（逗号分隔的 IP 或 CIDR），否则登录限流会把所有请求都算到代理这一个地址上。反之，直接暴露端口时必须留空：登录限流按客户端地址计数，无条件信任 `X-Forwarded-For` 等于让调用方每次请求都换一个新配额。

## 版本与镜像发布

版本号由根目录的 `VERSION` 文件统一管理。`make build` 和 `make docker-build` 会读取该文件，通过 `-ldflags` 注入 `internal/version.Value`，服务启动时以 `nexusmail starting` 日志输出版本。

**每次推送到 `main`** 都会触发 `.github/workflows/publish-image.yml` 构建并推送镜像：

- GHCR：`ghcr.io/aho1ic/nexusmail`（无需配置，使用工作流内置的 `GITHUB_TOKEN`）
- Docker Hub：`docker.io/aho1ic901/nexusmail`（需配置下述两个 secret，未配置时自动跳过，只发 GHCR）

标签规则：

| 标签 | 何时推送 | 说明 |
| --- | --- | --- |
| `sha-<短 SHA>` | 每次推送 | 不可变，指向唯一一次构建 |
| `latest` | 每次推送 | 始终跟随 `main` 最新提交 |
| `<version>` / `v<version>` | 仅当本次推送改动了 `VERSION` | 一个语义标签只指向声明它的那一次构建 |

非发布构建注入的版本号带 `-<短 SHA>` 后缀（如 `0.2.0-c1dca66`），启动日志可直接定位来源提交。

镜像为 `linux/amd64` + `linux/arm64` 双架构 manifest list。两个架构各在原生 runner 上构建（arm64 用 `ubuntu-24.04-arm`，公开仓库免费），按 digest 推送后再合并成 manifest list —— 避免 QEMU 模拟这个 CGO 静态构建。合并前不会有任何标签指向单架构镜像，合并后会回读 manifest 断言两个架构都在，缺一即失败。

Docker Hub 需要的两个 Actions secret：

```bash
gh secret set DOCKERHUB_USERNAME --body '<你的 Docker Hub 用户名>'
gh secret set DOCKERHUB_TOKEN    --body '<Docker Hub Access Token>'
```

Token 在 Docker Hub 的 Account settings → Personal access tokens 生成，权限选 **Read & Write**，不要用登录密码。

发布新版本：修改 `VERSION` 内容（例如 `0.3.0`）并提交到 `main`。`VERSION` 不符合 `MAJOR.MINOR.PATCH` 格式时工作流会直接失败。工作流也支持在 Actions 页面手动 `workflow_dispatch` 触发（手动触发一律按发布处理）。

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
