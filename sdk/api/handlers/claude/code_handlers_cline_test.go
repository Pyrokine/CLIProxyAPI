package claude_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	backendconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	claudehandlers "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/claude"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type clineHandlerTransport struct{ http.RoundTripper }

func (transport clineHandlerTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return transport.RoundTripper
}

type clineHandlerRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip clineHandlerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestClaudeHandlerClineNonstreamModelValidation(t *testing.T) {
	const model = "cline-pass/deepseek-v4.1-flash-1017"
	const completion = `{"id":"cline-http","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	for _, tc := range []struct {
		name        string
		body        string
		contentType string
		context1m   bool
		status      int
		wantPath    string
		wantValue   string
		wantStop    string
	}{
		{name: "one token thinking", body: `data: {"id":"cline-http","model":"` + model + `","choices":[{"index":0,"delta":{"reasoning_content":"Think"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\ndata: [DONE]\n\n", contentType: "text/event-stream", status: http.StatusOK, wantPath: "content.0.thinking", wantValue: "Think", wantStop: "max_tokens"},
		{name: "context suffix", body: `data: {"id":"cline-http","model":"` + model + `","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\ndata: [DONE]\n\n", contentType: "text/event-stream", context1m: true, status: http.StatusOK, wantPath: "content.0.text", wantValue: "OK", wantStop: "max_tokens"},
		{name: "wrapped JSON", body: `{"success":true,"data":` + completion + `}`, contentType: "application/json", status: http.StatusOK, wantPath: "content.0.text", wantValue: "OK", wantStop: "end_turn"},
		{name: "business error", body: `{"success":false,"error":"empty response content"}`, contentType: "application/json", status: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			cfg := &backendconfig.Config{}
			executor := runtimeexecutor.NewOpenAICompatExecutor("cline-handler-fixture", cfg)
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(executor)
			auth := &coreauth.Auth{ID: "cline-handler-" + strings.ReplaceAll(tc.name, " ", "-"), Provider: executor.Identifier(), Status: coreauth.StatusActive, Attributes: map[string]string{"base_url": "https://api.cline.bot/api/v1", "api_key": "synthetic-fixture"}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model, ContextLength: 1_000_000}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			calls := 0
			manager.SetRoundTripperProvider(clineHandlerTransport{clineHandlerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.String() != "https://api.cline.bot/api/v1/chat/completions" {
					return nil, fmt.Errorf("unexpected offline target: %s", request.URL)
				}
				payload, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if !gjson.GetBytes(payload, "stream").Bool() || gjson.GetBytes(payload, "max_tokens").Int() != 1 || gjson.GetBytes(payload, "model").String() != model {
					t.Errorf("model validation request changed: %s", payload)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: request}, nil
			})})
			handler := claudehandlers.NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
			router := gin.New()
			router.POST("/v1/messages", handler.ClaudeMessages)
			requestedModel := model
			if tc.context1m {
				requestedModel += "[1m]"
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+requestedModel+`","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"Reply OK"}]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Anthropic-Version", "2023-06-01")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			body := recorder.Body.Bytes()
			if recorder.Code != tc.status || calls != 1 || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("HTTP result: status=%d calls=%d headers=%v body=%s", recorder.Code, calls, recorder.Header(), body)
			}
			if tc.status == http.StatusOK {
				if gjson.GetBytes(body, tc.wantPath).String() != tc.wantValue || gjson.GetBytes(body, "stop_reason").String() != tc.wantStop || gjson.GetBytes(body, "usage.input_tokens").Int() != 3 || gjson.GetBytes(body, "usage.output_tokens").Int() != 1 {
					t.Errorf("actual Messages response lost content or usage: %s", body)
				}
			} else if !gjson.GetBytes(body, "error").Exists() {
				t.Errorf("business failure missing from HTTP error: %s", body)
			}
		})
	}
}
