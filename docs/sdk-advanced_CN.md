# SDK 执行器与翻译器

本文说明如何为内嵌的 CLIProxyAPI 服务实现自定义 Provider 执行器、协议翻译器和模型注册。SDK 使用 Go 1.27 和
`/v6` 模块路径。

`examples/custom-provider/main.go` 是可构建的参考实现。下列片段只展示单个接口，省略外围 package 和错误处理代码。

既有 SDK 可继续使用 `openai.OpenAIResponsesAPIHandler` 和 `openai.NewOpenAIResponsesAPIHandler`，
处理流程复用当前的 `ResponsesAPIHandler` 和 `NewResponsesAPIHandler`，Responses 流只交付完整 SSE 事件，
流关闭时尚未完成的事件返回错误

零值 `APIHandler` 的图像接口仍对不支持的模型返回 HTTP 400，执行支持的图像模型时使用 `NewAPIHandler(base)`，缺少处理器配置时返回
HTTP 500

`disable-image-generation` 接受 `off`、`all`、`chat`、`passthrough`，兼容旧 bool 值及 `on`／`yes`／`no` 输入
JSON 将 `off`／`all` 分别输出为 `false`／`true`，扩展模式保持字符串
`all` 关闭图像端点，`chat` 在图像端点之外移除图像工具及对应的强制选择
`passthrough` 保留传入的图像工具，并跳过 Codex 的自动图像工具添加
执行器先过滤工具，再应用 payload 规则，显式 override 决定最终请求的工具

## Provider 执行器接口

每个自定义 Provider key 需要注册一个 `sdk/cliproxy/auth.ProviderExecutor` 实现。当前接口包含以下方法：

```text
type Executor struct{}

func (Executor) Identifier() string
func (Executor) Execute(
    context.Context,
    *coreauth.Auth,
    clipexec.Request,
    clipexec.Options,
) (clipexec.Response, error)
func (Executor) ExecuteStream(
    context.Context,
    *coreauth.Auth,
    clipexec.Request,
    clipexec.Options,
) (*clipexec.StreamResult, error)
func (Executor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error)
func (Executor) CountTokens(
    context.Context,
    *coreauth.Auth,
    clipexec.Request,
    clipexec.Options,
) (clipexec.Response, error)
func (Executor) HttpRequest(
    context.Context,
    *coreauth.Auth,
    *http.Request,
) (*http.Response, error)
```

`Identifier` 返回认证记录使用的 Provider key。`Execute` 返回完整的 Provider 响应。`ExecuteStream` 返回响应头和
`StreamChunk` channel。流生产者通过 `StreamChunk.Err` 报告终态错误，通过 `StreamChunk.Terminal` 标记明确的完成
边界，最后关闭 channel。

`HttpRequest` 执行原始 Provider 请求。调用方负责关闭非 nil 响应的 body。Provider 不支持 token count 接口时，
`CountTokens` 应返回 request-scoped 错误，避免认证管理器把能力缺失误判为凭证故障。

上游因 `store=false` 无法读取历史 item 时，原 404 和正文返回给调用方，凭据与模型保持可用，普通、计数和流式入口使用相同的请求范围，失败计数继续记录实际尝试

凭据的 `request_retry` 只参与符合当前请求资格的重试，旧冷却记录中的 400／401／402／404 错误保留原有的重试限制，已记录的显式重试许可按当前
phase 和 outcome 判断；结果未知的 500／502 保留原错误并结束本次调用，后续独立请求继续检查当前可用性

凭据准备阶段使用相同的重试许可，尚未发送且允许重试的失败可选择下一个凭据，明确 `no_retry` 时结束当前请求；执行器调用后的
Windows socket read 或 unexpected EOF 属于结果未知，结束当前请求，后续独立请求仍可使用保持可用的凭据

请求级错误保留执行器明确给出的处理结果；Codex 在 HTTP 200 后报告缺失终态时，继续返回原超时错误并保持 `unknown`、`no_retry`
，已经收到的部分输出继续交付

请求开始时固定全局重试默认值，修改后的默认值用于后续请求；每个凭据的 `request_retry` 限定可参与的额外轮次，
`max-retry-credentials` 限定每轮尝试的凭据数量，被本轮上限跳过的凭据仍按当前轮次判断资格；Home 在当前轮次尚未选中凭据时等待短暂冷却，保持当前轮次

启用冷却时，HTTP 429 的显式 `Retry-After` 低于 10 s 时按 10 s 保存，同一轮已经尝试过的凭据，即使冷却在其他凭据执行期间到期，再次尝试前仍至少等待
10 s；关闭冷却后，已到期的凭据保留立即重试资格，尚未尝试且冷却已到期的凭据立即可用

非流式 Chat 和 Completions 请求在等待执行期间，按 `nonstream-keepalive-interval` 指定的秒数发送空行，配置为 0
时关闭；执行完成、失败或取消后，先停止并等待保活写入结束，再发送最终成功或错误正文

Chat 和 Completions 流在所有已观察的 choice 都提供非空 `finish_reason` 后发送 `[DONE]`，只有 role 的 choice
也需要自己的结束标记，null 或空字符串使该 choice 保持未完成，流关闭时仍有未完成 choice 则返回截断错误；完成后的纯 usage
块继续保留

Chat、Completions、Responses 和 Claude 在尚未发送响应内容时发生上游错误，返回带原 HTTP 状态和安全 `Retry-After` 的 JSON
错误；已发送心跳或内容时，按对应协议返回流式错误，内容通道关闭后继续保留待交付的错误及响应头；Claude 错误对象保留上游重置时间等附加字段，嵌套
JSON 与整数原值保持

按 chunk 消费时，对应内容的 write 和 flush 成功后调用 `StreamChunk.Acknowledge(nil)`，失败时传入实际交付错误，终态交付确认成功后，生产者保存
replay 并发布成功用量

Manager 等待上游 chunk 时，调用方取消会释放并发占用；逐帧 ACK 流记录请求范围的交付失败并关闭输出，普通 SDK 流最多等待 1
s，收到源的终态错误或源关闭后结束等待并关闭输出，源错误保留执行结果回调，单独的请求取消不提交执行结果；请求取消保持凭据可用性

原 byte 流调用方继续使用 `sdk/cliproxy/usage` 的整流确认 API，`WithStreamDelivery`
返回独立交付上下文与完成函数，所有退出路径调用完成函数，第一次调用的结果生效；生产者关闭输出后再调用 `WaitStreamDelivery`
，请求取消保留对交付结果的等待，`SupportStreamDelivery` 注册一个活动生产者并返回可重复调用的释放函数，最后一个生产者释放后
`StreamDeliverySupported` 返回 false，内部模型与协议执行隔离外层交付归属，保留取消传递

插件流的发送项被接受后保持成功结果，取消尚未交给接收方的发送会结束等待；流取消丢弃未交付内容，正常关闭按顺序交付已接受内容，客户端交付继续以
write／flush 后的 ACK 为准

执行器还可以实现 `coreauth.RequestPreparer`：

