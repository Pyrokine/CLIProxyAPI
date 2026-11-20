package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
)

func TestClaudeModelScopedRateLimitDoesNotBlockSiblingModel(t *testing.T) {
	tests := []struct {
		name        string
		targetModel string
		headers     http.Header
		wantDelay   time.Duration
	}{
		{
			name:        "missing status reset keeps scoped deadline",
			targetModel: "claude-sonnet-4-5",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Reset": {
					time.Now().Add(600 * time.Second).Format(time.RFC3339),
				},
			},
			wantDelay: 600 * time.Second,
		},
		{
			name:        "ordinary model rejection",
			targetModel: "claude-sonnet-4-5",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Status": {"allowed_warning"},
			},
		},
		{
			name:        "overage-only spend cap",
			targetModel: "claude-sonnet-4-5",
			headers: http.Header{
				"Retry-After":                                         {"60"},
				"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
				"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":          {"0.00"},
				"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"},
			},
		},
		{
			name:        "Fable-only rejection",
			targetModel: "claude-fable-5-1",
			headers: http.Header{
				"Retry-After":                              {"60"},
				"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed_warning"},
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				const siblingModel = "claude-opus-4-7"
				var targetAttempts, siblingAttempts atomic.Int32
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							body, errRead := io.ReadAll(r.Body)
							if errRead != nil {
								http.Error(w, "failed to read sanitized request", http.StatusBadRequest)
								return
							}
							w.Header().Set("Content-Type", "application/json")
							if strings.Contains(string(body), test.targetModel) {
								targetAttempts.Add(1)
								for name, values := range test.headers {
									for _, value := range values {
										w.Header().Add(name, value)
									}
								}
								w.WriteHeader(http.StatusTooManyRequests)
								_, _ = w.Write(
									[]byte(
										(`{"type":"error","error":{"type":"rate_limit_error","message":"model ` +
											`usage window ` +
											`rejected"}}`),
									),
								)
								return
							}
							if strings.Contains(string(body), siblingModel) {
								siblingAttempts.Add(1)
								w.WriteHeader(http.StatusOK)
								_, _ = w.Write(
									[]byte(
										(`{"id":"msg-sibling-ok","type":"message","model":"claude-opus-4-7","rol` +
											`e":"assistant","content":[{"type":"text","text":"ok"}]}`),
									),
								)
								return
							}
							http.Error(w, "unexpected sanitized model", http.StatusBadRequest)
						},
					),
				)
				defer server.Close()

				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetRetryConfig(0, 0, 0)
				manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))

				auth := &cliproxyauth.Auth{
					ID:       uuid.NewString() + "-claude-model-scope",
					Provider: "claude",
					Status:   cliproxyauth.StatusActive,
					Attributes: map[string]string{
						"api_key":  "sanitized-test-key",
						"base_url": server.URL,
					},
				}
				registry.GetGlobalRegistry().RegisterClient(
					auth.ID, "claude", []*registry.ModelInfo{
						{ID: test.targetModel},
						{ID: siblingModel},
					},
				)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
					t.Fatalf("register auth: %v", errRegister)
				}

				requestPayload := []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"reply OK"}]}`)
				options := cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatClaude,
					ResponseFormat: sdktranslator.FormatClaude,
				}
				_, errTarget := manager.Execute(
					t.Context(), []string{"claude"}, cliproxyexecutor.Request{
						Model:   test.targetModel,
						Payload: requestPayload,
					}, options,
				)
				if errTarget == nil {
					t.Fatal("target model error = nil")
				}
				if got := targetAttempts.Load(); got != 1 {
					t.Fatalf("target model attempts = %d, want 1", got)
				}
				current, ok := manager.GetByID(auth.ID)
				if !ok || current == nil {
					t.Fatal("auth missing after model-scoped rejection")
				}
				if current.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential ||
					(current.LastError !=
						nil &&
						current.LastError.ExplicitExecutionDisposition().EffectScope ==
							cliproxyexecutor.StateEffectScopeCredential) {
					t.Fatalf("model-scoped rejection became credential-scoped: %+v", current)
				}
				targetState := current.ModelStates[test.targetModel]
				if targetState == nil || !targetState.Unavailable ||
					targetState.EffectScope != cliproxyexecutor.StateEffectScopeExactModel {
					t.Fatalf("target model state = %+v, want exact-model unavailable", targetState)
				}

				if test.wantDelay > 0 {
					remaining := time.Until(targetState.NextRetryAfter)
					if remaining < test.wantDelay-time.Second || remaining > test.wantDelay+30*time.Second {
						t.Fatalf(
							"scoped deadline=%s want=[%s,%s]", remaining, test.wantDelay-time.Second,
							test.wantDelay+30*time.Second,
						)
					}
				}

				response, errSibling := manager.Execute(
					t.Context(), []string{"claude"}, cliproxyexecutor.Request{
						Model:   siblingModel,
						Payload: requestPayload,
					}, options,
				)
				if errSibling != nil {
					t.Fatalf("sibling model error = %v", errSibling)
				}
				if len(response.Payload) == 0 {
					t.Fatal("sibling model response is empty")
				}
				if got := siblingAttempts.Load(); got != 1 {
					t.Fatalf("sibling model attempts = %d, want 1", got)
				}
			},
		)
	}
}
