# Changelog

## v7.2.67-aug.3 - 2026-08-03

### Release Basis

- Upstream semantic basis: v7.2.67
- Previous local release: v7.2.67-aug.2
- Local release: v7.2.67-aug.3
- Required management panel: v1.17.14-aug.2 or newer

### What's Changed

- fix(usage): Limit legacy `/usage` details to 10,000 events and a conservative 32 MiB estimate that includes JSON escaping and decoded header structures, then return global and time aggregates with `apis: {}` and an explicit truncation warning
- fix(usage): Propagate request cancellation through SQLite and in-memory snapshots, response redaction, response encoding, and usage import responses; report post-import maintenance timeouts as successful imports with an explicit warning; query totals with a constant-memory SQL aggregate
- fix(usage): Keep degraded in-memory day and hour aggregates in UTC, matching SQLite-backed responses
- fix(tui): Keep dashboard and usage views available when legacy details are truncated, and display the server warning in both views
- test(usage): Cover event, structure, escaping and byte limits, high-cardinality aggregate responses, UTC parity, TUI warning display, cancellation, import totals, maintenance warnings, and allocations against a 20,000-event SQLite dataset

### Migration and Compatibility

- SQLite remains on schema v4; existing events, buckets, costs, and backups are unchanged
- Legacy `/usage` keeps request details only while the dataset is at most 10,000 events and the conservative decoded/encoded size estimate is at most 32 MiB; otherwise it returns global and time aggregates with `apis: {}`, `details_truncated: true`, and a warning that identifies `/usage/summary` and `/usage/events`
- Management panel v1.17.14-aug.2 already uses the aggregate and paginated APIs for the usage page
- Docker release builds continue to use the published CPAM v1.17.14-aug.2 `management.html` asset

---

### 版本依据

- 上游语义基线：v7.2.67
- 上一个本地版本：v7.2.67-aug.2
- 本地发布版本：v7.2.67-aug.3
- 要求的管理面板版本：v1.17.14-aug.2 或更新

### 更新内容

- fix(usage)：旧版 `/usage` 明细限制为 10,000 个事件和包含 JSON 转义、响应头解码结构的保守 32 MiB 估算，超限后返回全局与时间聚合、`apis: {}` 和明确的截断警告
- fix(usage)：SQLite 与内存快照、响应脱敏、响应编码和使用统计导入响应均继承请求取消信号；导入后维护超时改为返回成功并附明确警告；总数查询改用常量内存 SQL 聚合
- fix(usage)：降级内存路径的日期和小时聚合统一使用 UTC，与 SQLite 响应一致
- fix(tui)：旧版请求明细被截断时继续显示 dashboard 和使用统计，并在两个页面展示后端警告
- test(usage)：覆盖事件、结构、转义和字节限制、高基数聚合、UTC 一致性、TUI 警告展示、请求取消、导入总数、维护警告，以及 20,000 个 SQLite 事件下的分配量限制

### 迁移与兼容性

- SQLite schema 保持 v4，现有事件、bucket、费用和备份格式不变
- 旧版 `/usage` 仅在事件不超过 10,000 个且保守的解码/编码大小估算不超过 32 MiB 时保留请求明细；超限后返回全局与时间聚合、`apis: {}`、`details_truncated: true` 和指向 `/usage/summary`、`/usage/events` 的警告
- 管理面板 v1.17.14-aug.2 的使用统计页面已经使用聚合和分页接口
- Docker 发布构建继续使用已发布的 CPAM v1.17.14-aug.2 `management.html`

## v7.2.67-aug.2 - 2026-08-02

### Release Basis

- Upstream semantic basis: v7.2.67
- Previous local release: v7.2.67-aug.1
- Local release: v7.2.67-aug.2
- Required management panel: v1.17.14-aug.2 or newer

### What's Changed

- fix(usage): Preserve credential-source type information and store API-key sources only as masked values for live writes and non-encrypted legacy imports
- fix(management): Conservatively mask untyped historical credential sources in summary, time-series, event, and legacy usage responses while preserving typed identity labels and provider grouping
- fix(filters): Resolve raw and masked credential filters, merge API-key mask collisions, disambiguate identity/API-key display collisions, preserve provider-qualified filtering, and keep unmatched filters fail-closed
- fix(release): Pin Docker images and source-build helpers to management panel v1.17.14-aug.2