```text
func (Executor) PrepareRequest(request *http.Request, auth *coreauth.Auth) error
```

该方法用于向原始请求注入 Provider 凭证，不应修改共享 transport 或认证记录。

## 认证错误

`coreauth.Error` 按顺序保留 `Code`、`Message`、`Retryable` 和 `HTTPStatus` 四个公开字段，原位置字面量继续有效：

```go
legacy := coreauth.Error{"legacy", "original SDK message", true, 429}
```

在 `*Error` 上调用 `WithExecutionDisposition(disposition, generation)` 设置发送阶段、执行结果、重试许可、作用范围与代际，
`ExplicitExecutionDisposition()` 读取已设置的值，`ExecutionDisposition()` 在未提供执行结果时按 HTTP 状态归类，
`ExecutionStateGeneration()` 读取附加代际；`ErrorCodeTransientTransport` 标识传输失败，实际发送阶段与执行结果决定重试资格

| 执行字段          | 读取方式                                            | 通过 `WithExecutionDisposition` 设置 |
|-------------------|-----------------------------------------------------|--------------------------------------|
| `DispatchPhase`   | `err.ExplicitExecutionDisposition().DispatchPhase`  | `disposition.DispatchPhase`          |
| `Outcome`         | `err.ExplicitExecutionDisposition().Outcome`        | `disposition.Outcome`                |
| `RetryDirective`  | `err.ExplicitExecutionDisposition().RetryDirective` | `disposition.RetryDirective`         |
| `EffectScope`     | `err.ExplicitExecutionDisposition().EffectScope`    | `disposition.EffectScope`            |
| `EffectKey`       | `err.ExplicitExecutionDisposition().EffectKey`      | `disposition.EffectKey`              |
| `StateGeneration` | `err.ExecutionStateGeneration()`                    | `generation` 参数                    |

单独更新一个字段时，读取显式 disposition，修改对应字段，再传入当前代际，设置方法会用输入值替换全部执行字段，显式零值同样生效

通过 `err.Clone()` 完整复制公开字段、执行状态和运行时原因，`copied := *err` 这样的普通值复制保留四个公开字段；向
`json.Marshal` 传入 `*Error` 可保存执行字段，传入 `Error` 值时输出四字段形式，解码部分对象只更新输入中出现的字段，JSON
`null` 保留原错误，运行时原因保留在错误链中并从 JSON 省略

无可用凭据的错误通过返回值提供 `Headers()` 和 `WithAuthError(base)`，`Headers()` 返回独立响应头副本，`Retry-After`
使用本地计算的剩余时间并向上取整到秒；`WithAuthError` 替换认证错误描述，保留原等待时间和上游原因，共用同一 base 的包装错误保持
`errors.Is` 身份，通过 `IsTerminalAuthError` 判断凭据是否需要重新认证

## 请求元数据

`Options.EnsureMetadata()` 在 `Metadata` 为 nil 时初始化 map，并返回该 map，已有字段和显式零值保持不变，调用执行器前设置请求自身的元数据：

```go
opts := clipexec.Options{}
opts.EnsureMetadata()["custom_route"] = "primary"
```

该方法直接返回已有 map，需要独立元数据的请求分别使用自己的 options 和 map

鉴权后回调收到独立的请求容器，回调返回 `Handled=true` 时，管理器再次复制选用的请求和选项，再执行请求：

| 字段                                         | 复制方式            | 迁移后的所有权                                             |
|----------------------------------------------|---------------------|------------------------------------------------------------|
| `Request.Payload`、`Options.OriginalRequest` | 独立字节缓冲区      | 回调修改保留调用方原输入，回调后续修改保留已选用的执行正文 |
| `Options.Headers`、`Options.Query`           | 独立 map 及逐值数组 | 每个请求分别修改自己的 header 和 query                     |
| `Request.Metadata`、`Options.Metadata`       | 独立顶层 map        | 嵌套扩展值沿用原所有权                                     |

`$CPA-SESSION-ID` 使用当前请求的会话值，`util.WithSessionID` 的显式值和显式空值优先，空值省略该动态头；仅提供 logging
metadata 的旧调用保留其 `SessionID`，直接使用请求头工具时，可在处理请求前设置 `util.SessionIDResolver`，回调接收当次
context 和调用方请求头

鉴权后拦截器清除派生会话时，移除对应请求头和会话 metadata，显式 context 的值继续保持；普通、计数和流式入口使用相同规则，会话亲和开关保持这些规则，
`Authorization` 等凭据头继续由所选凭据提供

`ExtractSessionID(headers, body, metadata)` 返回公开 v6 会话值，沿用原来源优先级；内部统计与路由需要带来源的身份时使用
`CanonicalSessionID`，两个结果分别保存：

| 输入或字段                                                         | `ExtractSessionID` 返回值    | canonical 消费                     |
|--------------------------------------------------------------------|------------------------------|------------------------------------|
| `Session-Id: s` 或 `Session_id: s`                                 | `s`                          | `codex:s`                          |
| `X-Session-ID: s`                                                  | `s`                          | `header:s`                         |
| 正文同时含 `prompt_cache_key: cache` 与 `session_id: session`      | `cache`                      | `session:session`                  |
| Claude JSON `metadata.user_id` 中的 `session_id`                   | 保留原 `claude:` 表示        | 按会话及 agent 形成独立身份        |
| `CanonicalSessionIDMetadataKey`、`LCPAffinitySessionIDMetadataKey` | 保留原公开来源与内容推导结果 | 在缺少显式身份时优先读取这两个字段 |
| 嵌套 `request` 的会话与 agent 字段                                 | 保留原公开来源规则           | 从嵌套字段读取当前身份             |

`ExecutionSessionMetadataKey` 的公开结果保留 `execution:`，`user:`、`derived:` 与内容推导结果也沿用原表示；原值本身含有
`session:` 等前缀时保留该字面值，合法的 256 字节原会话值保持完整，canonical ID 单独限制到 256 字节并保留长值的唯一性

Codex 在最终 payload 确定后计算 prompt-cache 与 replay 身份，replay 按服务、凭据、调用方、会话和最终模型隔离；同一凭据刷新
token 后继续使用原身份，切换凭据或调用方后分别读取自己的记录，内置执行器沿请求 context 使用 Required 读／删除入口，成功交付终态后追加回合，原生
Responses 请求保持独立，缓存保留 1 h 滑动有效期与独立字节副本，读取及清理错误继续可见

`Result.AuthGeneration`、`Result.StateGeneration` 和 `Result.ModelQuotaEpoch` 将执行结果绑定到管理器当时观察到的凭据及状态；通过
typed mutation 方法和预期 generation 更新托管凭据，管理器拒绝旧结果对已替换凭据或较新状态的写入，请求代理用于执行，凭据刷新继续使用自身的传输规则

Responses WebSocket 依次采用 `X-Client-Request-Id`、`X-Codex-Turn-Metadata` 中的 `session_id`、`Session-Id` 和
`Session_id` 识别下游会话，带 `previous_response_id` 的请求保留完整的 `function_call` 和 `custom_tool_call`
，工具缓存在响应成功交付后提交，失败请求保留上一回合的缓存，最后一个同会话连接结束时清除缓存，合并历史与缓存值保留独立的字节缓冲区

