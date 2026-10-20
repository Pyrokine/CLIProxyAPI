# Changelog

## v8.0.23-aug.3 - 2026-10-20

### Release basis

- Upstream release: v8.0.23 (`d318bcc3afb9ea8782862f5e8cdb541afdc9dfa5`)
- Fork point: `09fec34e1cdfd99ac79be458fff29f94b834dbcc`
- Previous local release: v8.0.23-aug.2
- Paired management panel: CPAM v1.26.0-aug.3

### Fixes

- Handle Cline non-streaming Chat requests through a single upstream stream without increasing the original token limit
- Preserve actual text, reasoning, tool calls, stop reasons and reported usage; unwrap successful `success/data` responses and report failed or incomplete responses as errors
- Match update-health versions with or without the standard `v` prefix, including compatibility with older update guards; preserve full build identity and per-attempt authentication
- Bundle the released CPAM v1.26.0-aug.3 panel in both Docker architectures

### Compatibility

- Database schemas remain management 4 and usage 9; this patch adds no schema migration
- The Go SDK module remains `/v6` and the management API remains `/v0/management`
- Existing core and native-plugin package distinctions remain unchanged

### 中文

### 版本依据

- 上游版本：v8.0.23（`d318bcc3afb9ea8782862f5e8cdb541afdc9dfa5`）
- 分叉点：`09fec34e1cdfd99ac79be458fff29f94b834dbcc`
- 上一个本地版本：v8.0.23-aug.2
- 配套管理面板：CPAM v1.26.0-aug.3

### 修复

- Cline 非流式 Chat 请求通过一次上游流式请求处理，保持调用方原有 token 上限
- 保留真实正文、思考、工具调用、停止原因和用量，正确读取成功的 `success/data` 包装，失败或不完整响应明确返回错误
- 升级健康检查兼容标准 `v` 前缀及旧版更新守护程序，保留完整构建标识与逐次安装认证
- 两种 Docker 架构均内置已发布的 CPAM v1.26.0-aug.3 面板

### 兼容性

- 数据库 schema 保持 management 4／usage 9，本补丁不增加迁移
- Go SDK module 保持 `/v6`，管理 API 保持 `/v0/management`
- core 与 native-plugin 安装包的能力区别保持不变

**Full Changelog**: https://github.com/Pyrokine/CLIProxyAPI/compare/v8.0.23-aug.2...v8.0.23-aug.3

## v8.0.23-aug.2 - 2026-10-20

### Release basis

- Upstream release: v8.0.23 (`d318bcc3afb9ea8782862f5e8cdb541afdc9dfa5`)
- Fork point: `09fec34e1cdfd99ac79be458fff29f94b834dbcc`
- Previous local release: v7.3.2-aug.1
- Paired release: CPAM v1.26.0-aug.2

### Changes

- Record canceled downstream delivery as canceled even before the request context is canceled; preserve upstream and write failures
- Keep bootstrap replay tests from treating a closed channel as response data
- Give race-test packages a finite 25-minute CI timeout while retaining all tests and the 45-minute job limit

- Preserve exact credential numbers when saving files and comparing quota identities; keep ordinary numeric settings compatible
- Restore quota after metadata normalization only when the source material can be verified; isolate legacy numeric identities whose precision cannot be proven
- Keep plugin authentication available when quota metadata cannot be validated, with an explicit quota warning
- Keep quota windows, credits, reset allowances and observation times across credential enable/disable changes and restart recovery, while isolating credential replacements
- Stop adding successful image records when a text response only reports unused image statistics; retain completed image calls with zero reported usage
- Use consistent account scope for quota, reset-credit details and subscription queries; retain available results and identify failed auxiliary queries

