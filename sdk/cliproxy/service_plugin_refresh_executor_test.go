package cliproxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type customOwnedExecutor struct {
	id string
}

func (e *customOwnedExecutor) Identifier() string { return e.id }

func (e *customOwnedExecutor) Execute(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *customOwnedExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *customOwnedExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth.Clone(), nil
}

func (e *customOwnedExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *customOwnedExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, nil
}

func TestEnsureExecutorsForAuthNamespacedPluginRefreshUsesActualKey(t *testing.T) {
	oldHasAuthProvider := pluginHostHasAuthProvider
	pluginHostHasAuthProvider = func(host *pluginhost.Host, provider string) bool {
		return host != nil && provider == "plugin-provider"
	}
	t.Cleanup(
		func() {
			pluginHostHasAuthProvider = oldHasAuthProvider
		},
	)

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  pluginhost.New(),
	}
	auth := &coreauth.Auth{
		ID:       "plugin-auth-1",
		Provider: "plugin-provider",
		Attributes: map[string]string{
			"compat_name":  "custom",
			"provider_key": "openai-compatible-custom",
			"base_url":     "https://compat.example.com/v1",
		},
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-1",
		},
	}

	if !service.ensureExecutorsForAuthWithConfigContext(
		context.Background(), auth, false, service.cfg,
	) {
		t.Fatal("ensureExecutorsForAuthWithConfigContext() = false")
	}
	resolved, ok := service.coreManager.Executor("openai-compatible-custom")
	if !ok || resolved == nil {
		t.Fatal("expected executor for actual provider_key")
	}
	if !pluginhost.IsPluginRefreshCompatExecutor(resolved) {
		t.Fatalf("executor type = %T, want plugin refresh wrapper", resolved)
	}
	inner, okInner := pluginhost.UnwrapPluginRefreshCompatExecutor(resolved)
	if !okInner {
		t.Fatal("expected wrapped native executor")
	}
	if _, okCompat := inner.(*runtimeexecutor.OpenAICompatExecutor); !okCompat {
		t.Fatalf("inner executor type = %T", inner)
	}
	if _, exists := service.coreManager.Executor("plugin-provider"); exists {
		t.Fatal("unexpected executor registered under auth.Provider")
	}
}

func TestEnsureExecutorsForAuthNoProviderKeepsNativeNoOpExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  pluginhost.New(),
	}
	auth := &coreauth.Auth{
		ID:       "compat-auth-1",
		Provider: "custom-compat",
		Attributes: map[string]string{
			"base_url": "https://compat.example.com/v1",
			"api_key":  "sk-test",
		},
	}

	if !service.ensureExecutorsForAuthWithConfigContext(
		context.Background(), auth, false, service.cfg,
	) {
		t.Fatal("ensureExecutorsForAuthWithConfigContext() = false")
	}
	resolved, ok := service.coreManager.Executor("custom-compat")
	if !ok || resolved == nil {
		t.Fatal("expected native executor")
	}
	if pluginhost.IsPluginRefreshCompatExecutor(resolved) {
		t.Fatal("unexpected plugin refresh wrapper")
	}
	if _, okCompat := resolved.(*runtimeexecutor.OpenAICompatExecutor); !okCompat {
		t.Fatalf("executor type = %T", resolved)
	}
}

func TestEnsureExecutorsForAuthUpgradesOwnedNativeExecutor(t *testing.T) {
	oldHasAuthProvider := pluginHostHasAuthProvider
	pluginHostHasAuthProvider = func(host *pluginhost.Host, provider string) bool {
		return host != nil && provider == "plugin-provider"
	}
	t.Cleanup(
		func() {
			pluginHostHasAuthProvider = oldHasAuthProvider
		},
	)

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor("openai-compatible-custom", &config.Config{}))
	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
		pluginHost:  pluginhost.New(),
	}
	auth := &coreauth.Auth{
		ID:       "plugin-auth-1",
		Provider: "plugin-provider",
		Attributes: map[string]string{
			"compat_name":  "custom",
			"provider_key": "openai-compatible-custom",
		},
	}

	if !service.ensureExecutorsForAuthWithConfigContext(
		context.Background(), auth, false, service.cfg,
	) {
		t.Fatal("ensureExecutorsForAuthWithConfigContext() = false")
	}
	resolved, ok := manager.Executor("openai-compatible-custom")
	if !ok || !pluginhost.IsPluginRefreshCompatExecutor(resolved) {
		t.Fatalf("executor type = %T, want wrapper", resolved)
	}
}

func TestEnsureExecutorsForAuthPreservesCustomExecutorOwnership(t *testing.T) {
	oldHasAuthProvider := pluginHostHasAuthProvider
	pluginHostHasAuthProvider = func(host *pluginhost.Host, provider string) bool {
		return host != nil && provider == "plugin-provider"
	}
	t.Cleanup(
		func() {
			pluginHostHasAuthProvider = oldHasAuthProvider
		},
	)

	manager := coreauth.NewManager(nil, nil, nil)
	custom := &customOwnedExecutor{id: "openai-compatible-custom"}
	manager.RegisterExecutor(custom)
	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
		pluginHost:  pluginhost.New(),
	}
	auth := &coreauth.Auth{
		ID:       "plugin-auth-1",
		Provider: "plugin-provider",
		Attributes: map[string]string{
			"compat_name":  "custom",
			"provider_key": "openai-compatible-custom",
			"base_url":     "https://compat.example.com/v1",
		},
	}

	if !service.ensureExecutorsForAuthWithConfigContext(
		context.Background(), auth, true, service.cfg,
	) {
		t.Fatal("ensureExecutorsForAuthWithConfigContext() = false")
	}
	resolved, ok := manager.Executor("openai-compatible-custom")
	if !ok || resolved != custom {
		t.Fatalf("executor = %T %p, want custom %p", resolved, resolved, custom)
	}
}
