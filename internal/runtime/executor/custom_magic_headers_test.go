package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestOpenAICompatPrepareRequestUsesCallerHeadersAndClearsStaleCredentials(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://compat.example/v1/chat/completions", nil)
	req = req.WithContext(util.WithSessionID(req.Context(), "canonical-session"))
	req.Header = http.Header{
		"X-Client":       {"first", "second"},
		"Authorization":  {"Bearer stale"},
		"X-Api-Key":      {"stale-claude"},
		"X-Goog-Api-Key": {"stale-gemini"},
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"header:X-Copied":     "$x-client",
		"header:X-Literal":    "literal",
		"header:X-Session":    "$cpa-session-id",
		"header:X-Missing":    "$Missing",
		"header:X-Credential": "$Authorization",
	}}

	if err := NewOpenAICompatExecutor("compat", &config.Config{}).PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	assertOpenAICompatCustomHeaders(t, req.Header)
}

func TestOpenAICompatCustomHeadersUseExplicitRequestBoundary(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
		media  bool
	}{
		{name: "chat"},
		{name: "chat stream", stream: true},
		{name: "image", media: true},
		{name: "image stream", stream: true, media: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			captured := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					if test.media {
						_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
					} else {
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if test.media {
					_, _ = w.Write([]byte(`{"created":1,"data":[]}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
				}
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor("compat", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url":              server.URL + "/v1",
				"header:X-Copied":       "$x-client",
				"header:X-Literal":      "literal",
				"header:X-Session":      "$CPA-SESSION-ID",
				"header:X-Missing":      "$Missing",
				"header:X-Credential":   "$Authorization",
				"header:Authorization":  "Bearer custom",
				"header:X-Api-Key":      "custom-claude",
				"header:X-Goog-Api-Key": "custom-gemini",
			}}
			req := cliproxyexecutor.Request{
				Model:   "upstream-model",
				Payload: []byte(`{"model":"alias-model","messages":[{"role":"user","content":"hi"}]}`),
			}
			opts := cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatOpenAI,
				Stream:       test.stream,
				Headers: http.Header{
					"x-client":       {"first", "second"},
					"Authorization":  {"Bearer inbound"},
					"X-Api-Key":      {"inbound-claude"},
					"X-Goog-Api-Key": {"inbound-gemini"},
					"Content-Type":   {"application/json"},
				},
			}
			if test.media {
				opts.SourceFormat = sdktranslator.FromString(openAICompatImageHandlerType)
				opts.Metadata = map[string]any{
					cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
				}
				req.Payload = []byte(`{"model":"alias-image","prompt":"draw"}`)
			}
			ctx := util.WithSessionID(context.Background(), "canonical-session")
			if test.stream {
				result, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream() error = %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error = %v", chunk.Err)
					}
				}
			} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			assertOpenAICompatCustomHeaders(t, <-captured)
		})
	}
}

func assertOpenAICompatCustomHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	if got := headers.Get("X-Copied"); got != "first" {
		t.Fatalf("X-Copied = %q, want first", got)
	}
	if got := headers.Get("X-Literal"); got != "literal" {
		t.Fatalf("X-Literal = %q, want literal", got)
	}
	if got := headers.Get("X-Session"); got != "canonical-session" {
		t.Fatalf("X-Session = %q, want canonical-session", got)
	}
	for _, name := range []string{
		"X-Missing", "X-Credential", "Authorization", "X-Api-Key", "X-Goog-Api-Key",
	} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
}
