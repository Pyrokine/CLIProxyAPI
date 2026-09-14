# `sdk/access` reference

`github.com/Pyrokine/CLIProxyAPI/v6/sdk/access` defines inbound request authentication for embedded servers. A `Manager`
evaluates registered credential providers in order and returns a provider-specific principal and metadata.

## Install

```bash
go get github.com/Pyrokine/CLIProxyAPI/v6/sdk/access
```

## Provider registry

`RegisterProvider(type, provider)` stores an initialized provider under a type identifier. The first registration of each
type fixes its position. A later registration with the same type replaces the instance without changing that position.
`RegisteredProviders()` returns the current instances in registration order.

A manager uses a snapshot of the registry:

```text
manager := sdkaccess.NewManager()
manager.SetProviders(sdkaccess.RegisteredProviders())
```

`SetProviders` and `Providers` use defensive slice copies. A nil manager or a manager with no providers returns
`nil, nil` from `Authenticate`, which represents disabled access control.

## Authenticate a request

```text
result, authErr := manager.Authenticate(ctx, request)
switch {
case authErr == nil:
    // result identifies the provider, principal, and provider metadata.
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials):
    // No recognized credential was supplied.
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential):
    // A recognized credential was supplied but rejected.
default:
    // A provider returned an internal or transport failure.
}
```

`Manager.Authenticate` returns the first success. `AuthErrorCodeNotHandled` advances to the next provider.
`AuthErrorCodeNoCredentials` and `AuthErrorCodeInvalidCredential` are accumulated; invalid credentials take precedence if
no provider succeeds. Internal errors return immediately.

## Built-in `config-api-key` provider

`sdk/cliproxy.Builder` registers the built-in `config-api-key` provider from `Config.SDKConfig.APIKeys` and installs the
current registry snapshot on the access manager.

The provider accepts these headers:

- `Authorization: Bearer <key>`
- `X-Goog-Api-Key: <key>`
- `X-Api-Key: <key>`

The query parameters `?key=` and `?auth_token=` are accepted only when `allow-query-auth: true` is configured. Successful
results set `Metadata["source"]` to `authorization`, `x-goog-api-key`, `x-api-key`, `query-key`, or
`query-auth-token`.

```yaml
api-keys:
  - sk-test-123
allow-query-auth: false
```

Configured keys must contain at least eight printable ASCII characters. Comparison is constant-time across all configured
keys.

## Write a custom provider

This complete package registers a provider during initialization:

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

A provider must implement `Identifier()` and `Authenticate(context.Context, *http.Request)`. Use `Metadata` for
provider-specific audit context such as the credential source; do not place credential material in metadata.

An external provider module can register itself through a blank import:

```text
import _ "github.com/acme/xplatform/sdk/access/providers/partner"
```

Import it before calling `RegisteredProviders` or `cliproxy.Builder.Build`.

## Integrate with `sdk/cliproxy`

Supply a manager when the host needs to share or inspect the same access-control instance:

```text
accessManager := sdkaccess.NewManager()
service, err := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath(configPath).
    WithRequestAccessManager(accessManager).
    Build()
```

`Build` registers the config-backed provider and initializes the manager snapshot. The service repeats that update after a
configuration change. Custom provider implementations that change at runtime must register the replacement instance and
refresh the snapshot:

```text
sdkaccess.RegisterProvider("partner-token", replacement)
accessManager.SetProviders(sdkaccess.RegisteredProviders())
```

## Error semantics

- `NewNotHandledError()` means this provider did not recognize the request
- `NewNoCredentialsError()` means this provider recognized its credential scheme but found no credential
- `NewInvalidCredentialError()` means a recognized credential failed validation
- `NewInternalAuthError(message, cause)` reports a provider failure that must stop authentication

The CLI HTTP layer maps missing or invalid credentials to HTTP 401 and internal authentication failures to HTTP 500.
