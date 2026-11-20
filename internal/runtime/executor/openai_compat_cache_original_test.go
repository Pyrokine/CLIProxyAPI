package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorPayloadOverrideWinsOverThinkingSuffix(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotBody = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte((`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"` +
						`role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":` +
						`1,"completion_tokens":1,"total_tokens":2}}`)),
				)
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			Payload: config.PayloadConfig{
				Override: []config.PayloadRule{
					{
						Models: []config.PayloadModelRule{
							{Name: "custom-openai", Protocol: "openai"},
						},
						Params: map[string]any{
							"reasoning_effort": "low",
						},
					},
				},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test",
		},
	}
	payload := []byte(`{"model":"custom-openai(high)","messages":[{"role":"user","content":"hi"}]}`)
	_, err := executor.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "custom-openai(high)",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Stream:       false,
		},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "low" {
		t.Fatalf("reasoning_effort = %q, want %q; body=%s", got, "low", string(gotBody))
	}
}

func TestOpenAICompatExecutorApplyPromptCacheKey(t *testing.T) {
	tests := []struct {
		name        string
		support     bool
		from        string
		payload     string
		metadata    map[string]any
		wantKey     string
		wantPresent bool
	}{
		{
			name:        "disabled",
			support:     false,
			from:        "claude",
			payload:     `{"model":"gpt-5.6","metadata":{"user_id":"{\"session_id\":\"cache-session\"}"}}`,
			wantPresent: false,
		},
		{
			name:        "derived",
			support:     true,
			from:        "claude",
			payload:     `{"model":"gpt-5.6","metadata":{"user_id":"{\"session_id\":\"cache-session\"}"}}`,
			wantPresent: true,
		},
		{
			name:    "explicit caller key wins",
			support: true,
			from:    "claude",
			payload: `{"model":"gpt-5.6","prompt_cache_key":"caller-key","metadata":` +
				`{"user_id":"{\"session_id\":` +
				`\"cache-session\"}"}}`,
			wantKey: "caller-key",
		},
		{
			name:        "non Claude source without identity",
			support:     true,
			from:        "openai",
			payload:     `{"model":"gpt-5.6","messages":[{"role":"user","content":"hello"}]}`,
			wantPresent: false,
		},
		{
			name:        "OpenAI",
			support:     true,
			from:        "openai",
			payload:     `{"model":"gpt-5.6","messages":[{"role":"user","content":"hello"}]}`,
			metadata:    map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:openai"},
			wantPresent: true,
		},
		{
			name:        "OpenAI responses",
			support:     true,
			from:        "openai-response",
			payload:     `{"model":"gpt-5.6","input":"hello"}`,
			metadata:    map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:responses"},
			wantPresent: true,
		},
		{
			name:        "Gemini",
			support:     true,
			from:        "gemini",
			payload:     `{"model":"gemini-3","contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			metadata:    map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:gemini"},
			wantPresent: true,
		},
		{
			name:        "Interactions",
			support:     true,
			from:        "interactions",
			payload:     `{"model":"gpt-5.6","input":"hello"}`,
			metadata:    map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:interactions"},
			wantPresent: true,
		},
		{
			name:        "Codex",
			support:     true,
			from:        "codex",
			payload:     `{"model":"gpt-5.6","input":"hello"}`,
			metadata:    map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:codex"},
			wantPresent: true,
		},
		{
			name:        "Antigravity",
			support:     true,
			from:        "antigravity",
			payload:     `{"model":"gpt-5.6","input":"hello"}`,
			metadata:    map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:antigravity"},
			wantPresent: true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				executor := NewOpenAICompatExecutor(
					"openai-compatibility", &config.Config{
						OpenAICompatibility: []config.OpenAICompatibility{
							{
								Name:                  "compat",
								SupportPromptCacheKey: test.support,
							},
						},
					},
				)
				auth := &cliproxyauth.Auth{
					Provider: "openai-compatibility",
					Attributes: map[string]string{
						"compat_name":  "compat",
						"provider_key": "compat",
					},
				}
				translated := executor.applyPromptCacheKey(
					auth,
					sdktranslator.FromString(test.from),
					"gpt-5.6",
					cliproxyexecutor.Request{Model: "gpt-5.6", Payload: []byte(test.payload)},
					cliproxyexecutor.Options{Metadata: test.metadata},
					[]byte(`{"model":"gpt-5.6","messages":[]}`),
					context.Background(),
				)
				gotKey := gjson.GetBytes(translated, "prompt_cache_key").String()
				if test.wantKey != "" {
					if gotKey != test.wantKey {
						t.Fatalf("prompt_cache_key = %q, want %q", gotKey, test.wantKey)
					}
					return
				}
				if present := gotKey != ""; present != test.wantPresent {
					t.Fatalf(
						"prompt_cache_key present = %t, want %t; body=%s",
						present,
						test.wantPresent,
						string(translated),
					)
				}
			},
		)
	}
}

func TestOpenAICompatExecutorPromptCacheKeyCallerValueWinsPayloadOverride(t *testing.T) {
	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:                  "compat",
					SupportPromptCacheKey: true,
				},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}
	for _, test := range []struct {
		name            string
		payload         []byte
		originalRequest []byte
		want            string
	}{
		{
			name:    "request payload",
			payload: []byte(`{"model":"gpt-5.6","prompt_cache_key":"caller-key"}`),
			want:    "caller-key",
		},
		{
			name:            "original request",
			originalRequest: []byte(`{"model":"gpt-5.6","prompt_cache_key":"caller-key"}`),
			want:            "caller-key",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				translated := executor.applyPromptCacheKey(
					auth,
					sdktranslator.FromString("openai"),
					"gpt-5.6",
					cliproxyexecutor.Request{Model: "gpt-5.6", Payload: test.payload},
					cliproxyexecutor.Options{OriginalRequest: test.originalRequest},
					[]byte(`{"model":"gpt-5.6","prompt_cache_key":"payload-override"}`),
					context.Background(),
				)
				if got := gjson.GetBytes(translated, "prompt_cache_key").String(); got != test.want {
					t.Fatalf("prompt_cache_key = %q, want %q", got, test.want)
				}
			},
		)
	}
}

func TestOpenAICompatExecutorPromptCacheKeyIsModelAndProtocolScoped(t *testing.T) {
	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:                  "compat",
					SupportPromptCacheKey: true,
				},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}
	metadata := map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "execution-session"}
	derive := func(t *testing.T, model string, from string) string {
		t.Helper()
		translated := executor.applyPromptCacheKey(
			auth,
			sdktranslator.FromString(from),
			model,
			cliproxyexecutor.Request{
				Model:   model,
				Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			},
			cliproxyexecutor.Options{Metadata: metadata},
			[]byte(`{"model":"`+model+`","messages":[]}`),
			context.Background(),
		)
		return gjson.GetBytes(translated, "prompt_cache_key").String()
	}

	baseKey := derive(t, "gpt-5.6", "openai")
	if baseKey == "" {
		t.Fatal("base prompt_cache_key is empty")
	}
	if modelKey := derive(t, "gpt-5.5", "openai"); modelKey == baseKey {
		t.Fatalf("different model reused prompt_cache_key %q", baseKey)
	}
	if protocolKey := derive(t, "gpt-5.6", "openai-response"); protocolKey == baseKey {
		t.Fatalf("different protocol reused prompt_cache_key %q", baseKey)
	}
}

func TestOpenAICompatExecutorPromptCacheKeyUsesConfigIndex(t *testing.T) {
	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "duplicate", SupportPromptCacheKey: false},
				{Name: "duplicate", SupportPromptCacheKey: true},
			},
		},
	)
	payload := []byte(`{"model":"gpt-5.6","metadata":{"user_id":"{\"session_id\":\"cache-session\"}"}}`)
	for _, test := range []struct {
		name        string
		configIndex string
		wantPresent bool
	}{
		{name: "first config", configIndex: "0", wantPresent: false},
		{name: "second config", configIndex: "1", wantPresent: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				auth := &cliproxyauth.Auth{
					Provider: "openai-compatibility",
					Attributes: map[string]string{
						"compat_name":  "duplicate",
						"provider_key": "duplicate",
						"config_index": test.configIndex,
						"source":       "config:duplicate[0]",
					},
				}
				translated := executor.applyPromptCacheKey(
					auth,
					sdktranslator.FromString("claude"),
					"gpt-5.6",
					cliproxyexecutor.Request{Model: "gpt-5.6", Payload: payload},
					cliproxyexecutor.Options{},
					[]byte(`{"model":"gpt-5.6","messages":[]}`),
					context.Background(),
				)
				gotPresent := gjson.GetBytes(translated, "prompt_cache_key").String() != ""
				if gotPresent != test.wantPresent {
					t.Fatalf(
						"prompt_cache_key present = %t, want %t; body=%s", gotPresent, test.wantPresent,
						string(translated),
					)
				}
			},
		)
	}
}

func TestOpenAICompatExecutorPromptCacheKeyIgnoresConfigIndexForNonConfigAuth(t *testing.T) {
	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "duplicate", SupportPromptCacheKey: false},
				{Name: "duplicate", SupportPromptCacheKey: true},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"compat_name":  "duplicate",
			"provider_key": "duplicate",
			"config_index": "1",
		},
	}
	translated := executor.applyPromptCacheKey(
		auth,
		sdktranslator.FromString("openai"),
		"gpt-5.6",
		cliproxyexecutor.Request{
			Model:   "gpt-5.6",
			Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		},
		cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:non-config"},
		},
		[]byte(`{"model":"gpt-5.6","messages":[]}`),
		context.Background(),
	)
	if gjson.GetBytes(translated, "prompt_cache_key").Exists() {
		t.Fatalf("unexpected prompt_cache_key for non-config auth: %s", string(translated))
	}
}

func TestOpenAICompatExecutorPromptCacheKeyExecute(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte((`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"` +
						`role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)),
				)
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:                  "compat",
					SupportPromptCacheKey: true,
				},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}
	_, errExecute := executor.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gpt-5.6",
			Payload: []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"hello"}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Metadata:     map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:openai"},
		},
	)
	if errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	if gotKey := gjson.GetBytes(gotBody, "prompt_cache_key").String(); gotKey == "" {
		t.Fatalf("prompt_cache_key is missing from upstream body: %s", string(gotBody))
	}
}

func TestOpenAICompatExecutorPromptCacheKeyExecuteStream(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[]}` + "\n\n"),
				)
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:                  "compat",
					SupportPromptCacheKey: true,
				},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}
	result, errExecute := executor.ExecuteStream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gpt-5.6",
			Payload: []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Stream:       true,
			Metadata:     map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:openai-stream"},
		},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream error: %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	if gotKey := gjson.GetBytes(gotBody, "prompt_cache_key").String(); gotKey == "" {
		t.Fatalf("prompt_cache_key is missing from upstream stream body: %s", string(gotBody))
	}
}

func TestOpenAICompatExecutorPromptCacheKeyStreamCompactSkipped(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[]}` + "\n\n"),
				)
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			},
		),
	)
	defer server.Close()

	executor := NewOpenAICompatExecutor(
		"openai-compatibility", &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:                  "compat",
					SupportPromptCacheKey: true,
				},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}
	result, errExecute := executor.ExecuteStream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gpt-5.6",
			Payload: []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Alt:          "responses/compact",
			Stream:       true,
			Metadata:     map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:compact-stream"},
		},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream error: %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	if gjson.GetBytes(gotBody, "prompt_cache_key").Exists() {
		t.Fatalf("unexpected prompt_cache_key in streaming compact body: %s", string(gotBody))
	}
}
