# AGENTS.md

本文件是 NexusMail 仓库的开发与代码代理指南，适用于整个仓库。当前没有更深层的 `AGENTS.md`；如果以后在子目录增加该文件，子目录规则优先于本文件。与用户当前请求冲突时，以用户请求为准；架构细节以 [`docs/architecture-plan.md`](docs/architecture-plan.md)、HTTP 契约以 [`api/openapi.yaml`](api/openapi.yaml) 为准。

## 项目概览

NexusMail 是单用户、自托管的统一邮件网关和客户端。一个 Go 进程聚合 QQ、163、Gmail 和 Outlook，提供 IMAP 同步、SQLite FTS5 搜索、SMTP outbox、远端草稿、REST/WebSocket API 和 React UI。

技术栈与运行时基线：

- Go 1.26，CGO，`go-sqlite3`，SQLite WAL + FTS5。
- Gin HTTP 服务、GORM 普通 CRUD；迁移、FTS 和分页热路径使用显式 SQL。
- IMAP 使用 `emersion/go-imap/v2`，SMTP 使用 `go-smtp`，OAuth 使用 `x/oauth2`。
- Node.js 24，npm lockfile，React + TypeScript + Vite + Tailwind CSS，Vitest 和 Playwright Chromium。
- Docker 多阶段构建最终单二进制，运行用户为非 root，持久化目录为 `/data`。

## 目录职责

- `cmd/server/main.go`：进程组装、信号处理、优雅关闭和后台维护任务。
- `internal/config`：环境变量、`_FILE` secret 读取和配置校验。
- `internal/domain`：领域实体及数据库映射；不要在这里放 HTTP 或 provider 逻辑。
- `internal/ports`：repository、provider、blob 和发布事件等边界接口。
- `internal/service`：`account`、`message`、`draft`、`send`、`session` 应用用例；依赖 ports，不依赖 Gin/GORM 具体实现。
- `internal/repository/sqlite`：SQLite 持久化；事务、migration、FTS 和分页查询的实现边界。
- `internal/provider/{imap,smtp,oauth,auth}`：外部服务适配器；服务商差异应隔离在这里。
- `internal/mail`：MIME 解析、字符集转换、HTML 清洗和 MIME 构造。
- `internal/storage`：内容寻址 blob 存储与 LRU 淘汰。
- `internal/realtime`：有界 WebSocket hub 和实时事件广播。
- `internal/transport/http`：Gin 路由、认证、CSRF/Origin、防护、REST 和 SPA 静态资源。
- `migrations`：嵌入二进制的显式 SQL migration。
- `api/openapi.yaml`：REST 契约和统一错误格式。
- `web/src`：React 客户端、API 封装、类型、单元测试；`web/e2e` 为 Playwright 测试。
- `internal/transport/http/static/dist`：前端构建后复制的嵌入产物，属于生成文件，不要手工编辑。

## 环境与本地运行

首次准备：

```bash
cp .env.example .env
openssl rand -base64 32  # 填入 NEXUSMAIL_MASTER_KEY
cd web && npm ci
cd ..
```

本地 Go 构建需要可用的 CGO 编译器和 SQLite FTS5。应用要求 `NEXUSMAIL_API_KEY` 至少 32 个字符，`NEXUSMAIL_MASTER_KEY` 必须是 base64 编码的 32 字节密钥。`.env`、数据库、`data/`、前端依赖和构建产物都已被忽略，禁止提交真实 secret。

常用命令：

| 命令 | 用途 |
| --- | --- |
| `make dev` | 使用 `sqlite_fts5` tag 运行 Go 服务 |
| `make test` | Go 全量测试 + 前端 Vitest |
| `make test-race` | Go race detector 测试 |
| `make web-install` | 在 `web/` 执行 `npm ci` |
| `make web-test` | 前端 Vitest |
| `make web-build` | TypeScript 检查、Vite 构建并更新嵌入 SPA 产物 |
| `make test-e2e` | Playwright Chromium 冒烟测试 |
| `make build` | `web-build` 后构建 `bin/nexusmail` |
| `make docker-build` | 构建本地 Docker 镜像 |
| `docker compose up --build` | 容器化运行完整服务 |

前端开发可在 `web/` 执行 `npm run dev`，Vite 默认监听 `5173` 并将 `/api`、`/healthz`、`/readyz` 代理到 Go 服务 `8080`。生产或嵌入式验证使用 `make build`；`make web-build` 会修改被忽略的 `web/dist` 和 `internal/transport/http/static/dist`，这是预期行为。

## 开发约定

### 通用

- 先读相关实现、测试、架构计划和 OpenAPI，再修改代码；保持改动聚焦，不顺手做无关重构。
- 尊重工作区已有改动，不覆盖或回滚用户文件；不要使用破坏性的 `git reset --hard`、`git checkout --` 或批量删除。
- 默认使用 ASCII；只有项目已有文件或协议明确需要时才引入 Unicode。注释只解释非显然的设计原因。
- 时间统一使用 UTC Unix 毫秒；跨边界的错误使用稳定 machine code，响应遵循 `{"error":{"code","message","request_id","details"}}`。
- 新增行为必须有与风险匹配的测试，优先在拥有该行为的 package 附近添加测试。

### Go 后端

