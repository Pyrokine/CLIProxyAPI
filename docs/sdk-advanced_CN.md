# SDK 执行器与翻译器

本文说明如何为内嵌的 CLIProxyAPI 服务实现自定义 Provider 执行器、协议翻译器和模型注册。SDK 使用 Go 1.27 和
`/v6` 模块路径。

`examples/custom-provider/main.go` 是可构建的参考实现。下列片段只展示单个接口，省略外围 package 和错误处理代码。

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

执行器还可以实现 `coreauth.RequestPreparer`：

```text
func (Executor) PrepareRequest(request *http.Request, auth *coreauth.Auth) error
```

该方法用于向原始请求注入 Provider 凭证，不应修改共享 transport 或认证记录。

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

注册方向决定查找结果。请求按 `from → to` 查找；响应按 Provider 到客户端的反向路径查找。没有匹配转换时，注册表会
保留原始负载，并继续应用已注册的插件钩子。

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
