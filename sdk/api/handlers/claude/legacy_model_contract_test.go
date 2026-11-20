package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	claudemodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/claude/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type claudeModelCaptureExecutor struct {
	claudeEmptyStreamExecutor
	requests []coreexecutor.Request
}

func (e *claudeModelCaptureExecutor) Execute(
	_ context.Context, _ *coreauth.Auth, request coreexecutor.Request, _ coreexecutor.Options,
) (coreexecutor.Response, error) {
	e.requests = append(e.requests, request)
	return coreexecutor.Response{
		Payload: []byte(
			`{"id":"fixture","type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`,
		),
	}, nil
}

func (e *claudeModelCaptureExecutor) CountTokens(
	_ context.Context, _ *coreauth.Auth, request coreexecutor.Request, _ coreexecutor.Options,
) (coreexecutor.Response, error) {
	e.requests = append(e.requests, request)
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func (e *claudeModelCaptureExecutor) ExecuteStream(
	ctx context.Context, auth *coreauth.Auth, request coreexecutor.Request, opts coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	e.requests = append(e.requests, request)
	stream := claudeFormatStreamExecutor{
		payloads: []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{" +
				"\"id\":\"fixture\",\"role\":\"assistant\",\"content\":[]}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	}
	return stream.ExecuteStream(ctx, auth, request, opts)
}

func TestPublishedClaudeModelBodyInputs(t *testing.T) {
	for _, tt := range []struct{ body, want string }{
		{`{"model":"claude-fable-5-dd-o4-tpg","messages":[]}`, "gpt-4o"},
		{`{"model":"claude-sonnet-4-6","messages":[]}`, "claude-sonnet-4-6"},
		{`{"model":"claude-fable-5-dd-o4-tpg(high)","stream":true}`, "gpt-4o(high)"},
		{`{"messages":[]}`, ""},
	} {
		body := resolveClaudeClientModelInBody([]byte(tt.body), nil, false)
		if got := gjson.GetBytes(body, "model").String(); got != tt.want {
			t.Fatalf("input=%s model=%q want=%q", tt.body, got, tt.want)
		}
	}
}

func TestPublishedClaudeModelListingInputs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		id, display string
		disabled    bool
	}{
		{id: "claude-display-name-catalog-test", display: "Configured Claude Name"},
		{id: "gpt-disable-model-list-cloaking-test", disabled: true},
	} {
		t.Run(
			tt.id, func(t *testing.T) {
				registry.GetGlobalRegistry().RegisterClient(
					tt.id, "claude", []*registry.ModelInfo{
						{
							ID: tt.id, Object: "model", OwnedBy: "test", DisplayName: tt.display,
						},
					},
				)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(tt.id) })
				cfg := &config.SDKConfig{
					ClaudeCode: config.ClaudeCodeConfig{DisableCloakingModelList: tt.disabled},
				}
				handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, nil))
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				handler.ClaudeModels(ctx)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				for _, model := range gjson.GetBytes(recorder.Body.Bytes(), "data").Array() {
					if model.Get("id").String() == tt.id {
						if tt.display != "" && model.Get("display_name").String() != tt.display {
							t.Fatalf("display=%q want=%q", model.Get("display_name").String(), tt.display)
						}
						return
					}
				}
				t.Fatalf("model %q missing: %s", tt.id, recorder.Body.String())
			},
		)
	}
}

func TestClaudeLegacyAndCurrentModelIDsReachEveryExecutionEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, model := range []struct{ name, id string }{
		{"legacy", "claude-fable-5-dd-o4-tpg(high)"},
		{"current", claudemodels.ClientModelID("gpt-4o") + "(high)"},
	} {
		for _, operation := range []string{"execute", "count", "stream"} {
			t.Run(
				model.name+"/"+operation, func(t *testing.T) {
					executor := &claudeModelCaptureExecutor{}
					manager := coreauth.NewManager(nil, nil, nil)
					manager.RegisterExecutor(executor)
					auth := &coreauth.Auth{
						ID: "fixture-model-" + model.name + "-" + operation, Provider: executor.Identifier(),
						Status: coreauth.StatusActive,
					}
					if _, err := manager.Register(t.Context(), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(
						auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-4o"}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
					router := gin.New()
					router.POST("/v1/messages", handler.ClaudeMessages)
					router.POST("/v1/messages/count_tokens", handler.ClaudeCountTokens)
					path := "/v1/messages"
					if operation == "count" {
						path += "/count_tokens"
					}
					stream := "false"
					if operation == "stream" {
						stream = "true"
					}
					body := `{"model":"` +
						model.id +
						`","stream":` +
						stream +
						`,"max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`
					request := httptest.NewRequest(
						http.MethodPost, path, strings.NewReader(body),
					).WithContext(t.Context())
					request.Header.Set("Content-Type", "application/json")
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)
					if recorder.Code != http.StatusOK || len(executor.requests) != 1 {
						t.Fatalf(
							"status=%d calls=%d body=%s",
							recorder.Code,
							len(executor.requests),
							recorder.Body.String(),
						)
					}
					seen := executor.requests[0]
					if seen.Model != "gpt-4o(high)" || gjson.GetBytes(
						seen.Payload, "model",
					).String() != "gpt-4o(high)" {
						t.Fatalf("execution model=%q payload=%s", seen.Model, seen.Payload)
					}
				},
			)
		}
	}
}