错误帧保留 JSON 错误节点及嵌套扩展字段中数字的原始值与写法，包括大整数、小数和指数形式

Responses 流从已接纳的 `response.output_item.done` 事件重建缺失或为空的最终 `response.output`
，已有的非空输出保持优先；流开始后发生内部失败时，普通客户端和 Codex 客户端收到通用 `Bad Gateway`
事件，请求错误保留脱敏后的具体信息，WebSocket 在关闭连接前使用相同的错误反馈规则

## Home 凭据请求与订阅

`RPopAuth` 和 `RPopAuthWithPolicy` 保留原有的 `count`，省略 `retry_round`；`RPopAuthWithConstraints` 每次请求一个凭据，并显式发送从
0 开始的重试轮次，`ExcludedAuthIDs=nil` 省略排除字段，显式空数组发送 `excluded_auth_ids: []`

配置订阅收到有效 ACK 并完成新命令连接的探测后才就绪，每个实际订阅频道的 ACK 都校验类型、频道和累计数量；取消 context
会关闭订阅读取并返回 `context.Canceled`

Service 停止时撤销 Home 分派，取消仍在执行的请求，等待并发占用释放得到 Home 确认，再关闭客户端；普通、计数和流式请求完成或取消后，各自的执行资源只释放一次

流式请求收到上游错误后，管理器继续消费剩余源事件，直到源关闭、请求取消或达到 1 s
上限，再结束本次流式调用；下游收到第一个错误和在此之前的内容，执行器未返回流式数据源时，
`Manager.ExecuteStream` 直接返回错误

Home 选择凭据和上游模型，鉴权后拦截器更改目标时，管理器重新读取最终模型的能力，包括 `[1m]`
支持；处理结果未知时保留原错误并结束调用，已明确拒绝处理的请求按重试许可选择下一个凭据，重新分派前等待 Home 确认前一次并发占用已释放

固定凭据时，凭据自身的 `request_retry` 与 Home 汇总预算共同限定额外轮次，采用较低值；同轮已经尝试过该凭据时，在本地结束当前轮次，新轮次使用新的轮内排除集合；旧
`RPopAuth` 分派器在明确安全拒绝后继续按预算重试，声明支持约束却返回已排除凭据时结束当前请求

达到 `max-retry-credentials` 时结束当前轮次，重试许可和剩余预算允许时立即请求下一轮候选

Home 轮次结束时，错误保留本轮已尝试凭据提供的最早有效等待时间，后续 Home 冷却响应提供权威等待时间；普通、计数和流式错误通过
`SafeResponseHeaders` 返回向上取整到秒的等待提示，流式开始阶段的错误同时保留对应响应头

SDK 的 `AuthSelectionModelMetadataKey` 与 `Request.Model` 不同时，Home 使用选择模型取得凭据，执行器继续使用
`Request.Model`；Home 启用 force mapping 时，`RequestedModelMetadataKey` 决定响应中客户端可见的模型名，并保留前缀与思考后缀，缺省时使用
`Request.Model`，关闭 force mapping 后保留 Provider 的响应模型

上游返回 401 时，保留原错误并结束本次调用，`RefreshHomeSelectionAfterUnauthorized` 是只读快照查询，返回的布尔值表示同一启用凭据的选择中已安装不同的非空
token；Home 返回的 auth_not_found、auth_unavailable、refresh_temporarily_unavailable 和 home_unavailable 错误采用 HTTP
503，并保留原错误代码与正文

流开始后返回上游错误时保留原错误，源关闭、请求取消或现有清理期限到达后，结束对应的 Home 选择；成功的 WebSocket
流继续保留选择，供同一会话复用

查询参数 `key` 或 `auth_token` 在请求未带鉴权头时作为 `X-Goog-Api-Key` 发送给 Home，已有 `Authorization`、`X-Goog-Api-Key`
和 `X-Api-Key` 保持原值，查询来源的鉴权 metadata 使用相同规则；Home 选择返回的 `user_api_key` 更新该请求的 API key
身份，供后续处理读取

通过 `PublishHomeDispatch(client, registry, generation)` 一起发布 Home 客户端与执行登记表，并保存返回的生命周期对象，
`HomeDispatchBundle()` 读取当前活动对象，`ClearHomeDispatchBundle(bundle)` 仅在该对象仍处于活动状态时撤销它

使用 `GetExecutionSessionAuthByID(sessionID, authID)` 读取 Home 会话凭据，快照由仍有效的选择持有，选择结束后清除；同一
WebSocket 会话的请求依次执行，最后一个调用方结束后回收对应锁记录，`CloseExecutionSession(sessionID)` 结束指定会话的选择，其他会话继续保留，
`CloseExecutionSession(CloseAllExecutionSessionsID)` 结束全部会话，重复关闭时每份资源和并发占用只释放一次

Home 从 `SourceFormat` 推断 LCP Provider 时忽略大小写，同一协议的不同大小写名称保留相同会话绑定；共同父命名空间需要非空，以
`:` 开头的合法会话 ID 保留原身份及各自的执行会话凭据；prompt-cache 与 conversation 别名采用配置的亲和 TTL，TTL
变更后清空别名缓存，缓存软上限为 4096 个别名条目，超限时按整组淘汰

`HomeDispatchSelection.CloneAuth()` 返回独立凭据副本，`ReplaceAuth(auth)` 可更新独立或托管的选择对象，并保留以下路由属性：

| `Auth.Attributes` 字段 | 含义                   | 替换规则         |
|------------------------|------------------------|------------------|
| `home_upstream_model`  | Home 选择的上游模型    | 保留当前选择的值 |
| `home_force_mapping`   | 响应是否使用客户端别名 | 保留当前选择的值 |
| `home_original_alias`  | 客户端可见的原始模型名 | 保留当前选择的值 |

托管的运行时凭据刷新提交成功后，活动选择和会话快照使用同一份新凭据；旧 owner、已替换执行器或最终提交被拒绝时，两处凭据保持原值

Codex 和 xAI 保留 `UpstreamDisconnectChan` 的首次断连订阅，空闲上游断开后结束旧的 Home 选择并释放并发占用，下游继续保留连接，下一次请求由
Home 重新选择凭据

## 使用请求所属 Home 的 KV

在执行器内将管理器传入的 context 交给 `coreauth.HomeKVClientFromContext(ctx)`，返回的 `HomeKVClient` 提供
GET、SET、DEL、EXPIRE、TTL、INCRBY、MGET 和 MSET，SET 的选项使用公开的 `coreauth.HomeKVSetOptions` 类型

显式本地作用域返回 `homeMode=false`，启用 Home 后连接不可用或客户端缺少常用操作时返回错误，请求准备应继续向调用方返回该错误；未指定作用域的
context 沿用当前客户端兼容路径

