package executor

import (
	"context"
	"fmt"
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
		{name: "key disables", keyDisabled: new("true"), wantAgent: "custom-ua", wantOrigin: "custom-origin"},
		{
			name: "key enables over global", globalDisabled: true, keyDisabled: new("false"), wantAgent: codexUserAgent,
			wantOrigin: codexOriginator,
		},
		{name: "key inherits enabled", keyDisabled: nil, wantAgent: codexUserAgent, wantOrigin: codexOriginator},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
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
					headers := applyCodexWebsocketHeaders(
						context.Background(), http.Header{}, auth, "api-key", cfg, native,
					)
					if got := headers.Get("User-Agent"); got != testCase.wantAgent {
						t.Fatalf(
							"WebSocket native=%t User-Agent = %q, want %q",
							native,
							got,
							testCase.wantAgent,
						)
					}
					if got := headers.Get("Originator"); got != testCase.wantOrigin {
						t.Fatalf(
							"WebSocket native=%t Originator = %q, want %q",
							native,
							got,
							testCase.wantOrigin,
						)
					}
				}
			},
		)
	}
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
		{
			name: "key enabled over global", globalDisabled: true, keyDisabled: "false",
			wantSession: "generated-session", wantConversation: "generated-conversation",
		},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: testCase.globalDisabled}}
				auth := &cliproxyauth.Auth{
					Attributes: map[string]string{
						"api_key": "api-key",
						cliproxyauth.AttributeCodexDisableCloaking: testCase.keyDisabled,
					},
				}
				headers := applyCodexWebsocketHeaders(
					context.Background(), http.Header{
						"Session-Id":      {"generated-session"},
						"Conversation_id": {"generated-conversation"},
					}, auth, "api-key", cfg, true,
				)
				if got := headers.Get("Session-Id"); got != testCase.wantSession {
					t.Fatalf("Session-Id = %q, want %q", got, testCase.wantSession)
				}
				if got := headers.Get("Conversation_id"); got != testCase.wantConversation {
					t.Fatalf("Conversation_id = %q, want %q", got, testCase.wantConversation)
				}
			},
		)
	}
}

func TestCodexCredentialCloakingReachesOutboundExecutions(t *testing.T) {
	const terminal = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]` +
		`,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	for _, testCase := range []struct {
		name           string
		globalDisabled bool
		keyDisabled    string
		wantAgent      string
		wantOrigin     string
	}{
		{name: "key disables", keyDisabled: "true", wantAgent: "custom-ua", wantOrigin: "custom-origin"},
		{
			name: "key enables over global", globalDisabled: true, keyDisabled: "false", wantAgent: codexUserAgent,
			wantOrigin: codexOriginator,
		},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				upstreamHeaders := make(chan http.Header, 3)
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							upstreamHeaders <- r.Header.Clone()
							if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
								w.WriteHeader(http.StatusUpgradeRequired)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = w.Write([]byte("data: " + terminal + "\n\n"))
						},
					),
				)
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
				req := cliproxyexecutor.Request{
					Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
				}
				opts := cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatOpenAIResponse,
					ResponseFormat: sdktranslator.FormatOpenAIResponse,
				}

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
			},
		)
	}
}

