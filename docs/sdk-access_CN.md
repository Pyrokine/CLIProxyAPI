# `sdk/access` 开发指引

`github.com/Pyrokine/CLIProxyAPI/v6/sdk/access` 定义内嵌服务器的入站请求认证。`Manager` 按顺序调用已注册的
凭证 Provider，并返回 Provider 标识、认证主体和元数据。

## 安装

```bash
go get github.com/Pyrokine/CLIProxyAPI/v6/sdk/access
```

## Provider 注册表

`RegisterProvider(type, provider)` 用类型标识保存已初始化的 Provider。每个类型第一次注册时确定位置；使用相同类型
再次注册会替换实例，但不会改变顺序。`RegisteredProviders()` 按注册顺序返回当前实例。

管理器使用注册表快照：

```text
manager := sdkaccess.NewManager()
manager.SetProviders(sdkaccess.RegisteredProviders())
```

`SetProviders` 和 `Providers` 都会复制切片。管理器为 nil 或未配置 Provider 时，`Authenticate` 返回
`nil, nil`，表示关闭访问认证。

## 认证请求

```text
result, authErr := manager.Authenticate(ctx, request)
switch {
case authErr == nil:
    // result 包含 Provider、主体和 Provider 元数据
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials):
    // 请求没有提供可识别的凭证
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential):
    // 请求提供了可识别的凭证，但校验失败
default:
    // Provider 返回内部或传输错误
}
```

`Manager.Authenticate` 遇到第一次成功后立即返回。`AuthErrorCodeNotHandled` 会继续尝试下一个 Provider。
`AuthErrorCodeNoCredentials` 和 `AuthErrorCodeInvalidCredential` 会在遍历结束后汇总；没有 Provider 成功时，非法凭证
优先于缺少凭证。内部错误会立即返回。

## 内建 `config-api-key` Provider

`sdk/cliproxy.Builder` 根据 `Config.SDKConfig.APIKeys` 注册内建 `config-api-key` Provider，并把当前注册表快照
写入访问管理器。

Provider 接受以下请求头：

- `Authorization: Bearer <key>`
- `X-Goog-Api-Key: <key>`
- `X-Api-Key: <key>`

只有配置 `allow-query-auth: true` 时，才接受 `?key=` 和 `?auth_token=` 查询参数。认证成功后，
`Metadata["source"]` 会记录 `authorization`、`x-goog-api-key`、`x-api-key`、`query-key` 或
`query-auth-token`。

```yaml
api-keys:
  - sk-test-123
allow-query-auth: false
```

配置的 key 必须至少包含八个可打印 ASCII 字符。Provider 会用常量时间比较遍历所有已配置 key。

## 编写自定义 Provider

下面是完整的 Provider 包，它在初始化时完成注册：

```go
package partneraccess

import (
    "context"
    "net/http"

    sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
)

type provider struct{}

func (*provider) Identifier() string { return "partner-token" }

func (p *provider) Authenticate(
    _ context.Context,
    request *http.Request,
) (*sdkaccess.Result, *sdkaccess.AuthError) {
    token := request.Header.Get("X-Partner-Token")
    if token == "" {
        return nil, sdkaccess.NewNotHandledError()
    }
    if token != "expected" {
        return nil, sdkaccess.NewInvalidCredentialError()
    }
    return &sdkaccess.Result{
        Provider:  p.Identifier(),
        Principal: "service-user",
        Metadata:  map[string]string{"source": "x-partner-token"},
    }, nil
}

func init() {
    sdkaccess.RegisterProvider("partner-token", &provider{})
}
```

Provider 必须实现 `Identifier()` 和 `Authenticate(context.Context, *http.Request)`。`Metadata` 用于保存凭证
来源等审计上下文，不应写入凭证内容。

外部 Provider 模块可以通过空白导入触发注册：

```text
import _ "github.com/acme/xplatform/sdk/access/providers/partner"
```

必须在调用 `RegisteredProviders` 或 `cliproxy.Builder.Build` 前完成导入。

## 与 `sdk/cliproxy` 集成

宿主程序需要共享或检查访问管理器时，可传入同一个实例：

```text
accessManager := sdkaccess.NewManager()
service, err := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath(configPath).
    WithRequestAccessManager(accessManager).
    Build()
```

`Build` 会注册配置型 Provider 并初始化管理器快照。配置变化后，服务会再次执行该更新。自定义 Provider 在运行时
变化时，需要注册替换实例并刷新快照：

```text
sdkaccess.RegisterProvider("partner-token", replacement)
accessManager.SetProviders(sdkaccess.RegisteredProviders())
```

## 错误语义

- `NewNotHandledError()` 表示当前 Provider 不识别该请求
- `NewNoCredentialsError()` 表示 Provider 识别该凭证类型，但请求没有携带凭证
- `NewInvalidCredentialError()` 表示已识别的凭证校验失败
- `NewInternalAuthError(message, cause)` 表示必须终止认证流程的 Provider 故障

CLI HTTP 层会把缺少凭证和非法凭证映射为 HTTP 401，把内部认证错误映射为 HTTP 500。