`KVTTL` 返回剩余时间、是否存在和错误，键不存在时 `found=false`，永久键返回零时间和 `found=true`；`KVMGet` 保留键的输入顺序，通过
found 标记区分缺失值和空值；`KVMSet` 使用一次命令写入全部键值对，INCRBY 保留有符号 64 位整数

内置思考签名缓存使用相同的请求作用域，Home 读取或延长有效期失败时在发送 Provider 请求前返回请求级 HTTP
503，凭据保持可用；已完成响应的缓存写入失败会记录日志，已交付内容保持不变，本地签名缓存按服务隔离

Antigravity 启用积分功能后，普通请求的后台预热优先读取当前 Home 的提示和已有余额，通过 SET NX 协调 10 min 的查询间隔，查询结果使用
30 min 有效期

积分查询最多执行 5 s，普通预热使用独立预算，token 刷新后的查询随调用方取消，取消后允许立即重新查询；新 token
或较新的凭据代际会取消旧查询，旧代际和取消后的结果会被拒绝

使用 `GetAntigravityCreditsHintRequired(ctx, authID)` 读取积分观察，返回 hint、found 和 error；本地作用域读取进程提示，Home
作用域读取当前 Home 的观察，保留已知可用、已知不可用和未知状态，读取失败返回原错误，空身份返回空值及 found=false

自定义执行器通过 `auth.PublishAntigravityCreditsHintWithContext(ctx, hint)` 发布当前凭据的观察值，Home 发布使用 CAS
保留较新的观察值，客户端缺少 CAS 或写入失败时记录日志并返回 false；原 `PublishAntigravityCreditsHint(hint)` 保持本地发布行为

Antigravity 生成与流式请求读取 Home 的短冷却，期限内返回 HTTP 429 和剩余等待时间，过期后删除旧键；KV 读取、写入或删除失败时返回请求级
HTTP 503，原错误原因保持可查，计数请求沿用独立端点，关闭冷却或显式使用积分时跳过生成请求的短冷却

有重试预算时，相同凭据的单次等待上限为 500 ms；超过该值且小于 5 min 的终态限流 429 按提示写入 Home 短冷却，其他等待由管理器继续调度

## 注册执行器

使用 SDK token store 创建管理器，注册执行器，再传给 builder：

```text
tokenStore := sdkauth.GetTokenStore()
if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
    setter.SetBaseDir(cfg.AuthDir)
}
core := coreauth.NewManager(tokenStore, nil, nil)
core.RegisterExecutor(Executor{})

service, err := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath(configPath).
    WithCoreAuthManager(core).
    Build()
```

认证记录的有效 Provider key 与 `Identifier()` 一致时，请求会交给该执行器。`RegisterExecutor` 会替换同 key 的
当前执行器。插件生命周期需要拒绝过期替换时，可使用带 generation 校验的注册方法。

认证记录带有 `compat_name` 时，直接使用 SDK 注册的 `provider_key` 经首尾空白清理和小写归一后作为执行器 key，缺少该值时使用
`compat_name`，请求执行、请求期刷新和自动刷新使用同一个 key

| 字段                         | 直接使用 SDK 注册                                       | Service 配置发布                                                                 |
|------------------------------|---------------------------------------------------------|----------------------------------------------------------------------------------|
| `Auth.Provider`              | 保留逻辑 Provider，包括插件 Provider                    | 配置凭据使用带命名空间的 key，插件凭据保留逻辑 Provider                          |
| `Attributes["provider_key"]` | 与已注册的 `Identifier()` 一致，保留显式 raw 或前缀 key | 经 `OpenAICompatibleProviderKey` 归一，例如 custom 变为 openai-compatible-custom |
| `Attributes["compat_name"]`  | provider_key 为空时作为 key                             | 保留原渠道名称                                                                   |

迁移上游 provider_key=custom、执行器名为 openai-compatible-custom 的记录时，使用 Service 发布，或将 SDK 记录的 provider_key
设为 openai-compatible-custom；直接注册同样支持 custom／custom 的 raw key 组合，凭据 ID、base URL 和 token metadata 保持原有含义

Service 发布原生凭据时，将 kimi.com 归为 kimi，将 kimi.ai 归为 kimi-ai，两站使用各自的执行器和凭据；使用这些站点名称的兼容配置渠道单独采用
openai-compatible- key

`Builder.WithWatcherFactory` 沿用 `WatcherFactory` 的 `reload func(*config.Config)` 回调类型，`Build`
保留当时选定的工厂，内置监听器通过接受结果和文件版本确认控制发布，更新被拒绝时保留此前配置和文件确认状态

## 凭证身份与路由

会话亲和使用 `routing.session-affinity`，旧配置中的 `routing.claude-code-session-affinity` 在读取时迁移为新设置；新键已存在时使用新值，包括显式
`false`，读取不会改写配置文件，下次成功保存仅在有效的 routing 配置中保留新键，并保留其他字段与注释

程序化配置使用 `cfg.Routing.SessionAffinity`，旧字段 `ClaudeCodeSessionAffinity` 已退役；配置文件、管理 YAML
保存和可视化编辑共用这项迁移规则，迁移日志会提示保存配置

Gemini、Claude、Codex 和 OpenAI 兼容凭证支持通过旧 API key 索引查询，响应继续返回当前区分来源的索引，多个凭证共用旧索引时，使用当前索引选择凭证；认证文件请求还可以在
`name` 中指定准确的凭证 ID，仅提供有歧义的旧索引时，修改请求返回错误

`Auth.MatchesIndex` 检查当前索引和受支持的旧引用，检查时保持索引不变，显式索引和 seed 保留选定的身份，并发索引查询和近期请求快照保留身份及计数，Clone
后的请求历史保持独立

外部调用方可直接为 `Auth.Index` 赋值，或使用 `AssignIndex` 设置索引，`EnsureIndex`、注册、Clone 和管理查询保留该索引，用量导出与恢复保留原
`auth_index` 引用

`Auth.AuthKind()` 优先使用 `Attributes["auth_kind"]`，再读取 `Metadata["auth_kind"]`，最后兼容旧字段推断；`oauth` 凭证可以保留历史
`api_key`，Codex 使用当前 `access_token`、OAuth 账户头及 OAuth 模型规则，模型路由、请求规则和自动刷新使用该身份，email 保持展示用途

API Key 凭证沿用原密钥字段及模型配置，显式 OAuth 缺少 `access_token` 时发送的认证头保持为空；调用方使用原有刷新或错误处理路径恢复凭证，历史
`api_key` 保留在记录中

使用 Kimi 的 Claude 兼容入口时，上游请求将 `kimi-k3` 等模型名规范为 `k3`，普通与流式响应返回调用方请求的模型名；用量记录中的
`ResponseModel` 保留上游实际返回的模型

Claude 清理不兼容的历史签名时，debug 日志记录请求 ID、目标模型、删除或替换数量，以及首个处理原因

`claude-header-defaults.stabilize-device-profile` 为 `false` 或未配置时，请求提供的 OS／架构保持原值，缺省值取代理的运行环境，调用方身份、CLI
profile 和 OAuth 请求使用相同的平台规则
设为 `true` 时，已选定的 CLI／OAuth profile 使用配置中的平台基线
该设置保留调用方的 User-Agent 和认证方式，CLI 身份仍由原有 profile 设置选择