### Migration and Compatibility

- SQLite remains on schema v4; existing events and aggregates are not rewritten
- Existing untyped source values stay unchanged on disk and are conservatively masked at the authenticated management response boundary
- Legacy imports store untyped source values only as masked API-key credentials; encrypted backups preserve source values exactly for recovery
- Encrypted usage exports remain reversible and keep their existing format
- Docker release builds require the published CPAM v1.17.14-aug.2 `management.html` asset

---

### 版本依据

- 上游语义基线：v7.2.67
- 上一个本地版本：v7.2.67-aug.1
- 本地发布版本：v7.2.67-aug.2
- 要求的管理面板版本：v1.17.14-aug.2 或更新

### 更新内容

- fix(usage)：保存 credential source 类型信息，实时写入和非加密 legacy 导入中的 API-key source 仅以脱敏值保存
- fix(management)：未标记类型的历史 credential source 在 summary、时间序列、事件和旧版 usage 响应中保守脱敏，同时保留已标记的身份标签与 provider 分组
- fix(filters)：credential filter 同时支持原值和脱敏值，合并 API-key 脱敏碰撞，区分 identity/API-key 展示碰撞，保留 provider 限定查询，无匹配时继续返回空结果
- fix(release)：Docker 镜像和源码构建脚本固定使用管理面板 v1.17.14-aug.2

### 迁移与兼容性

- SQLite schema 保持 v4，不改写现有事件和聚合数据
- 现有未标记类型的 source 在磁盘中保持不变，在已认证的管理响应边界进行保守脱敏
- legacy 导入仅以脱敏 API-key credential 保存未标记类型的 source；加密备份为恢复用途精确保留 source
- 加密 usage export 保持可恢复能力与现有格式
- Docker 发布构建要求 CPAM v1.17.14-aug.2 已发布 `management.html`

## v7.2.67-aug.1 - 2026-07-31

### Release Basis

- Upstream semantic basis: v7.2.67
- Previous local release: v7.2.50-aug.1
- Local release: v7.2.67-aug.1
- Required management panel: v1.17.14-aug.1 or newer

### What's Changed

- feat(config): Make configuration and auth-file updates transactional, keep last-good recovery data, and update dependent runtime state only after persistence succeeds
- feat(plugin): Harden plugin host startup, authenticated private-repository release selection, artifact storage, resource cleanup, and scheduler lifecycle handling
- feat(auth): Update provider credentials, quota refresh, exhausted-account routing, model aliases, authentication metadata, and concurrency-safe quota status snapshots
- feat(runtime): Refresh provider executors, management handlers, interactions APIs, and protocol translators for the v7.2.67 semantic base
- feat(usage): Migrate SQLite usage storage to schema v4, preserve latency and cost aggregates, add encrypted portable backups, and enforce database capacity limits
- feat(model-catalog): Refresh catalog metadata and default prices, centralize server-side pricing, and apply catalog mutations transactionally
- fix(usage): Run historical cost recalculation in the background, expose task status, and merge concurrent price updates into one follow-up pass using the latest prices
- fix(release): Pin Docker images to management panel v1.17.14-aug.1 and reject missing, empty, mismatched, or fallback panel assets

### Migration and Compatibility

- The first startup after upgrading an older usage database runs the schema v4 migration before the usage store becomes ready; large databases can take several minutes
- Model-price saves and model-catalog price updates return without waiting for historical repricing; use the recalculation status endpoint to observe completion
- Version 3 usage backups preserve canonical SQLite event fields and costs; cross-server restores require the same management credential or `X-Usage-Backup-Key`
- Docker release builds require the published CPAM v1.17.14-aug.1 `management.html` asset and do not fall back to another panel version

---

### 版本依据

- 上游语义基线：v7.2.67
- 上一个本地版本：v7.2.50-aug.1
- 本地发布版本：v7.2.67-aug.1
- 要求的管理面板版本：v1.17.14-aug.1 或更新

