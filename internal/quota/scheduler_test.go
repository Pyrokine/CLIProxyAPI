package quota

import (
	"sync"
	"testing"
	"time"
)

func TestScheduler_RestorePreservesRegisteredDisabledState(t *testing.T) {
	tests := []struct {
		name               string
		registeredDisabled bool
		persistedDisabled  bool
	}{
		{
			name:               "registered disabled stays disabled",
			registeredDisabled: true,
			persistedDisabled:  false,
		},
		{
			name:               "registered enabled stays enabled",
			registeredDisabled: false,
			persistedDisabled:  true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				scheduler := NewScheduler(DefaultConfig(), nil)
				scheduler.Register("claude-user.json", TypeClaude, "claude")
				scheduler.SetDisabled("claude-user.json", TypeClaude, tt.registeredDisabled)

				lastRefresh := time.Now().Add(-time.Hour)
				nextRefresh := time.Now().Add(time.Hour)
				scheduler.Restore(
					map[string]EntryState{
						EntryKey(TypeClaude, "claude-user.json"): {
							FileName:    "claude-user.json",
							Type:        TypeClaude,
							AuthIndex:   "claude",
							Status:      StatusSuccess,
							LastRefresh: &lastRefresh,
							NextRefresh: &nextRefresh,
							Disabled:    tt.persistedDisabled,
						},
					},
				)

				entry := scheduler.GetStatus().Credentials[EntryKey(TypeClaude, "claude-user.json")]
				if entry == nil {
					t.Fatal("missing restored entry")
				}
				if entry.Disabled != tt.registeredDisabled {
					t.Fatalf("Disabled = %v, want registered state %v", entry.Disabled, tt.registeredDisabled)
				}
				if entry.Status != StatusSuccess {
					t.Fatalf("Status = %s, want %s", entry.Status, StatusSuccess)
				}
			},
		)
	}
}

func TestSchedulerRegisterUsesProviderAwareStatusKeys(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Register("shared.json", TypeClaude, "claude-auth")
	scheduler.Register("shared.json", TypeCodex, "codex-auth")

	status := scheduler.GetStatus()
	claude := status.Credentials[EntryKey(TypeClaude, "shared.json")]
	codex := status.Credentials[EntryKey(TypeCodex, "shared.json")]
	if claude == nil || codex == nil {
		t.Fatalf("credentials = %+v, want claude and codex entries", status.Credentials)
	}
	if claude.Type != TypeClaude || claude.AuthIndex != "claude-auth" {
		t.Fatalf("claude entry = %+v", claude)
	}
	if codex.Type != TypeCodex || codex.AuthIndex != "codex-auth" {
		t.Fatalf("codex entry = %+v", codex)
	}
}

func TestSchedulerRegisterReplacesSameAuthWhenCredentialTypeChanges(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Register("shared.json", TypeClaude, "auth-1")
	scheduler.Register("shared.json", TypeCodex, "auth-1")

	status := scheduler.GetStatus()
	if status.Credentials[EntryKey(TypeClaude, "shared.json")] != nil {
		t.Fatalf("stale claude entry was not removed: %+v", status.Credentials)
	}
	got := status.Credentials[EntryKey(TypeCodex, "shared.json")]
	if got == nil {
		t.Fatal("missing codex entry")
	}
	if got.Type != TypeCodex || got.AuthIndex != "auth-1" {
		t.Fatalf("entry identity = (%s, %s), want (%s, auth-1)", got.Type, got.AuthIndex, TypeCodex)
	}
	if got.Status != StatusIdle || got.LastRefresh != nil || got.Data != nil || got.FailureCount != 0 {
		t.Fatalf("new entry started with stale quota state: %+v", got)
	}
}

func TestSchedulerRefreshNowFileNameRefreshesMatchingProviderEntries(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	refreshed := make(map[CredentialType]int)
	scheduler.fetchFn = func(entry *Entry) ([]byte, error) {
		refreshed[entry.Type]++
		return []byte(`{}`), nil
	}

	scheduler.Register("shared.json", TypeClaude, "claude-auth")
	scheduler.Register("shared.json", TypeCodex, "codex-auth")

	scheduler.RefreshNow([]string{"shared.json"})

	if refreshed[TypeClaude] != 1 || refreshed[TypeCodex] != 1 {
		t.Fatalf("refreshed = %+v, want both provider entries", refreshed)
	}
}

