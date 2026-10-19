package helps

import (
	"fmt"
	"testing"
	"time"
)

func TestClaudeDiagnosticsTracksCompletedMessagePerCredentialSession(t *testing.T) {
	resetClaudeDiagnosticsForTest()
	defer resetClaudeDiagnosticsForTest()

	key, sequence, previous := BeginClaudeDiagnostics("credential-a", "session-a")
	if key == "" || sequence != 1 || previous != "" {
		t.Fatalf("first begin = %q/%d/%q, want key/1/empty", key, sequence, previous)
	}
	CommitClaudeDiagnostics(key, sequence, "msg_first")
	_, secondSequence, previous := BeginClaudeDiagnostics("credential-a", "session-a")
	if secondSequence != 2 || previous != "msg_first" {
		t.Fatalf("second begin = %d/%q, want 2/msg_first", secondSequence, previous)
	}

	_, _, otherSession := BeginClaudeDiagnostics("credential-a", "session-b")
	_, _, otherCredential := BeginClaudeDiagnostics("credential-b", "session-a")
	if otherSession != "" || otherCredential != "" {
		t.Fatalf("diagnostics leaked across identity: session=%q credential=%q", otherSession, otherCredential)
	}
}

func TestClaudeDiagnosticsRejectsExpiredGenerationCommit(t *testing.T) {
	resetClaudeDiagnosticsForTest()
	defer resetClaudeDiagnosticsForTest()

	key, expiredSequence, _ := BeginClaudeDiagnostics("credential", "session")
	claudeDiagnosticsState.Lock()
	entry := claudeDiagnosticsState.entries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	claudeDiagnosticsState.entries[key] = entry
	claudeDiagnosticsState.Unlock()

	newKey, currentSequence, previous := BeginClaudeDiagnostics("credential", "session")
	if newKey != key || currentSequence <= expiredSequence || previous != "" {
		t.Fatalf("new generation = %q/%d/%q, want same key/new sequence/empty", newKey, currentSequence, previous)
	}
	CommitClaudeDiagnostics(newKey, currentSequence, "msg_current")
	CommitClaudeDiagnostics(key, expiredSequence, "msg_expired")
	_, _, previous = BeginClaudeDiagnostics("credential", "session")
	if previous != "msg_current" {
		t.Fatalf("previous message = %q, want current generation", previous)
	}
}

func TestClaudeDiagnosticsCacheEvictsOldestEntriesWithinCapacity(t *testing.T) {
	resetClaudeDiagnosticsForTest()
	defer resetClaudeDiagnosticsForTest()

	firstKey, firstSequence, _ := BeginClaudeDiagnostics("credential", "session-0")
	var newestKey string
	for index := 1; index <= claudeDiagnosticsMaxEntries; index++ {
		newestKey, _, _ = BeginClaudeDiagnostics("credential", fmt.Sprintf("session-%d", index))
	}

	claudeDiagnosticsState.Lock()
	entryCount := len(claudeDiagnosticsState.entries)
	_, firstFound := claudeDiagnosticsState.entries[firstKey]
	_, newestFound := claudeDiagnosticsState.entries[newestKey]
	claudeDiagnosticsState.Unlock()
	if entryCount > claudeDiagnosticsMaxEntries {
		t.Fatalf("cache entries = %d, want at most %d", entryCount, claudeDiagnosticsMaxEntries)
	}
	if firstFound {
		t.Fatal("oldest diagnostics entry was not evicted")
	}
	if !newestFound {
		t.Fatal("newest diagnostics entry was evicted")
	}

	newKey, newSequence, _ := BeginClaudeDiagnostics("credential", "session-0")
	if newKey != firstKey || newSequence <= firstSequence {
		t.Fatalf("recreated generation = %q/%d, want same key after sequence %d", newKey, newSequence, firstSequence)
	}
	CommitClaudeDiagnostics(newKey, newSequence, "msg_recreated")
	CommitClaudeDiagnostics(firstKey, firstSequence, "msg_evicted")
	_, _, previous := BeginClaudeDiagnostics("credential", "session-0")
	if previous != "msg_recreated" {
		t.Fatalf("previous message = %q, want recreated generation", previous)
	}
}