- Update provider authentication, model metadata, and protocol conversion while retaining the `/v6` SDK module, `/v0/management` API, and flat configuration format
- Preserve tool identity, signatures, namespaces, and delivery state across streaming and non-streaming responses
- Keep optional Codex `apply_patch` capability negotiation disabled by default; supported translators convert tool declarations, history, and responses without executing patches on the server
- Preserve plugin OAuth identity and nested model callback ownership, and release streams when their callback, context, or plugin instance ends
- Query plugin models by the target provider without triggering unrelated discovery
- Preserve plugin error messages and HTTP status codes across RPC and native calls; keep request buffers valid during reentrant Windows callbacks
- Classify delivery errors caused by context cancellation as canceled; retain failed status for independent downstream write errors
- Return failure from `-t` for invalid configuration without replacing the source configuration with its last-good copy
- Skip empty Claude turns when translating to Gemini and Gemini CLI while preserving attachment rejection
- Preserve Gemini and Gemini CLI tool call IDs, result ordering across user messages, structured results, and per-message attachment errors; schema cleanup no longer changes ordinary tool input JSON
- Retain valid signatures in Gemini tool continuations and keep standalone signatures separate
- Preserve pending Gemini tool pairing when an unmatched result becomes user text; retain media defaults for generic MIME types and reject invalid data URLs in base64 sources
- Preserve Claude tool pairing for interrupted calls, late results, and colliding IDs; retain image results, cache controls, and reversible tool names
- Use declared wire names for Claude tool additions, removals and inline replacements; preserve caller-owned MCP and server tools
- Restore redacted reasoning, recognize prefill restrictions across model versions, and report unsupported content without logging request payloads; byte request conversion returns no body on attachment errors
- Deduplicate repeated complete Gemini function-call snapshots in streaming and non-streaming responses; preserve call identity and reject conflicting snapshots with an SDK 502 error
- Preserve late and interleaved Claude tool calls; validate fragmented arguments against complete snapshots and return SDK 502 errors for conflicting input
- Convert native Claude Messages JSON while preserving model identity, content order, citations, reasoning signatures, and cache usage
- Mark Claude Responses incomplete when the token limit is reached or the provider pauses the turn
- Reject Responses requests when conversion removes all usable content from any user message, returning an error before contacting the provider
- Preserve tool namespaces and reversible names across Responses and Chat declarations, history, tool choices, and responses
- Return a failure for inconsistent custom apply_patch calls instead of a completed response; HTTP errors do not expose internal conflict details
- Preserve valid Codex audio, file references, stringified image tool results, and reversible tool names
- Apply final payload rules to Codex local token counts and select the tokenizer from the final model
- Deliver remaining tool arguments from complete snapshots; reject conflicting snapshots with SDK 502 errors over HTTP and WebSocket
- Return generated Codex images across streaming and non-streaming responses
- Preserve Gemini inline media and file references when translating to Codex
- Keep Interactions URI attachments and developer instructions; reject user turns emptied by unsupported media
- Preserve HTTP(S) Interactions images and documents, ordered tool results, structured payloads, and reversible tool names in Claude conversions
- Report Claude provider errors as failed Interactions and ignore later response frames
- Preserve Gemini and Antigravity Interactions parallel tool history and thought summaries
- Preserve late Interactions tool identities and ordinary argument prefixes; reject inconsistent source identities, snapshots and stop boundaries, and report premature source EOF as failure
- Preserve explicit zero cache usage fields and valid continuation signatures
- Retain valid signatures with visible text, tool calls, and inline media; return no byte request body after attachment rejection
- Preserve HTTP(S), file ID and inline image inputs when translating Claude requests to Codex
- Keep tool-result ordering, explicit tool choices and typed web search sources
- Preserve compatible Grok reasoning and Schema unions; reject conflicting completed tool arguments without a successful stream ending
- Finalize Gemini Responses when a complete, consistent usage tail arrives; preserve the finish reason and mark the active message incomplete for `MAX_TOKENS`; use `cpaUsageMetadata` only when `usageMetadata` is absent
- Refresh Devin metadata independently in Home mode; `local-model` still disables remote catalog refresh
- Use valid GLM-5.2 1M request model identifiers while retaining catalog thinking levels and output token limits
- Measure first-token latency from effective output across Chat, Responses, Claude Messages and Gemini streams, excluding keepalive and metadata-only frames
- Honor per-request proxy selection for protected TLS requests; retain integer fields in Codex Desktop tools sent to Google providers
- Keep the served image model in usage records after completion; ignore late model metadata while preserving image stream bytes, usage and service tier
- Apply quota-aware routing changes immediately, including supported aliases and session affinity, without restarting the service
- Preserve newer saved credential settings when an older watcher update arrives
- Keep Home authoritative for account models while preserving plugin model discovery
- Apply reasoning updates to custom Claude models while retaining manual or adaptive mode
- Preserve normalized Claude thinking settings and keep summary display changes independent
- Exclude management endpoints from ordinary request logs and retain full request identifiers
- Preserve the entry request for SDK callers that omit OriginalRequest, including custom tool declarations and validation
- Report an empty native tool translation as failure instead of a successful empty response
- Record failed Gemini Interactions as failures across response formats and retain observed usage
- Close canceled or failed xAI WebSocket attempts and confirm downstream delivery before recording success
- Apply Meta subscription quota exhaustion to the credential while preserving its retry deadline
- Preserve Meta response failures and observed usage through translation, EOF, cancellation and completion
- Honor explicit positive retry delays for 404 responses while retaining the default for missing or invalid hints
- Keep sendable Devin text and tool results together and omit prompts emptied by unsupported media
- Respect original JSON and multipart image fields; apply automatic Codex image tools only for the final model and configured tool policy
- Apply configured payload rules after provider request preparation and use the same source format and request path for local token counts
- Return plugin response translation failures as request-scoped errors, retain observed usage and release the source stream when execution ends
- Preserve Home Anthropic model types, display names, token limits and RFC3339 creation timestamps
- Apply the Opus 5.5 fallback for the final Claude model while preserving explicit caller and payload-rule choices
- Reserve one positive cache-control budget slot for non-null Claude threads
- Attach cc_turn_origin=human to signed Claude main-session billing tags
- Return HTTP 413 for oversized AI Studio request bodies instead of forwarding truncated content
- Cancel AI Studio relay readers when translation fails or output ends
- Report failed AI Studio provider responses as errors while preserving observed usage
- Record failed AI Studio request preparation exactly once
- Continue safe unauthorized recovery with a healthy credential when the current credential cannot recover
- Honor downstream commitment when handling unauthorized execution results
- Enforce active quota recovery deadlines independently of availability and retry flags
- Use the first real user text block for Claude multipart request fingerprints
- Match native UTF-16 code-unit sampling for Claude fingerprints
- Assign recovery deadlines to exact-model HTTP 429 limits while keeping sibling models available
- Preserve terminal upstream WebSocket errors that arrive before request activation
- Derive Codex OAuth routing hints from the final model and service tier while preserving valid header overrides
- Normalize both max_context_length and maxContextLength in Home model metadata
- Repair Claude tool IDs before applying final payload rules
- Keep credential cooldown model projections consistent with actual scheduling decisions
- Keep credential cooldown state separate from independent model quota deadlines and backoff
- Clear active rejected-access fingerprints after a successful same-token credential renewal
- Preserve explicit Responses streaming error sequence numbers and exact integer error details
- Handle incremental WebSocket prewarm locally for SSE upstreams without discarding prior turns
- Preserve Responses stream write and short-write failure causes in usage records
- Preserve observed upstream stream failures when request cleanup cancels execution
- Preserve operator-owned Claude system content through automatic reporting and cache processing while retaining OAuth CCH signing
- Distinguish client tools named advisor from server-side advisor calls
- Exclude native Claude hints from unconfirmed caller passthrough while preserving ordinary transport headers
- Preserve successful Claude completion after the terminal frame is delivered, including a trailing upstream read error
- Verify stale credit rejection after Antigravity credentials are removed and recreated
- Remove internal prompt cache options before signing Claude OAuth bodies so CCH matches the sent body
- Accept complete named terminal snapshots on native Gemini and Vertex Interactions streams while preserving translated-response validation
- Return HTTP 502 for empty Claude upstream responses instead of returning successful empty output
- Preserve explicitly configured input IDs in Codex HTTP and WebSocket requests, including streaming requests
- Evaluate Claude payload conditions after forced-tool thinking cleanup while preserving final model and tool validation
- Accept complete Gemini and Vertex Interactions requires_action snapshots as terminal responses while rejecting missing steps and unfinished snapshots
- Write the Devin client name to metadata field 1 and remove obsolete field 28 without changing credential and device metadata
- Apply xAI compact payload overrides after built-in cleanup across HTTP, streaming triggers, and WebSocket triggers
- Preserve explicit xAI thinking overrides after model-capability checks and keep existing cleanup when no override is configured
- Return xAI HTTP response translation failures as failed outcomes, retain observed token usage, and publish successful usage only after conversion and stream completion
- Propagate Kimi and Kimi.ai Chat and Responses translation failures in streaming and non-streaming requests without losing observed token usage
- Reject empty non-streaming translations across Codex HTTP, compact, WebSocket, OpenAI-compatible, Qwen, iFlow, Gemini CLI, and Devin without recording successful usage
- Reject empty xAI compact response translations with HTTP 502 and retain observed token usage as failed usage
- Honor configured generation, tool, and safety settings in Gemini token-count requests
- Record the actual response model in Codex image usage, preserving an earlier model when the terminal event omits it
- Honor configured model, service tier, and cache overrides or field removal for Codex image generation and editing in standard and streamed responses
- Require Go 1.27.2 and use `golang.org/x/net` v0.60.0; use Staticcheck v0.8.2 for the updated toolchain

### Compatibility