func TestCodexConfiguredDefaultsRespectCredentialBoundary(t *testing.T) {
	for _, kind := range []string{"oauth", "apikey"} {
		for _, withCaller := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/caller=%t", kind, withCaller), func(t *testing.T) {
					cfg := &config.Config{
						Codex: config.CodexConfig{DisableCodexCloaking: true},
						CodexHeaderDefaults: config.CodexHeaderDefaults{
							UserAgent: "configured-client/1.0", BetaFeatures: "configured-feature",
						},
					}
					auth := &cliproxyauth.Auth{
						Provider:   "codex",
						Attributes: map[string]string{"auth_kind": kind},
					}
					if kind == "apikey" {
						auth.Attributes["api_key"] = "synthetic-upstream-key"
					}
					caller := http.Header{}
					wantAgent, wantBeta := codexUserAgent, ""
					if kind == "oauth" {
						wantAgent, wantBeta = cfg.CodexHeaderDefaults.UserAgent, cfg.CodexHeaderDefaults.BetaFeatures
					}
					if withCaller {
						caller.Set("User-Agent", "caller-agent/1.0")
						caller.Set("X-Codex-Beta-Features", "caller-feature")
						caller.Set("Authorization", "Bearer caller-secret")
						wantAgent, wantBeta = "caller-agent/1.0", "caller-feature"
					}
					req, err := http.NewRequestWithContext(
						t.Context(), http.MethodPost, "https://upstream.invalid/responses", nil,
					)
					if err != nil {
						t.Fatal(err)
					}
					applyCodexHeaders(req, auth, "synthetic-upstream-key", false, cfg, caller)
					wsHeaders := applyCodexWebsocketHeaders(
						t.Context(), nil, auth, "synthetic-upstream-key", cfg, true, caller,
					)
					for _, headers := range []http.Header{req.Header, wsHeaders} {
						if headers.Get("User-Agent") != wantAgent {
							t.Errorf("%s User-Agent = %q, want %q", kind, headers.Get("User-Agent"), wantAgent)
						}
						if headers.Get("Authorization") != "Bearer synthetic-upstream-key" {
							t.Fatal("caller headers changed the selected upstream credential")
						}
					}
					if got := wsHeaders.Get("X-Codex-Beta-Features"); got != wantBeta {
						t.Errorf("WebSocket beta features = %q, want %q", got, wantBeta)
					}
				},
			)
		}
	}
	for _, test := range []struct {
		name          string
		configAgent   string
		configBeta    string
		existingAgent string
		existingBeta  string
		callerAgent   string
		callerBeta    string
		apiKey        string
		wantAgent     string
		wantBeta      string
	}{
		{name: "original-defaults", wantAgent: codexUserAgent},
		{
			name: "original-oauth-config", configAgent: "my-codex-client/1.0", configBeta: "feature-a,feature-b",
			wantAgent: "my-codex-client/1.0", wantBeta: "feature-a,feature-b",
		},
		{
			name: "original-existing-client-config", configAgent: "config-ua", configBeta: "config-beta",
			existingAgent: "existing-ua", existingBeta: "existing-beta", callerAgent: "client-ua",
			callerBeta: "client-beta",
			wantAgent:  "client-ua", wantBeta: "client-beta",
		},
		{
			name: "original-client-config", configAgent: "config-ua", configBeta: "config-beta",
			callerAgent: "client-ua", callerBeta: "client-beta", wantAgent: "client-ua", wantBeta: "client-beta",
		},
		{
			name: "original-apikey-config", configAgent: "config-ua", configBeta: "config-beta", apiKey: "sk-test",
			wantAgent: codexUserAgent,
		},
	} {
		for _, native := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/native=%t", test.name, native), func(t *testing.T) {
					var cfg *config.Config
					var auth *cliproxyauth.Auth
					if test.name != "original-defaults" {
						cfg = &config.Config{
							Codex: config.CodexConfig{DisableCodexCloaking: true},
							CodexHeaderDefaults: config.CodexHeaderDefaults{
								UserAgent: test.configAgent, BetaFeatures: test.configBeta,
							},
						}
						auth = &cliproxyauth.Auth{
							Provider: "codex", Metadata: map[string]any{"email": "user@example.com"},
						}
						if test.apiKey != "" {
							auth.Metadata = nil
							auth.Attributes = map[string]string{"api_key": test.apiKey}
						}
					}
					headers := http.Header{}
					if test.existingAgent != "" {
						headers.Set("User-Agent", test.existingAgent)
						headers.Set("X-Codex-Beta-Features", test.existingBeta)
					}
					ctx := contextWithGinHeaders(
						map[string]string{
							"User-Agent": test.callerAgent, "X-Codex-Beta-Features": test.callerBeta,
						},
					)
					got := applyCodexWebsocketHeaders(ctx, headers, auth, test.apiKey, cfg, native)
					if got.Get("User-Agent") != test.wantAgent ||
						got.Get("X-Codex-Beta-Features") != test.wantBeta {
						t.Fatalf(
							"User-Agent=%q beta=%q, want %q %q", got.Get("User-Agent"),
							got.Get("X-Codex-Beta-Features"), test.wantAgent, test.wantBeta,
						)
					}
					if got.Get("OpenAI-Beta") != codexResponsesWebsocketBetaHeaderValue ||
						got.Get("Version") != "" {
						t.Fatalf("unexpected beta/version headers: %v", got)
					}
				},
			)
		}
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