Antigravity 的 Claude 请求在签名检查前，从当前请求所属缓存恢复缺失签名，恢复后的签名继续按目标 Provider
验证，普通、流式和计数入口采用相同步骤，清理不兼容的 thinking 块时，debug 日志记录请求 ID、目标模型、处理阶段、原因及清理前后的数量，日志省略正文和签名原值

携带 `config_index` 的凭据按索引绑定配置，索引无效或与当前凭据身份不匹配时，保持模型配置未绑定；无索引的原生 API Key 凭据按密钥和
base URL 匹配，相同身份的多个条目优先选择与 `Auth.Prefix`、`Auth.ProxyURL` 均一致的条目，其次使用首个凭据匹配项，再使用原有的仅密钥匹配；OpenAI
兼容通道在无索引时按 Provider 名称匹配，Home 使用当次选定凭据的选项

请求规则优先读取凭据 metadata，显式 OAuth 凭据使用对应 Provider 的 OAuth 规则；API Key 凭据可仅凭 `config_index`
绑定规则，已提供的密钥或 base URL 继续参与身份校验，旧 openai-compatible-custom 和 openai-compatibility:custom 身份均可解析
custom 渠道的规则

`stop-and-cooldown` 和 `continue-and-cooldown` 保留实际模型或凭据范围，瞬时错误的显式冷却在
`transient-error-cooldown-seconds` 为负数时仍保留上游正值 `Retry-After`，缺少等待提示时使用配置的正值间隔或默认 60
s；普通瞬时冷却在负数配置下继续关闭，已有更晚期限保持

TPM 限流未提供有效 `Retry-After` 时保留 60 s 等待，有效响应头优先，包括 HTTP 503 的等待提示，原状态码和正文保持

模型冷却错误保留请求模型，候选的原上游原因通过错误链读取；模型原因优先于凭据级原因，同类原因采用最新记录；有有效等待期限时，
`SafeResponseHeaders(err)` 返回本地计算的 `Retry-After`，错误文本使用安全诊断

模型配置的 `force-mapping: true` 让普通与流式响应中的模型字段使用配置的 `alias`，发往上游的 `name` 保留请求中的思考后缀和
`[1m]` 装饰；设为 `false` 时，响应模型字段保持原值

同一 API Key 凭据的多个 `name` 可以共用一个 `alias`，本地请求轮换可用目标，受限目标跳过执行；
明确的模型拒绝或配额拒绝允许按现有重试权限选择下一个目标，结果未知的失败和空流保持终止
每次执行使用实际目标的响应映射、能力和状态，鉴权后改模同样检查最终目标，成功计数保留当前可用性，模型拒绝按实际目标记录；Home
使用远端选定的目标

OAuth 别名按所选凭据解析实际 target，选择、能力、冷却和执行结果使用该 target，Executor 的 context 保留原请求 alias；直接上报结果时，
`Result.Model` 填实际 target，`Result.AuthGeneration` 使用所选快照的 `Generation`

管理器使用 `SessionAffinitySelector` 时，显式会话和 LCP 按实际 target 检查绑定凭据，目标恢复后保持当前健康绑定，新会话按配置优先级选择；全部
target 冷却时返回等待错误，执行器保持未调用，直接调用 `SessionAffinitySelector.Pick` 保留最高可用优先级规则

模型目录重新登记后，调用 `RefreshSchedulerEntry(authID)` 发布当前模型状态，旧 registration epoch 或发布代际的投影被拒绝；别名移除或重新指向健康
target 后，按新目录选择，原模型的独立冷却和恢复权限继续保留，`ResetQuota` 被取消或认证存储保存失败时返回原错误，保留当前已发布状态

`coreauth.QuotaCooldownDisabledForAuthWithConfig(auth, cfg)` 读取统一冷却策略，凭据 override 的显式 `true` 或 `false` 优先于
Provider 和全局配置；Antigravity 的自动积分刷新、积分禁用提示和凭据调度使用相同策略

显式积分执行由 `QuotaExceeded.AntigravityCredits` 单独启用，关闭冷却时仍按该开关执行；普通与流式积分请求执行前，管理器准备所选凭据并保留原请求正文和
metadata，发送前的准备失败按已有许可尝试下一个凭据，取消、未知结果和 no_retry 保留原错误并终止；Home 模式使用自己的选择结果，Home
不可用时在本地执行前返回 HTTP 503

`Manager.SetAuthAffinity(key, authID)` 设置本地凭据绑定，`AuthAffinity(key)` 读取绑定，`ClearAuthAffinity(key)` 删除绑定；请求选项
`Metadata["auth_affinity_key"]` 接受字符串或字节数组，显式指定的凭据 ID 优先，本地执行成功或流建立后保存当前凭据的绑定，凭据删除或替换时清理对应绑定，Home
模式继续由 Home 选择凭据

`coreauth.NewSessionCache` 和 `coreauth.NewSessionCacheWithCapacity` 在 TTL 为零或负值时使用 30
min，正值保持传入时长，内置会话亲和选择器的默认时长保持 1 h

## 保存凭据与发布字段

常规凭据修改使用 `Register` 或 `Update`，已准备好的持久字段使用 `MergePersistentUpdate(ctx, expected, next)`
，保存成功后，返回值、Manager、scheduler 和 hook 使用同一已保存字段与 Manager 分配的发布代际；预期持久状态发生变化时，
`MergePersistentUpdate` 返回空记录，调用方读取最新快照后再决定是否提交，`Register` 或 `Update` 保存失败时返回原存储错误，并保留已发布凭据

`Register`、`Update`、`Load` 和 `RestoreCooldownStates`
将思考后缀的模型状态归并到基础模型名，优先保留较新状态代际，同代际时保留更新时间较新的记录；配额独立保留自己的代际、期限和重置权限，恢复后按归并结果发布模型目录与调度状态，仅含历史退避次数的模型条目保留且不产生冷却，禁用凭据继续禁选

发送请求前使用 `PrepareRequestAuth(ctx, executor, selected)` 调用执行器的 `RequestAuthPreparer`，Manager
将准备结果合并到当前记录并保留运行时限制；准备期间凭据已发生变化时，使用最新记录重新准备，`ForceRefreshAuth` 执行 Provider
的刷新交换并返回下文所述的安全摘要；等待期间取消时，返回原 context 错误并释放准备、刷新与凭据修改锁

Home 在普通、计数和流式请求中准备所选临时凭据，准备后的 metadata 与执行结果保持独立，同 ID
的本地凭据、调度快照、模型目录和持久记录保持原值；本地凭据准备成功后保存缺失 metadata，后续请求复用已保存结果，准备阶段返回
HTTP 403 时，在发送请求前应用凭据冷却

