package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

func TestCodexExplicitOAuthWithoutAccessTokenDoesNotUseLegacyKey(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex", Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "api_key": "synthetic-retained-key",
		},
	}
	if token, _ := codexCreds(auth); token != "" {
		t.Fatal("OAuth without an access token selected the retained API key")
	}
	request, err := http.NewRequest(http.MethodPost, "https://identity-contract.invalid/raw", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer synthetic-previous-auth")
	if err := newCodexExecutor(nil).PrepareRequest(request, auth); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("missing OAuth access token retained an authorization header: %q", got)
	}
}

func TestCodexExplicitOAuthImageRouteIgnoresRetainedAPIKeyConfig(t *testing.T) {
	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey: "synthetic-retained-key", BaseURL: "https://identity-contract.invalid",
				Models: []config.CodexModel{{Name: "tenant/gpt-image-2"}},
			},
		},
	}
	for _, oauth := range []bool{false, true} {
		auth := &coreauth.Auth{
			Attributes: map[string]string{
				"api_key": "synthetic-retained-key", "base_url": cfg.CodexKey[0].BaseURL,
				coreauth.AttributeConfigIndex: "0",
			},
		}
		want := "tenant/gpt-image-2"
		if oauth {
			auth.Attributes[coreauth.AttributeAuthKind] = coreauth.AuthKindOAuth
			want = "gpt-image-2"
		}
		got := newCodexExecutor(cfg).directOpenAIImageModel(
			auth,
			coreexecutor.Request{Model: "tenant/gpt-image-2", Payload: []byte(`{"model":"gpt-image-2"}`)},
			codexOpenAIImageTestOptions(codexImagesGenerationsPath, false),
		)
		if got != want {
			t.Errorf("OAuth=%t image route=%q, want %q", oauth, got, want)
		}
	}
}

func TestCodexExplicitOAuthIdentityReachesHTTPAndWebsocket(t *testing.T) {
	for _, kind := range []string{"attribute-oauth", "metadata-oauth", "legacy-api-key", "explicit-api-key"} {
		for _, transport := range []string{"http", "http-stream", "websocket", "raw-http"} {
			t.Run(
				kind+"/"+transport, func(t *testing.T) {
					const terminal = `{"type":"response.completed","response":{"id":"synthetic-response","` +
						`object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"` +
						`input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
					seen := make(chan http.Header, 1)
					server := httptest.NewServer(
						http.HandlerFunc(
							func(w http.ResponseWriter, r *http.Request) {
								seen <- r.Header.Clone()
								if transport == "websocket" {
									conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
									if err != nil {
										t.Error(err)
										return
									}
									defer func() { _ = conn.Close() }()
									if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
										t.Error(err)
										return
									}
									if _, _, err := conn.ReadMessage(); err != nil {
										t.Error(err)
										return
									}
									if err := conn.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
										t.Error(err)
									}
									return
								}
								if _, err := io.Copy(io.Discard, r.Body); err != nil {
									t.Error(err)
									return
								}
								w.Header().Set("Content-Type", "text/event-stream")
								if _, err := io.WriteString(w, "data: "+terminal+"\n\n"); err != nil {
									t.Error(err)
								}
							},
						),
					)
					defer server.Close()
					cfg := &config.Config{
						Codex:                  config.CodexConfig{DisableCodexCloaking: true},
						DisableImageGeneration: config.DisableImageGenerationAll,
						CodexHeaderDefaults: config.CodexHeaderDefaults{
							UserAgent: "codex_cli_rs/0.132.0 (Linux; x64)", BetaFeatures: "synthetic-feature",
						},
					}
					auth := &coreauth.Auth{
						ID: t.Name(), Provider: "codex", ProxyURL: "direct",
						Attributes: map[string]string{
							"api_key":  "synthetic-retained-key",
							"base_url": server.URL,
						},
						Metadata: map[string]any{
							"access_token": "synthetic-current-oauth", "account_id": "synthetic-account",
							"email": "synthetic@example.invalid",
						},
					}
					wantToken, wantAccount, wantOriginator, wantAgent := "synthetic-retained-key", "", "",
						codexUserAgent
					if kind == "attribute-oauth" || kind == "metadata-oauth" {
						wantToken, wantAccount, wantOriginator, wantAgent = "synthetic-current-oauth", "synthetic-"+
							"account", codexOriginator, cfg.CodexHeaderDefaults.UserAgent
						if kind == "attribute-oauth" {
							auth.Attributes[coreauth.AttributeAuthKind] = coreauth.AuthKindOAuth
						} else {
							auth.Metadata[coreauth.AttributeAuthKind] = coreauth.AuthKindOAuth
						}
					} else if kind == "explicit-api-key" {
						auth.Attributes[coreauth.AttributeAuthKind] = coreauth.AuthKindAPIKey
					}
					before := auth.Clone()
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					request := coreexecutor.Request{
						Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"identity fixture"}`),
					}
					opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
					exec := newCodexExecutor(cfg)
					switch transport {
					case "http":
						if _, err := exec.Execute(ctx, auth, request, opts); err != nil {
							t.Fatal(err)
						}
					case "http-stream", "websocket":
						var stream *coreexecutor.StreamResult
						var err error
						if transport == "websocket" {
							ws := newCodexWebsocketsExecutor(cfg)
							defer ws.CloseExecutionSession(coreauth.CloseAllExecutionSessionsID)
							stream, err = ws.ExecuteStream(ctx, auth, request, opts)
						} else {
							stream, err = exec.ExecuteStream(ctx, auth, request, opts)
						}
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range stream.Chunks {
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(chunk.Err)
							}
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					case "raw-http":
						raw, err := http.NewRequestWithContext(
							ctx, http.MethodPost, server.URL+"/raw", bytes.NewReader(request.Payload),
						)
						if err != nil {
							t.Fatal(err)
						}
						response, err := exec.HttpRequest(ctx, auth, raw)
						if err != nil {
							t.Fatal(err)
						}
						if err := response.Body.Close(); err != nil {
							t.Fatal(err)
						}
					}
					var headers http.Header
					select {
					case headers = <-seen:
					case <-ctx.Done():
						t.Fatal("synthetic upstream did not receive the request")
					}
					if got := headers.Get("Authorization"); got != "Bearer "+wantToken {
						t.Errorf("Authorization=%q, want the selected credential kind", got)
					}
					if transport != "raw-http" {
						for header, expected := range map[string]string{
							"ChatGPT-Account-ID": wantAccount, "Originator": wantOriginator, "User-Agent": wantAgent,
						} {
							if got := headers.Get(header); got != expected {
								t.Errorf("%s=%q, want %q", header, got, expected)
							}
						}
						if transport == "websocket" {
							wantBeta := ""
							if kind == "attribute-oauth" || kind == "metadata-oauth" {
								wantBeta = cfg.CodexHeaderDefaults.BetaFeatures
							}
							if got := headers.Get("X-Codex-Beta-Features"); got != wantBeta {
								t.Errorf("OAuth beta features=%q, want %q", got, wantBeta)
							}
						}
					}
					if auth.Attributes["api_key"] != before.Attributes["api_key"] ||
						auth.Metadata["access_token"] != before.Metadata["access_token"] {
						t.Fatal("request preparation mutated retained credential fields")
					}
				},
			)
		}
	}
}
