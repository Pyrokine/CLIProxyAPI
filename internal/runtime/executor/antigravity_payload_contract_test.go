package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	antigravityauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityPayloadSourceProtocol(t *testing.T) {
	for _, route := range []string{"generate", "aggregate", "stream", "count"} {
		for _, source := range []struct {
			format sdktranslator.Format
			body   string
		}{
			{sdktranslator.FormatAntigravity, `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`},
			{sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
			{sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"hello"}]}`},
		} {
			for _, from := range []string{"", source.format.String(), "responses"} {
				t.Run(fmt.Sprintf("%s/%s/from=%s", route, source.format, from), func(t *testing.T) {
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "antigravity", FromProtocol: from, Headers: map[string]string{"X-Payload-Test": "match"}}}, Params: map[string]any{"contents.0.parts.0.text": "configured-content"}}}}}
					model := "gemini-2.5-flash"
					if route == "aggregate" {
						model = "claude-sonnet-4-6"
					}
					body := googlePayloadCall(t, "antigravity", route, model, cfg, []byte(source.body), cliproxyexecutor.Options{SourceFormat: source.format, ResponseFormat: sdktranslator.FormatAntigravity, Headers: http.Header{"X-Payload-Test": {"match"}}})
					want := "configured-content"
					if from == "responses" {
						want = "hello"
					}
					if got := gjson.GetBytes(body, "request.contents.0.parts.0.text").String(); got != want {
						t.Fatalf("text=%q want=%q body=%s", got, want, body)
					}
				})
			}
		}
	}
}

func TestAntigravityFinalPayloadRules(t *testing.T) {
	for _, route := range []string{"generate", "aggregate", "stream"} {
		for _, kind := range []string{"none", "default", "override", "filter", "once"} {
			t.Run(route+"/"+kind, func(t *testing.T) {
				models := []config.PayloadModelRule{{Name: "*", Protocol: "antigravity", FromProtocol: "antigravity"}}
				cfg := &config.Config{}
				payload := `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`
				wantLimit := int64(0)
				switch kind {
				case "none":
					payload = `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":9}}}`
				case "default":
					cfg.Payload.Default = []config.PayloadRule{{Models: models, Params: map[string]any{"generationConfig.maxOutputTokens": int64(31)}}}
					wantLimit = 31
				case "override":
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"generationConfig.maxOutputTokens": int64(37)}}}
					wantLimit = 37
				case "filter":
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"generationConfig.maxOutputTokens": int64(37)}}}
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"generationConfig.maxOutputTokens", "sessionId"}}}
				case "once":
					cfg.Payload.Override = []config.PayloadRule{
						{Models: []config.PayloadModelRule{{Name: "*", Protocol: "antigravity", Exist: []string{"once"}}}, Params: map[string]any{"applied_twice": true}},
						{Models: models, Params: map[string]any{"once": true}},
					}
				}
				model := "gemini-2.5-flash"
				if route == "aggregate" {
					model = "claude-sonnet-4-6"
					if kind == "none" {
						wantLimit = 9
					}
				}
				body := googlePayloadCall(t, "antigravity", route, model, cfg, []byte(payload), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity})
				limit := gjson.GetBytes(body, "request.generationConfig.maxOutputTokens")
				if (wantLimit == 0 && limit.Exists()) || (wantLimit != 0 && (!limit.Exists() || limit.Int() != wantLimit)) {
					t.Fatalf("limit=%s want=%d body=%s", limit.Raw, wantLimit, body)
				}
				if kind == "filter" && gjson.GetBytes(body, "request.sessionId").Exists() {
					t.Fatalf("filter restored after config: %s", body)
				}
				if kind == "once" && (!gjson.GetBytes(body, "request.once").Bool() || gjson.GetBytes(body, "request.applied_twice").Exists()) {
					t.Fatalf("rules repeated: %s", body)
				}
				if route == "aggregate" && gjson.GetBytes(body, "request.toolConfig.functionCallingConfig.mode").String() != "VALIDATED" {
					t.Fatalf("Claude tool mode missing: %s", body)
				}
			})
		}
	}
}

