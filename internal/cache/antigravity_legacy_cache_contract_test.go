package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/tidwall/gjson"
)

const legacyAntigravityCacheItem = `{"type":"function_call_part","contentIndex":1,` +
	`"partIndex":0,"name":"Read","call_id":"id1","args":{"file_path":"/a"},` +
	`"thoughtSignature":"sig-first"}`

// noinspection GoBoolExpressions
func TestAntigravityLegacyCacheHelpersPreserveReadWriteDelete(t *testing.T) {
	if AntigravityReasoningReplayCacheTTL != time.Hour || AntigravityReasoningReplayCacheMaxEntries != 10240 ||
		AntigravityReasoningReplayCacheEvictBatchSize != 128 {
		t.Fatal("legacy cache limits changed")
	}
	for _, homeMode := range []bool{false, true} {
		for _, write := range []string{"single", "items", "context", "current alias"} {
			for _, requiredDelete := range []bool{false, true} {
				name := write + "/local"
				if homeMode {
					name = write + "/Home"
				}
				if requiredDelete {
					name += "/required delete"
				}
				t.Run(
					name, func(t *testing.T) {
						ClearAntigravityReasoningReplayCache()
						t.Cleanup(ClearAntigravityReasoningReplayCache)
						client := newFakeAntigravityReasoningReplayKVClient()
						client.casErr = homekv.ErrCompareAndSwapUnsupported
						useFakeAntigravityReasoningReplayKVClient(t, client, homeMode)
						const model = "gemini-3-flash-agent"
						session := "session:" + t.Name()
						input := []byte(legacyAntigravityCacheItem)
						var stored bool
						switch write {
						case "single":
							stored = CacheAntigravityReasoningReplayItem(model, session, input)
						case "items":
							stored = CacheAntigravityReasoningReplayItems(model, session, [][]byte{input})
						case "context":
							stored = CacheAntigravityReasoningReplayItemsBestEffort(
								t.Context(), model, session, [][]byte{input},
							)
						case "current alias":
							stored = AntigravityReasoningReplayItems(model, session, [][]byte{input})
						}
						if !stored {
							t.Fatal("legacy cache write failed")
						}
						input[0] = 'x'
						items, found, err := GetAntigravityReasoningReplayItemsRequired(t.Context(), model, session)
						if err != nil || !found || len(items) != 1 ||
							gjson.GetBytes(items[0], "thoughtSignature").String() != "sig-first" {
							t.Fatalf("legacy read = %q, %t, %v", items, found, err)
						}
						items[0][0] = 'x'
						item, found := GetAntigravityReasoningReplayItem(model, session)
						if !found || gjson.GetBytes(item, "name").String() != "Read" {
							t.Fatalf("returned data changed the stored item: %s", item)
						}
						if homeMode && (client.setOptions.EX != time.Hour || client.expireTTL != time.Hour) {
							t.Fatalf("legacy Home TTL changed: set=%+v read=%s", client.setOptions, client.expireTTL)
						}
						if requiredDelete {
							if err := DeleteAntigravityReasoningReplayItemRequired(
								t.Context(), model, session,
							); err != nil {
								t.Fatal(err)
							}
						} else {
							DeleteAntigravityReasoningReplayItem(model, session)
						}
						if _, found := GetAntigravityReasoningReplayItem(model, session); found {
							t.Fatal("deleted legacy item is still visible")
						}
					},
				)
			}
		}
	}
}