### 更新内容

- feat(config)：配置和 Auth file 更新改为事务处理，保留 last-good 恢复数据，只在持久化成功后更新依赖的运行时状态
- feat(plugin)：加强 plugin host 启动、私有仓库 release 认证查询与选择、artifact 存储、资源清理和 scheduler 生命周期处理
- feat(auth)：更新 provider 凭证、quota 刷新、额度耗尽账号路由、模型别名、认证元数据和并发安全的 quota 状态快照
- feat(runtime)：按 v7.2.67 语义基线更新 provider executor、管理 handler、interactions API 和协议 translator
- feat(usage)：SQLite 使用量存储迁移到 schema v4，保留 latency 和成本聚合，新增加密便携备份，并限制数据库容量
- feat(model-catalog)：刷新模型目录元数据和默认价格，统一使用后端价格源，并以事务方式应用目录修改
- fix(usage)：历史费用重算改为后台任务，提供任务状态，并把并发价格更新合并为一次使用最新价格的后续重算
- fix(release)：Docker 镜像固定使用管理面板 v1.17.14-aug.1，拒绝缺失、空白、标签不匹配或 fallback 面板资产

### 迁移与兼容性

- 从旧版使用量数据库升级后的首次启动会在 usage store 就绪前执行 schema v4 迁移，大型数据库可能需要数分钟
- 模型价格保存和模型目录价格修改不再等待历史费用重算，可通过重算状态接口查看完成结果
- 版本 3 使用量备份保留 SQLite 标准事件字段和成本，跨服务器恢复需要相同管理凭证或 `X-Usage-Backup-Key`
- Docker 发布构建要求 CPAM v1.17.14-aug.1 已发布 `management.html`，不会改用其他面板版本

## v7.2.50-aug.1 - 2026-07-07

### Release Basis

- Upstream: v7.2.50
- Previous local release: v7.1.68-aug.1
- Local release: v7.2.50-aug.1
- Required management panel: v1.17.10-aug.1 or newer

### What's Changed

- chore(deps): Upgrade Go dependencies for the v7.2.x release base
- feat(plugin): Add plugin store registry, Home plugin sync/status APIs, plugin resources, and hardened plugin host platform handling
- feat(management): Expand config, logs, account inspection, auth-file, quota, version, and plugin management APIs
- feat(runtime): Update Codex/OpenAI image and video paths, xAI/Grok execution, Antigravity grounding, Claude signing/uTLS, and provider translators
- feat(auth): Improve quota scheduling, OAuth model alias mapping, auth session affinity, and Codex prompt cache reuse
- feat(models): Refresh model catalog metadata, disable-image-generation mode, and config watcher/synthesizer behavior
- fix(security): Redact sensitive log/download/request outputs and harden plugin download, proxy, and Grok validation paths
- refactor(amp): Remove Amp backend module and management routes

### Breaking Changes

- Amp backend management/module support is removed in this release
- Management compatibility is raised to CPAM v1.17.10-aug.1 because the new panel depends on the expanded management API surface
- No other public proxy route break is intended by this release

---

### 版本依据

- 上游版本：v7.2.50
- 上一个本地版本：v7.1.68-aug.1
- 本地发布版本：v7.2.50-aug.1
- 要求的管理面板版本：v1.17.10-aug.1 或更新

### 更新内容

- chore(deps)：升级 Go 依赖，匹配 v7.2.x 发版基线
- feat(plugin)：新增 plugin store registry、Home plugin 同步和状态 API、插件资源能力，并加固 plugin host 平台处理
- feat(management)：扩展配置、日志、账号巡检、Auth file、quota、版本和插件管理 API
- feat(runtime)：更新 Codex/OpenAI 图片和视频链路、xAI/Grok 执行、Antigravity grounding、Claude signing/uTLS 和 provider translator
- feat(auth)：改进 quota 调度、OAuth model alias 映射、认证会话亲和和 Codex prompt cache 复用
- feat(models)：刷新模型 catalog 元数据、disable-image-generation 模式和配置 watcher/synthesizer 行为
- fix(security)：日志、下载和请求输出脱敏，并加固插件下载、proxy 和 Grok 校验链路
- refactor(amp)：移除 Amp 后端模块和管理路由

