# CLI Proxy SDK guide

The `sdk/cliproxy` package embeds CLIProxyAPI in another Go process. It exposes the service lifecycle, configuration,
authentication, routing, translation, plugins, and hot reload without requiring the CLI entry point.

The public SDK requires Go 1.27.2 and uses the `/v6` module path.

## Install

```bash
go get github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy
```

## Embed the service

This complete program loads the public SDK configuration and runs the service until the process receives an interrupt:

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

`Service.Run` manages configuration and auth-file watching, background credential refresh, and shutdown. Cancel the parent
context to stop it. `Run` already invokes `Shutdown`; call `Service.Shutdown` directly only when the host owns a separate
shutdown sequence.

## Customize the HTTP server

`Builder.WithServerOptions` accepts options from `sdk/api`:

```text
api.WithMiddleware(...gin.HandlerFunc)
api.WithEngineConfigurator(func(*gin.Engine))
api.WithRouterConfigurator(func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config))
api.WithRequestLoggerFactory(func(*config.Config, string) logging.RequestLogger)
api.WithPostAuthHook(coreauth.PostAuthHook)
```

Use `sdk/api/handlers`, `sdk/config`, and `sdk/logging` in callback signatures. External modules must not import the
corresponding `internal` packages. `examples/custom-provider/main.go` contains a buildable server-options example.

## Supply a core auth manager

The default `Service` creates and wires its own auth manager and built-in provider executors. A host that needs custom
selection, transports, or executors can pass a manager through `WithCoreAuthManager`.

The following is an integration fragment; `executor` must implement the full `coreauth.ProviderExecutor` contract:

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

`ProviderExecutor` requires `Identifier`, `Execute`, `ExecuteStream`, `Refresh`, `CountTokens`, and `HttpRequest`.
`RequestPreparer` is an optional interface for credential injection. See `sdk-advanced.md` and
`examples/custom-provider/main.go` for the current signatures.

Programmatic execution uses translated executor requests:

```text
response, err := core.Execute(ctx, []string{"gemini"}, request, options)
stream, err := core.ExecuteStream(ctx, []string{"gemini"}, request, options)
if err == nil {
    for chunk := range stream.Chunks {
        // Handle chunk.Payload, chunk.Err, and chunk.Terminal.
    }
}
```

A stand-alone manager has no built-in executors unless the host registers them. Running `Service` performs that wiring
for the built-in providers.

## Replace credential sources

Implement `cliproxy.TokenClientProvider` or `cliproxy.APIKeyClientProvider` when credentials do not come from the default
stores:

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

## Observe lifecycle events

`cliproxy.Hooks` exposes `OnBeforeStart` and `OnAfterStart`:

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

Pass the hooks with `Builder.WithHooks` before `Build`.

## Request access and management API

`Builder.WithRequestAccessManager` accepts an `sdk/access.Manager`. The builder refreshes the built-in config-backed
access provider when configuration changes and updates the manager's provider snapshot.

Management routes are mounted under `/v0/management` only when `remote-management.secret-key` is configured. Requests
from non-loopback clients also require `remote-management.allow-remote: true`.

## Runtime behavior

Changes to `config.yaml` and the configured auth directory are applied by the watcher. Request logging can be changed
through the management API. `gemini-web.*`, Home, plugin, model-catalog, and usage settings are read from the same public
`sdk/config.Config` used by the CLI server.