func TestAntigravityLegacyLocalReadMissKeepsStateAbsent(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "missing"
		if expired {
			name = "expired"
		}
		t.Run(
			name, func(t *testing.T) {
				ClearAntigravityReasoningReplayCache()
				t.Cleanup(ClearAntigravityReasoningReplayCache)
				useFakeAntigravityReasoningReplayKVClient(t, newFakeAntigravityReasoningReplayKVClient(), false)
				const model, session = "gemini-3-flash-agent", "session:legacy-local-read-miss"
				key := antigravityReasoningReplayCacheKey(model, session)
				if expired {
					if !CacheAntigravityReasoningReplayItem(model, session, []byte(legacyAntigravityCacheItem)) {
						t.Fatal("legacy local write failed")
					}
					antigravityReasoningReplayMu.Lock()
					entry := antigravityReasoningReplayEntries[key]
					entry.Timestamp = time.Now().Add(-2 * time.Hour)
					antigravityReasoningReplayEntries[key] = entry
					antigravityReasoningReplayMu.Unlock()
				}
				items, found, err := GetAntigravityReasoningReplayItemsRequired(t.Context(), model, session)
				if err != nil || found || items != nil {
					t.Fatalf("legacy local miss = %q, %t, %v", items, found, err)
				}
				antigravityReasoningReplayMu.Lock()
				_, exists := antigravityReasoningReplayEntries[key]
				antigravityReasoningReplayMu.Unlock()
				if exists {
					t.Fatal("legacy local read reserved missing state")
				}
			},
		)
	}
}

func TestAntigravityLegacyHomeReadMissKeepsCASOptional(t *testing.T) {
	client := newFakeAntigravityReasoningReplayKVClient()
	client.casErr = homekv.ErrCompareAndSwapUnsupported
	useFakeAntigravityReasoningReplayKVClient(t, client, true)
	const model, session = "gemini-3-flash-agent", "session:legacy-read-miss"
	items, found, err := GetAntigravityReasoningReplayItemsRequired(t.Context(), model, session)
	if err != nil || found || items != nil || len(client.values) != 0 {
		t.Fatalf("legacy read miss acquired a new CAS requirement: %q, %t, %v", items, found, err)
	}
	if _, _, _, err := GetAntigravityReasoningReplayItemsWithSnapshotRequired(
		t.Context(), model, session,
	); !errors.Is(err, homekv.ErrCompareAndSwapUnsupported) {
		t.Fatalf("snapshot read lost its required CAS failure: %v", err)
	}
}

func TestAntigravityLegacyScopedHomeKeepsCASOptional(t *testing.T) {
	client := newFakeAntigravityReasoningReplayKVClient()
	ctx := homekv.WithClientScope(
		t.Context(), homekv.ClientScope{
			Client: struct{ homekv.KVClient }{client}, ID: "legacy-home", Enabled: true,
		},
	)
	const model, session = "gemini-3-flash-agent", "session:legacy-scoped-home"
	if !CacheAntigravityReasoningReplayItemsBestEffort(
		ctx, model, session, [][]byte{[]byte(legacyAntigravityCacheItem)},
	) {
		t.Fatal("legacy scoped Home write requires CAS")
	}
	items, found, err := GetAntigravityReasoningReplayItemsRequired(ctx, model, session)
	if err != nil || !found || len(items) != 1 {
		t.Fatalf("legacy scoped Home read = %q, %t, %v", items, found, err)
	}
	if _, _, _, err := GetAntigravityReasoningReplayItemsWithSnapshotRequired(
		ctx, model, session,
	); !errors.Is(err, homekv.ErrCompareAndSwapUnsupported) {
		t.Fatalf("snapshot read lost its required CAS capability check: %v", err)
	}
	if err := DeleteAntigravityReasoningReplayItemRequired(ctx, model, session); err != nil {
		t.Fatal(err)
	}
	if _, found, err := GetAntigravityReasoningReplayItemsRequired(ctx, model, session); err != nil || found {
		t.Fatalf("legacy scoped Home deletion = %t, %v", found, err)
	}
}