外部文件或数据库修改使用 `MutatePersistentAuth`，通过 `WithExpectedAuthGeneration(ctx, expected.Generation)`
绑定预期代际，callback 返回其已保存的凭据状态；代际不匹配时，在执行 callback 前返回 `ErrAuthGenerationConflict`，callback
返回错误时原样传播错误，已发布凭据字段保持不变

登录、导入或直接调用 Store 的 `Save`／`SaveWithBaseDir` 时，调用方持有尚未发布的 Storage，原 `SetMetadata`／`SaveTokenToFile`
实现继续有效，保存保留 Provider 自行生成的字段与原错误

Manager 保存已发布凭据时，Storage 实现提供 `CloneForPersistence`，副本独立持有 setter 和 saver
会修改的私有状态，闭包捕获的可变状态也由实现者复制；缺少复制能力时，Manager 在调用 Store 前返回明确错误，typed nil
副本及复用原可变对象的指针型副本在调用 setter 前拒绝

保存准备复制普通 metadata 容器，保留数值类型和扩展对象的原所有权；保存失败时，已发布 Storage 和 metadata 保持原值，Provider
已签发的新 token 继续遵循下文的持久状态不确定规则，凭据内容合并保留新 token 的权威和并发用户设置，显式空 metadata map
删除未被并发修改的原字段，并保留新添加的字段

内置文件、Git、对象和 PostgreSQL Store 在保存锁内调用 `RecordPersistedAuthVersion(ctx, path)`，将实际文件版本交给
Manager，后续条件删除使用该版本；自定义 Store 可保留原 `Save` 合同，文件型实现也可使用此函数记录版本，文件已经替换后，权限或目录同步确认失败返回
`ErrAuthDurableStateUncertain` 并保留原错误，Provider 内部暂存文件的失败按原错误返回

Provider 已签发新 token 后保存失败时，返回 `ErrAuthDurableStateUncertain` 和原存储错误，Manager
保留已取得的凭据材料并重试保存；当前刷新仍有活动的未保存结果时，`Load` 返回对应的持久化状态错误

### 手动刷新的返回字段

`Manager.ForceRefreshAuth(ctx, id)` 返回 `(ForceRefreshResult, error)`，摘要包含下表的五个公开字段；刷新失败时，返回可取得的身份摘要和原错误，nil
context 使用 `context.Background()`；完整 metadata 和模型状态通过 `GetByID` 或 `List` 读取，编辑这些独立快照时，管理器中的状态保持原值

Provider 的刷新交换在凭据修改锁之外执行，结果保存和发布与 `Update` 串行提交；交换期间凭据已被替换时，刷新返回安全摘要和过期结果错误，仅修改用户设置时，合法刷新合并新
token 并保留用户字段

同一凭据身份下，刷新仍返回已拒绝的 access token 时继续禁选，仅更新到期时间或 refresh token 时保持该限制；有效 access token
轮换后清除认证失败，活动配额保留自己的期限和重置权限

恢复依据已记录的错误分类和权限，只含 `StatusError` 或未归类文字错误的旧记录在 token 刷新后保留原错误状态；已归类的鉴权失败可恢复，Provider
故障和模型配额继续使用各自的期限与恢复规则；模型范围的成功保持凭据限制，凭据范围的成功按观察到的状态代际和重置权限恢复

管理端的单项和批量 HTTP 刷新在成功、失败时均使用相同的五字段摘要

| HTTP 字段      | SDK 数据来源                          | 摘要读取规则                             |
|----------------|---------------------------------------|------------------------------------------|
| `filename`     | `Auth.FileName`，为空时使用 `Auth.ID` | 仅保留文件名                             |
| `provider`     | `Auth.Provider`                       | 清理首尾空白                             |
| `auth_index`   | `Auth.Index`                          | 缺失时在独立副本中计算索引               |
| `status`       | `Auth.Status` 和当前拒绝状态          | 保留 disabled，已拒绝的 token 显示 error |
| `refreshed_at` | `Auth.LastRefreshedAt`                | 保留刷新完成时间                         |

批量刷新通过 `POST /v0/management/auth-files/refresh` 提交 `{"all":true}`，按刷新材料或运行时能力选择已启用的凭据，逐项返回摘要与结果；
`auth-auto-refresh-workers` 限定并发 worker 数量，缺省、零值和负值使用 16

请求取消后，排队项返回 `credential_refresh_cancelled` 并保留原凭据；刷新交换已返回新材料时，使用独立的有界提交 context
完成保存，保留请求的 context 值并发布成功结果，独立配额继续保留各自的重置权限

### 自动刷新生命周期

`Manager.StopAutoRefresh(ctx)` 保持已发布的 `func(context.Context) error` 签名，传 nil 时默认等待 5 s，传入 context
时使用其取消信号和期限；期限到达时 Provider 仍在执行，返回原 context 错误和 `ErrAuthDurableStateUncertain`

重启最多等待旧生命周期 5 s，记录仍在执行的 Provider 后为新生命周期建立独立 worker，旧结果保持与替换凭据隔离；禁用凭据停止后台
token 刷新，取消排队作业时释放其 pending 标记并保留 Provider 冷却

## 读取冷却状态与切换存储

凭证限制与模型限制分别保留自己的期限和退避级别，同一作用范围内的后续失败保留较长的有效期限；模型配额在凭证鉴权失败之前或之后到达时，均保持模型范围，调度与客户端模型展示采用相同的有效限制

`CooldownSnapshotForAuth` 列出有有效截止时间的本地等待，剩余秒数向上取整，返回安全分类并保持凭据原值；无截止时间的权威限制继续阻止选择，按其代际与恢复权限解除，结果未知的普通
HTTP 500 结束本次请求，并保留当前凭据状态

新建配额冷却时，`NextRetryAfter` 与 `Quota.NextRecoverAt` 使用同一截止时间，并去除 Go 的单调时钟读数，其他独立限制继续保留自己的期限

访问 token 被拒绝后，在等待刷新、刷新失败和刷新返回同一 token 时继续禁选，仅修改 metadata 也保留该拒绝状态；不同的访问 token
可恢复认证，独立模型配额继续保留原期限，取消刷新时释放该作业的 pending 状态，Provider 冷却保持原值；管理器使用私有指纹维护拒绝状态，拒绝发布更新
`UpdatedAt`，`Generation` 继续标识凭据版本；`GetByID` 和 `List` 在 token 已拒绝时返回 `Unavailable=true` 及 error
状态，刷新摘要返回对应状态，已有 disabled 状态保持，metadata 更新继续保留拒绝视图，更换 token 后清除原拒绝视图

### 凭据版本与迟到结果

从 `Register`、`GetByID`、`List` 或 `Update` 取得所选凭据快照，`Load` 为存储中的凭据分配非零的运行时 `Generation`
，请求保留该值，版本值由 Manager 分配

