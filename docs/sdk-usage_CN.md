# CLI Proxy SDK 使用指南

`sdk/cliproxy` 包用于在其它 Go 进程中内嵌 CLIProxyAPI。宿主程序无需调用 CLI 入口，即可使用服务生命周期、配置、认证、
路由、协议转换、插件和热更新能力。

公共 SDK 要求 Go 1.27.2，并采用 `/v6` 模块路径

## 安装

```bash
go get github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy
```

## 内嵌服务

下面是完整程序。它通过公共配置包加载配置，并在进程收到退出信号前持续运行：

```go
package main

import (
    "context"
    "errors"
    "os"
    "os/signal"
    "path/filepath"
    "syscall"

    "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy"
    "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func main() {
    configPath, err := filepath.Abs("config.yaml")
    if err != nil {
        panic(err)
    }
    cfg, err := config.LoadConfig(configPath)
    if err != nil {
        panic(err)
    }

    service, err := cliproxy.NewBuilder().
        WithConfig(cfg).
        WithConfigPath(configPath).
        Build()
    if err != nil {
        panic(err)
    }

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    if errRun := service.Run(ctx); errRun != nil && !errors.Is(errRun, context.Canceled) {
        panic(errRun)
    }
}
```

`Service.Run` 管理配置与认证文件监听、后台凭证刷新和关闭流程。取消父上下文即可停止服务。`Run` 已经调用
`Shutdown`；只有宿主程序自行编排关闭顺序时，才需要直接调用 `Service.Shutdown`。

## 自定义 HTTP 服务器

`Builder.WithServerOptions` 接收 `sdk/api` 提供的选项：

```text
api.WithMiddleware(...gin.HandlerFunc)
api.WithEngineConfigurator(func(*gin.Engine))
api.WithRouterConfigurator(func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config))
api.WithRequestLoggerFactory(func(*config.Config, string) logging.RequestLogger)
api.WithPostAuthHook(coreauth.PostAuthHook)
```

回调签名应使用 `sdk/api/handlers`、`sdk/config` 和 `sdk/logging`。外部模块不能导入对应的 `internal`
包。`examples/custom-provider/main.go` 提供了可以构建的服务器选项示例。

## 提供核心认证管理器

默认 `Service` 会创建认证管理器，并注册内建 Provider 执行器。需要自定义选择策略、传输或执行器时，可通过
`WithCoreAuthManager` 传入管理器。

下面是集成片段，其中 `executor` 必须满足完整的 `coreauth.ProviderExecutor` 接口：

```text
tokenStore := sdkauth.GetTokenStore()
if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
    setter.SetBaseDir(cfg.AuthDir)
}
core := coreauth.NewManager(tokenStore, nil, nil)
core.SetRoundTripperProvider(roundTripperProvider)
core.RegisterExecutor(executor)

service, err := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath(configPath).
    WithCoreAuthManager(core).
    Build()
```

`ProviderExecutor` 必须实现 `Identifier`、`Execute`、`ExecuteStream`、`Refresh`、`CountTokens` 和
`HttpRequest`。`RequestPreparer` 是可选的凭证注入接口。当前签名见 `sdk-advanced_CN.md` 和
`examples/custom-provider/main.go`。

编程式执行使用已经转换为执行器格式的请求：

```text
response, err := core.Execute(ctx, []string{"gemini"}, request, options)
stream, err := core.ExecuteStream(ctx, []string{"gemini"}, request, options)
if err == nil {
    for chunk := range stream.Chunks {
        // 处理 chunk.Payload、chunk.Err 和 chunk.Terminal
    }
}
```

单独创建的管理器不会自动获得内建执行器。通过 `Service` 运行时，服务会完成内建 Provider 的注册。

### 请求与响应格式

`executor.Options.SourceFormat` 指定请求格式，`ResponseFormat` 指定响应格式，未设置 `ResponseFormat` 时使用 `SourceFormat`，非流式、流式和计数响应遵循相同规则

Vertex 原生 Interactions 要求 `SourceFormat` 为 `translator.FormatInteractions`，并在选中凭证中启用以下任一标志：`Metadata["native_interactions"]` 或 `Metadata["interactions"]` 为布尔值 `true`；`Attributes["interactions"]` 为 `"true"` 或 `"1"`，字符串属性忽略大小写和首尾空白，metadata 字符串 `"true"` 不启用该功能

启用后，API key 使用 `X-Goog-Api-Key`，服务账号使用 Bearer token，有 project 的请求使用 global Interactions endpoint，`base_url` 属性可指定接口地址；未启用的凭证、Chat 请求和 `CountTokens` 保留原路径

此选项用于认证管理器的编程式执行，不新增公共 HTTP endpoint

### Codex 客户端工具能力

模型目录默认不声明 freeform `apply_patch`，显式启用以下配置后，仅支持文本且全部路由执行器支持该工具的模型声明能力，图像、视频和 audio-only 模型不声明

```yaml
client:
  codex:
    enable-apply-patch: true
```

执行器通过可选的 `auth.ApplyPatchSupport` 接口提供 `SupportsApplyPatch() bool`；实现了 `APIKeyConfigExecutor` 时，`ForAPIKey()` 返回的视图也必须支持，未提供合同、缺失执行器或任一候选不支持时均不声明，不按 Provider 名称推断

