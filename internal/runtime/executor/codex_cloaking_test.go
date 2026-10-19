package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestApplyCodexHeadersCloaking(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		globalDisabled bool
		keyDisabled    *string
		wantAgent      string
		wantOrigin     string
	}{
		{name: "default", wantAgent: codexUserAgent, wantOrigin: codexOriginator},
		{name: "global disabled", globalDisabled: true, wantAgent: "custom-ua", wantOrigin: "custom-origin"},
		{name: "key disables", keyDisabled: cloakingAttribute("true"), wantAgent: "custom-ua", wantOrigin: "custom-origin"},
		{name: "key enables over global", globalDisabled: true, keyDisabled: cloakingAttribute("false"), wantAgent: codexUserAgent, wantOrigin: codexOriginator},
		{name: "key inherits enabled", keyDisabled: nil, wantAgent: codexUserAgent, wantOrigin: codexOriginator},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: testCase.globalDisabled}}
			auth := &cliproxyauth.Auth{
				Provider: "codex",
				Attributes: map[string]string{
					"api_key":           "api-key",
					"header:User-Agent": "custom-ua",
					"header:Originator": "custom-origin",
				},
			}
			if testCase.keyDisabled != nil {
				auth.Attributes[cliproxyauth.AttributeCodexDisableCloaking] = *testCase.keyDisabled
			}
			for _, stream := range []bool{false, true} {
				req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
				if err != nil {
					t.Fatalf("NewRequest() error = %v", err)
				}
				applyCodexHeadersFromSources(req, auth, "api-key", stream, cfg, http.Header{}, nil)
				if got := req.Header.Get("User-Agent"); got != testCase.wantAgent {
					t.Fatalf("HTTP stream=%t User-Agent = %q, want %q", stream, got, testCase.wantAgent)
				}
				if got := req.Header.Get("Originator"); got != testCase.wantOrigin {
					t.Fatalf("HTTP stream=%t Originator = %q, want %q", stream, got, testCase.wantOrigin)
				}
			}
			for _, native := range []bool{false, true} {
				headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "api-key", cfg, native)
				if got := headers.Get("User-Agent"); got != testCase.wantAgent {
					t.Fatalf("WebSocket native=%t User-Agent = %q, want %q", native, got, testCase.wantAgent)
				}
				if got := headers.Get("Originator"); got != testCase.wantOrigin {
					t.Fatalf("WebSocket native=%t Originator = %q, want %q", native, got, testCase.wantOrigin)
				}
			}
		})
	}
}

func cloakingAttribute(value string) *string {
	return &value
}

func TestCodexCredentialCloakingControlsNativeWebsocketSessionHeaders(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		globalDisabled   bool
		keyDisabled      string
		wantSession      string
		wantConversation string
	}{
		{name: "key disabled", keyDisabled: "true"},
		{name: "key enabled over global", globalDisabled: true, keyDisabled: "false", wantSession: "generated-session", wantConversation: "generated-conversation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: testCase.globalDisabled}}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key": "api-key",
				cliproxyauth.AttributeCodexDisableCloaking: testCase.keyDisabled,
			}}
			headers := applyCodexWebsocketHeaders(context.Background(), http.Header{
				"Session-Id":      {"generated-session"},
				"Conversation_id": {"generated-conversation"},
			}, auth, "api-key", cfg, true)
			if got := headers.Get("Session-Id"); got != testCase.wantSession {
				t.Fatalf("Session-Id = %q, want %q", got, testCase.wantSession)
			}
			if got := headers.Get("Conversation_id"); got != testCase.wantConversation {
				t.Fatalf("Conversation_id = %q, want %q", got, testCase.wantConversation)
			}
		})
	}
}

func TestCodexCredentialCloakingReachesOutboundExecutions(t *testing.T) {
	const terminal = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	for _, testCase := range []struct {
		name           string
		globalDisabled bool
		keyDisabled    string
		wantAgent      string
		wantOrigin     string
	}{
		{name: "key disables", keyDisabled: "true", wantAgent: "custom-ua", wantOrigin: "custom-origin"},
		{name: "key enables over global", globalDisabled: true, keyDisabled: "false", wantAgent: codexUserAgent, wantOrigin: codexOriginator},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			upstreamHeaders := make(chan http.Header, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHeaders <- r.Header.Clone()
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					w.WriteHeader(http.StatusUpgradeRequired)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + terminal + "\n\n"))
			}))
			t.Cleanup(server.Close)
			cfg := &config.Config{
				DisableImageGeneration: config.DisableImageGenerationAll,
				Codex:                  config.CodexConfig{DisableCodexCloaking: testCase.globalDisabled},
			}
			auth := &cliproxyauth.Auth{
				Provider: "codex",
				Attributes: map[string]string{
					"api_key": "test-key", "base_url": server.URL,
					"header:User-Agent": "custom-ua", "header:Originator": "custom-origin",
					cliproxyauth.AttributeCodexDisableCloaking: testCase.keyDisabled,
				},
			}
			req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}

			httpExecutor := newCodexExecutor(cfg)
			if _, err := httpExecutor.Execute(context.Background(), auth, req, opts); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			verifyCodexCloakingRequest(t, <-upstreamHeaders, testCase.wantAgent, testCase.wantOrigin)

			opts.Stream = true
			stream, err := httpExecutor.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
				}
			}
			verifyCodexCloakingRequest(t, <-upstreamHeaders, testCase.wantAgent, testCase.wantOrigin)

			websocketExecutor := newCodexWebsocketsExecutor(cfg)
			_, err = websocketExecutor.ExecuteStream(context.Background(), auth, req, opts)
			if statusErrCode(err) != http.StatusUpgradeRequired {
				t.Fatalf("WebSocket ExecuteStream() error = %v, want 426", err)
			}
			verifyCodexCloakingRequest(t, <-upstreamHeaders, testCase.wantAgent, testCase.wantOrigin)
		})
	}
}

func verifyCodexCloakingRequest(t *testing.T, headers http.Header, wantAgent, wantOrigin string) {
	t.Helper()
	if got := headers.Get("User-Agent"); got != wantAgent {
		t.Fatalf("outbound User-Agent = %q, want %q", got, wantAgent)
	}
	if got := headers.Get("Originator"); got != wantOrigin {
		t.Fatalf("outbound Originator = %q, want %q", got, wantOrigin)
	}
}