func TestAntigravityLegacyHomeValueMigratesToConditionalSnapshot(t *testing.T) {
	client := newFakeAntigravityReasoningReplayKVClient()
	useFakeAntigravityReasoningReplayKVClient(t, client, true)
	const model, session = "gemini-3-flash-agent", "session:legacy-value"
	raw, err := json.Marshal([][]byte{[]byte(legacyAntigravityCacheItem)})
	if err != nil {
		t.Fatal(err)
	}
	key := antigravityReasoningReplayKVKey(model, session)
	client.values[key] = bytes.Clone(raw)
	if items, found, err := GetAntigravityReasoningReplayItemsRequired(
		t.Context(), model, session,
	); err != nil || !found || len(items) != 1 {
		t.Fatalf("legacy Home value was not read: %q, %t, %v", items, found, err)
	}
	_, snapshot, found, err := GetAntigravityReasoningReplayItemsWithSnapshotRequired(t.Context(), model, session)
	if err != nil || !found {
		t.Fatalf("legacy Home snapshot = %t, %v", found, err)
	}
	if swapped, err := ReplaceAntigravityReasoningReplayItemsIfUnchanged(
		t.Context(), model, session, snapshot, [][]byte{[]byte(legacyAntigravityCacheItem)},
	); err != nil || !swapped || bytes.Equal(client.values[key], raw) {
		t.Fatalf("legacy value was not migrated by conditional replacement: %t, %v", swapped, err)
	}
	if deleted, err := DeleteAntigravityReasoningReplayItemsIfUnchanged(
		t.Context(), model, session, snapshot,
	); err != nil || deleted {
		t.Fatalf("old snapshot deleted the migrated value: %t, %v", deleted, err)
	}
}

func TestAntigravityLegacyCacheExpiresThroughSharedCleanup(t *testing.T) {
	ClearAntigravityReasoningReplayCache()
	t.Cleanup(ClearAntigravityReasoningReplayCache)
	useFakeAntigravityReasoningReplayKVClient(t, newFakeAntigravityReasoningReplayKVClient(), false)
	const model = "gemini-3-flash-agent"
	for _, session := range []string{"expired", "live"} {
		if !CacheAntigravityReasoningReplayItem(model, session, []byte(legacyAntigravityCacheItem)) {
			t.Fatal("legacy local write failed")
		}
	}
	expiredKey := antigravityReasoningReplayCacheKey(model, "expired")
	liveKey := antigravityReasoningReplayCacheKey(model, "live")
	antigravityReasoningReplayMu.Lock()
	entry := antigravityReasoningReplayEntries[expiredKey]
	entry.Timestamp = time.Now().Add(-2 * time.Hour)
	antigravityReasoningReplayEntries[expiredKey] = entry
	antigravityReasoningReplayMu.Unlock()
	purgeExpiredCaches()
	antigravityReasoningReplayMu.Lock()
	_, expiredExists := antigravityReasoningReplayEntries[expiredKey]
	_, liveExists := antigravityReasoningReplayEntries[liveKey]
	antigravityReasoningReplayMu.Unlock()
	if expiredExists || !liveExists {
		t.Fatalf("shared cleanup: expired=%t live=%t", expiredExists, liveExists)
	}
}

func TestAntigravityLegacyRequiredOperationsPreserveHomeFailure(t *testing.T) {
	previous := currentAntigravityReasoningReplayKVClient
	cause := errors.New("synthetic legacy Home unavailable")
	currentAntigravityReasoningReplayKVClient = func() (antigravityReasoningReplayKVClient, bool, error) {
		return nil, true, cause
	}
	t.Cleanup(func() { currentAntigravityReasoningReplayKVClient = previous })
	if _, found, err := GetAntigravityReasoningReplayItemsRequired(
		context.Background(), "gemini-3-flash-agent", "session:legacy-error",
	); !errors.Is(err, cause) || found {
		t.Fatalf("legacy Required read lost Home error: %t, %v", found, err)
	}
	if err := DeleteAntigravityReasoningReplayItemRequired(
		context.Background(), "gemini-3-flash-agent", "session:legacy-error",
	); !errors.Is(err, cause) {
		t.Fatalf("legacy Required delete lost Home error: %v", err)
	}
}
