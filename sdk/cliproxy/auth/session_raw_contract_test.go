package auth_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestPublicRawSessionIDAndCanonicalIdentityRemainIndependent(t *testing.T) {
	for _, sample := range []struct {
		name, body, raw, canonical string
		headers                    http.Header
		metadata                   map[string]any
	}{
		{
			name: "Claude header", headers: http.Header{"X-Claude-Code-Session-Id": {"s"}}, raw: "claude:s",
			canonical: "claude:s",
		},
		{name: "Codex dashed header", headers: http.Header{"Session-Id": {"s"}}, raw: "s", canonical: "codex:s"},
		{name: "Codex underscore header", headers: http.Header{"Session_id": {"s"}}, raw: "s", canonical: "codex:s"},
		{name: "generic header", headers: http.Header{"X-Session-ID": {"s"}}, raw: "s", canonical: "header:s"},
		{name: "affinity header", headers: http.Header{"X-Session-Affinity": {"s"}}, raw: "s", canonical: "affinity:s"},
		{name: "conversation header", headers: http.Header{"Conversation_id": {"s"}}, raw: "s", canonical: "conv:s"},
		{name: "Amp header", headers: http.Header{"X-Amp-Thread-Id": {"s"}}, raw: "s", canonical: "amp:s"},
		{
			name: "request header", headers: http.Header{"X-Client-Request-Id": {"s"}}, raw: "s",
			canonical: "clientreq:s",
		},
		{name: "conversation object", body: `{"conversation":{"id":"s"}}`, raw: "s", canonical: "conv:s"},
		{name: "conversation text", body: `{"conversation":"s"}`, raw: "s", canonical: "conv:s"},
		{name: "cache body", body: `{"prompt_cache_key":"s"}`, raw: "s", canonical: "pck:s"},
		{name: "session snake", body: `{"session_id":"s"}`, raw: "s", canonical: "session:s"},
		{name: "session camel", body: `{"sessionId":"s"}`, raw: "s", canonical: "session:s"},
		{name: "user metadata", body: `{"metadata":{"user_id":"s"}}`, raw: "user:s", canonical: "user:s"},
		{
			name: "execution metadata", metadata: map[string]any{core.ExecutionSessionMetadataKey: "s"},
			raw: "execution:s", canonical: "execution:s",
		},
		{
			name: "derived metadata", metadata: map[string]any{"derived_session_id": "s"}, raw: "derived:s",
			canonical: "derived:s",
		},
		{
			name:     "canonical metadata stays internal",
			metadata: map[string]any{core.CanonicalSessionIDMetadataKey: "ctx:v1:s"}, canonical: "ctx:v1:s",
		},
		{
			name:     "LCP metadata stays internal",
			metadata: map[string]any{core.LCPAffinitySessionIDMetadataKey: "lcp:v1:s"}, canonical: "lcp:v1:s",
		},
		{
			name: "raw cache priority", body: `{"session_id":"session","prompt_cache_key":"cache"}`, raw: "cache",
			canonical: "session:session",
		},
		{
			name: "raw header priority", headers: http.Header{"Session-Id": {"codex"}, "X-Session-ID": {"generic"}},
			raw: "codex", canonical: "codex:codex",
		},
		{
			name: "opaque prefix stays literal", headers: http.Header{"X-Session-ID": {"session:literal"}},
			raw: "session:literal", canonical: "header:session:literal",
		},
		{
			name:      "nested identity stays canonical",
			body:      `{"request":{"sessionId":"root","metadata":{"agent_id":"worker"}}}`,
			canonical: "session:root:agent:worker",
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				if got := auth.ExtractSessionID(
					sample.headers, []byte(sample.body), sample.metadata,
				); got != sample.raw {
					t.Errorf("raw identity = %q, want %q", got, sample.raw)
				}
				if got := auth.CanonicalSessionID(
					sample.headers, []byte(sample.body), sample.metadata,
				); got != sample.canonical {
					t.Errorf("canonical identity = %q, want %q", got, sample.canonical)
				}
			},
		)
	}
}

func TestPublicRawSessionIDKeepsContentFallbackAndMaximumID(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	original := auth.ExtractSessionID(nil, body, nil)
	if original == "" {
		t.Fatal("content fallback identity is empty")
	}
	metadata := map[string]any{core.CanonicalSessionIDMetadataKey: "ctx:v1:reported"}
	if got := auth.ExtractSessionID(nil, body, metadata); got != original {
		t.Fatalf("reporting metadata changed the public content fallback: %q != %q", got, original)
	}
	if got := auth.CanonicalSessionID(nil, body, metadata); got != "ctx:v1:reported" {
		t.Fatalf("canonical metadata = %q", got)
	}
	for _, suffix := range []string{"a", "b"} {
		raw := strings.Repeat("x", 255) + suffix
		headers := http.Header{"X-Session-ID": {raw}}
		if got := auth.ExtractSessionID(headers, nil, nil); got != raw {
			t.Fatalf("maximum-length public identity changed: %q", got)
		}
		if got := auth.CanonicalSessionID(headers, nil, nil); len(got) != 256 || !strings.HasPrefix(
			got, "header:",
		) || !strings.Contains(got, "#") {
			t.Fatalf("maximum-length canonical identity is not bounded with its source: %q", got)
		}
	}
}
