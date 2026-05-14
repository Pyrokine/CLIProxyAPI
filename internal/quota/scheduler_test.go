// Last compiled: 2026-05-07
// Author: pyro

package quota

import (
	"sync"
	"testing"
	"time"
)

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
	for _, name := range []string{"codex-user.json", "gemini-user.json"} {
		entry := status.Credentials[name]
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