| 上游字段                                               | 当前公开输入                                  | 当前含义                                                                         |
|--------------------------------------------------------|-----------------------------------------------|----------------------------------------------------------------------------------|
| `Auth.CredentialVersion`                               | `Auth.Generation`                             | 标识已发布快照的版本，修改标签同样发布新版本                                     |
| `Auth.RegistrationEpoch`                               | 重新登记返回的 `Auth.Generation`              | 删除后重新登记时使用新快照，旧登记的结果保持隔离                                 |
| `Result.CredentialVersion`、`Result.RegistrationEpoch` | `Result.AuthGeneration = selected.Generation` | 直接调用 `MarkResult` 时绑定所选发布快照，同时保留观察到的状态代际和模型配额代际 |

Manager 执行的请求保留每次尝试所属的凭据身份，仅修改标签时继续接受原在途请求的有效 429；替换 token 或 API key
后，旧材料的失败保持隔离，替换凭据会清理原会话绑定，使用当前凭据的请求可建立新绑定，迟到结果保留该新绑定；原请求仍返回其 HTTP
错误，替换后的凭据状态保持原值

凭证级配额结果将原 `CredentialScope` 标记迁移为以下 disposition 字段：

| 字段                       | 取值                         | 含义                         |
|----------------------------|------------------------------|------------------------------|
| `DispatchPhase`            | `response_started`           | 上游已经开始响应             |
| `Outcome`                  | `rejected_before_processing` | 上游在处理前拒绝请求         |
| `RetryDirective`           | `retry_after`                | 按等待提示和现有预算决定重试 |
| `EffectScope`、`EffectKey` | `credential`、凭证 ID        | 该凭证的全部模型受限         |

`AuthGeneration` 标识选中凭证的版本，`StateGeneration` 标识该结果作用范围内观察到的状态代际，模型结果同时携带当时的
`ModelQuotaEpoch`；凭证或状态更新后，旧结果保持隔离，凭证配额的 `Quota.Reason` 使用 quota，凭证范围通过 `StateEffectScope`
判断；普通 401 使用凭证范围，429 使用额度族范围，结果未知的 500 保持 Provider 范围和 no_retry；默认模型范围的 404
在目标为空时保持凭证可用性，`invalid_grant` 可将有限等待转换为需要 reset 权限解除的无期限限制

自定义 Executor 使用当次执行的 context 调用 `coreauth.RecordUpstreamResponseHeaders(ctx, headers)`
，上报被动配额响应头，每次尝试分别保存自己的观察；受支持的配额响应头最多保留 64 项，每个值最多 512
字节，请求日志关闭时配额观察继续生效，计数请求保持已有 `Signals` 与 `ObservedAt`，成功计数及独立计数接口缺失保留可用性，其他失败按既有范围记录；CDS
保存过滤后的 `Signals` 和 `ObservedAt`，恢复时保留时间相同或更新的运行时观察

`QuotaCooldownDisabledForAuth(auth)` 读取凭证 override 和进程设置，带配置的调用继续使用
`QuotaCooldownDisabledForAuthWithConfig(auth, cfg)`，凭证 override 优先于 Provider 和全局配置

`HasUnauthorizedAuthFailure(auth)` 查询处于 StatusError、Unavailable 为 true、两个等待时间均为空的 401 或 unauthorized 错误，
`HasDisabledInvalidGrantFailure(auth)` 查询已禁用凭证的 invalid_grant 正文或代码；两者保持输入记录，`Unavailable`
没有期限时继续按当前恢复权限阻止选择

`Manager.SetConfigSnapshot(cfg)` 发布独立配置副本并更新运行状态，返回 true 表示清除了冷却，需要调用
`PersistCooldownStates(ctx)` 保存，配置复制失败时记录错误并保持当前配置；此入口在返回前完成内存更新，保存由后续调用执行；
`SetConfig` 和组合配置更新同样发布独立副本，调用方后续编辑保留已发布状态

`Manager.SwapCooldownStateStore(ctx, store, persistOld)` 在旧状态保存完成后发布替换存储，保存失败或取消时返回
false，并保留当前存储及待保存状态；使用有效 context 再次调用时会继续保存原待保存状态，无待保存状态时，`persistOld=false`
直接更换存储，nil context 使用 `context.Background()`

配置和冷却存储需要一起更新时，使用 `ApplyConfigWithCooldownStateStore(ctx, cfg, store)`
，旧存储保存失败时保持当前配置，旧快照保存成功后发生取消时继续完成对应配置和存储发布

## 批量更新凭证别名

使用 `coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)` 保留现有批量调用方式，并在所有工作完成后调用
`Manager.RefreshAPIKeyModelAlias()` 一次：

```go
batchCtx := coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)
defer core.RefreshAPIKeyModelAlias()
for _, next := range auths {
    if _, err := core.Update(batchCtx, next); err != nil {
        return err
    }
}
```

| 输入或入口                                                            | 保留的含义                                                                       | 发布行为                                    |
|-----------------------------------------------------------------------|----------------------------------------------------------------------------------|---------------------------------------------|
| `ctx`                                                                 | 保留取消、deadline 和已有 context 值，nil 父 context 使用 `context.Background()` | 派生 context 内的凭证修改延迟别名表重建     |
| `Auth.ID`、`Attributes`、`Metadata`                                   | 沿用原凭证身份、配置选择和字段含义                                               | 每次成功修改仍提交凭证快照和 scheduler 状态 |
| `Generation`                                                          | 沿用返回快照的代际，typed mutation 和条件删除继续验证预期代际                    | 过期修改保持拒绝状态                        |
| `Register`、`Update`、`Remove`                                        | 保留原入口、返回值和错误传播                                                     | 同时支持延迟发布                            |
| `MutatePersistentAuth`、`MergePersistentUpdate`、`RemoveIfGeneration` | 保留现有持久化和条件提交合同                                                     | 使用相同的延迟标记                          |
| `RefreshAPIKeyModelAlias()`                                           | 使用当前已提交凭证及最新配置                                                     | 原子替换完整别名表，已有读取快照保持独立    |

并发批次在所有 worker 完成后执行最后一次重建，批次中途失败或取消时仍发布已经提交的修改，并返回或记录原有错误；这不会把整个批次改为事务回滚

持久化策略保持独立，常规更新继续保存，watcher 已保存的数据按既有合同使用 `WithSkipPersist`；普通 context 的修改和
`SetConfig` 仍即时重建别名表

## 读取旧 Provider 密钥配置

旧配置中的上游密钥列表通过现行 YAML 解码和配置文件读取入口加载：

| 旧键                                                                                                     | 读取结果                                            | 保存方式                                                 |
|----------------------------------------------------------------------------------------------------------|-----------------------------------------------------|----------------------------------------------------------|
| `generative-language-api-key`                                                                            | 字符串列表转换为 `gemini-api-key` 的 `api-key` 字段 | 显式保存时使用现行字段并移除旧键                         |
| `openai-compatibility[].api-keys`                                                                        | 字符串列表转换为同一 Provider 的 `api-key-entries`  | 保留该 Provider 的地址、模型和请求头，显式保存时移除旧键 |
| `amp-upstream-url`、`amp-upstream-api-key`、`amp-restrict-management-to-localhost`、`amp-model-mappings` | 返回包含退役字段名的迁移诊断                        | 移除退役字段后再保存                                     |

