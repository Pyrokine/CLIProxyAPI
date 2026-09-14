package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestApplyCodexHeadersCloaking(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":           "api-key",
			"header:User-Agent": "custom-ua",
			"header:Originator": "custom-origin",
		},
	}

	for _, testCase := range []struct {
		name       string
		disabled   bool
		wantAgent  string
		wantOrigin string
	}{
		{name: "enabled", wantAgent: codexUserAgent, wantOrigin: codexOriginator},
		{name: "disabled", disabled: true, wantAgent: "custom-ua", wantOrigin: "custom-origin"},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: testCase.disabled}}
				req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
				if err != nil {
					t.Fatalf("NewRequest() error = %v", err)
				}

				applyCodexHeadersFromSources(req, auth, "api-key", false, cfg, http.Header{})
				if got := req.Header.Get("User-Agent"); got != testCase.wantAgent {
					t.Fatalf("HTTP User-Agent = %q, want %q", got, testCase.wantAgent)
				}
				if got := req.Header.Get("Originator"); got != testCase.wantOrigin {
					t.Fatalf("HTTP Originator = %q, want %q", got, testCase.wantOrigin)
				}

				headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "api-key", cfg)
				if got := headers.Get("User-Agent"); got != testCase.wantAgent {
					t.Fatalf("WebSocket User-Agent = %q, want %q", got, testCase.wantAgent)
				}
				if got := headers.Get("Originator"); got != testCase.wantOrigin {
					t.Fatalf("WebSocket Originator = %q, want %q", got, testCase.wantOrigin)
				}
			},
		)
	}
}
