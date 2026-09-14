package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexResponsesBodyUsesResolvedModelIsCompat(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{OptimizeMultiAgentV2: true}}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Headers:      http.Header{"User-Agent": []string{"codex-tui/0.145.0"}},
	}
	payload := []byte(`{"model":"compat-model","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"task"}]}]}`)

	build := func(isCompat bool) codexResponsesBodyResult {
		t.Helper()
		req := cliproxyexecutor.Request{
			Model:   "compat-model",
			Payload: payload,
			Metadata: map[string]any{
				"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: isCompat},
			},
		}
		prepared, err := codexResponsesBody(
			context.Background(),
			cfg,
			&cliproxyauth.Auth{},
			"compat-model",
			"compat-model",
			"codex",
			sdktranslator.FormatOpenAIResponse,
			opts,
			req,
			false,
		)
		if err != nil {
			t.Fatalf("codexResponsesBody() error = %v", err)
		}
		return prepared
	}

	if got := gjson.GetBytes(build(false).Body, "input.0.type").String(); got != "agent_message" {
		t.Fatalf("non-compat input type = %q, want agent_message", got)
	}
	compat := build(true)
	if got := gjson.GetBytes(compat.Body, "input.0.type").String(); got != "message" {
		t.Fatalf("compat input type = %q, want message; output=%s", got, compat.Body)
	}
	if got := gjson.GetBytes(compat.Body, "input.0.content.0.text").String(); got != "task" {
		t.Fatalf("compat input text = %q, want task; output=%s", got, compat.Body)
	}
}
