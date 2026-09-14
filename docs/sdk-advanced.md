# SDK executors and translators

This guide covers custom provider executors, protocol translators, and model registration for an embedded CLIProxyAPI
service. The SDK uses Go 1.27 and the `/v6` module path.

`examples/custom-provider/main.go` is the buildable reference implementation. The excerpts below focus on individual
contracts and omit surrounding package and error-handling code.

## Provider executor contract

Register an implementation of `sdk/cliproxy/auth.ProviderExecutor` for each custom provider key. The interface currently
contains these methods:

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

`Identifier` returns the provider key used by auth records. `Execute` returns the complete provider payload.
`ExecuteStream` returns response headers and a channel of `StreamChunk` values. A stream producer reports terminal errors
through `StreamChunk.Err`, marks an explicit completion boundary with `StreamChunk.Terminal`, and closes the channel.

`HttpRequest` executes raw provider requests. Its caller owns any non-nil response body and closes it. `CountTokens` should
return a request-scoped error when the provider has no token-count endpoint, so the auth manager does not treat that
capability gap as a credential failure.

An executor may also implement `coreauth.RequestPreparer`:

```text
func (Executor) PrepareRequest(request *http.Request, auth *coreauth.Auth) error
```

Use it to inject provider credentials into a raw request. Do not mutate shared transports or auth records from this method.

## Register the executor

Create the manager from the SDK token store, register the executor, and pass the manager to the builder:

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

Auth records whose effective provider key matches `Identifier()` route to this executor. `RegisterExecutor` replaces the
current executor for that key. Generation-aware registration methods are available when a plugin lifecycle must reject a
stale replacement.

## Per-auth transports

`Manager.SetRoundTripperProvider` installs a provider that returns an `http.RoundTripper` for each auth record:

```text
type roundTripperProvider struct{}

func (roundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
    return transportFor(auth.ProxyURL)
}
```

Executors should use the provided transport policy instead of modifying `http.DefaultTransport`. For raw request flows,
`Manager.InjectCredentials(request, authID)` invokes the selected executor's optional `RequestPreparer`.

## Translator registration

`sdk/translator.Register` stores a request transform from the inbound format to the provider format and response transforms
for the reverse direction. Request transforms return `[]byte`; streaming response transforms return `[][]byte`; non-stream
response transforms return `[]byte`.

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

Registration direction matters. Request lookup uses `from → to`; response lookup uses the reverse provider-to-client
path. When no matching transform exists, the registry preserves the input payload and still applies registered plugin
hooks.

## Model registration

Register custom models under each auth ID so they appear in `/v1/models` and model lookup:

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

The service registers built-in providers automatically. Custom executors should publish models after their auth records are
available and replace that registration when the provider's inventory changes.

## Verification

Keep executor tests local by using an `httptest.Server` or a fake `http.RoundTripper`. Cover non-stream responses, stream
terminal behavior, context cancellation, response-body ownership, credential injection, token-count errors, translator
direction, and model replacement. The repository's `examples/custom-provider` package is compiled by the release checks.
