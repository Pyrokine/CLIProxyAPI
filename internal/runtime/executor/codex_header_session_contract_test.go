package executor

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketOriginalIdentityAndSessionFields(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}
	credential := &auth.Auth{Provider: "codex", Metadata: map[string]any{"email": "user@example.com"}}
	caller := map[string]string{
		"Originator": "Codex Desktop", "User-Agent": "codex_cli_rs/0.1.0", "Version": "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`, "X-Client-Request-Id": "019d2233-e240-7162-992d-38df0a2a0e0d",
		"session-id": "legacy-session", "Thread-Id": "thread-1",
		"X-Codex-Routing-Hint": "route-1", "X-Codex-Window-Id": "window-1",
	}
	for _, fromGin := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("gin=%t", fromGin), func(t *testing.T) {
				ctx := t.Context()
				explicit := http.Header{}
				for name, value := range caller {
					explicit.Set(name, value)
				}
				if fromGin {
					ctx, explicit = contextWithGinHeaders(caller), nil
				}
				initial := http.Header{"session_id": {"cache-key"}, "Conversation_id": {"cache-key"}}
				got := applyCodexWebsocketHeaders(ctx, initial, credential, "", cfg, true, explicit)
				for name, want := range caller {
					if got.Get(name) != want {
						t.Errorf("original %s=%q, want %q", name, got.Get(name), want)
					}
				}
				if headerValueCaseInsensitive(got, "session_id") != "" ||
					headerValueCaseInsensitive(got, "conversation_id") != "" {
					t.Fatalf("native caller identity retained obsolete cache aliases: %#v", got)
				}
			},
		)
	}
}

func TestCodexWebsocketOriginalNativeSessionCombinations(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}
	credential := &auth.Auth{Provider: "codex", Metadata: map[string]any{"email": "user@example.com"}}
	for _, test := range []struct{ session, thread string }{
		{session: "sess-both", thread: "thread-both"},
		{session: "sess-only"},
		{thread: "thread-only"},
		{},
	} {
		for _, aliases := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("session=%s/thread=%s/cache_aliases=%t", test.session, test.thread, aliases),
				func(t *testing.T) {
					caller := map[string]string{}
					if test.session != "" {
						caller["Session-Id"] = test.session
					}
					if test.thread != "" {
						caller["Thread-Id"] = test.thread
					}
					initial := http.Header{}
					if aliases {
						initial = http.Header{"session_id": {"cache-alias"}, "Conversation_id": {"cache-alias"}}
					}
					got := applyCodexWebsocketHeaders(contextWithGinHeaders(caller), initial, credential, "", cfg, true)
					if got.Get("Session-Id") != test.session || got.Get("Thread-Id") != test.thread ||
						headerValueCaseInsensitive(got, "session_id") != "" ||
						headerValueCaseInsensitive(got, "conversation_id") != "" {
						t.Fatalf("original native session combination changed: %#v", got)
					}
				},
			)
		}
	}
}

func TestCodexOriginalEmptyAPIKeyIdentity(t *testing.T) {
	cfg := &config.Config{
		Codex: config.CodexConfig{DisableCodexCloaking: true},
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent: "oauth-default-ua", BetaFeatures: "oauth-beta",
		},
	}
	for _, transport := range []string{"http", "websocket"} {
		t.Run(
			transport, func(t *testing.T) {
				accountID, baseURL := "acc-12345", "https://custom-codex.example.com"
				if transport == "websocket" {
					accountID = "acc-ws-123"
				}
				credential := &auth.Auth{
					Provider:   "codex",
					Attributes: map[string]string{"auth_kind": "apikey", "base_url": baseURL},
					Metadata:   map[string]any{"account_id": accountID},
				}
				var got http.Header
				if transport == "websocket" {
					got = applyCodexWebsocketHeaders(t.Context(), nil, credential, "", cfg, false)
				} else {
					request, err := http.NewRequestWithContext(
						t.Context(), http.MethodPost,
						"https://example.com/responses", nil,
					)
					if err != nil {
						t.Fatal(err)
					}
					applyCodexHeaders(request, credential, "", false, cfg)
					got = request.Header
				}
				if got.Get("Authorization") != "" || got.Get("ChatGPT-Account-ID") != "" ||
					got.Get("Originator") != "" || got.Get("X-Codex-Beta-Features") != "" ||
					got.Get("User-Agent") == "oauth-default-ua" {
					t.Fatalf("empty API key inherited OAuth identity: %#v", got)
				}
			},
		)
	}
}

