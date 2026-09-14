# CLI Proxy SDK 使用指南

`sdk/cliproxy` 包用于在其它 Go 进程中内嵌 CLIProxyAPI。宿主程序无需调用 CLI 入口，即可使用服务生命周期、配置、认证、
路由、协议转换、插件和热更新能力。

公共 SDK 使用 Go 1.27，并采用 `/v6` 模块路径。

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

## 运行时行为

`config.yaml` 和认证目录发生变化后，watcher 会应用新状态。请求日志可通过管理 API 调整。`gemini-web.*`、Home、
插件、模型目录和用量设置都来自 CLI 服务器共用的公共 `sdk/config.Config`。
