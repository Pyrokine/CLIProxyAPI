package session

import (
	"net/http"
	"testing"
)

func TestExtractSessionInfoCodexCanonicalAgentParent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		thread       string
		parentThread string
		payload      string
		wantParent   string
	}{
		{name: "omitted-thread", wantParent: "codex:sess-main-999"},
		{name: "same-thread", thread: "sess-main-999", wantParent: "codex:sess-main-999"},
		{name: "child-thread", thread: "child-thread", wantParent: "codex:sess-main-999"},
		{name: "explicit-parent", parentThread: "parent-thread", wantParent: "codex:parent-thread"},
		{name: "canonical-self", parentThread: "sess-main-999:agent:worker"},
		{
			name: "body-parent-after-canonical-self", parentThread: "sess-main-999:agent:worker",
			payload: `{"parent_session_id":"body-parent"}`, wantParent: "codex:body-parent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{
				"Session-Id":        []string{"sess-main-999"},
				"X-Openai-Subagent": []string{"collab_spawn"},
				"X-Codex-Turn-Metadata": []string{
					`{"session_id":"sess-main-999","agent_name":"/root/worker","subagent_kind":"thread_spawn"}`,
				},
			}
			if tc.thread != "" {
				headers.Set("Thread-Id", tc.thread)
			}
			if tc.parentThread != "" {
				headers.Set("X-Codex-Parent-Thread-Id", tc.parentThread)
			}
			info, ok := ExtractSessionInfo(headers, []byte(tc.payload), nil)
			if !ok || info.SessionID != "codex:sess-main-999:agent:worker" ||
				info.ParentSessionID != tc.wantParent || info.AgentName != "worker" || !info.IsSubagent {
				t.Fatalf("session info = %#v, %t; want canonical worker child and parent %q", info, ok, tc.wantParent)
			}
		})
	}
}
