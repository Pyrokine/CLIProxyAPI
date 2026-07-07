# Changelog

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