原生 Codex 保留 freeform 工具协议，xAI 将 custom `apply_patch` 声明和历史转换为只有 string `input` 字段的函数参数，向 Responses／Codex 客户端还原 freeform 调用；Chat／Messages 响应使用函数参数形式，普通同名 function 不参与转换，服务端不执行 patch

Claude、Gemini、Gemini Interactions、GeminiCLI、Vertex、Antigravity、AI Studio、OpenAI-compatible、Qwen、iFlow 和 Devin 使用内建转换器，将 Responses 请求的声明、tool choice 和 custom 调用历史转换为函数形式；返回 Responses 时恢复 freeform `input`、namespace 和客户端声明，保留普通同名 function

SDK 响应调用须复用请求的 `param`，调用后读取 `translator.ResponseTranslationError(&param)`；无效参数通过错误返回或 `StreamChunk.Err` 交付，必需的转换回调和执行器接口不变；JSON 终态后释放请求正文与转换缓冲区，插件 hooks 仍收到客户端原始请求；插件自行接管 `TranslateResponse` 时承担其转换合同

OpenAI-compatible、Qwen 和 iFlow 的 freeform 流须观察到源协议的 `finish_reason`，干净 EOF 可结束已完成的响应；提前 DONE 和未完成的 EOF 返回失败，读取失败保留已观察用量，取消不生成完成事件

xAI 的 `apply_patch` 转换支持晚到 name／item ID／call ID、增量参数、稀疏终态和 namespace dispatcher，转换事件保留响应标识及附加元数据，完整终态保留源顺序，稀疏终态按真实身份与已观察到的 output_index 恢复工具、消息和 reasoning，终态快照优先于较早快照

xAI 的 freeform `apply_patch` 请求将 `response.done` 统一为 `response.completed`，响应完成后释放工具身份和解码状态，接受一个可选的 `[DONE]`，不重复交付后续进度，提前 `[DONE]`、缺失响应终态、读取失败、取消或下游交付失败不会提交新的 replay，完成后的上游错误仍返回失败，启用交付确认的 HTTP 流在 EOF 后等待下游确认再提交 replay，WebSocket 在 JSON 终态结束当前请求并复用连接

内建转换与模型目录开关由离线合同测试覆盖，真实账号请求未验证；Kimi 的原生 Responses 路径不继承 Claude 的能力声明，配置默认保持关闭

## 替换凭证来源

凭证不来自默认存储时，实现 `cliproxy.TokenClientProvider` 或 `cliproxy.APIKeyClientProvider`：

```text
type memoryTokenProvider struct{}

func (*memoryTokenProvider) Load(
    ctx context.Context,
    cfg *config.Config,
) (*cliproxy.TokenClientResult, error) {
    return &cliproxy.TokenClientResult{}, nil
}

service, err := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath(configPath).
    WithTokenClientProvider(&memoryTokenProvider{}).
    WithAPIKeyClientProvider(cliproxy.NewAPIKeyClientProvider()).
    Build()
```

## 观察生命周期事件

`cliproxy.Hooks` 提供 `OnBeforeStart` 和 `OnAfterStart`：

```text
hooks := cliproxy.Hooks{
    OnBeforeStart: func(cfg *config.Config) {
        log.Printf("starting on port %d", cfg.Port)
    },
    OnAfterStart: func(service *cliproxy.Service) {
        log.Print("service started")
    },
}
```

在 `Build` 前通过 `Builder.WithHooks` 传入这些钩子。

## 请求认证与管理 API

`Builder.WithRequestAccessManager` 接收 `sdk/access.Manager`。配置变化时，builder 会刷新内建的配置型访问
Provider，并更新管理器的 Provider 快照。

只有配置了 `remote-management.secret-key`，服务才会在 `/v0/management` 下挂载管理路由。非回环地址访问还要求
`remote-management.allow-remote: true`。

### 插件 OAuth

`GET /v0/management/plugins` 的 `oauth_provider` 来自 `AuthProvider.Identifier()`，与登录路由使用同一身份，空值仍返回该字段，不用插件二进制 ID 推断其他 Provider

管理页面仅为已生效且声明有效 Provider 的插件提供登录入口，登录、轮询、手动回调和取消使用该 Provider，状态按当前认证连接隔离；内建 Provider 的 Gemini alias、xAI code 处理和 Devin 校验不用于插件登录

### 嵌套模型流

`host.model.execute_stream` 创建的流仅供发起插件及其加载实例读取或关闭，回调结束、父上下文取消、加载实例关闭、EOF 或错误都会释放资源，结束后移除回调清理记录，已关闭实例不能登记晚到的流

卸载、热替换后的旧实例退出和宿主关闭也会取消该实例的模型上下文，释放阻塞的流读取；同名新实例的流不受影响

### 插件本地冷却恢复

插件可调用 `host.routing.reset_cooldown`，请求为 `{"auth_index":"凭证索引"}`，成功响应返回 `auth_index` 和已恢复路由的 `models`，请求、响应类型分别为 `sdk/pluginapi.HostRoutingResetCooldownRequest` 和 `HostRoutingResetCooldownResponse`

该操作清除本地配额冷却和临时重试限制，保留禁用状态与永久认证错误，不写入 token 文件，不恢复上游配额，也不消耗主动重置次数；无效索引、认证管理器不可用或已关闭实例返回错误

## 运行时行为

`config.yaml` 和认证目录发生变化后，watcher 会应用新状态。请求日志可通过管理 API 调整。`gemini-web.*`、Home、
插件、模型目录和用量设置都来自 CLI 服务器共用的公共 `sdk/config.Config`。