func TestAntigravityTokenCountPayloadRules(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "*", Protocol: "antigravity", FromProtocol: "gemini"}}
	for _, kind := range []string{"override", "default", "filter", "none"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &config.Config{}
			payload := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"systemInstruction":{"parts":[{"text":"original-system"}]}}`
			wantText, wantSystem := "hello", "original-system"
			switch kind {
			case "override":
				cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"contents.0.parts.0.text": "configured-content", "systemInstruction.parts.0.text": "configured-system", "tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "token_tool", "description": "configured-tool"}}}}, "sessionId": "should-be-removed"}}}
				wantText, wantSystem = "configured-content", "configured-system"
			case "default":
				payload = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
				cfg.Payload.Default = []config.PayloadRule{{Models: models, Params: map[string]any{"systemInstruction.parts.0.text": "configured-system", "contents.0.parts.0.text": "unused"}}}
				wantSystem = "configured-system"
			case "filter":
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"systemInstruction"}}}
				wantSystem = ""
			}
			body := googlePayloadCall(t, "antigravity", "count", "gemini-2.5-flash", cfg, []byte(payload), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini})
			if got := gjson.GetBytes(body, "request.contents.0.parts.0.text").String(); got != wantText {
				t.Fatalf("text=%q want=%q body=%s", got, wantText, body)
			}
			if got := gjson.GetBytes(body, "request.systemInstruction.parts.0.text").String(); got != wantSystem {
				t.Fatalf("system=%q want=%q body=%s", got, wantSystem, body)
			}
			if gjson.GetBytes(body, "project").Exists() || gjson.GetBytes(body, "model").Exists() || gjson.GetBytes(body, "request.sessionId").Exists() || gjson.GetBytes(body, "request.toolConfig").Exists() {
				t.Fatalf("count-only fields restored: %s", body)
			}
			if kind == "override" && gjson.GetBytes(body, "request.tools.0.functionDeclarations.0.description").String() != "configured-tool" {
				t.Fatalf("tool count content missing: %s", body)
			}
		})
	}
}

func TestAntigravityProjectDiscoveryStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			ctx := util.WithRoundTripper(t.Context(), googlePayloadTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if !strings.Contains(req.URL.Path, "loadCodeAssist") {
					return nil, fmt.Errorf("unexpected request %s", req.URL)
				}
				resp := googlePayloadResponse(req, fmt.Sprintf(`{"error":{"code":%d,"message":"synthetic project rejection"}}`, status))
				resp.StatusCode = status
				return resp, nil
			}))
			auth := googlePayloadAuth("antigravity")
			delete(auth.Metadata, "project_id")
			_, err := NewAntigravityExecutor(&config.Config{}).PrepareRequestAuth(ctx, auth)
			var upstream *antigravityauth.HTTPStatusError
			if !errors.As(err, &upstream) || upstream.StatusCode() != status || calls != 1 {
				t.Fatalf("error=%v calls=%d want typed status=%d", err, calls, status)
			}
			if _, exists := auth.Metadata["project_id"]; exists {
				t.Fatal("failed project discovery mutated caller auth")
			}
		})
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx := util.WithRoundTripper(t.Context(), googlePayloadTransport(func(*http.Request) (*http.Response, error) { return nil, cause }))
			auth := googlePayloadAuth("antigravity")
			delete(auth.Metadata, "project_id")
			_, err := NewAntigravityExecutor(&config.Config{}).PrepareRequestAuth(ctx, auth)
			if !errors.Is(err, cause) {
				t.Fatalf("error=%v want=%v", err, cause)
			}
		})
	}
	_, err := NewAntigravityExecutor(&config.Config{}).projectIDForRequest(t.Context(), nil, "")
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("missing local project error=%v", err)
	}
}

func TestAntigravityEmptyTranslationPublishesFailure(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash", "claude-sonnet-4-6"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%t", model, empty), func(t *testing.T) {
				format := sdktranslator.Format("empty-translation-" + t.Name())
				sdktranslator.Register(format, sdktranslator.FormatAntigravity, nil, sdktranslator.ResponseTransform{
					NonStream: func(_ context.Context, _ string, _, _, body []byte, _ *any) []byte {
						if empty {
							return nil
						}
						return body
					},
				})
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
				registerTestUsagePlugin(t, plugin)
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), groundingURLRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					body := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`
					if strings.Contains(req.URL.Path, "streamGenerateContent") {
						body = "data: " + body + "\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				}))
				ctx = sdktranslator.WithPluginHooks(ctx, nil)
				resp, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Attributes: map[string]string{"base_url": "https://synthetic.invalid"}, Metadata: map[string]any{"access_token": "synthetic-token", "project_id": "synthetic-project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}},
					cliproxyexecutor.Request{Model: model, Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: format})
				if calls != 1 {
					t.Fatalf("synthetic upstream calls=%d, want 1", calls)
				}
				if empty && err == nil {
					t.Errorf("empty translated payload returned success: payload=%q error=%v", resp.Payload, err)
				}
				if !empty && (err != nil || len(resp.Payload) == 0) {
					t.Fatalf("nonempty control: payload=%q err=%v", resp.Payload, err)
				}
				select {
				case record := <-plugin.records:
					if record.Failed != empty {
						t.Errorf("usage.Failed=%t, want %t after empty=%t translation; detail=%+v", record.Failed, empty, empty, record.Detail)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("usage record missing")
				}
			})
		}
	}
}
