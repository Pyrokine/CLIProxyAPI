package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type antigravityHomeModelCapabilityDispatcher struct {
	baseURL    string
	dispatches int
}

func (*antigravityHomeModelCapabilityDispatcher) HeartbeatOK() bool { return true }

func (d *antigravityHomeModelCapabilityDispatcher) RPopAuth(
	context.Context, string, string, http.Header, int,
) ([]byte, error) {
	d.dispatches++
	return json.Marshal(
		map[string]any{
			"model":    "gemini-3.8-flash-high",
			"provider": "antigravity",
			"model_info": map[string]any{
				"id":             "gemini-3.8-flash-high",
				"type":           "gemini",
				"context_length": 1048576,
				"thinking": map[string]any{
					"levels": []string{"low", "medium", "high"},
				},
				"user_defined": false,
			},
			"auth": cliproxyauth.Auth{
				ID:       "home-antigravity-3.8-auth",
				Provider: "antigravity",
				Status:   cliproxyauth.StatusActive,
				Attributes: map[string]string{
					cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
					"base_url":                     d.baseURL,
				},
				Metadata: map[string]any{
					"access_token": "token",
					"project_id":   "project-1",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			},
		},
	)
}

func (*antigravityHomeModelCapabilityDispatcher) AbortAmbiguousDispatch() {}

func TestAntigravityHomeModelThinkingLevelRemainsLevel(t *testing.T) {
	for _, test := range []struct {
		name        string
		localBudget bool
	}{
		{name: "original Home model"},
		{name: "Home levels override local budget", localBudget: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if test.localBudget {
					modelRegistry := registry.GetGlobalRegistry()
					const localID = "test-antigravity-home-local-budget"
					modelRegistry.RegisterClient(
						localID, "antigravity", []*registry.ModelInfo{
							{
								ID: "gemini-3.8-flash-high", Type: "gemini",
								Thinking: &registry.ThinkingSupport{Min: 1, Max: 32768},
							},
						},
					)
					t.Cleanup(func() { modelRegistry.UnregisterClient(localID) })
					local := registry.LookupModelInfo("gemini-3.8-flash-high", "antigravity")
					if local == nil || local.Thinking == nil || local.Thinking.Max != 32768 ||
						len(local.Thinking.Levels) != 0 {
						t.Fatalf("local budget control = %+v", local)
					}
				}
				requestBodies := make(chan []byte, 1)
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							body, errRead := io.ReadAll(r.Body)
							if errRead != nil {
								http.Error(w, errRead.Error(), http.StatusInternalServerError)
								return
							}
							requestBodies <- body
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(
								w, `{"response":{"candidates":[{"content":{"role":"model",`+
									`"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{`+
									`"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`,
							)
						},
					),
				)
				t.Cleanup(server.Close)

				cfg := &config.Config{RequestRetry: 1}
				cfg.Home.Enabled = true
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				manager.RegisterExecutor(NewAntigravityExecutor(cfg))
				dispatcher := &antigravityHomeModelCapabilityDispatcher{baseURL: server.URL}
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)

				payload := []byte(`{"model":"gemini-3.8-flash-high","reasoning_effort":"medium",` +
					`"messages":[{"role":"user","content":"hello"}]}`)
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				_, errExecute := manager.Execute(
					ctx, []string{"antigravity"}, cliproxyexecutor.Request{
						Model:   "gemini-3.8-flash-high",
						Payload: payload,
					}, cliproxyexecutor.Options{
						SourceFormat:    sdktranslator.FormatOpenAI,
						ResponseFormat:  sdktranslator.FormatOpenAI,
						OriginalRequest: payload,
					},
				)
				if errExecute != nil {
					t.Fatalf("Execute() error = %v", errExecute)
				}
				if dispatcher.dispatches != 1 {
					t.Fatalf("Home dispatches = %d, want 1", dispatcher.dispatches)
				}
				select {
				case upstreamBody := <-requestBodies:
					thinkingConfig := gjson.GetBytes(upstreamBody, "request.generationConfig.thinkingConfig")
					if got := thinkingConfig.Get("thinkingLevel").String(); got != "medium" {
						t.Fatalf("thinkingLevel = %q, want medium; body=%s", got, upstreamBody)
					}
					if budget := thinkingConfig.Get("thinkingBudget"); budget.Exists() {
						t.Fatalf("thinkingBudget should be absent, got %s; body=%s", budget.Raw, upstreamBody)
					}
				default:
					t.Fatal("successful execution did not send an Antigravity request")
				}
			},
		)
	}
}