func TestScheduler_RefreshNowQueuesConcurrentManualRequests(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)

	firstStarted := make(chan string, 1)
	secondStarted := make(chan string, 1)
	releaseFirst := make(chan struct{})

	var mu sync.Mutex
	calls := make([]string, 0, 2)
	scheduler.fetchFn = func(entry *Entry) ([]byte, error) {
		mu.Lock()
		calls = append(calls, entry.FileName)
		callIndex := len(calls)
		mu.Unlock()

		if callIndex == 1 {
			firstStarted <- entry.FileName
			<-releaseFirst
		} else {
			secondStarted <- entry.FileName
		}
		return []byte(`{}`), nil
	}

	scheduler.Register("codex-user.json", TypeCodex, "codex")
	scheduler.Register("gemini-user.json", TypeGeminiCli, "gemini")

	doneFirst := make(chan struct{})
	go func() {
		scheduler.RefreshNow([]string{"codex-user.json"})
		close(doneFirst)
	}()

	select {
	case got := <-firstStarted:
		if got != "codex-user.json" {
			t.Fatalf("first refresh started for %q, want codex-user.json", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}

	doneSecond := make(chan struct{})
	go func() {
		scheduler.RefreshNow([]string{"gemini-user.json"})
		close(doneSecond)
	}()

	select {
	case <-secondStarted:
		t.Fatal("second refresh started before first refresh finished")
	case <-doneSecond:
		t.Fatal("second refresh returned before first refresh finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)

	select {
	case got := <-secondStarted:
		if got != "gemini-user.json" {
			t.Fatalf("second refresh started for %q, want gemini-user.json", got)
		}
	case <-time.After(time.Second):
		t.Fatal("second refresh did not run after first refresh finished")
	}

	select {
	case <-doneFirst:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not return")
	}
	select {
	case <-doneSecond:
	case <-time.After(time.Second):
		t.Fatal("second refresh did not return")
	}

	status := scheduler.GetStatus()
	typeByName := map[string]CredentialType{
		"codex-user.json":  TypeCodex,
		"gemini-user.json": TypeGeminiCli,
	}
	for _, name := range []string{"codex-user.json", "gemini-user.json"} {
		entry := status.Credentials[EntryKey(typeByName[name], name)]
		if entry == nil {
			t.Fatalf("missing status entry for %s", name)
		}
		if entry.LastRefresh == nil {
			t.Fatalf("expected LastRefresh for %s", name)
		}
		if entry.Status != StatusSuccess {
			t.Fatalf("status for %s = %s, want %s", name, entry.Status, StatusSuccess)
		}
	}
}

func TestSummarizeEntriesByProvider(t *testing.T) {
	claudeLast := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	claudeNext := time.Date(2026, 7, 7, 11, 0, 0, 0, time.UTC)
	codexLast := time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)
	codexNext := time.Date(2026, 7, 7, 10, 30, 0, 0, time.UTC)
	entries := map[string]*Entry{
		EntryKey(TypeClaude, "claude-a.json"): {
			FileName:     "claude-a.json",
			Type:         TypeClaude,
			Status:       StatusSuccess,
			LastRefresh:  &claudeLast,
			NextRefresh:  &claudeNext,
			FailureCount: 1,
		},
		EntryKey(TypeClaude, "claude-b.json"): {
			FileName:     "claude-b.json",
			Type:         TypeClaude,
			Status:       StatusBanned,
			Disabled:     true,
			FailureCount: 2,
		},
		EntryKey(TypeCodex, "codex.json"): {
			FileName:     "codex.json",
			Type:         TypeCodex,
			Status:       StatusError,
			LastRefresh:  &codexLast,
			NextRefresh:  &codexNext,
			FailureCount: 3,
		},
	}

	byProvider := summarizeEntriesByProvider(entries)
	claude := byProvider[string(TypeClaude)]
	if claude.Provider != string(TypeClaude) || claude.CredentialCount != 2 {
		t.Fatalf("claude summary identity = %+v", claude)
	}
	if claude.Success != 1 || claude.Banned != 1 || claude.Disabled != 1 || claude.FailureCount != 3 {
		t.Fatalf("claude summary counters = %+v", claude)
	}
	if claude.LatestRefreshAt == nil || !claude.LatestRefreshAt.Equal(claudeLast) {
		t.Fatalf("claude latest refresh = %v, want %v", claude.LatestRefreshAt, claudeLast)
	}
	if claude.NextRefreshAt == nil || !claude.NextRefreshAt.Equal(claudeNext) {
		t.Fatalf("claude next refresh = %v, want %v", claude.NextRefreshAt, claudeNext)
	}

	codex := byProvider[string(TypeCodex)]
	if codex.Error != 1 || codex.FailureCount != 3 {
		t.Fatalf("codex summary counters = %+v", codex)
	}
}