### 不兼容变更

- 本版本移除 Amp 后端管理和模块支持
- 管理面板兼容要求提升到 CPAM v1.17.10-aug.1，因为新版面板依赖扩展后的管理 API
- 除已移除的 Amp 集成外，本版本不声明其他公开代理路由契约破坏

## v7.1.68-aug.1 - 2026-06-24

### Release Basis

- Upstream: v7.1.68 / ed52c614
- Previous local release: v7.1.19-aug.2
- Local release: v7.1.68-aug.1
- Required management panel: v1.15.3-aug.1 or newer

### What's Changed

- feat(plugin): Add the plugin host runtime, callback bridge, scheduler hooks, ABI types, and runnable plugin examples
- feat(models): Add model catalog storage, default pricing data, model metadata APIs, and management endpoints
- feat(management): Expand configuration, auth-file, account-inspection, model-price, and plugin management APIs
- feat(runtime): Extend provider execution paths for Codex, Gemini, Claude, Antigravity, OpenAI-compatible, xAI, Kimi,
  Qwen, and Amp flows
- feat(observability): Enrich request logging, request timelines, usage events, source/provider metadata, and usage
  persistence
- fix(release): Keep Docker image builds working when the management panel asset is absent from a clean backend checkout
- chore(release): Add a repository changelog and make tag releases consume the matching changelog entry as release notes

### Breaking Changes

- Public proxy API routes are not intentionally broken by this release
- Management compatibility is raised to CPAM v1.15.3-aug.1 because the new panel depends on the expanded management API
  surface

---

### 版本依据

- 上游版本：v7.1.68 / ed52c614
- 上一个本地版本：v7.1.19-aug.2
- 本地发布版本：v7.1.68-aug.1
- 要求的管理面板版本：v1.15.3-aug.1 或更新

### 更新内容

- feat(plugin)：新增 plugin host 运行时、callback bridge、scheduler hooks、ABI 类型和可运行插件示例
- feat(models)：新增模型 catalog 存储、默认价格数据、模型元数据 API 和管理端点
- feat(management)：扩展配置、Auth file、账号巡检、模型价格和插件管理 API
- feat(runtime)：扩展 Codex、Gemini、Claude、Antigravity、OpenAI-compatible、xAI、Kimi、Qwen 和 Amp 执行链路
- feat(observability)：增强请求日志、请求时间线、使用事件、source/provider 元数据和使用统计持久化
- fix(release)：后端干净 checkout 缺少管理面板资产时，Docker 镜像构建仍可完成
- chore(release)：新增仓库 changelog，tag release 使用对应 tag 段落作为 release notes

### 不兼容变更

- 本版本不声明破坏公开代理 API 路由契约
- 管理面板兼容要求提升到 CPAM v1.15.3-aug.1，因为新版面板依赖扩展后的管理 API

## v7.1.19-aug.2 - 2026-05-27

### What's Changed

- fix(ci): Publish Docker images to `pyrokine/cli-proxy-api`

---

### 更新内容

- fix(ci)：Docker 镜像发布到 `pyrokine/cli-proxy-api`

## v7.1.19-aug.1 - 2026-05-27

### Release Basis

- Upstream: v7.1.19
- Local release: v7.1.19-aug.1

### What's Changed

- feat(home): Add certificate provisioning and auto-refresh for self-hosted setups
- feat(openai): Add image and video endpoint handlers, plus Codex client model listing
- feat(claude): Add request signing and uTLS transport support
- feat(usage): Add SQLite `provider_id` dimension for per-provider usage breakdowns
- feat(cluster): Add Docker Compose template and environment configuration for multi-node deployments
- feat(auth): Update quota handlers, auth-file endpoints, and credit tracking
- refactor(runtime): Update multi-provider executors, translators, thinking logic, registry, config, middleware, and
  logging layers
- test: Expand coverage for credits, quota, headers, auth flows, and flaky Amp cache behavior
- docs: Update README, config example, and ignore rules