Model catalog entries describe supported identifiers and routing capabilities; access still depends on the account and provider

The Go SDK module remains `/v6`; the binary release tag is not a compatible Go module version, so pin SDK dependencies to the release commit

Linux and macOS core packages built with `CGO_ENABLED=0` cannot load standard dynamic-library plugins; use a compatible native-plugin build when those plugins are required

### 中文

### 版本依据

- 上游版本：v8.0.23（`d318bcc3afb9ea8782862f5e8cdb541afdc9dfa5`）
- 分叉点：`09fec34e1cdfd99ac79be458fff29f94b834dbcc`
- 上一个本地版本：v7.3.2-aug.1
- 配套版本：CPAM v1.26.0-aug.2

#### 更新内容

- 先收到下游取消确认时，仍按取消结束，避免误记为服务失败；上游错误和写入失败保持原结果
- 修正流式重试测试将关闭通道误判为响应数据的问题
- CI 的 race 测试使用 25 分钟包级时限，保留全部测试及 45 分钟任务上限

- 保存凭证文件和比较配额身份时保留数字精度，常规数值设置保持兼容
- 元数据归一化后仅恢复可证明对应当前材料的配额，无法验证数字精度的旧版本保持失效
- 配额身份校验失败时保留插件认证，并记录明确告警
- 凭证启停与重启恢复保留配额窗口、Credit、重置次数和观测时间，凭证材料替换继续隔离旧数据
- 文本响应中未使用的生图统计不再增加成功调用记录，保留已完成但用量为零的生图调用
- Codex 配额、重置明细与订阅查询使用同一账号范围，保留已取得的数据并标明附加查询失败

