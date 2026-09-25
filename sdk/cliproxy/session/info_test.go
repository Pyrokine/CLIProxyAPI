package session

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestExtractSessionInfoNestedProtocols(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		headers    http.Header
		payload    string
		wantID     string
		wantParent string
		wantFork   bool
		wantSub    bool
	}{
		{
			name:       "antigravity nested request",
			payload:    `{"request":{"sessionId":"child","parentSessionId":"parent"}}`,
			wantID:     "session:child",
			wantParent: "session:parent",
			wantSub:    true,
		},
		{
			name:       "gemini nested cached content parent",
			payload:    `{"request":{"cachedContent":"cache-child","parent_session_id":"cache-parent"}}`,
			wantID:     "geminicache:cache-child",
			wantParent: "geminicache:cache-parent",
			wantSub:    true,
		},
		{
			name: "claude nested metadata agent",
			payload: `{"request":{"metadata":{"user_id":"{\"session_id\":\"main\",` +
				`\"parent_session_id\":\"parent\",\"agent_id\":\"reviewer\"}"}}}`,
			wantID:     "claude:main:agent:reviewer",
			wantParent: "claude:parent",
			wantSub:    true,
		},
		{
			name:       "codex nested fork",
			headers:    http.Header{"Session-Id": {"parent"}},
			payload:    `{"request":{"thread_id":"child","forked_from_thread_id":"parent"}}`,
			wantID:     "codex:child",
			wantParent: "codex:parent",
			wantFork:   true,
		},
		{
			name: "codex multi agent",
			headers: http.Header{
				"Session-Id":        {"root"},
				"Thread-Id":         {"child-thread"},
				"X-Openai-Subagent": {"collab_spawn"},
				"X-Codex-Turn-Metadata": {
					`{"session_id":"root","thread_id":"child-thread","agent_name":"/root/reviewer","parent_thread_id":"root","subagent_kind":"thread_spawn"}`,
				},
			},
			wantID:     "codex:root:agent:reviewer",
			wantParent: "codex:root",
			wantSub:    true,
		},
		{
			name: "codex fork",
			headers: http.Header{
				"Session-Id": {"parent"},
				"Thread-Id":  {"fork"},
				"X-Codex-Turn-Metadata": {
					`{"session_id":"parent","thread_id":"fork","forked_from_thread_id":"parent"}`,
				},
			},
			wantID:     "codex:fork",
			wantParent: "codex:parent",
			wantFork:   true,
		},
		{
			name:       "generic nested task hierarchy",
			payload:    `{"request":{"taskId":"task-child","parentTaskId":"task-parent"}}`,
			wantID:     "task:task-child",
			wantParent: "task:task-parent",
			wantSub:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			info, ok := ExtractSessionInfo(test.headers, []byte(test.payload), nil)
			if !ok {
				t.Fatal("ExtractSessionInfo() returned no identity")
			}
			if info.SessionID != test.wantID || info.ParentSessionID != test.wantParent {
				t.Fatalf("identity = (%q, %q), want (%q, %q)", info.SessionID, info.ParentSessionID, test.wantID, test.wantParent)
			}
			if info.IsFork != test.wantFork || info.IsSubagent != test.wantSub {
				t.Fatalf("flags = fork:%v subagent:%v, want fork:%v subagent:%v", info.IsFork, info.IsSubagent, test.wantFork, test.wantSub)
			}
		})
	}
}

func TestExtractSessionInfoExplicitPriorityAndMetadata(t *testing.T) {
	t.Parallel()

	headers := http.Header{"X-Session-ID": {"generic"}}
	payload := []byte(`{"metadata":{"user_id":"{\"session_id\":\"claude\",\"agent_id\":\"worker\"}"}}`)
	info, ok := ExtractSessionInfo(headers, payload, map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"})
	if !ok {
		t.Fatal("ExtractSessionInfo() returned no identity")
	}
	if info.SessionID != "claude:claude:agent:worker" || info.ParentSessionID != "claude:claude" {
		t.Fatalf("Claude identity did not outrank generic header: %+v", info)
	}
	if info.CallerScope != "caller" {
		t.Fatalf("CallerScope = %q, want caller", info.CallerScope)
	}
}

func TestExtractSessionInfoSelfParentAndLongIdentity(t *testing.T) {
	t.Parallel()

	info, ok := ExtractSessionInfo(
		http.Header{"X-Session-ID": {"same"}},
		[]byte(`{"parent_session_id":"same"}`),
		nil,
	)
	if !ok || info.ParentSessionID != "" {
		t.Fatalf("self parent was not cleared: %+v", info)
	}

	longA := "session:" + strings.Repeat("界", 100) + "a"
	longB := "session:" + strings.Repeat("界", 100) + "b"
	boundedA := BoundSessionIdentity(longA)
	boundedB := BoundSessionIdentity(longB)
	if len(boundedA) > 256 || !utf8.ValidString(boundedA) {
		t.Fatalf("bounded identity is invalid: bytes=%d utf8=%v", len(boundedA), utf8.ValidString(boundedA))
	}
	if boundedA == boundedB {
		t.Fatal("different long identities collided")
	}
}

func TestNormalizeToCanonicalUUID(t *testing.T) {
	t.Parallel()

	uuid := "018F3B2A-7C4D-7ABC-8DEF-0123456789AB"
	if got := NormalizeToCanonicalUUID("codex:" + uuid); got != strings.ToLower(uuid) {
		t.Fatalf("prefixed UUID = %q", got)
	}
	projected := NormalizeToCanonicalUUID("lcp:v1:opaque")
	if len(projected) != 36 || projected[14] != '8' {
		t.Fatalf("projected UUIDv8 = %q", projected)
	}
	if got := NormalizeToCanonicalUUID(projected); got != projected {
		t.Fatalf("normalization is not idempotent: %q", got)
	}
}
