# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 权威文档

- **[`AGENTS.md`](AGENTS.md) 是开发约定的唯一权威**：目录职责、分层边界、Go/前端风格、数据与安全约束、测试与验证清单。修改代码前先读它，本文件不重复其内容。
- 架构基线：[`docs/architecture-plan.md`](docs/architecture-plan.md)；HTTP 契约：[`api/openapi.yaml`](api/openapi.yaml)。契约冲突时以 OpenAPI 为准。

## 命令

所有 Go 命令必须带 `-tags sqlite_fts5`。缺少该 tag 时程序能编译但启动即失败于 migration（`store.go` 会返回 `apply migration (build with -tags sqlite_fts5)`），因为 schema 依赖 FTS5 虚拟表。本地构建还需可用的 CGO 工具链。

```bash
make dev          # 运行 Go 服务（默认 :8080）
make test         # Go 全量测试 + 前端 Vitest
make test-race    # race detector；改动并发/连接生命周期/hub 后必跑
make web-build    # tsc -b + vite build，并把产物复制进嵌入目录
make build        # web-build 后产出 bin/nexusmail（注入 VERSION）
make test-e2e     # Playwright Chromium；用 route mock，不连真实邮箱
```

单测与聚焦验证：

```bash
go test -tags sqlite_fts5 ./internal/provider/imap/          # 单 package
go test -tags sqlite_fts5 -run TestSupervisorIdle ./internal/provider/imap/
go test -tags sqlite_fts5 -race -run TestHub ./internal/realtime/
cd web && npm run lint                 # 等价于 tsc -b --pretty false
cd web && npx vitest run src/lib/api.test.ts
cd web && npx playwright test e2e/mailbox.spec.ts
```

前端开发：`cd web && npm run dev`（Vite `5173`，代理 `/api`、`/healthz`、`/readyz` 到 `8080`）。首次需 `make web-install`。

运行前提：`NEXUSMAIL_API_KEY` ≥ 32 字符，`NEXUSMAIL_MASTER_KEY` 为 base64 编码的 32 字节。所有 secret 支持 `_FILE` 变体。应用监听 `:8080`，docker-compose 映射为 `13737:8080`——README 中的 `13737` 是容器端口映射，不是应用默认值。

## 架构

### 进程装配

`cmd/server/main.go` 是唯一装配点，顺序即依赖顺序：`sqlite.Open` → `cryptobox` → `storage`(blob) → `realtime.Hub` → `account` → `oauth` → **`imap.Supervisor`** → `message`/`draft` → `session` → `smtp` → `send.Worker` → `transport/http`。Supervisor 被 message、draft、send 三个 service 共享，是事实上的中心组件。

所有后台 goroutine 都挂在 `signal.NotifyContext` 的 `rootCtx` 上：每账户 2 个循环 + 4 个 body worker（Supervisor）、send worker、15 分钟维护 ticker（清理过期 session + blob LRU 淘汰）。新增后台任务必须接受该 ctx 并有可验证的退出路径。

### Supervisor 的文件分工

`internal/provider/imap` 是单个 package，按职责分文件，改动前先定位到对应文件：

| 文件 | 职责 |
| --- | --- |
| `supervisor.go` | `Supervisor` 类型与生命周期（`Start`/`StartAccount`/`Stop`/`runtime`） |
| `tuning.go` | 全部时间与批量常量，每个都带取值理由；`observe` 慢阶段埋点 |
| `runtime.go` | 每账户运行态、命令连接的两级锁、`requestSync`/`RequestMailbox` |
| `conn.go` | 拨号、TLS、认证、`stallGuard` 读写停滞检测 |
| `loop.go` | `commandLoop` / `idleLoop` / `probeInbox` / `pollWithoutIdle` |
| `sync.go` | mailbox 目录刷新、按账户与按 mailbox 的增量 UID 同步 |
| `reconcile.go` | flag 与 expunge 修复（`staleUIDs` 决定哪些 UID 算已删） |
| `ingest.go` | fetch 结果转行、地址解码与格式化、批量落库 |
| `body.go` | 正文与附件抓取、后台预取 worker |
| `actions.go` | 用户操作：flags、archive、批量已读 |
| `remotedrafts.go` | 远端草稿同步与 Sent APPEND |
| `errors.go` | 错误分类与退避算术 |

### 每账户双连接模型

`loop.go` 为每个账户起两个 goroutine：

- `commandLoop` 独占命令连接，负责所有同步与用户操作；连接失败按 1s→5m 指数退避。它同时持有两个 ticker：`periodicSyncInterval`（5 分钟全量）和 `realtimePollInterval`（5 秒 `probeInbox`）。**5 秒探针刻意放在 commandLoop 而非 IDLE 循环**，因为命令连接始终存活，用于兜住那些声明支持 IDLE 却延迟或丢弃 EXISTS 通知的服务商。
- `idleLoop` 只监听，收到通知后通过 `rt.syncReq` 发信号，从不自己执行 IMAP 操作。

### 命令连接的优先级锁（易踩坑）