- 更新 Provider 认证、模型元数据与协议转换，保留 `/v6` SDK module、`/v0/management` API 和 flat 配置格式
- 在流式与非流式响应中保留工具身份、signature、namespace 和交付状态
- Codex `apply_patch` 能力协商默认关闭，支持的转换器处理工具声明、历史与响应，服务端不执行 patch
- 保留插件 OAuth 身份和嵌套模型回调归属，回调、上下文或插件实例结束时释放流
- 按目标 Provider 查询插件模型，不触发无关 Provider 的发现
- 保留 RPC 与原生插件的业务错误和 HTTP 状态，Windows 重入回调期间保持请求缓冲有效
- 上下文取消造成的交付错误记为 canceled，独立下游写入错误仍记为 failed
- `-t` 遇到无效配置时返回失败，不用 last-good 副本覆盖源配置
- Claude 转 Gemini 和 GeminiCLI 时跳过空消息，保留附件拒绝
- Gemini 和 GeminiCLI 保留工具调用 ID、跨 user 消息的结果顺序、结构化结果及逐消息附件错误，Schema 清理不再改写普通工具输入 JSON
- Gemini 响应保留合法工具续段 signature，连续独立 signature 分别返回
- Gemini 未匹配工具结果转为 user 文本时保留待完成调用的配对，通用 MIME 使用媒体默认类型，拒绝 base64 source 中的非法 data URL
- Claude 保留中断调用、晚到结果与冲突 ID 的工具配对，以及图片结果、缓存控制和可逆工具名称
- Claude 的工具添加、删除和内联替换使用声明对应的传输名称，保留调用方 MCP 与服务端工具
- 恢复 redacted reasoning，按模型版本识别 prefill 限制，报告不支持的内容且不记录请求正文；附件转换失败时 byte 请求入口不返回正文
- Gemini 流式与非流式响应中的重复完整工具快照只交付一次，保留调用身份，快照冲突返回 SDK 502 错误
- 保留晚到和交错的 Claude 工具调用，验证分段参数与完整快照一致性，输入冲突返回 SDK 502 错误
- 支持原生 Claude Messages JSON，保留模型、内容顺序、引用、reasoning signature 和缓存用量
- Claude Responses 达到 token 上限或 Provider 暂停回合时返回 incomplete
- Responses 转换使任一 user 消息失去全部可发送内容时，返回请求错误，不调用 Provider
- Responses 与 Chat 的声明、历史、工具选择和响应保留工具 namespace 与可逆名称
- custom apply_patch 调用不一致时返回失败，不返回 completed；HTTP 错误不暴露内部冲突信息
- 保留 Codex 合法音频、文件引用、字符串化图片工具结果和可逆工具名称
- Codex 本地 token 计数应用最终 payload 规则，并根据最终 model 选择 tokenizer
- 从完整快照交付尚未发送的工具参数；冲突快照经 SDK、HTTP 和 WebSocket 返回 502 错误
- Codex 流式与非流式响应交付生成图像
- Gemini 转为 Codex 时保留内联媒体和文件引用
- 保留 Interactions URI 附件与 developer instruction，媒体无法交付且 user turn 无内容时返回请求错误
- Claude 与 Interactions 转换保留 HTTP(S) 图片和文档、工具结果顺序、结构化内容及可逆工具名称
- Claude 上游错误返回 Interactions 失败状态，失败后不再交付后续内容
- Gemini 和 Antigravity 的 Interactions 转换保留并行工具历史及 thought summary
- 保留 Interactions 晚到的工具身份和普通工具参数前缀，拒绝源身份、快照和停止边界矛盾，源流在终态前结束时返回失败
- 保留显式零缓存用量和有效续段签名
- 合法 signature 与文本、工具调用和内联媒体一同保留；附件拒绝时 byte 请求入口不返回正文
- Claude 请求转为 Codex 时保留 HTTP(S)、file ID 和内联图片
- 保留工具结果顺序、指定工具选择及内建搜索来源
- 保留兼容的 Grok reasoning 和 Schema 联合类型；完整工具参数冲突时返回失败，不发送成功终态
- Gemini Responses 收到完整且一致的用量尾帧后返回终态，保留原 finish reason，`MAX_TOKENS` 时将活动消息标为 incomplete；`usageMetadata` 缺失时使用 `cpaUsageMetadata`
- Home 独立刷新 Devin 元数据，`local-model` 仍禁止远端目录刷新
- GLM-5.2 1M 请求使用有效模型标识，保留目录中的 thinking 档位与输出 token 上限
- Chat、Responses、Claude Messages 和 Gemini 的首个 token 耗时按有效输出计算，忽略保活与纯元数据帧
- 受保护 TLS 请求遵循逐请求代理选择，向 Google Provider 发送 Codex Desktop 工具参数时保留整数类型
- 图像流完成后保持用量记录中的返回模型，忽略迟到模型元数据，保留原始图像流、用量和 service tier
- quota-aware 调度策略修改即时生效，支持既有别名与 session affinity，无需重启服务
- 旧 watcher 更新到达时保留较新的已保存凭证设置
- Home 模式由 Home 提供账号模型，保留插件模型发现
- 自定义 Claude 模型应用 reasoning 更新，保留 manual 或 adaptive 模式
- Claude 请求保留归一化后的思考设置，摘要显示变化不改变思考强度
- 管理路径不生成普通请求日志，请求标识继续完整保留
- SDK 调用省略 OriginalRequest 时保留入口请求，继续验证客户端工具声明与参数
- 原生工具转换未返回正文时报告失败，不记为成功响应
- Gemini Interactions 失败状态在各响应格式中保持失败，保留已观察用量
- xAI WebSocket 请求取消或交付失败时关闭连接，交付确认后才记录成功
- Meta 订阅额度耗尽按凭证限制，保留实际重试时间
- Meta 转换、EOF、取消和完成过程保留实际失败状态与已观察用量
- 404 响应采用明确有效的重试时间，缺失或无效时保留原默认值
- Devin 连续 user 消息保留可发送文本和工具结果，省略媒体丢弃后无内容的 prompt
- 图像默认值遵循原始 JSON 与 multipart 字段，Codex 自动生图工具遵循最终模型与 tools 配置
- Provider 请求准备完成后应用 payload 配置，本地 token 计数使用相同来源协议与请求路径
- 插件响应转换失败返回请求级错误，保留已观察用量，执行结束时释放源流
- Home Anthropic 模型目录保留类型、显示名、token 上限及 RFC3339 创建时间
- 按最终 Claude 型号设置 Opus 5.5 自动回退，保留调用方及 payload 规则的显式选择
- 非 null Claude thread 从正数 cache-control 预算中预留一个名额
- 为签名 Claude 主会话的 billing 标签添加 cc_turn_origin=human
- AI Studio 请求正文超限时返回 HTTP 413，保持完整正文语义
- AI Studio 翻译失败或输出结束时取消对应 relay 读取
- AI Studio 上游失败响应返回错误并保留已观察用量
- AI Studio 请求准备失败时记录一次失败用量
- 当前凭证无法恢复认证时，按安全重派规则选择健康凭证
- 已向下游交付响应时，按终态处理认证失败结果
- 按有效额度恢复期限决定可选状态，独立处理可用性与重试标志
- Claude 多段消息指纹使用首个真实用户文本块
- Claude 指纹按原生 UTF-16 代码单元取样
- 为精确模型的 HTTP 429 限流设置恢复期限，保留其他模型的可用性
- 保留请求激活前到达的 WebSocket 上游终态错误
- 依据最终模型和服务等级生成 Codex OAuth 路由提示，保留有效请求头覆盖
- Home 模型元数据同时识别 max_context_length 与 maxContextLength
- Claude 工具 ID 在最终 payload 规则应用前完成修复
- 凭证冷却的模型状态展示与实际调度判断保持一致
- 凭证冷却独立维护自身期限与退避，保留独立模型额度状态
- 同 token 凭证成功续期后清除当前拒绝访问指纹
- Responses 流式错误保留显式序号和精确整数详情
- SSE 上游的增量 WebSocket 预热在本地处理，并保留此前回合
- Responses 流式用量记录保留写入和短写失败的实际原因
- 请求清理取消执行时，保留已观察到的上游流式失败
- Claude 配置的 system 内容经自动 reporting 和缓存处理后保持原文，OAuth 仍完成 CCH 签名
- 区分名为 advisor 的客户端工具与服务端 advisor 调用
- 未确认调用方的透传请求排除 Claude 原生提示头，并保留普通传输头部
- 保留 Claude 完整终态交付后的成功结果，尾部读取错误按已完成状态处理
- 验证 Antigravity 凭证删除重建后拒绝旧额度结果
- Claude OAuth 在最终 CCH 签名前清理内部缓存选项，签名与实际发送正文一致
- Gemini 与 Vertex 原生 Interactions 流识别具名完整终态快照，转换响应保持原有完整性校验
- Claude 上游返回空正文时返回 HTTP 502，空响应按失败处理
- Codex HTTP 和 WebSocket 的普通、流式请求保留配置明确指定的 input ID
- Claude 条件规则观察强制工具处理后的 thinking 状态，保留最终模型与工具校验
- Gemini 和 Vertex Interactions 接受完整 requires_action 终态快照，继续拒绝缺失 steps 和未完成快照
- Devin 客户端名称写入 metadata field 1，移除旧 field 28，保留凭证与设备元数据
- xAI compact 在内建清理后应用配置覆盖，HTTP、流式触发与 WebSocket 触发使用同一规则
- xAI 模型能力校验后保留明确配置的 thinking 覆盖值，无配置时保持既有清理行为
- xAI HTTP 响应翻译失败按失败返回并保留已观察 token，转换及流结束成功后发布成功用量
- Kimi 和 Kimi.ai 的 Chat、Responses 普通与流式请求传递翻译失败，保留已观察 token 用量
- Codex HTTP、compact、WebSocket、OpenAI-compatible、Qwen、iFlow、Gemini CLI 和 Devin 拒绝空的非流式翻译结果，并记录失败用量
- xAI compact 响应翻译结果为空时返回 502，并按失败记录已观察 token 用量
- Gemini token 计数请求保留显式配置的生成参数、工具与安全设置
- Codex 图像用量记录实际响应模型，终态缺少模型时保留早期值
- Codex 图像生成和编辑的普通及流式请求保留模型、服务等级与缓存配置的覆盖和字段删除
- 要求 Go 1.27.2，使用 `golang.org/x/net` v0.60.0，更新后的工具链使用 Staticcheck v0.8.2

#### 兼容性

模型目录说明支持的标识与路由能力，实际访问权限仍由账号和 Provider 决定

Go SDK module 保持 `/v6`，二进制发布 tag 不满足 Go module 的版本约束，SDK 依赖应固定发行提交

Linux 和 macOS 的 `CGO_ENABLED=0` 核心包不能加载标准动态库插件，需要这些插件时使用兼容的原生插件构建

Full changelog: https://github.com/Pyrokine/CLIProxyAPI/compare/v7.3.2-aug.1...v8.0.23-aug.2

## v7.3.2-aug.1 - 2026-09-25

### Release Basis

- Upstream semantic basis: v7.3.2 (`7fa443dc8bf8ca2f1ffd81c2472deb31b097b697`)
- Fork point with upstream v7.3.2: `09fec34e1cdfd99ac79be458fff29f94b834dbcc`
- Previous local release: v7.2.127-aug.1
- Local release: v7.3.2-aug.1
- Bundled management panel: v1.23.1-aug.1

### What's Changed

