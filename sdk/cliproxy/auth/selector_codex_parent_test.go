package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestSessionAffinityCodexSubagentWithOmittedThreadIDRetainsParent(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()

	auths := []*Auth{{ID: "auth-a"}, {ID: "auth-b"}}

	parentOpts := cliproxyexecutor.Options{
		Headers:         http.Header{"Session-Id": []string{"sess-main-999"}},
		OriginalRequest: []byte(`{"input":[{"role":"user","content":"parent"}]}`),
		Metadata:        map[string]any{},
	}
	parentAuth, errParent := selector.Pick(context.Background(), "openai", "model", parentOpts, auths)
	if errParent != nil || parentAuth == nil || parentAuth.ID != "auth-a" {
		t.Fatalf("parent selection = %#v, %v; want auth-a", parentAuth, errParent)
	}

	subOpts := cliproxyexecutor.Options{
		Headers: http.Header{
			"Session-Id":        []string{"sess-main-999"},
			"X-Openai-Subagent": []string{"collab_spawn"},
			"X-Codex-Turn-Metadata": []string{
				`{"session_id":"sess-main-999","agent_name":"/root/worker","subagent_kind":"thread_spawn"}`,
			},
		},
		OriginalRequest: []byte(`{"input":[{"role":"user","content":"subagent"}]}`),
		Metadata:        map[string]any{},
	}
	subAuth, errSub := selector.Pick(context.Background(), "openai", "model", subOpts, auths)
	if errSub != nil {
		t.Fatalf("subagent Pick() error = %v", errSub)
	}
	if subAuth == nil {
		t.Fatal("subagent selection returned nil")
	}
	if subAuth.ID != parentAuth.ID {
		t.Fatalf("subagent did not inherit parent auth: got %q, want %q", subAuth.ID, parentAuth.ID)
	}
	if got := subOpts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; got != "codex:sess-main-999" {
		t.Fatalf("expected ParentSessionID=codex:sess-main-999, got %v", got)
	}
	if got := subOpts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; got != "codex:sess-main-999:agent:worker" {
		t.Fatalf("expected CanonicalSessionID=codex:sess-main-999:agent:worker, got %v", got)
	}
}
