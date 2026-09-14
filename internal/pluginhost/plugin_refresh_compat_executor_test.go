package pluginhost

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type stubRefreshCompatExecutor struct {
	id           string
	executeCalls int
	refreshCalls int
}

func (e *stubRefreshCompatExecutor) Identifier() string { return e.id }

func (e *stubRefreshCompatExecutor) Execute(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.executeCalls++
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *stubRefreshCompatExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return &cliproxyexecutor.StreamResult{}, nil
}

func (e *stubRefreshCompatExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.refreshCalls++
	return auth.Clone(), nil
}

func (e *stubRefreshCompatExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *stubRefreshCompatExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, nil
}

func (e *stubRefreshCompatExecutor) PrepareRequest(*http.Request, *coreauth.Auth) error { return nil }

func TestPluginRefreshCompatExecutorNamespacedCredentialUsesPluginAuthProvider(t *testing.T) {
	refreshCalls := 0
	host := newHostWithRecords(
		capabilityRecord{
			id: "auth-plugin",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					AuthProvider: fakeAuthProvider{
						identifier: "plugin-provider",
						refreshAuth: func(
							ctx context.Context,
							req pluginapi.AuthRefreshRequest,
						) (pluginapi.AuthRefreshResponse, error) {
							refreshCalls++
							if req.AuthID != "auth-1" || req.AuthProvider != "plugin-provider" {
								t.Fatalf("RefreshAuth request = %#v", req)
							}
							if req.Attributes["provider_key"] != "openai-compatible-custom" {
								t.Fatalf("provider_key = %q", req.Attributes["provider_key"])
							}
							return pluginapi.AuthRefreshResponse{
								Auth: pluginapi.AuthData{
									Metadata: map[string]any{
										"access_token":  "new-token",
										"refresh_token": "refresh-1",
									},
								},
							}, nil
						},
					},
				},
			},
		},
	)
	inner := &stubRefreshCompatExecutor{id: "openai-compatible-custom"}
	wrapped := NewPluginRefreshCompatExecutor(inner, host)
	auth := &coreauth.Auth{
		ID:       "auth-1",
		Provider: "plugin-provider",
		Attributes: map[string]string{
			"provider_key": "openai-compatible-custom",
			"compat_name":  "custom",
			"base_url":     "https://compat.example.com/v1",
		},
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-1",
		},
	}

	if _, errExecute := wrapped.Execute(
		context.Background(), auth, cliproxyexecutor.Request{}, cliproxyexecutor.Options{},
	); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	refreshed, errRefresh := wrapped.Refresh(context.Background(), auth)
	if errRefresh != nil {
		t.Fatalf("Refresh() error = %v", errRefresh)
	}
	if refreshCalls != 1 || inner.refreshCalls != 0 || inner.executeCalls != 1 {
		t.Fatalf(
			"calls = plugin refresh %d, inner refresh %d, inner execute %d",
			refreshCalls, inner.refreshCalls, inner.executeCalls,
		)
	}
	if refreshed == nil || refreshed.Metadata["access_token"] != "new-token" {
		t.Fatalf("Refresh() auth = %#v", refreshed)
	}
	if refreshed.Provider != "plugin-provider" ||
		refreshed.Attributes["provider_key"] != "openai-compatible-custom" {
		t.Fatalf("Refresh() routing = provider %q attributes %#v", refreshed.Provider, refreshed.Attributes)
	}
	if refreshed.RefreshCommit == nil {
		t.Fatal("Refresh() commit guard = nil")
	}
}

func TestPluginRefreshCompatExecutorPropagatesFailureAndCancellation(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "failure", err: errors.New("refresh denied")},
		{name: "cancellation", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				host := newHostWithRecords(
					capabilityRecord{
						id: "auth-plugin",
						plugin: pluginapi.Plugin{
							Capabilities: pluginapi.Capabilities{
								AuthProvider: fakeAuthProvider{
									identifier: "plugin-provider",
									refreshAuth: func(
										context.Context,
										pluginapi.AuthRefreshRequest,
									) (pluginapi.AuthRefreshResponse, error) {
										return pluginapi.AuthRefreshResponse{}, tt.err
									},
								},
							},
						},
					},
				)
				inner := &stubRefreshCompatExecutor{id: "openai-compatible-custom"}
				wrapped := NewPluginRefreshCompatExecutor(inner, host)
				auth := &coreauth.Auth{ID: "auth-1", Provider: "plugin-provider"}
				refreshed, errRefresh := wrapped.Refresh(context.Background(), auth)
				if !errors.Is(errRefresh, tt.err) {
					t.Fatalf("Refresh() error = %v, want %v", errRefresh, tt.err)
				}
				if refreshed == nil || refreshed.RefreshCommit == nil {
					t.Fatalf("Refresh() guarded auth = %#v", refreshed)
				}
				if inner.refreshCalls != 0 {
					t.Fatalf("inner refresh calls = %d", inner.refreshCalls)
				}
			},
		)
	}
}

func TestPluginRefreshCompatExecutorNoProviderUsesInnerBehavior(t *testing.T) {
	inner := &stubRefreshCompatExecutor{id: "openai-compatible-custom"}
	wrapped := NewPluginRefreshCompatExecutor(inner, New())
	auth := &coreauth.Auth{
		ID:       "auth-1",
		Provider: "custom-provider",
		Attributes: map[string]string{
			"api_key": "sk-test",
		},
	}
	refreshed, errRefresh := wrapped.Refresh(context.Background(), auth)
	if errRefresh != nil {
		t.Fatalf("Refresh() error = %v", errRefresh)
	}
	if inner.refreshCalls != 1 {
		t.Fatalf("inner refresh calls = %d, want 1", inner.refreshCalls)
	}
	if refreshed == nil || refreshed.Attributes["api_key"] != "sk-test" {
		t.Fatalf("Refresh() auth = %#v", refreshed)
	}
}

func TestPluginRefreshCompatExecutorIdentityHelpers(t *testing.T) {
	inner := &stubRefreshCompatExecutor{id: " OpenAI-Compatible-Custom "}
	wrapped := NewPluginRefreshCompatExecutor(inner, New())
	if !IsPluginRefreshCompatExecutor(wrapped) {
		t.Fatal("IsPluginRefreshCompatExecutor() = false")
	}
	if got, ok := UnwrapPluginRefreshCompatExecutor(wrapped); !ok || got != inner {
		t.Fatalf("UnwrapPluginRefreshCompatExecutor() = (%T, %t)", got, ok)
	}
	if got := wrapped.Identifier(); got != "openai-compatible-custom" {
		t.Fatalf("Identifier() = %q", got)
	}
	if !strings.Contains(wrapped.Identifier(), "custom") {
		t.Fatalf("Identifier() = %q", wrapped.Identifier())
	}
}