- feat (models): Update sourced model capabilities and prices for Claude, Codex, Gemini, DeepSeek, xAI and image models; keep unconfirmed or non-runtime models out of the default requestable inventory
- feat (credentials): Preserve credential identity and configuration across watcher updates, storage backends and management changes; restore durable and runtime state after partial plugin OAuth failures
- feat (routing): Carry quota freshness and cooldown identity through scheduling, refresh and retries without consuming active-reset credits during status checks; keep session affinity within the highest available credential priority and exclude prior-request diagnostics from cooldown responses
- feat (plugins): Extend ABI and RPC compatibility for auth, quota, request observers, affinity and plugin lifecycle
- feat (management): Integrate plugin OAuth, credential refresh, network-scoped proxy calls and guarded update state with Home runtime registration
- feat (usage): Preserve request attribution and accounting across asynchronous delivery, diagnostics and logs
- feat (protocols): Update Claude, OpenAI, Gemini, Codex, Antigravity and Interactions translation, including streaming tools, signatures and terminal errors
- feat (runtime): Update provider execution, reasoning replay, WebSocket transport, image handling and dynamic request headers
- fix (realtime): Revoke client secrets after access-provider changes without rejecting unrelated configuration reloads; honor ordinary keys with an `ek_` prefix, enforce API-key restrictions (including rejecting hard-limit policies that Realtime cannot meter), and reject malformed session requests
- feat (devin): Add OAuth, model catalog, execution, quota and session isolation support
- fix (server): Connect management and model routes to current runtime state, and retain trusted Codex client versions without sending a fabricated version for other clients

### Compatibility

- Docker release builds use the published CPAM v1.23.1-aug.1 `management.html`; publish that panel before the backend tag
- Existing credential stores remain supported through the current storage adapters
- Real paid Provider inference and active quota reset were not part of the local release checks

---

### 版本依据

- 上游语义基线：v7.3.2（`7fa443dc8bf8ca2f1ffd81c2472deb31b097b697`）
- 与上游 v7.3.2 的分叉点：`09fec34e1cdfd99ac79be458fff29f94b834dbcc`
- 上一个本地版本：v7.2.127-aug.1
- 本地发布版本：v7.3.2-aug.1
- 内置管理面板：v1.23.1-aug.1

### 更新内容

- feat (models)：更新 Claude、Codex、Gemini、DeepSeek、xAI 和图片模型的来源、能力与价格；未经确认或没有运行时凭证的模型不进入默认可请求目录
- feat (credentials)：在 watcher 更新、存储后端和管理操作之间保留凭证身份与配置；插件 OAuth 部分保存失败时恢复持久化与运行时状态
- feat (routing)：调度、刷新和重试期间传递 quota 新鲜度与 cooldown 身份，状态检查不消耗主动重置次数；会话粘性仅在当前最高可用凭证优先级内生效，冷却错误不向其他请求返回历史诊断
- feat (plugins)：扩展 auth、quota、请求观察、affinity 和生命周期相关的 ABI 与 RPC 兼容能力
- feat (management)：融合插件 OAuth、凭证刷新、限定网络范围的代理请求、更新保护状态与 Home 运行时注册
- feat (usage)：在异步交付、诊断与日志路径保留请求归属和使用统计
- feat (protocols)：更新 Claude、OpenAI、Gemini、Codex、Antigravity 与 Interactions 转译，包括流式工具调用、签名和终态错误
- feat (runtime)：更新 Provider 执行、推理重放、WebSocket 传输、图片处理和动态请求头
- fix (realtime)：认证 Provider 变更后撤销临时密钥，无关配置刷新不误撤销；支持以 `ek_` 开头的普通 API key，执行当前 API key 限制，无法计量的 Realtime 请求不绕过硬额度策略，并拒绝无效 session JSON
- feat (devin)：增加 OAuth、模型目录、执行、quota 和 session 隔离支持
- fix (server)：管理与模型路由使用当前运行时状态；透传受信 Codex 客户端版本，不为其他客户端伪造版本号

### 兼容性

- Docker 正式构建使用已发布的 CPAM v1.23.1-aug.1 `management.html`，后端 tag 发布前须先发布前端
- 现有凭证存储继续通过当前存储适配器支持
- 本地发版检查没有发起真实付费 Provider 推理或主动重置配额

## v7.2.127-aug.1 - 2026-09-13

### Release Basis

- Upstream semantic basis: v7.2.127
- Previous local release: v7.2.67-aug.3
- Local release: v7.2.127-aug.1
- Bundled management panel: v1.22.2-aug.1

### What's Changed

- feat (providers): Add Astra Codex compatibility and current DeepSeek V4.1 catalog entries for `deepseek-flash` and
  `deepseek-v4-pro`, including retirement aliases that resolve to V4.1 Flash
- feat (images): Support the official GPT Image 2.5 Flare and Sunburst floating and 2026-09-08 snapshot model IDs across
  image generations, edits, inpainting, and Responses image tools without adding an unsupported generic alias
- feat (usage): Record immutable request-time pricing receipts, expose model-price source and refresh metadata, apply the
  2026-09-10 default catalog decisions, and report asynchronous historical repricing progress
- feat (policy): Reserve API-key policy capacity before provider dispatch, preserve execution identity across retries and
  streams, and record explicit committed, released, or uncertain outcomes
- feat (management): Add guarded update sessions with artifact verification, installation health checks, rollback, backup
  retention, and append-only management mutation auditing
- fix (credentials): Preserve credential identity and provider state across subscription changes, refreshes, and concurrent
  management requests
- fix (quota): Return provider quota freshness, active-reset availability, remaining reset credits, and expiration metadata
  without consuming a reset during status refresh; recover only matching, authoritative, refresh-fresh Codex quota-family
  cooldowns so credentials become routable again without a service restart while newer errors remain intact
- fix (streaming): Normalize acknowledged-stream errors, terminal event handling, tool-call state, cancellation, and empty
  bootstrap failures across OpenAI, Responses, Claude, Gemini, and compatible provider paths; send shared no-buffer SSE
  headers, flush heartbeats during setup and response gaps, and bound both protocol-to-transport and post-terminal waits
- fix (runtime): Move HTTP/2 handling to the Go standard library while retaining uTLS dialing, proxy support, fixed-IP
  routing, public-host verification, and HTTP/2-only behavior
- chore (toolchain): Target Go 1.27.0, update all root required modules with available compatible releases, and clear
  actionable Staticcheck and vulnerability findings

### Compatibility

- Docker release builds bundle the published CPAM v1.22.2-aug.1 `management.html` asset
- The default DeepSeek inventory uses `deepseek-flash` and `deepseek-v4-pro`; retired V4 Flash identifiers remain aliases
  rather than separate inventory entries
- Existing Management API, provider configuration, usage backup, and SQLite data paths remain supported

---

### 版本依据

- 上游语义基线：v7.2.127
- 上一个本地版本：v7.2.67-aug.3
- 本地发布版本：v7.2.127-aug.1
- 内置管理面板：v1.22.2-aug.1

### 更新内容

