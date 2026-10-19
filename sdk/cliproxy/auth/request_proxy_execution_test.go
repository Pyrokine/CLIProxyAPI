package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestProxyRefreshExecutor struct {
	schedulerTestExecutor
	executeProxy []string
	refreshProxy []string
}

func (*requestProxyRefreshExecutor) Identifier() string { return "request-proxy-refresh" }

func (e *requestProxyRefreshExecutor) Execute(
	ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.executeProxy = append(e.executeProxy, cliproxyexecutor.RequestProxyURL(ctx))
	if auth.Metadata["access_token"] == "old-token" {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired token"}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *requestProxyRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.refreshProxy = append(e.refreshProxy, cliproxyexecutor.RequestProxyURL(ctx))
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new-token"
	return updated, nil
}

func TestRequestProxyAppliesToExecutionButNotCredentialRefresh(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &requestProxyRefreshExecutor{}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "request-proxy-refresh", Provider: executor.Identifier(), Status: StatusActive,
		Metadata: map[string]any{"access_token": "old-token", "refresh_token": "refresh-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(registered.ID, registered.Provider, []*registry.ModelInfo{{ID: "request-proxy-refresh-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(registered.ID) })

	const proxyURL = "socks5://127.0.0.1:1080"
	_, err = manager.Execute(
		context.Background(), []string{executor.Identifier()},
		cliproxyexecutor.Request{Model: "request-proxy-refresh-model"},
		cliproxyexecutor.Options{ProxyURL: proxyURL},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.executeProxy) != 2 || executor.executeProxy[0] != proxyURL || executor.executeProxy[1] != proxyURL ||
		len(executor.refreshProxy) != 1 || executor.refreshProxy[0] != "" {
		t.Fatalf("execution proxies=%v, refresh proxies=%v", executor.executeProxy, executor.refreshProxy)
	}
}
