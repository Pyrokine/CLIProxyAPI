package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func auditProtocolFinalWire(
	t *testing.T,
	provider, model string,
	source translator.Format,
	payload []byte,
	stream bool,
) []byte {
	t.Helper()
	cfg := &config.Config{}
	field := "generationConfig.temperature"
	var value any = 0.25
	if provider == "claude" {
		field, value = "max_tokens", 16
	}
	cfg.Payload.Override = []config.PayloadRule{
		{
			Models: []config.PayloadModelRule{{Name: "*", Protocol: provider}}, Params: map[string]any{field: value},
		},
	}
	auth := googlePayloadAuth(provider)
	auth.ID = "synthetic-protocol-wire-" + t.Name()
	delete(auth.Attributes, "interactions")
	if !tokenExpiry(auth.Metadata).After(time.Now().Add(refreshSkew)) {
		t.Fatal("synthetic token would enter expiry refresh; no request issued")
	}
	var wire []byte
	calls := 0
	transport := &auditClaudeContractTransport{
		respond: func(req *http.Request) (*http.Response, error) {
			calls++
			if req.URL.Host != "payload-contract.invalid" &&
				!(provider == "claude" && req.URL.Host == "api.anthropic.com") {
				return nil, fmt.Errorf("unexpected isolated upstream route %s", req.URL)
			}
			var err error
			wire, err = io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"fi` +
				`nishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidates` +
				`TokenCount":1,"totalTokenCount":3}}`
			if provider == "antigravity" {
				body = `{"response":` + body + `}`
			}
			if strings.Contains(req.URL.Path, "streamGenerateContent") {
				body = "data: " + body + "\n\n"
			}
			if provider == "claude" {
				body = `{"id":"synthetic-message","type":"message","role":"assistant","model":"claude-sonnet-` +
					`4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"` +
					`input_tokens":1,"output_tokens":1}}`
				if stream {
					body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"" +
						"synthetic-message\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-" +
						"sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
						"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":" +
						"\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"" +
						"type\":\"message_stop\"}\n\n"
				}
			}
			return googlePayloadResponse(req, body), nil
		},
	}
	ctx, cancel := context.WithTimeout(translator.WithPluginHooks(t.Context(), nil), 5*time.Second)
	defer cancel()
	ctx = util.WithRoundTripper(ctx, transport)
	var implementation coreauth.ProviderExecutor
	response := translator.FormatGemini
	switch provider {
	case "gemini":
		if newProxyAwareHTTPClient(ctx, cfg, auth, 0).Transport != transport || shouldExecuteNativeInteractions(
			auth, executor.Options{SourceFormat: source},
		) {
			t.Fatal("actual Gemini generate-content client is not isolated; no request issued")
		}
		implementation = NewGeminiExecutor(cfg)
	case "antigravity":
		if newAntigravityHTTPClient(ctx, cfg, auth, 0).Transport != transport {
			t.Fatal("actual Antigravity client is not isolated; no request issued")
		}
		implementation = NewAntigravityExecutor(cfg)
		response = translator.FormatAntigravity
	case "claude":
		client := newClaudeUtlsHTTPClient(ctx, cfg, auth, 0)
		selected, ok := client.Transport.(*claudeFallbackRoundTripper)
		if !ok || selected.anthropic != transport || selected.chrome != transport || selected.fallback != transport {
			t.Fatal("actual Claude transport branches are not isolated; no request issued")
		}
		implementation = NewClaudeExecutor(cfg)
		response = translator.FormatClaude
	default:
		t.Fatalf("unsupported final-wire provider %q", provider)
	}
	original := bytes.Clone(payload)
	request := executor.Request{Model: model, Payload: payload}
	opts := executor.Options{SourceFormat: source, ResponseFormat: response, Stream: stream}
	var err error
	if stream {
		var result *executor.StreamResult
		result, err = implementation.ExecuteStream(ctx, auth, request, opts)
		if err == nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		}
	} else {
		_, err = implementation.Execute(ctx, auth, request, opts)
	}
	if err != nil || calls != 1 || !bytes.Equal(original, payload) {
		t.Fatalf("final request error=%v calls=%d input unchanged=%t", err, calls, bytes.Equal(original, payload))
	}
	return wire
}