- feat (providers)：增加 Astra Codex 兼容支持和当前 DeepSeek V4.1 模型目录项 `deepseek-flash`、
  `deepseek-v4-pro`，退役模型标识统一转发到 V4.1 Flash
- feat (images)：支持 GPT Image 2.5 Flare 与 Sunburst 的 floating 和 2026-09-08 snapshot 官方模型 ID，覆盖图片生成、编辑、
  inpainting 和 Responses 图片工具，不增加未发布的通用 alias
- feat (usage)：记录请求时不可变价格凭据，返回模型价格来源与刷新元数据，应用 2026-09-10 默认目录决策，并报告异步历史重算进度
- feat (policy)：Provider 请求前预留 API Key 策略容量，在重试与流式请求中保留执行身份，并记录 committed、released 或 uncertain 结果
- feat (management)：增加更新会话、发布物校验、安装后健康检查、回滚、备份保留和 append-only 管理写操作审计
- fix (credentials)：套餐变化、数据刷新和并发管理请求期间保留凭证身份与 Provider 状态
- fix (quota)：状态刷新返回 Provider 配额新鲜度、主动重置可用状态、剩余次数和过期时间，刷新状态不会消耗主动重置次数；
  只恢复 matching、authoritative 且早于本次刷新的 Codex quota-family cooldown，额度恢复后无需重启即可重新路由，同时保留较新错误
- fix (streaming)：统一 OpenAI、Responses、Claude、Gemini 和兼容 Provider 的已确认流错误、终态事件、工具调用状态、取消与空 bootstrap 失败语义；
  共用 SSE no-buffer header，在 setup 和响应空档 flush heartbeat，并限制 protocol terminal 到 transport terminal 及 terminal 后等待时间
- fix (runtime)：HTTP/2 迁移到 Go 标准库，同时保留 uTLS 拨号、代理、固定 IP、公开主机校验和 HTTP/2-only 行为
- chore (toolchain)：使用 Go 1.27.0，升级根 module 中全部存在兼容更新的 required module，并清除可修复的 Staticcheck 与漏洞告警

### 兼容性

- Docker 正式构建内置已发布的 CPAM v1.22.2-aug.1 `management.html`
- DeepSeek 默认目录使用 `deepseek-flash` 和 `deepseek-v4-pro`；退役 V4 Flash 标识保留为 alias，不作为独立目录项
- 现有 Management API、Provider 配置、使用统计备份和 SQLite 数据路径继续兼容

## v7.2.67-aug.3 - 2026-08-03

### Release Basis

- Upstream semantic basis: v7.2.67
- Previous local release: v7.2.67-aug.2
- Local release: v7.2.67-aug.3
- Required management panel: v1.17.14-aug.2 or newer

### What's Changed

- fix (usage): Limit legacy `/usage` details to 10,000 events and a conservative 32 MiB estimate that includes JSON
  escaping and decoded header structures, then return global and time aggregates with `apis: {}` and an explicit
  truncation warning
- fix (usage): Propagate request cancellation through SQLite and in-memory snapshots, response redaction, response
  encoding, and usage import responses; report post-import maintenance timeouts as successful imports with an explicit
  warning; query totals with a constant-memory SQL aggregate
- fix (usage): Keep degraded in-memory day and hour aggregates in UTC, matching SQLite-backed responses
- fix (tui): Keep dashboard and usage views available when legacy details are truncated, and display the server warning
  in both views
- test (usage): Cover event, structure, escaping and byte limits, high-cardinality aggregate responses, UTC parity, TUI
  warning display, cancellation, import totals, maintenance warnings, and allocations against a 20,000-event SQLite
  dataset

### Migration and Compatibility

- SQLite remains on schema v4; existing events, buckets, costs, and backups are unchanged
- Legacy `/usage` keeps request details only while the dataset is at most 10,000 events and the conservative
  decoded/encoded size estimate is at most 32 MiB; otherwise it returns global and time aggregates with `apis: {}`,
  `details_truncated: true`, and a warning that identifies `/usage/summary` and `/usage/events`
- Management panel v1.17.14-aug.2 already uses the aggregate and paginated APIs for the usage page
- Docker release builds continue to use the published CPAM v1.17.14-aug.2 `management.html` asset

---

### 版本依据

- 上游语义基线：v7.2.67
- 上一个本地版本：v7.2.67-aug.2
- 本地发布版本：v7.2.67-aug.3
- 要求的管理面板版本：v1.17.14-aug.2 或更新

### 更新内容

- fix (usage)：旧版 `/usage` 明细限制为 10,000 个事件和包含 JSON 转义、响应头解码结构的保守 32 MiB 估算，超限后返回全局与时间聚合、
  `apis: {}` 和明确的截断警告
- fix (usage)：SQLite 与内存快照、响应脱敏、响应编码和使用统计导入响应均继承请求取消信号；导入后维护超时改为返回成功并附明确警告；总数查询改用常量内存
  SQL 聚合
- fix (usage)：降级内存路径的日期和小时聚合统一使用 UTC，与 SQLite 响应一致
- fix (tui)：旧版请求明细被截断时继续显示 dashboard 和使用统计，并在两个页面展示后端警告
- test (usage)：覆盖事件、结构、转义和字节限制、高基数聚合、UTC 一致性、TUI 警告展示、请求取消、导入总数、维护警告，以及 20,000 个
  SQLite 事件下的分配量限制

### 迁移与兼容性

- SQLite schema 保持 v4，现有事件、bucket、费用和备份格式不变
- 旧版 `/usage` 仅在事件不超过 10,000 个且保守的解码/编码大小估算不超过 32 MiB 时保留请求明细；超限后返回全局与时间聚合、
  `apis: {}`、`details_truncated: true` 和指向 `/usage/summary`、`/usage/events` 的警告
- 管理面板 v1.17.14-aug.2 的使用统计页面已经使用聚合和分页接口
- Docker 发布构建继续使用已发布的 CPAM v1.17.14-aug.2 `management.html`

## v7.2.67-aug.2 - 2026-08-02

### Release Basis

- Upstream semantic basis: v7.2.67
- Previous local release: v7.2.67-aug.1
- Local release: v7.2.67-aug.2
- Required management panel: v1.17.14-aug.2 or newer

### What's Changed

- fix (usage): Preserve credential-source type information and store API-key sources only as masked values for live
  writes and non-encrypted legacy imports
- fix (management): Conservatively mask untyped historical credential sources in summary, time-series, event, and legacy
  usage responses while preserving typed identity labels and provider grouping
- fix (filters): Resolve raw and masked credential filters, merge API-key mask collisions, disambiguate identity/API-key
  display collisions, preserve provider-qualified filtering, and keep unmatched filters fail-closed
- fix (release): Pin Docker images and source-build helpers to management panel v1.17.14-aug.2

