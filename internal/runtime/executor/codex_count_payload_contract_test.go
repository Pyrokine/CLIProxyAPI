package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexCountTokensUsesFinalRequestPayload(t *testing.T) {
	marker := strings.Repeat("synthetic operator instructions affect the provider prompt ", 100)
	for _, test := range []struct {
		name    string
		payload string
		rules   config.PayloadConfig
	}{
		{name: "override", payload: `{"input":"hi"}`, rules: config.PayloadConfig{
			Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "codex"}}, Params: map[string]any{"instructions": marker}}},
		}},
		{name: "default", payload: `{"input":"hi"}`, rules: config.PayloadConfig{
			Default: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "codex"}}, Params: map[string]any{"instructions": marker}}},
		}},
		{name: "remove", payload: `{"input":"hi","instructions":"caller-provided instruction"}`, rules: config.PayloadConfig{
			Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "codex"}}, Params: []string{"instructions"}}},
		}},
		{name: "final model", payload: `{"input":"hi"}`, rules: config.PayloadConfig{
			Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "codex"}}, Params: map[string]any{"model": "gpt-4", "instructions": marker}}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll, Payload: test.rules}
			executor := NewCodexAutoExecutor(cfg)
			calls := 0
			var wire []byte
			ctx := util.WithRoundTripper(context.Background(), devinTestRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != "127.0.0.1:1" || request.URL.Path != "/responses" {
					t.Errorf("unexpected synthetic target: %s", request.URL)
				}
				calls++
				var err error
				wire, err = io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + runtimeCodexCompleted + "\n\n")), Request: request}, nil
			}))
			auth := runtimeCodexAuth("http://127.0.0.1:1")
			req := core.Request{Model: "gpt-5.4", Payload: []byte(test.payload)}
			opts := core.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
			if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(wire) == 0 {
				t.Fatalf("provider request count = %d, wire bytes = %d", calls, len(wire))
			}
			counted, err := executor.CountTokens(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("local token count dispatched an upstream request")
			}
			codec, err := tokenizerForCodexModel(gjson.GetBytes(wire, "model").String())
			if err != nil {
				t.Fatal(err)
			}
			want, err := countCodexInputTokens(codec, wire)
			if err != nil {
				t.Fatal(err)
			}
			got := gjson.GetBytes(counted.Payload, "response.usage.input_tokens").Int()
			if got != want {
				t.Fatalf("local count = %d, actual provider prompt count = %d", got, want)
			}
			if test.name != "remove" && want < 500 {
				t.Fatalf("operator instructions were not consumed: %d", want)
			}
		})
	}
}