---

### 版本依据

- 上游版本：v7.1.19
- 本地发布版本：v7.1.19-aug.1

### 更新内容

- feat(home)：新增自托管场景的证书申请和自动续期
- feat(openai)：新增图片和视频端点处理，以及 Codex 客户端模型列表
- feat(claude)：新增请求签名和 uTLS 传输支持
- feat(usage)：SQLite 使用统计新增 `provider_id` 维度，支持按 provider 分类
- feat(cluster)：新增多节点 Docker Compose 模板和环境配置
- feat(auth)：更新配额处理器、Auth file 端点和 credits 追踪
- refactor(runtime)：更新多 provider executor、translator、thinking、registry、config、middleware 和日志层
- test：扩展 credits、配额、请求头、认证流程和 Amp cache 覆盖
- docs：更新 README、config 示例和 ignore 规则

## v6.10.9-aug.4 - 2026-05-27

### What's Changed

- fix(update): Restart the service after auto-update and refresh release metadata after a successful download

---

### 更新内容

- fix(update)：自动更新完成后重启服务，并刷新 release 元数据

## v6.10.9-aug.3 - 2026-05-27

### What's Changed

- fix(update): Remove the download size cap that blocked large release archives during auto-update

---

### 更新内容

- fix(update)：移除自动更新下载大 release 归档时的大小限制

## v6.10.9-aug.2 - 2026-05-27

### What's Changed

- fix(update): Match versioned release archive filenames during auto-update

---

### 更新内容

- fix(update)：自动更新下载时正确匹配带版本号的 release 归档文件名

## v6.10.9-aug.1 - 2026-05-27

### Release Basis

- Upstream: v6.10.9
- Local release: v6.10.9-aug.1

### What's Changed

- feat(usage): Migrate usage statistics to SQLite for persistence and query efficiency
- feat(management): Add quota status and auth-file operation endpoints
- feat(models): Expose model context length and compatibility metadata through APIs
- refactor(translator): Normalize provider registration and adapter layering
- chore(build): Align repo tooling, CI config, and documentation

---

### 版本依据

- 上游版本：v6.10.9
- 本地发布版本：v6.10.9-aug.1

### 更新内容

- feat(usage)：将使用统计迁移到 SQLite，提升持久化能力和查询效率
- feat(management)：新增配额状态和 Auth file 操作端点
- feat(models)：通过 API 暴露模型上下文长度和兼容性元数据
- refactor(translator)：规范化 provider 注册和 adapter 分层
- chore(build)：对齐仓库工具链、CI 配置和文档

## v6.9.0-aug.1 - 2026-05-27

### Release Basis

- Upstream: v6.9.0
- Local release: v6.9.0-aug.1

### What's Changed

- feat(usage): Add persistent SQLite-backed usage tracking with management API queries by model, credential, and time
  range
- feat(security): Harden auth endpoints with TLS enforcement, constant-time key comparison, rate limiting, SSRF
  protection, DNS pinning, OAuth CSRF/nonce validation, and CSP headers
- feat(oauth): Consolidate OAuth constants and provider implementations across Anthropic, Gemini, Codex, and Antigravity
- refactor(translator): Extract shared translator layers, byte pipelines, and tool-call continuation helpers
- refactor(config): Split config loading and logging modules
- chore(build): Migrate Go module path and upgrade dependencies

---

### 版本依据

- 上游版本：v6.9.0
- 本地发布版本：v6.9.0-aug.1

### 更新内容

- feat(usage)：新增 SQLite 持久化使用统计，管理 API 支持按模型、凭证和时间范围查询
- feat(security)：加固认证端点，覆盖 TLS 强制、常量时间 key 比较、限流、SSRF 防护、DNS pinning、OAuth CSRF/nonce 校验和 CSP 响应头
- feat(oauth)：统一 Anthropic、Gemini、Codex 和 Antigravity 的 OAuth 常量及 provider 实现
- refactor(translator)：抽取共享 translator 层、字节管线和 tool-call continuation helper
- refactor(config)：拆分配置加载和日志模块
- chore(build)：迁移 Go module 路径并升级依赖
