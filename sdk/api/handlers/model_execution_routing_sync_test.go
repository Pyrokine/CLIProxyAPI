package handlers

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestPluginModelExecutionRestrictsProviderAndAuthAfterAPIKeyPolicy(t *testing.T) {
	const model = "plugin-model-route-policy"
	for _, stream := range []bool{false, true} {
		for _, test := range []struct {
			name           string
			allowed        string
			forcedProvider string
			pinnedAuth     string
			wantStatus     int
			wantAuth       string
		}{
			{name: "allowed", allowed: "claude", forcedProvider: " CLAUDE ", pinnedAuth: "plugin-claude", wantAuth: "plugin-claude"},
			{name: "provider denied by key", allowed: "openai", forcedProvider: "claude", wantStatus: http.StatusForbidden},
			{name: "pinned auth conflicts", forcedProvider: "claude", pinnedAuth: "plugin-openai", wantStatus: http.StatusConflict},
			{name: "unknown provider", forcedProvider: "missing", wantStatus: http.StatusBadRequest},
		} {
			t.Run(test.name+map[bool]string{true: " stream", false: " nonstream"}[stream], func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				var selected []string
				for _, provider := range []string{"openai", "claude"} {
					authID := "plugin-" + provider
					executor := &lifecycleCaptureExecutor{
						provider: provider,
						execute: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
							selected = append(selected, auth.ID)
							return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
						},
						stream: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
							selected = append(selected, auth.ID)
							chunks := make(chan coreexecutor.StreamChunk, 1)
							chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\\n\\n")}
							close(chunks)
							return &coreexecutor.StreamResult{Chunks: chunks}, nil
						},
					}
					manager.RegisterExecutor(executor)
					if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: provider, Status: coreauth.StatusActive}); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
				}
				apiKey := t.Name()
				cfg := &sdkconfig.SDKConfig{}
				if test.allowed != "" {
					cfg.APIKeyRules = map[string]internalconfig.APIKeyRule{
						apiKey: {Providers: []string{test.allowed}},
					}
				}
				handler := NewBaseAPIHandlers(cfg, manager)
				request := ModelExecutionRequest{
					EntryProtocol: constant.OpenAI, Model: model, Stream: stream,
					Body:           []byte(`{"model":"plugin-model-route-policy"}`),
					ForcedProvider: test.forcedProvider, AuthID: test.pinnedAuth,
				}
				ctx := apiKeyPolicyContext(apiKey, "plugin-model-policy")
				if stream {
					response, err := handler.ExecuteModelStream(ctx, request)
					if test.wantStatus != 0 {
						if err == nil || err.StatusCode != test.wantStatus {
							t.Fatalf("stream error = %#v, want status %d", err, test.wantStatus)
						}
					} else {
						if err != nil || response.StatusCode != http.StatusOK {
							t.Fatalf("stream response = %#v, error = %#v", response, err)
						}
						for range response.Chunks {
						}
					}
				} else {
					response, err := handler.ExecuteModel(ctx, request)
					if test.wantStatus != 0 {
						if err == nil || err.StatusCode != test.wantStatus {
							t.Fatalf("execution error = %#v, want status %d", err, test.wantStatus)
						}
					} else if err != nil || response.StatusCode != http.StatusOK {
						t.Fatalf("response = %#v, error = %#v", response, err)
					}
				}
				if test.wantAuth == "" && len(selected) != 0 || test.wantAuth != "" && (len(selected) != 1 || selected[0] != test.wantAuth) {
					t.Fatalf("selected auths = %v, want %q", selected, test.wantAuth)
				}
			})
		}
	}
}