`runtime.go` 用 `cmdMu` + `urgent atomic.Int32` 实现两级抢占，不是普通互斥锁：

- `rt.lock()`：前台工作（同步、用户操作）。进入前 `urgent++`，拿到锁后 `urgent--`。
- `rt.lockBackground(ctx)`：后台正文预取。只在 `urgent == 0` 时获取，并在拿锁后二次确认，否则释放并 25ms 后重试。

**任何新增的命令连接操作都必须显式选择其中之一**。用错会让新邮件同步排在正文预取积压之后，重现历史上的分钟级延迟问题。

### 持久化

- `internal/repository/sqlite/store.go` 用单个 `writeMu` 串行化**全部写操作**；读路径不加锁。新增写方法必须同样持有它，否则在 WAL + 8 连接池下会出现 `SQLITE_BUSY`。
- migration 运行器按版本号升序遍历 `migrations/*.up.sql`（`parseMigrationName` 取前缀数字），跳过 `schema_migrations` 中已记录的版本。新增 `000002_*.up.sql` 会被 `go:embed` 打包并自动执行，无需改 `migrate()`；文件名前缀必须是可解析的数字，否则会被静默忽略。
- 消息落库只有批量一条路径 `BatchCreateOrUpdateMessages`（单条写入的 `CreateOrUpdateMessage` 已删除）。它手工展开 `IN (?,?…)`，每个 dedupe key 必须包成 `blobArg`：GORM 会把紧跟 `(` 的 `?` 上绑定的 slice 按元素展开，裸 `[]byte` 会被当成 32 个整数比较，导致每批第一条永远去重失败并撞 unique 索引回滚整批。
- FTS5：`message_fts` 虚拟表由 3 个 trigger 维护（insert / delete / update of `subject,sender,recipients,body_text`）。绕过这些列直接改正文，或用非 trigger 路径写入，索引会静默失去同步。
- 分页是 keyset cursor：base64(JSON `{received_at,id}`)，配合 `(received_at DESC, id DESC)` 索引。不要改成 OFFSET。

### 发信状态机

`internal/service/send/worker.go` 的 `deliver` 区分四种结果，其中 `unknown` 最关键：

- `sent` → `CreateSentMessage` + 删除远端草稿；随后仅当 `provider.Preset.ServerSavesSent == false` 时才 APPEND 到远端 Sent（QQ/163 需要，Gmail/Outlook 会自己保存，重复 APPEND 会产生副本）。
- `retry_wait` → 仅对明确的临时失败，退避阶梯 `5s / 30s / 2m / 10m / 30m`，`AttemptCount` 上限 5。
- `failed` → 永久失败。
- `unknown` → `DATA` 之后连接中断，**终态，绝不自动重试**（可能已投递）。这类草稿的附件 blob 必须持久保存，不可被 LRU 淘汰。

### 传输层与前端

- `authenticate()` 是双通道：`X-API-Key` 命中即短路放行（外部客户端，不校验 CSRF）；否则走 HttpOnly cookie session，非幂等方法额外要求 `X-CSRF-Token` 与同源检查。
- 邮箱角色与同步档位由 `provider.ClassifyMailbox` 决定（`realtime`/`periodic`/`lazy`），先看 IMAP special-use attribute，再回退到名称匹配（含中文名）。新增服务商差异应收敛到 `internal/provider` 的 preset 与该函数。
- 前端无 router、无状态库：`web/src/App.tsx`（≈220 行）只负责装配与"当前视图"这一份状态，UI 拆到 `components/`（9 个），跨视图行为拆到 `hooks/`（`useRealtime` 持有唯一 socket，`useKeyboard` 绑定单键快捷键），纯函数拆到 `lib/`（`api.ts` 极薄请求封装、`format.ts`、`messagehtml.ts`、`notifications.ts`、`preferences.ts`），`types.ts` 手写对齐 OpenAPI。新增 UI 优先扩展现有结构，不要顺手引入组件框架。
- 前端"当前视图"只有一个定义：`App.tsx` 的 `viewParams()`。feed 列表与 mark-all-read 共用它，因此 `account_id`/`mailbox_id`/`query` 不会与屏幕上显示的邮件脱节。切账户与回 All Inboxes 时清 `selectedMailbox` 的责任在两个 handler 里（同一次 render 内完成），不要挪到 effect：effect 晚一个 render，会先按旧 mailbox 多发一次 feed 请求。
- `internal/transport/http/static/dist` 由 `make web-build` 从 `web/dist` 复制并 `go:embed`，是生成产物，禁止手工编辑。

## 版本与发布

`VERSION`（单行 semver）是唯一版本源：`make build`/`make docker-build` 读取它并通过 `-ldflags` 注入 `internal/version.Value`，启动日志输出。`.github/workflows/publish-image.yml` **仅在 push 到 `main` 且 `VERSION` 文件发生改动时**触发，推送 `<version>`、`v<version>`、`latest` 到 Docker Hub 与 GHCR；格式不合 semver 直接失败。仓库没有 CI 测试流水线，测试与验证完全依赖本地执行。
