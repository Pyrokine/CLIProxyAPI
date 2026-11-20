package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestCustomSessionHeaderSourcesPreserveCanonicalPriority(t *testing.T) {
	metadata := logging.WithClientRequestMetadata(t.Context(), logging.ClientRequestMetadata{SessionID: "sdk-session"})
	for _, test := range []struct {
		name, header, value, want string
		ctx                       context.Context
	}{
		{name: "SDK metadata", header: "X-Session-ID", value: "other-header", want: "sdk-session", ctx: metadata},
		{name: "Codex header", header: "Session-Id", value: "codex-thread-abc", want: "codex:codex-thread-abc"},
		{
			name: "generic header", header: "X-Session-ID", value: "gemini-session-123",
			want: "header:gemini-session-123",
		},
		{
			name: "affinity header", header: "X-Session-Affinity", value: "interactions-sess-456",
			want: "affinity:interactions-sess-456",
		},
		{
			name: "Claude header", header: "X-Claude-Code-Session-Id", value: "claude-sess-789",
			want: "claude:claude-sess-789",
		},
		{
			name: "canonical context", header: "Session-Id", value: "other-header", want: "canonical-session",
			ctx: util.WithSessionID(metadata, "canonical-session"),
		},
		{name: "explicit clear", header: "Session-Id", value: "other-header", ctx: util.WithSessionID(metadata, "")},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				ctx := test.ctx
				if ctx == nil {
					ctx = t.Context()
				}
				request := httptest.NewRequest(
					http.MethodPost, "https://custom-header.invalid/responses", nil,
				).WithContext(ctx)
				incoming := http.Header{}
				incoming.Set(test.header, test.value)
				util.ApplyCustomHeadersFromAttrs(
					request, map[string]string{
						"header:X-Session": "$CPA-SESSION-ID", "header:X-Lower-Case": "$cpa-session-id",
					}, incoming,
				)
				if request.Header.Get("X-Session") != test.want || request.Header.Get("X-Lower-Case") != test.want {
					t.Fatalf("custom session source=%#v, want %q", request.Header, test.want)
				}
			},
		)
	}
}

func TestLoggingSessionMetadataPreservesSnapshotAndExplicitContext(t *testing.T) {
	empty := logging.WithClientRequestMetadata(t.Context(), logging.ClientRequestMetadata{})
	if util.HasExplicitSessionID(empty) {
		t.Fatal("metadata without session created a request session boundary")
	}
	first := logging.WithClientRequestMetadata(empty, logging.ClientRequestMetadata{SessionID: "first-session"})
	second := logging.WithClientRequestMetadata(first, logging.ClientRequestMetadata{SessionID: "second-session"})
	cleared := logging.WithClientRequestMetadata(second, logging.ClientRequestMetadata{})
	if util.SessionIDFromContext(first) != "first-session" || util.SessionIDFromContext(second) != "second-session" ||
		util.SessionIDFromContext(
			cleared,
		) != "" || logging.GetClientRequestMetadata(first).SessionID != "first-session" {
		t.Fatal("metadata session updates mutated an old snapshot or retained a cleared session")
	}
	for _, explicit := range []string{"canonical-session", ""} {
		ctx := logging.WithClientRequestMetadata(
			util.WithSessionID(first, explicit),
			logging.ClientRequestMetadata{SessionID: "metadata-session"},
		)
		if util.SessionIDFromContext(ctx) != explicit {
			t.Fatalf("metadata replaced explicit session %q", explicit)
		}
	}
}

