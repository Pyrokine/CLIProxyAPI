package management

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestHandleQwenPollErrorIgnoresCancellation(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })
	registerOAuthSession("qwen-state", "qwen", "")

	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	if !handleQwenPollError("qwen-state", fmt.Errorf("poll stopped: %w", context.Canceled)) {
		t.Fatal("handleQwenPollError returned false for cancellation")
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level <= log.ErrorLevel {
			t.Fatalf("cancellation produced error log: %s", entry.Message)
		}
	}
	provider, status, ok := getOAuthSession("qwen-state")
	if !ok || provider != "qwen" || status == "Authentication failed" {
		t.Fatalf("session after cancellation = provider %q, status %q, ok %t", provider, status, ok)
	}
}

func TestHandleQwenSaveErrorIgnoresCancelledSession(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })
	registerOAuthSession("qwen-state", "qwen", "")

	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	if !handleQwenSaveError("qwen-state", errOAuthSessionNotPending) {
		t.Fatal("handleQwenSaveError returned false for cancelled session")
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level <= log.ErrorLevel {
			t.Fatalf("cancelled save produced error log: %s", entry.Message)
		}
	}
	provider, status, ok := getOAuthSession("qwen-state")
	if !ok || provider != "qwen" || status != "" {
		t.Fatalf("session after cancelled save = provider %q, status %q, ok %t", provider, status, ok)
	}
}

func TestHandleQwenPollErrorIgnoresAlreadyCancelledSession(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })
	registerOAuthSession("qwen-state", "qwen", "")
	if !cancelOAuthSession("qwen-state") {
		t.Fatal("cancelOAuthSession returned false")
	}

	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	if !handleQwenPollError("qwen-state", fmt.Errorf("authorization denied")) {
		t.Fatal("handleQwenPollError returned false")
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level <= log.ErrorLevel {
			t.Fatalf("cancelled session produced error log: %s", entry.Message)
		}
	}
}

func TestHandleQwenPollErrorAndCancellationAreAtomic(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	logger := log.StandardLogger()
	previousOutput := logger.Out
	logger.SetOutput(io.Discard)
	t.Cleanup(func() { logger.SetOutput(previousOutput) })
	for range 500 {
		previous := oauthSessions
		oauthSessions = newOAuthSessionStore(time.Minute)
		registerOAuthSession("qwen-state", "qwen", "")
		hook.Reset()

		start := make(chan struct{})
		var group sync.WaitGroup
		var cancelled bool
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			cancelled = cancelOAuthSession("qwen-state")
		}()
		go func() {
			defer group.Done()
			<-start
			handleQwenPollError("qwen-state", fmt.Errorf("authorization denied"))
		}()
		close(start)
		group.Wait()
		if cancelled {
			for _, entry := range hook.AllEntries() {
				if entry.Level <= log.ErrorLevel {
					oauthSessions = previous
					t.Fatalf("successful cancellation produced error log: %s", entry.Message)
				}
			}
		}
		oauthSessions = previous
	}
}

func TestHandleQwenPollErrorRecordsFailure(t *testing.T) {
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })
	registerOAuthSession("qwen-state", "qwen", "")

	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	if !handleQwenPollError("qwen-state", fmt.Errorf("provider rejected request")) {
		t.Fatal("handleQwenPollError returned false for failure")
	}
	entry := hook.LastEntry()
	if entry == nil || entry.Level != log.ErrorLevel {
		t.Fatalf("failure log entry = %#v", entry)
	}
	provider, status, ok := getOAuthSession("qwen-state")
	if !ok || provider != "qwen" || status != "Authentication failed" {
		t.Fatalf("session after failure = provider %q, status %q, ok %t", provider, status, ok)
	}
}
