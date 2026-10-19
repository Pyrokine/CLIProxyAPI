package executor

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityPayloadRulesValidateToolHistory(t *testing.T) {
	for _, route := range []string{"generate", "stream", "count"} {
		t.Run(route, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "antigravity"}}, Params: map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "missing_call", "response": map[string]any{"result": "orphan"}}}}}}}}}}}
			calls := 0
			ctx := util.WithRoundTripper(t.Context(), googlePayloadTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected HTTP request")
			}))
			exec := NewAntigravityExecutor(cfg)
			req := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"valid input"}]}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
			var err error
			switch route {
			case "generate":
				_, err = exec.Execute(ctx, googlePayloadAuth("antigravity"), req, opts)
			case "stream":
				_, err = exec.ExecuteStream(ctx, googlePayloadAuth("antigravity"), req, opts)
			case "count":
				_, err = exec.CountTokens(ctx, googlePayloadAuth("antigravity"), req, opts)
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusBadRequest || calls != 0 || !strings.Contains(err.Error(), "invalid Gemini function call history") {
				t.Fatalf("error=%v HTTP=%d", err, calls)
			}
		})
	}
}

func TestGooglePayloadRequestPath(t *testing.T) {
	for _, provider := range []string{"gemini", "antigravity"} {
		for _, path := range []string{"/v1/images/generations", "/v1/chat/completions"} {
			t.Run(fmt.Sprintf("%s/%s", provider, path), func(t *testing.T) {
				cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationChat}
				payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"type":"image_generation"}]}`)
				body := googlePayloadCall(t, provider, "generate", "gemini-2.5-flash", cfg, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: path}})
				root := ""
				if provider == "antigravity" {
					root = "request."
				}
				found := false
				for _, tool := range gjson.GetBytes(body, root+"tools").Array() {
					found = found || tool.Get("type").String() == "image_generation"
				}
				if found != (path == "/v1/images/generations") {
					t.Fatalf("image tool present=%t path=%s body=%s", found, path, body)
				}
			})
		}
	}
}