### Migration and Compatibility

- SQLite remains on schema v4; existing events and aggregates are not rewritten
- Existing untyped source values stay unchanged on disk and are conservatively masked at the authenticated management
  response boundary
- Legacy imports store untyped source values only as masked API-key credentials; encrypted backups preserve source
  values exactly for recovery
- Encrypted usage exports remain reversible and keep their existing format
- Docker release builds require the published CPAM v1.17.14-aug.2 `management.html` asset

---

### 版本依据

- 上游语义基线：v7.2.67
- 上一个本地版本：v7.2.67-aug.1
- 本地发布版本：v7.2.67-aug.2
- 要求的管理面板版本：v1.17.14-aug.2 或更新

### 更新内容

- fix (usage)：保存 credential source 类型信息，实时写入和非加密 legacy 导入中的 API-key source 仅以脱敏值保存
- fix (management)：未标记类型的历史 credential source 在 summary、时间序列、事件和旧版 usage 响应中保守脱敏，同时保留已标记的身份标签与
  provider 分组
- fix (filters)：credential filter 同时支持原值和脱敏值，合并 API-key 脱敏碰撞，区分 identity/API-key 展示碰撞，保留
  provider 限定查询，无匹配时继续返回空结果
- fix (release)：Docker 镜像和源码构建脚本固定使用管理面板 v1.17.14-aug.2

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

- feat (config): Make configuration and auth-file updates transactional, keep last-good recovery data, and update
  dependent runtime state only after persistence succeeds
- feat (plugin): Harden plugin host startup, authenticated private-repository release selection, artifact storage,
  resource cleanup, and scheduler lifecycle handling
- feat (auth): Update provider credentials, quota refresh, exhausted-account routing, model aliases, authentication
  metadata, and concurrency-safe quota status snapshots
- feat (runtime): Refresh provider executors, management handlers, interactions APIs, and protocol translators for the
  v7.2.67 semantic base
- feat (usage): Migrate SQLite usage storage to schema v4, preserve latency and cost aggregates, add encrypted portable
  backups, and enforce database capacity limits
- feat (model-catalog): Refresh catalog metadata and default prices, centralize server-side pricing, and apply catalog
  mutations transactionally
- fix (usage): Run historical cost recalculation in the background, expose task status, and merge concurrent price
  updates into one follow-up pass using the latest prices
- fix (release): Pin Docker images to management panel v1.17.14-aug.1 and reject missing, empty, mismatched, or fallback
  panel assets

### Migration and Compatibility

- The first startup after upgrading an older usage database runs the schema v4 migration before the usage store becomes
  ready; large databases can take several minutes
- Model-price saves and model-catalog price updates return without waiting for historical repricing; use the
  recalculation status endpoint to observe completion
- Version 3 usage backups preserve canonical SQLite event fields and costs; cross-server restores require the same
  management credential or `X-Usage-Backup-Key`
- Docker release builds require the published CPAM v1.17.14-aug.1 `management.html` asset and do not fall back to
  another panel version

---

### 版本依据

- 上游语义基线：v7.2.67
- 上一个本地版本：v7.2.50-aug.1
- 本地发布版本：v7.2.67-aug.1
- 要求的管理面板版本：v1.17.14-aug.1 或更新

### 更新内容

- feat (config)：配置和 Auth file 更新改为事务处理，保留 last-good 恢复数据，只在持久化成功后更新依赖的运行时状态
- feat (plugin)：加强 plugin host 启动、私有仓库 release 认证查询与选择、artifact 存储、资源清理和 scheduler 生命周期处理
- feat (auth)：更新 provider 凭证、quota 刷新、额度耗尽账号路由、模型别名、认证元数据和并发安全的 quota 状态快照
- feat (runtime)：按 v7.2.67 语义基线更新 provider executor、管理 handler、interactions API 和协议 translator
- feat (usage)：SQLite 使用量存储迁移到 schema v4，保留 latency 和成本聚合，新增加密便携备份，并限制数据库容量
- feat (model-catalog)：刷新模型目录元数据和默认价格，统一使用后端价格源，并以事务方式应用目录修改
- fix (usage)：历史费用重算改为后台任务，提供任务状态，并把并发价格更新合并为一次使用最新价格的后续重算
- fix (release)：Docker 镜像固定使用管理面板 v1.17.14-aug.1，拒绝缺失、空白、标签不匹配或 fallback 面板资产

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

- chore (deps): Upgrade Go dependencies for the v7.2.x release base
- feat (plugin): Add plugin store registry, Home plugin sync/status APIs, plugin resources, and hardened plugin host
  platform handling
- feat (management): Expand config, logs, account inspection, auth-file, quota, version, and plugin management APIs
- feat (runtime): Update Codex/OpenAI image and video paths, xAI/Grok execution, Antigravity grounding, Claude
  signing/uTLS, and provider translators
- feat (auth): Improve quota scheduling, OAuth model alias mapping, auth session affinity, and Codex prompt cache reuse
- feat (models): Refresh model catalog metadata, disable-image-generation mode, and config watcher/synthesizer behavior
- fix (security): Redact sensitive log/download/request outputs and harden plugin download, proxy, and Grok validation
  paths
- refactor (amp): Remove Amp backend module and management routes

### Breaking Changes

- Amp backend management/module support is removed in this release
- Management compatibility is raised to CPAM v1.17.10-aug.1 because the new panel depends on the expanded management API
  surface
- No other public proxy route break is intended by this release

---

### 版本依据

- 上游版本：v7.2.50
- 上一个本地版本：v7.1.68-aug.1
- 本地发布版本：v7.2.50-aug.1
- 要求的管理面板版本：v1.17.10-aug.1 或更新

### 更新内容

- chore (deps)：升级 Go 依赖，匹配 v7.2.x 发版基线
- feat (plugin)：新增 plugin store registry、Home plugin 同步和状态 API、插件资源能力，并加固 plugin host 平台处理
- feat (management)：扩展配置、日志、账号巡检、Auth file、quota、版本和插件管理 API
- feat (runtime)：更新 Codex/OpenAI 图片和视频链路、xAI/Grok 执行、Antigravity grounding、Claude signing/uTLS 和 provider
  translator
- feat (auth)：改进 quota 调度、OAuth model alias 映射、认证会话亲和和 Codex prompt cache 复用
- feat (models)：刷新模型 catalog 元数据、disable-image-generation 模式和配置 watcher/synthesizer 行为
- fix (security)：日志、下载和请求输出脱敏，并加固插件下载、proxy 和 Grok 校验链路
- refactor (amp)：移除 Amp 后端模块和管理路由

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

