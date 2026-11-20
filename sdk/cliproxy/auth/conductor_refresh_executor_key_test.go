package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type executorKeyRefreshExecutor struct {
	id           string
	refreshCalls atomic.Int32
}

func (e *executorKeyRefreshExecutor) Identifier() string { return e.id }

func (e *executorKeyRefreshExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *executorKeyRefreshExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *executorKeyRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	refreshed := auth.Clone()
	if refreshed.Metadata == nil {
		refreshed.Metadata = make(map[string]any)
	}
	refreshed.Metadata["access_token"] = "refreshed-token"
	return refreshed, nil
}

func (e *executorKeyRefreshExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *executorKeyRefreshExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, nil
}

func TestRefreshAuthForRequestUsesExecutorKeyAndPublishesResult(t *testing.T) {
	for _, sample := range []struct{ name, key, executorKey string }{
		{"prefixed", "openai-compatible-custom", "openai-compatible-custom"},
		{"raw", "custom", "custom"},
		{"compat name", "", "custom"},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				ctx := context.Background()
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				actual := &executorKeyRefreshExecutor{id: sample.executorKey}
				wrong := &executorKeyRefreshExecutor{id: "plugin-provider"}
				manager.RegisterExecutor(actual)
				manager.RegisterExecutor(wrong)
				registered, errRegister := manager.Register(
					ctx, &Auth{
						ID: "compat-oauth", Provider: "plugin-provider",
						Attributes: map[string]string{
							"compat_name": "custom", "provider_key": sample.key,
							"base_url": "https://compat.example.com/v1",
						},
						Metadata: map[string]any{"access_token": "old-token", "refresh_token": "refresh-1"},
					},
				)
				if errRegister != nil {
					t.Fatalf("Register() error = %v", errRegister)
				}
				refreshed, errRefresh := manager.refreshAuthForRequest(ctx, registered.ID, "old-token")
				if errRefresh != nil {
					t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
				}
				if actual.refreshCalls.Load() != 1 || wrong.refreshCalls.Load() != 0 {
					t.Fatalf(
						"refresh calls = actual %d, provider %d",
						actual.refreshCalls.Load(),
						wrong.refreshCalls.Load(),
					)
				}
				if refreshed == nil || refreshed.Metadata["access_token"] != "refreshed-token" {
					t.Fatalf("refreshed auth = %#v", refreshed)
				}
				published, okPublished := manager.GetByID(registered.ID)
				if !okPublished || published.Metadata["access_token"] != "refreshed-token" {
					t.Fatalf("published auth = %#v, ok = %t", published, okPublished)
				}
				if published.Provider != "plugin-provider" ||
					published.Attributes["provider_key"] != sample.key ||
					published.Metadata["refresh_token"] !=
						"refresh-1" ||
					published.Generation <=
						registered.Generation {
					t.Fatalf("published routing or generation changed: %#v", published)
				}
			},
		)
	}
}

func TestAutoRefreshUsesCredentialExecutorKey(t *testing.T) {
	for _, provider := range []string{"plugin-provider", " OPENAI-COMPATIBLE-CUSTOM "} {
		for _, sample := range []struct{ name, key, executorKey string }{
			{"prefixed", "openai-compatible-custom", "openai-compatible-custom"},
			{"raw", "custom", "custom"},
			{"compat name", "", "custom"},
		} {
			t.Run(
				provider+"/"+sample.name, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					executor := &executorKeyRefreshExecutor{id: sample.executorKey}
					wrong := &executorKeyRefreshExecutor{id: "plugin-provider"}
					manager.RegisterExecutor(executor)
					manager.RegisterExecutor(wrong)
					registered, err := manager.Register(
						t.Context(), &Auth{
							ID: "synthetic-auto-refresh-key", Provider: provider,
							Attributes: map[string]string{"compat_name": "custom", "provider_key": sample.key},
							Metadata: map[string]any{
								"access_token": "old-token", "refresh_token": "synthetic-refresh-token",
								"refresh_interval_seconds": 1,
							},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					manager.checkRefreshes(t.Context())
					manager.refreshWorkers.Wait()
					current, _ := manager.GetByID(registered.ID)
					if executor.refreshCalls.Load() != 1 || wrong.refreshCalls.Load() != 0 ||
						current.Metadata["access_token"] != "refreshed-token" {
						t.Fatalf(
							"auto refresh calls = actual %d, provider %d", executor.refreshCalls.Load(),
							wrong.refreshCalls.Load(),
						)
					}
				},
			)
		}
	}
}