func TestCustomSessionOriginalSourcesReachProviderExecutions(t *testing.T) {
	for _, test := range []struct {
		name, provider, model, payload, response, header, value, want string
		format                                                        translator.Format
		factory                                                       func(*config.Config) auth.ProviderExecutor
	}{
		{
			name: "OpenAI compatibility", provider: "openai-compatibility", model: "gpt-4o",
			payload:  `{"messages":[{"role":"user","content":"hi"}]}`,
			response: `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
			header:   "Session-Id", value: "codex-thread-abc", want: "codex:codex-thread-abc",
			format: translator.FormatOpenAI,
			factory: func(cfg *config.Config) auth.ProviderExecutor {
				return NewOpenAICompatExecutor("openai-compatibility", cfg)
			},
		},
		{
			name: "Gemini", provider: "gemini", model: "gemini-2.5-flash",
			payload:  `{"contents":[{"parts":[{"text":"hi"}]}]}`,
			response: `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`,
			header:   "X-Session-ID", value: "gemini-session-123", want: "header:gemini-session-123",
			format:  translator.FormatGemini,
			factory: func(cfg *config.Config) auth.ProviderExecutor { return NewGeminiExecutor(cfg) },
		},
		{
			name: "Gemini Interactions", provider: "gemini-interactions", model: "gemini-3.1-flash-lite",
			payload:  `{"messages":[{"role":"user","content":"hi"}]}`,
			response: `{"id":"interaction_1","status":"completed","outputs":[{"text":"ok"}]}`,
			header:   "X-Session-Affinity", value: "interactions-sess-456", want: "affinity:interactions-sess-456",
			format:  translator.FormatOpenAI,
			factory: func(cfg *config.Config) auth.ProviderExecutor { return NewGeminiInteractionsExecutor(cfg) },
		},
		{
			name: "Claude", provider: "claude", model: "claude-3-7-sonnet-20250219",
			payload:  `{"messages":[{"role":"user","content":"hi"}]}`,
			response: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`,
			header:   "X-Claude-Code-Session-Id", value: "claude-sess-789", want: "claude:claude-sess-789",
			format:  translator.FormatClaude,
			factory: func(cfg *config.Config) auth.ProviderExecutor { return NewClaudeExecutor(cfg) },
		},
		{
			name: "Codex", provider: "codex", model: "gpt-5-codex",
			payload: `{"messages":[{"role":"user","content":"hi"}]}`,
			response: "data: " + `{"type":"response.completed","response":{"id":"resp_1","object":"response",` +
				`"created_at":0,"status":"completed","background":false,"error":null,"output":[]}}` + "\n\n",
			header: "Session-Id", value: "codex-sess-uuid", want: "codex:codex-sess-uuid",
			format:  translator.FormatCodex,
			factory: func(cfg *config.Config) auth.ProviderExecutor { return newCodexExecutor(cfg) },
		},
		{
			name: "xAI", provider: "xai", model: "grok-2",
			payload: `{"messages":[{"role":"user","content":"hi"}]}`,
			response: "data: " + `{"type":"response.completed","response":{"id":"resp_1","object":"response",` +
				`"created_at":0,"status":"completed","background":false,"error":null,"output":[]}}` + "\n\n",
			header: "X-Session-ID", value: "xai-sess-111", want: "header:xai-sess-111",
			format:  translator.FormatOpenAI,
			factory: func(cfg *config.Config) auth.ProviderExecutor { return newXAIExecutor(cfg) },
		},
	} {
		for _, source := range []string{"explicit headers", "SDK metadata"} {
			t.Run(
				test.name+"/"+source, func(t *testing.T) {
					captured := make(chan http.Header, 4)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					ctx = home.WithClientScope(ctx, home.ClientScope{ID: t.Name(), Enabled: false})
					want := test.want
					if source == "SDK metadata" {
						want = "sdk:" + test.provider
						ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{SessionID: want})
					}
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								captured <- request.Header.Clone()
								contentType := "application/json"
								if test.provider == "codex" || test.provider == "xai" {
									contentType = "text/event-stream"
								}
								return &http.Response{
									StatusCode: http.StatusOK, Request: request,
									Header: http.Header{"Content-Type": {contentType}},
									Body:   io.NopCloser(strings.NewReader(test.response)),
								}, nil
							},
						),
					)
					cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}
					credential := &auth.Auth{
						ID: t.Name(), Provider: test.provider, Attributes: map[string]string{
							"base_url": "https://custom-header.invalid", "api_key": "synthetic-provider-key",
							"header:X-Session": "$CPA-SESSION-ID", "header:X-Lower-Case": "$cpa-session-id",
							"header:Authorization": "Bearer $CPA-SESSION-ID",
						},
					}
					incoming := http.Header{}
					incoming.Set(test.header, test.value)
					_, err := test.factory(cfg).Execute(
						ctx, credential,
						core.Request{Model: test.model, Payload: []byte(test.payload)},
						core.Options{SourceFormat: test.format, Headers: incoming},
					)
					if err != nil {
						t.Fatal(err)
					}
					if len(captured) != 1 {
						t.Fatalf("provider requests=%d, want 1", len(captured))
					}
					got := <-captured
					if got.Get("X-Session") != want || got.Get("X-Lower-Case") != want {
						t.Fatalf("original provider session source was lost: %#v, want %q", got, want)
					}
					if got.Get("Authorization") == "Bearer "+want {
						t.Fatal("session variable replaced the selected provider credential")
					}
				},
			)
		}
	}
}

func TestCustomSessionHeaderDoesNotDependOnRoutingAffinity(t *testing.T) {
	for _, affinity := range []bool{true, false} {
		t.Run(
			fmt.Sprintf("SessionAffinity=%t", affinity), func(t *testing.T) {
				var captured http.Header
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							captured = request.Header.Clone()
							return &http.Response{
								StatusCode: http.StatusOK, Request: request, Header: http.Header{},
								Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","choices":[]}`)),
							}, nil
						},
					),
				)
				cfg := &config.Config{}
				cfg.Routing.SessionAffinity = affinity
				credential := &auth.Auth{
					ID: "auth-compat-1", Provider: "openai-compatibility",
					Attributes: map[string]string{
						"base_url": "https://custom-header.invalid", "api_key": "key-1",
						"header:X-CPA-Session": "$CPA-SESSION-ID",
					},
				}
				_, err := NewOpenAICompatExecutor("openai-compatibility", cfg).Execute(
					ctx, credential,
					core.Request{Model: "gpt-4o", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)},
					core.Options{
						SourceFormat: translator.FormatOpenAI,
						Headers:      http.Header{"X-Session-ID": {"aff-test-sess-99"}},
					},
				)
				if err != nil || captured.Get("X-CPA-Session") != "header:aff-test-sess-99" {
					t.Fatalf("affinity=%t session header=%q, error=%v", affinity, captured.Get("X-CPA-Session"), err)
				}
			},
		)
	}
}