func TestCodexPromptCacheOriginalIdentityInputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		source translator.Format
		first  core.Request
		second core.Request
	}{
		{
			name: "explicit key", source: translator.FormatOpenAIResponse,
			first: core.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)},
		},
		{
			name: "derived root", source: translator.FormatInteractions,
			first: core.Request{
				Model: "gpt-5-codex", Payload: []byte(`{"input":"hello"}`),
				Metadata: map[string]any{core.DerivedSessionIDMetadataKey: "ctx:v1:derived-root"},
			},
		},
		{
			name: "incremental roots", source: translator.FormatOpenAIResponse,
			first: core.Request{
				Model: "gpt-5-codex", Payload: []byte(`{"input":"first"}`),
				Metadata: map[string]any{
					core.ExecutionSessionMetadataKey: "connection-1",
					core.DerivedSessionIDMetadataKey: "ctx:v1:first-root",
				},
			},
			second: core.Request{
				Model: "gpt-5-codex", Payload: []byte(`{"input":"second"}`),
				Metadata: map[string]any{
					core.ExecutionSessionMetadataKey: "connection-1",
					core.DerivedSessionIDMetadataKey: "ctx:v1:second-root",
				},
			},
		},
		{
			name: "Claude session across devices", source: translator.FormatClaude,
			first: core.Request{
				Model: "gpt-5-codex-claude-ws-cache-session", Payload: []byte(
					`{"metadata":{"user_id":"{\"device_id\":\"device-a\",\"account_uuid\":\"\",` +
						`\"session_id\":\"ws-cache-session-1\"}"},` +
						`"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
			},
			second: core.Request{
				Model: "gpt-5-codex-claude-ws-cache-session", Payload: []byte(
					`{"metadata":{"user_id":"{\"device_id\":\"device-b\",\"account_uuid\":\"\",` +
						`\"session_id\":\"ws-cache-session-1\"}"},` +
						`"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}]}`),
			},
		},
		{
			name: "stable legacy user identifier", source: translator.FormatClaude,
			first: core.Request{
				Model: "gpt-5-codex-claude-ws-cache-bare-user", Payload: []byte(
					`{"metadata":{"user_id":"same-user-across-chats"},` +
						`"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
			},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				body := []byte(`{"model":"gpt-5-codex"}`)
				firstBody, firstHeaders := applyCodexPromptCacheHeaders(test.source, test.first, body)
				firstKey := gjson.GetBytes(firstBody, "prompt_cache_key").String()
				if _, err := uuid.Parse(firstKey); err != nil || firstHeaders.Get("Session-Id") != firstKey ||
					firstHeaders.Get("Conversation_id") != firstKey || headerValueCaseInsensitive(
					firstHeaders, "session_id",
				) != "" {
					t.Fatalf(
						"stable model-scoped cache lost its paired canonical headers: %s / %#v",
						firstBody,
						firstHeaders,
					)
				}
				second := test.second
				if second.Model == "" {
					second = test.first
				}
				secondBody, secondHeaders := applyCodexPromptCacheHeaders(test.source, second, body)
				if gjson.GetBytes(secondBody, "prompt_cache_key").String() != firstKey ||
					secondHeaders.Get("Session-Id") != firstKey || secondHeaders.Get("Conversation_id") != firstKey {
					t.Fatalf("original same-session inputs changed cache identity: %s / %#v", secondBody, secondHeaders)
				}
				otherBody, _ := applyCodexPromptCacheHeaders(test.source, test.first, []byte(`{"model":"gpt-5.5"}`))
				if otherKey := gjson.GetBytes(
					otherBody, "prompt_cache_key",
				).String(); otherKey == "" || otherKey == firstKey {
					t.Fatalf("different final model shared cache identity: %s", otherBody)
				}
			},
		)
	}
}

func TestBuildCodexResponsesWebsocketURLRequiresHTTPURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://example.com/backend/responses", "wss://example.com/backend/responses"},
		{"http://127.0.0.1:1234/responses", "ws://127.0.0.1:1234/responses"},
		{"ws://127.0.0.1:1234/responses", "ws://127.0.0.1:1234/responses"},
		{"wss://example.com/backend/responses", "wss://example.com/backend/responses"},
	} {
		if got, err := buildCodexResponsesWebsocketURL(test.input); err != nil || got != test.want {
			t.Fatalf("URL %q = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"ftp://example.com/responses", "https:///responses"} {
		if _, err := buildCodexResponsesWebsocketURL(input); err == nil {
			t.Errorf("expected invalid WebSocket upstream URL error for %q", input)
		}
	}
}