func TestClaudeDiagnosticsRejectsLateOlderCommit(t *testing.T) {
	resetClaudeDiagnosticsForTest()
	defer resetClaudeDiagnosticsForTest()

	key, first, _ := BeginClaudeDiagnostics("credential", "session")
	_, second, _ := BeginClaudeDiagnostics("credential", "session")
	CommitClaudeDiagnostics(key, second, "msg_newer")
	CommitClaudeDiagnostics(key, first, "msg_older")
	_, _, previous := BeginClaudeDiagnostics("credential", "session")
	if previous != "msg_newer" {
		t.Fatalf("previous message = %q, want newer completed generation", previous)
	}
}

func TestClaudeJSONPrefilterKeepsClassifications(t *testing.T) {
	titles := []struct {
		name    string
		payload string
		want    bool
	}{
		{"plain system title", `{"system":"Return a short title for this session"}`, true},
		{"escaped system title", `{"system":"naming a\u0020coding session"}`, true},
		{"title schema", `{"output_config":{"format":{"schema":{"properties":{"title":{}}}}},"messages":[{"role":"user","content":"<session>test</session>"}]}`, true},
		{"ordinary user request", `{"messages":[{"role":"user","content":"Return a short title"}]}`, false},
		{"unrelated request", `{"system":"You are a coding assistant"}`, false},
	}
	for _, tt := range titles {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClaudeTitleHelperRequest([]byte(tt.payload)); got != tt.want {
				t.Fatalf("title helper = %t, want %t", got, tt.want)
			}
		})
	}

	ttls := []struct {
		name    string
		payload string
		want    bool
	}{
		{"tool", `{"tools":[{"cache_control":{"ttl":"1h"}}]}`, true},
		{"system", `{"system":[{"cache_control":{"ttl":"1h"}}]}`, true},
		{"escaped digit in message", `{"messages":[{"content":[{"cache_control":{"ttl":"\u0031h"}}]}]}`, true},
		{"escaped letter in tool", `{"tools":[{"cache_control":{"ttl":"1\u0068"}}]}`, true},
		{"unrelated one hour text", `{"system":[{"cache_control":{"ttl":"5m"}}],"note":"1h"}`, false},
		{"no one hour", `{"system":[{"cache_control":{"ttl":"5m"}}]}`, false},
		{"malformed payload", `{"system":[{"cache_control":{"ttl":"1h"}}]`, false},
	}
	for _, tt := range ttls {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClaudePayloadHas1hTTL([]byte(tt.payload)); got != tt.want {
				t.Fatalf("one-hour TTL = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestPinClaudeSessionDateResetsWithContinuityGeneration(t *testing.T) {
	resetClaudeDiagnosticsForTest()
	defer resetClaudeDiagnosticsForTest()

	key, _, _ := BeginClaudeDiagnostics("credential", "session")
	if first := PinClaudeSessionDate(key, "2026-08-01"); first != "2026-08-01" {
		t.Fatalf("first date = %q", first)
	}
	if second := PinClaudeSessionDate(key, "2026-08-02"); second != "2026-08-01" {
		t.Fatalf("second date = %q, want original date", second)
	}
	other, _, _ := BeginClaudeDiagnostics("credential", "other-session")
	if otherDate := PinClaudeSessionDate(other, "2026-08-02"); otherDate != "2026-08-02" {
		t.Fatalf("other session date = %q", otherDate)
	}

	claudeDiagnosticsState.Lock()
	entry := claudeDiagnosticsState.entries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	claudeDiagnosticsState.entries[key] = entry
	claudeDiagnosticsState.Unlock()
	BeginClaudeDiagnostics("credential", "session")
	if resetDate := PinClaudeSessionDate(key, "2026-08-02"); resetDate != "2026-08-02" {
		t.Fatalf("date after expiry = %q, want new date", resetDate)
	}
	if unknown := PinClaudeSessionDate("unknown", "2026-08-03"); unknown != "2026-08-03" {
		t.Fatalf("unknown session date = %q", unknown)
	}
}