- feat (plugin): Add the plugin host runtime, callback bridge, scheduler hooks, ABI types, and runnable plugin examples
- feat (models): Add model catalog storage, default pricing data, model metadata APIs, and management endpoints
- feat (management): Expand configuration, auth-file, account-inspection, model-price, and plugin management APIs
- feat (runtime): Extend provider execution paths for Codex, Gemini, Claude, Antigravity, OpenAI-compatible, xAI, Kimi,
  Qwen, and Amp flows
- feat (observability): Enrich request logging, request timelines, usage events, source/provider metadata, and usage
  persistence
- fix (release): Keep Docker image builds working when the management panel asset is absent from a clean backend
  checkout
- chore (release): Add a repository changelog and make tag releases consume the matching changelog entry as release
  notes

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

- feat (plugin)：新增 plugin host 运行时、callback bridge、scheduler hooks、ABI 类型和可运行插件示例
- feat (models)：新增模型 catalog 存储、默认价格数据、模型元数据 API 和管理端点
- feat (management)：扩展配置、Auth file、账号巡检、模型价格和插件管理 API
- feat (runtime)：扩展 Codex、Gemini、Claude、Antigravity、OpenAI-compatible、xAI、Kimi、Qwen 和 Amp 执行链路
- feat (observability)：增强请求日志、请求时间线、使用事件、source/provider 元数据和使用统计持久化
- fix (release)：后端干净 checkout 缺少管理面板资产时，Docker 镜像构建仍可完成
- chore (release)：新增仓库 changelog，tag release 使用对应 tag 段落作为 release notes

### 不兼容变更

- 本版本不声明破坏公开代理 API 路由契约
- 管理面板兼容要求提升到 CPAM v1.15.3-aug.1，因为新版面板依赖扩展后的管理 API

## v7.1.19-aug.2 - 2026-05-27

### What's Changed

- fix (ci): Publish Docker images to `pyrokine/cli-proxy-api`

---

### 更新内容

- fix (ci)：Docker 镜像发布到 `pyrokine/cli-proxy-api`

## v7.1.19-aug.1 - 2026-05-27

### Release Basis

- Upstream: v7.1.19
- Local release: v7.1.19-aug.1

### What's Changed

- feat (home): Add certificate provisioning and auto-refresh for self-hosted setups
- feat (openai): Add image and video endpoint handlers, plus Codex client model listing
- feat (claude): Add request signing and uTLS transport support
- feat (usage): Add SQLite `provider_id` dimension for per-provider usage breakdowns
- feat (cluster): Add Docker Compose template and environment configuration for multi-node deployments
- feat (auth): Update quota handlers, auth-file endpoints, and credit tracking
- refactor (runtime): Update multi-provider executors, translators, thinking logic, registry, config, middleware, and
  logging layers
- test: Expand coverage for credits, quota, headers, auth flows, and flaky Amp cache behavior
- docs: Update README, config example, and ignore rules

---

### 版本依据

- 上游版本：v7.1.19
- 本地发布版本：v7.1.19-aug.1

### 更新内容

- feat (home)：新增自托管场景的证书申请和自动续期
- feat (openai)：新增图片和视频端点处理，以及 Codex 客户端模型列表
- feat (claude)：新增请求签名和 uTLS 传输支持
- feat (usage)：SQLite 使用统计新增 `provider_id` 维度，支持按 provider 分类
- feat (cluster)：新增多节点 Docker Compose 模板和环境配置
- feat (auth)：更新配额处理器、Auth file 端点和 credits 追踪
- refactor (runtime)：更新多 provider executor、translator、thinking、registry、config、middleware 和日志层
- test：扩展 credits、配额、请求头、认证流程和 Amp cache 覆盖
- docs：更新 README、config 示例和 ignore 规则

## v6.10.9-aug.4 - 2026-05-27

### What's Changed

- fix (update): Restart the service after auto-update and refresh release metadata after a successful download

---

### 更新内容

- fix (update)：自动更新完成后重启服务，并刷新 release 元数据

## v6.10.9-aug.3 - 2026-05-27

### What's Changed

- fix (update): Remove the download size cap that blocked large release archives during auto-update

---

### 更新内容

- fix (update)：移除自动更新下载大 release 归档时的大小限制

## v6.10.9-aug.2 - 2026-05-27

### What's Changed

- fix (update): Match versioned release archive filenames during auto-update

---

### 更新内容

- fix (update)：自动更新下载时正确匹配带版本号的 release 归档文件名

## v6.10.9-aug.1 - 2026-05-27

### Release Basis

- Upstream: v6.10.9
- Local release: v6.10.9-aug.1

### What's Changed

- feat (usage): Migrate usage statistics to SQLite for persistence and query efficiency
- feat (management): Add quota status and auth-file operation endpoints
- feat (models): Expose model context length and compatibility metadata through APIs
- refactor (translator): Normalize provider registration and adapter layering
- chore (build): Align repo tooling, CI config, and documentation

---

### 版本依据

- 上游版本：v6.10.9
- 本地发布版本：v6.10.9-aug.1

### 更新内容

- feat (usage)：将使用统计迁移到 SQLite，提升持久化能力和查询效率
- feat (management)：新增配额状态和 Auth file 操作端点
- feat (models)：通过 API 暴露模型上下文长度和兼容性元数据
- refactor (translator)：规范化 provider 注册和 adapter 分层
- chore (build)：对齐仓库工具链、CI 配置和文档

## v6.9.0-aug.1 - 2026-05-27

### Release Basis

- Upstream: v6.9.0
- Local release: v6.9.0-aug.1

### What's Changed

- feat (usage): Add persistent SQLite-backed usage tracking with management API queries by model, credential, and time
  range
- feat (security): Harden auth endpoints with TLS enforcement, constant-time key comparison, rate limiting, SSRF
  protection, DNS pinning, OAuth CSRF/nonce validation, and CSP headers
- feat (oauth): Consolidate OAuth constants and provider implementations across Anthropic, Gemini, Codex, and
  Antigravity
- refactor (translator): Extract shared translator layers, byte pipelines, and tool-call continuation helpers
- refactor (config): Split config loading and logging modules
- chore (build): Migrate Go module path and upgrade dependencies

---

### 版本依据

- 上游版本：v6.9.0
- 本地发布版本：v6.9.0-aug.1

### 更新内容

- feat (usage)：新增 SQLite 持久化使用统计，管理 API 支持按模型、凭证和时间范围查询
- feat (security)：加固认证端点，覆盖 TLS 强制、常量时间 key 比较、限流、SSRF 防护、DNS pinning、OAuth CSRF/nonce 校验和 CSP
  响应头
- feat (oauth)：统一 Anthropic、Gemini、Codex 和 Antigravity 的 OAuth 常量及 provider 实现
- refactor (translator)：抽取共享 translator 层、字节管线和 tool-call continuation helper
- refactor (config)：拆分配置加载和日志模块
- chore (build)：迁移 Go module 路径并升级依赖
