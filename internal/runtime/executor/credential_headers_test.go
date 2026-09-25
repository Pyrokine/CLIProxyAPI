package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestPrepareRequestClearsStaleCredentialHeaders(t *testing.T) {
	cfg := &config.Config{}
	tests := []struct {
		name    string
		prepare func(*http.Request, *cliproxyauth.Auth) error
	}{
		{name: "claude", prepare: NewClaudeExecutor(cfg).PrepareRequest},
		{name: "codex", prepare: newCodexExecutor(cfg).PrepareRequest},
		{name: "gemini", prepare: NewGeminiExecutor(cfg).PrepareRequest},
		{name: "openai-compat", prepare: NewOpenAICompatExecutor("compat", cfg).PrepareRequest},
		{name: "kimi", prepare: NewKimiExecutor(cfg).PrepareRequest},
		{name: "iflow", prepare: NewIFlowExecutor(cfg).PrepareRequest},
		{name: "qwen", prepare: NewQwenExecutor(cfg).PrepareRequest},
		{name: "xai", prepare: (&XAIExecutor{cfg: cfg}).PrepareRequest},
		{name: "devin", prepare: NewDevinExecutor(cfg).PrepareRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			request.Header.Set("Authorization", "Bearer stale")
			request.Header.Set("X-Api-Key", "stale-claude")
			request.Header.Set("X-Goog-Api-Key", "stale-gemini")

			if errPrepare := test.prepare(request, &cliproxyauth.Auth{}); errPrepare != nil {
				t.Fatalf("PrepareRequest() error = %v", errPrepare)
			}
			for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
				if value := request.Header.Get(name); value != "" {
					t.Fatalf("stale %s = %q", name, value)
				}
			}
		})
	}
}

func TestWebsocketHeadersClearStaleCredentialSet(t *testing.T) {
	headers := http.Header{
		"Authorization":  {"Bearer stale"},
		"X-Api-Key":      {"stale-claude"},
		"X-Goog-Api-Key": {"stale-gemini"},
	}
	got := applyCodexWebsocketHeaders(context.Background(), headers, nil, "", &config.Config{}, false)
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		if value := got.Get(name); value != "" {
			t.Fatalf("stale websocket %s = %q", name, value)
		}
	}
	if value := applyXAIWebsocketHeaders(nil, "", "").Get("Authorization"); value != "" {
		t.Fatalf("empty xAI websocket credential = %q", value)
	}
}
