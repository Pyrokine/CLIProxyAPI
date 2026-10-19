package pluginhost

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// pluginRefreshCompatExecutor keeps native OpenAI-compatible inference while
// routing credential refresh to a plugin AuthProvider.
type pluginRefreshCompatExecutor struct {
	inner    coreauth.ProviderExecutor
	host     *Host
	provider string
}

// NewPluginRefreshCompatExecutor wraps a native provider executor so Refresh is
// handled by the plugin AuthProvider that owns the auth credential.
func NewPluginRefreshCompatExecutor(inner coreauth.ProviderExecutor, host *Host) coreauth.ProviderExecutor {
	if inner == nil {
		return nil
	}
	return &pluginRefreshCompatExecutor{
		inner:    inner,
		host:     host,
		provider: strings.ToLower(strings.TrimSpace(inner.Identifier())),
	}
}

// IsPluginRefreshCompatExecutor reports whether executor is a plugin-refresh wrapper.
func IsPluginRefreshCompatExecutor(executor coreauth.ProviderExecutor) bool {
	_, ok := executor.(*pluginRefreshCompatExecutor)
	return ok
}

// UnwrapPluginRefreshCompatExecutor returns the wrapped native executor.
func UnwrapPluginRefreshCompatExecutor(executor coreauth.ProviderExecutor) (coreauth.ProviderExecutor, bool) {
	wrapper, ok := executor.(*pluginRefreshCompatExecutor)
	if !ok || wrapper == nil || wrapper.inner == nil {
		return nil, false
	}
	return wrapper.inner, true
}

func (e *pluginRefreshCompatExecutor) SupportsApplyPatch() bool {
	if e == nil || e.inner == nil {
		return false
	}
	support, ok := e.inner.(coreauth.ApplyPatchSupport)
	return ok && support.SupportsApplyPatch()
}

func (e *pluginRefreshCompatExecutor) ForAPIKey() coreauth.ProviderExecutor {
	if e == nil {
		return nil
	}
	view := *e
	if scoped, ok := e.inner.(coreauth.APIKeyConfigExecutor); ok {
		view.inner = scoped.ForAPIKey()
	}
	return &view
}

func (e *pluginRefreshCompatExecutor) Identifier() string {
	if e == nil {
		return ""
	}
	if e.provider != "" {
		return e.provider
	}
	if e.inner != nil {
		return e.inner.Identifier()
	}
	return ""
}

func (e *pluginRefreshCompatExecutor) Execute(
	ctx context.Context,
	auth *coreauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e == nil || e.inner == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("plugin refresh compat executor is unavailable")
	}
	return e.inner.Execute(ctx, auth, req, opts)
}

func (e *pluginRefreshCompatExecutor) ExecuteStream(
	ctx context.Context,
	auth *coreauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.inner == nil {
		return nil, fmt.Errorf("plugin refresh compat executor is unavailable")
	}
	return e.inner.ExecuteStream(ctx, auth, req, opts)
}

func (e *pluginRefreshCompatExecutor) CountTokens(
	ctx context.Context,
	auth *coreauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if e == nil || e.inner == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("plugin refresh compat executor is unavailable")
	}
	return e.inner.CountTokens(ctx, auth, req, opts)
}

func (e *pluginRefreshCompatExecutor) HttpRequest(
	ctx context.Context,
	auth *coreauth.Auth,
	req *http.Request,
) (*http.Response, error) {
	if e == nil || e.inner == nil {
		return nil, fmt.Errorf("plugin refresh compat executor is unavailable")
	}
	return e.inner.HttpRequest(ctx, auth, req)
}

// PrepareRequest forwards credential injection to the native executor when supported.
func (e *pluginRefreshCompatExecutor) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if e == nil || e.inner == nil {
		return fmt.Errorf("plugin refresh compat executor is unavailable")
	}
	preparer, ok := e.inner.(interface {
		PrepareRequest(*http.Request, *coreauth.Auth) error
	})
	if !ok || preparer == nil {
		return nil
	}
	return preparer.PrepareRequest(req, auth)
}

func (e *pluginRefreshCompatExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if e == nil || e.inner == nil {
		return nil, fmt.Errorf("plugin refresh compat executor is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if e.host != nil {
		if refreshed, handled, errRefresh := e.host.refreshAuth(ctx, auth); handled {
			return refreshed, errRefresh
		}
	}
	return e.inner.Refresh(ctx, auth)
}
