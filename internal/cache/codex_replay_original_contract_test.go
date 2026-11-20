package cache

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

func TestCodexReasoningReplayCacheRejectsInvalidItems(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	if cacheCodexReasoningReplayItemsBestEffort(
		ctx, "gpt-5.4", "session", [][]byte{[]byte(`{"type":"reasoning","encrypted_content":"bad","summary":[]}`)},
	) {
		t.Fatal("invalid encrypted_content was cached")
	}
	if _, found, err := GetCodexReasoningReplayItemsRequired(ctx, "gpt-5.4", "session"); err != nil || found {
		t.Fatalf("invalid item read: found=%t error=%v", found, err)
	}
}

func TestCodexReasoningReplayCacheScopesByModelAndSession(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	item := validCodexReasoningReplayItemForTest(7)
	original := bytes.Clone(item)
	if !cacheCodexReasoningReplayItemsBestEffort(ctx, "gpt-5.4", "session-a", [][]byte{item}) {
		t.Fatal("valid item was not cached")
	}
	for _, pair := range [][2]string{{"gpt-5.5", "session-a"}, {"gpt-5.4", "session-b"}} {
		if _, found, err := GetCodexReasoningReplayItemsRequired(ctx, pair[0], pair[1]); err != nil || found {
			t.Fatalf("unrelated model/session read: %v found=%t error=%v", pair, found, err)
		}
	}
	item[0] = '!'
	items, found, err := GetCodexReasoningReplayItemsRequired(ctx, "gpt-5.4", "session-a")
	if err != nil || !found || len(items) != 1 || !bytes.Equal(items[0], original) {
		t.Fatalf("original entry changed: %q found=%t error=%v", items, found, err)
	}
	items[0][0] = '!'
	items, found, err = GetCodexReasoningReplayItemsRequired(ctx, "gpt-5.4", "session-a")
	if err != nil || !found || len(items) != 1 || !bytes.Equal(items[0], original) {
		t.Fatalf("caller changed the cached entry: %q found=%t error=%v", items, found, err)
	}
}

func TestCodexReasoningReplayCacheBatchEvictsWhenFull(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	item := validCodexReasoningReplayItemForTest(9)
	for index := 0; index <= codexReasoningReplayCacheMaxEntries; index++ {
		if !cacheCodexReasoningReplayItemsBestEffort(ctx, "gpt-5.4", fmt.Sprintf("session-%d", index), [][]byte{item}) {
			t.Fatalf("cache insert %d failed", index)
		}
	}
	codexReasoningReplayMu.Lock()
	count := len(codexReasoningReplayEntries)
	codexReasoningReplayMu.Unlock()
	if count >= codexReasoningReplayCacheMaxEntries {
		t.Fatalf("cache entries = %d, want batch eviction below %d", count, codexReasoningReplayCacheMaxEntries)
	}
}

// noinspection GoBoolExpressions
func TestCodexOriginalReplayCacheSlidingHourAndDelete(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	if codexReasoningReplayCacheTTL != time.Hour {
		t.Fatalf("cache TTL = %v, want one hour", codexReasoningReplayCacheTTL)
	}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	if !cacheCodexReasoningReplayItemsBestEffort(
		ctx, "gpt-5.4", "session", [][]byte{validCodexReasoningReplayItemForTest(3)},
	) {
		t.Fatal("initial cache write failed")
	}
	key := codexReasoningReplayLocalCacheKey(t.Name(), codexReasoningReplayCacheKey("gpt-5.4", "session"))
	previous := time.Now().Add(-30 * time.Minute)
	codexReasoningReplayMu.Lock()
	entry := codexReasoningReplayEntries[key]
	entry.Timestamp = previous
	codexReasoningReplayEntries[key] = entry
	codexReasoningReplayMu.Unlock()
	if _, found, err := GetCodexReasoningReplayItemsRequired(ctx, "gpt-5.4", "session"); err != nil || !found {
		t.Fatalf("valid half-hour entry: found=%t error=%v", found, err)
	}
	codexReasoningReplayMu.Lock()
	entry = codexReasoningReplayEntries[key]
	refreshed := entry.Timestamp.After(previous)
	entry.Timestamp = time.Now().Add(-time.Hour - time.Second)
	codexReasoningReplayEntries[key] = entry
	codexReasoningReplayMu.Unlock()
	if !refreshed {
		t.Fatal("reading the entry did not refresh its TTL")
	}
	if _, found, err := GetCodexReasoningReplayItemsRequired(ctx, "gpt-5.4", "session"); err != nil || found {
		t.Fatalf("expired entry: found=%t error=%v", found, err)
	}
	if !cacheCodexReasoningReplayItemsBestEffort(
		ctx, "gpt-5.4", "session", [][]byte{validCodexReasoningReplayItemForTest(3)},
	) {
		t.Fatal("replacement cache write failed")
	}
	if err := DeleteCodexReasoningReplayItemRequired(ctx, "gpt-5.4", "session"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := GetCodexReasoningReplayItemsRequired(ctx, "gpt-5.4", "session"); err != nil || found {
		t.Fatalf("deleted entry: found=%t error=%v", found, err)
	}
}