现行字段存在时优先，包括显式的 `[]` 和 `null`；旧密钥列表去除首尾空白、空项和重复项，合法 YAML alias 与 merge 继续生效

兼容读取仅构造现行 Provider 字段，显式保存时才替换旧键；旧键类型不合法时，错误指出对应字段并省略密钥值，管理校验和文件读取采用相同规则

## 无活动插件时保留凭据轮转

将插件调度器交给 `Manager.SetPluginScheduler` 后，Manager 在普通和混合 Provider 选择中读取可选的 `HasScheduler()`，返回
false 时直接使用内建选择器，保持原凭据轮转

| 入口或字段                              | 调用条件         | 行为                                                                                                                                            |
|-----------------------------------------|------------------|-------------------------------------------------------------------------------------------------------------------------------------------------|
| `PluginScheduler.PickAuth(ctx, req)`    | 调度器的必需入口 | 沿用 `SchedulerPickRequest`、`SchedulerPickResponse` 和错误返回，`Provider`、`Providers`、`Model`、`Stream`、`Options`、`Candidates` 保留原含义 |
| `HasScheduler() bool`                   | 调度器可选实现   | false 时跳过 `PickAuth`，true 时继续候选过滤和调度；仅实现 `PickAuth` 的旧调度器继续参与选择                                                    |
| `SchedulerWantsAcrossPriorities() bool` | 调度器可选实现   | true 时允许跨优先级选择，默认使用最高可用优先级，冷却和禁用条件继续生效                                                                         |

Host 的能力查询使用当前插件快照，重配、停用或卸载后，旧快照停止发起新的调度调用；请求取消后，仍在执行的 native
调用保留动态库使用权，返回后再完成卸载

## 按认证记录提供 transport

`Manager.SetRoundTripperProvider` 可根据认证记录返回 `http.RoundTripper`：

```text
type roundTripperProvider struct{}

func (roundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
    return transportFor(auth.ProxyURL)
}
```

执行器应遵守宿主提供的 transport 策略，不要修改 `http.DefaultTransport`。原始请求路径可调用
`Manager.InjectCredentials(request, authID)`，它会调用所选执行器的可选 `RequestPreparer`。

Antigravity 默认使用短连接，显式将 `antigravity.connection-pool.enabled` 设为 `true` 后复用连接，开启后默认空闲超时为 30
s，每个目标地址保留 2 条空闲连接，两项上限分别为 210 s 和 100 条空闲连接；连接池按凭据和代理隔离，池配置变化时清理缓存并关闭旧空闲连接，后续请求可重新创建连接池

## 注册翻译器

`sdk/translator.Register` 保存从入站格式到 Provider 格式的请求转换，以及反向的响应转换。请求转换返回 `[]byte`，
流式响应转换返回 `[][]byte`，非流式响应转换返回 `[]byte`。

```text
var (
    formatOpenAI = sdktr.Format("openai.chat")
    formatMyProv = sdktr.Format("myprov.chat")
)

sdktr.Register(
    formatOpenAI,
    formatMyProv,
    func(model string, raw []byte, stream bool) []byte {
        return convertOpenAIToMyProv(model, raw, stream)
    },
    sdktr.ResponseTransform{
        Stream: func(
            ctx context.Context,
            model string,
            originalRequest, translatedRequest, raw []byte,
            param *any,
        ) [][]byte {
            return convertStreamMyProvToOpenAI(
                model, originalRequest, translatedRequest, raw,
            )
        },
        NonStream: func(
            ctx context.Context,
            model string,
            originalRequest, translatedRequest, raw []byte,
            param *any,
        ) []byte {
            return convertMyProvToOpenAI(
                model, originalRequest, translatedRequest, raw,
            )
        },
    },
)
```

注册方向决定查找结果，请求按 `from → to` 查找；响应按 Provider 到客户端的反向路径查找，没有匹配转换时，注册表会
保留原始负载，并继续应用已注册的插件钩子

v6 的 `HasRequestTransformerByFormatName`、`HasResponseTransformerByFormatName`、
`HasStreamResponseTransformerByFormatName` 和 `HasNonStreamResponseTransformerByFormatName` 按原注册方向查询能力，
仅注册 Token 计数转换时，通用响应能力查询返回 true，流式和非流式响应能力查询返回 false

插件响应转换器优先处理响应，插件不处理时才调用原生转换器；原生转换器返回空结果时，该输入已被消费，原始负载不会继续转发，
注册表在调用转换器前释放查询锁，转换器可在回调中注册其他转换

使用 `TranslateRequestChecked` 或检查 `RequestEnvelope.Err` 处理被拒绝的输入，Chat→Claude 按原始调用 ID 匹配前一组助手调用，匹配后再规范化
ID；结果缺少 ID 或找不到调用时返回请求级 HTTP 400 和空正文，相邻助手消息及中间的用户提醒保留合法配对，byte 入口对被拒绝的输入也返回空正文

Responses→Gemini 保留无签名文本和 reasoning，省略其 `thoughtSignature`，省略没有内容的无签名 part；有效且兼容当前 Provider
的签名保留原值，函数调用沿用独立的签名兼容规则，媒体数据与显式 MIME 在顶层 input、消息内容和工具结果中保留，不支持的 file
ID、非法或空附件返回 checked 错误

工具结果字符串包含受支持媒体块的 JSON 数组或对象时，媒体随对应工具结果写入 `functionResponse.parts`；其他字符串保留原值，包括普通
JSON 文本、字面 `"null"` 和空字符串

Devin 原生 Interactions 的普通和流式响应保留自定义工具参数的原始文本，Responses
请求沿用已声明的自定义工具及现有参数转换，转换后的流在参数完整时发送自定义输入；转换时缺少匹配声明的调用返回错误，并保留已经观测到的失败用量

Devin 将 0～1 范围内的数值 `top_p` 写入 Connect 请求，显式 0 保留原值；请求显式包含 `presence_penalty`、`frequency_penalty`
或 `stop_sequences` 时，在发送上游请求前返回请求级 HTTP 400

Responses 来源的 Vertex 请求在最终正文中省略 `functionCall.id` 和 `functionResponse.id`，配置规则引入的这两个字段也按相同规则处理，参数与结果对象内部的
ID 保留原值

## 注册模型

按认证 ID 注册模型后，它们会出现在 `/v1/models` 和模型查询中：

```text
models := []*cliproxy.ModelInfo{
    {
        ID:          "myprov-pro-1",
        Object:      "model",
        Type:        "myprov",
        DisplayName: "MyProv Pro 1",
    },
}
cliproxy.GlobalModelRegistry().RegisterClient(authID, "myprov", models)
```

服务会自动注册内建 Provider。自定义执行器应在认证记录可用后发布模型，并在 Provider 模型列表变化时替换该注册。

## 验证

执行器测试应使用 `httptest.Server` 或假的 `http.RoundTripper`，避免依赖真实上游。覆盖非流式响应、流终态、上下文
取消、响应 body 所有权、凭证注入、token count 错误、翻译方向和模型替换。发布检查会编译仓库中的
`examples/custom-provider` package。