- 使用 `gofmt`；导入分组和命名遵循现有 Go 风格。完成后至少运行受影响 package 的测试。
- 保持依赖方向：transport -> service -> ports，repository/provider/storage 实现 ports；不要让 service 直接依赖 Gin、GORM 查询对象或具体 provider client。
- 普通 CRUD 可用 GORM；数据库初始化、显式 migration、FTS5、游标分页和性能敏感查询使用参数化 SQL。
- 不使用 GORM `AutoMigrate`，不修改已经发布的 migration。schema 变更新增递增编号的 `.up.sql` 与匹配 `.down.sql`，并确认 `migrations/embed.go` 的 `go:embed *.sql` 能包含它们。
- 写入涉及消息、邮箱映射、草稿、outbox 或事件时，明确事务边界和提交后事件顺序；不要在事务中进行不可控的网络调用。
- context 必须贯穿 I/O、后台 worker 和 shutdown；goroutine、ticker、IMAP 连接和 WebSocket 都要有可验证的退出路径。
- IMAP 遵守双连接模型：IDLE 连接只监听并发出同步信号，命令连接负责实际操作；处理 UIDVALIDITY 变化、IDLE 重建、退避和不支持 IDLE 的轮询。
- SMTP 状态必须区分成功、永久失败、可重试失败和 `DATA` 后连接中断的 `unknown`；只有明确允许的临时失败可自动重试。
- adapter 隔离 `go-imap/v2` beta API 和各服务商差异，避免把预发布库类型扩散到 domain/service。

### 数据、邮件与安全

- 凭据使用 AES-256-GCM 加密落库；主密钥不落库，OAuth access token 只驻留内存，refresh token 加密持久化。
- 日志严禁包含 API key、OAuth token、邮箱授权码、邮件正文和附件名；新增错误信息也必须检查是否泄露敏感值。
- 浏览器 session 使用 HttpOnly、SameSite=Strict cookie；变更请求继续校验 CSRF 和 Origin。外部客户端使用 `X-API-Key`。
- 邮件 HTML 必须服务端清洗，并在 sandbox iframe 中显示；默认不加载远程资源，只允许受控的 CID 内联附件映射。
- MIME 处理要覆盖 multipart、UTF-8/GBK/GB2312、Base64、Quoted-Printable、CID 和恶意 HTML；附件按需抓取，不因打开列表而下载全部附件。
- 内容寻址 blob 使用 SHA-256；可重取内容可以按 LRU 淘汰，但草稿和 `unknown` 发信结果所需附件必须持久保存。
- 对输入、上传大小、分页 limit、provider 名称和路径做边界校验；SQL、URL、header 和文件名不要通过字符串拼接形成注入风险。

### API 与前端

- REST 路由、状态码、错误 code、字段命名和 `If-Match` 乐观锁行为以 `api/openapi.yaml` 为准。契约变化同时更新 handler、前端 `web/src/lib/api.ts`、`web/src/types.ts` 和测试。
- WebSocket 事件不持久化；事件带单调 sequence 和 UTC 毫秒时间，断线后客户端通过 REST 重新获取首屏。
- TypeScript 保持 `strict`；优先复用现有 API 封装和组件样式，避免在组件里重复实现请求、认证或错误解析。
- 前端 UI 改动同时考虑 loading、empty、error、权限/未登录、断线和移动端布局状态；不要把服务端 HTML 当作可信 DOM 直接插入。
- 修改 React 页面后运行 `cd web && npm run lint && npm test`；涉及交互流程时补充或更新 `web/e2e` Playwright 测试。
- 不手工改 `web/dist` 或嵌入静态目录；通过 `make web-build` 生成。不要把 `node_modules`、截图报告或 Playwright test-results 加入提交。

## 测试与验证

根据改动范围选择最小且足够的验证：

- Go 逻辑或 repository：`go test -tags sqlite_fts5 ./...`，并在并发、连接生命周期或 hub 改动后追加 `go test -tags sqlite_fts5 -race ./...`。
- 前端逻辑或组件：`cd web && npm run lint && npm test`。
- API/前后端交互：同时检查 OpenAPI、Go handler、前端 API client，并运行 `make test-e2e`；该测试使用 Playwright route mock，不依赖真实邮箱服务。
- migration/FTS/邮件解析/安全：添加回归测试，覆盖完整性、更新/删除索引、编码/嵌套/CID、恶意 HTML、敏感日志和错误状态。
- 打包或静态资源：运行 `make web-build`、`make build`，必要时运行 `make docker-build`，并检查 `/healthz`、`/readyz`。

提交或报告完成前：

1. 重新运行与最终改动对应的命令，不把改动前的旧结果当作证据。
2. 检查命令退出码、失败/跳过测试数量和生成文件状态。
3. 用 `git diff --check` 和 `git diff --stat` 检查空白、意外文件、debug 代码和 secret。
4. 检查 `git status --short`，确认没有误加入 `.env`、数据库、构建目录、报告或密钥。
5. 在交付说明中明确写出已运行、未运行及受环境限制的验证项，不宣称未验证的构建或测试通过。

## 变更清单

实现功能时至少确认：

- 相关领域/ports/service/adapter/transport 层是否都保持正确边界。
- API 契约、migration、前端类型/API 调用和用户可见状态是否同步。
- 错误、重试、取消、超时、重启恢复和并发路径是否覆盖。
- secret、邮件正文、附件、OAuth 和 HTML 渲染是否符合安全约束。
- 测试、文档、Docker/Makefile 或嵌入资源是否需要同步更新。
