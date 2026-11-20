package cache

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

type legacyKimiMigrationKV struct {
	*fakeKimiThinkingReplayKVClient
	once          sync.Once
	beforeCAS     func()
	forceConflict bool
	casCalls      atomic.Int32
}

func (c *legacyKimiMigrationKV) KVCompareAndSwap(
	ctx context.Context,
	key string,
	expected []byte,
	exists bool,
	value []byte,
	ttl time.Duration,
) (bool, error) {
	c.casCalls.Add(1)
	if c.beforeCAS != nil {
		c.once.Do(c.beforeCAS)
	}
	if c.forceConflict {
		return false, nil
	}
	return c.fakeKimiThinkingReplayKVClient.KVCompareAndSwap(ctx, key, expected, exists, value, ttl)
}

func TestKimiThinkingReplayMigratesLegacyHomeArray(t *testing.T) {
	for _, name := range []string{"ordinary", "HTML at size boundary"} {
		t.Run(
			name, func(t *testing.T) {
				legacy := kimiReplayTestContent("legacy")
				if name == "HTML at size boundary" {
					prefix, suffix := `[{"type":"thinking","thinking":"`, `","signature":"synthetic"}]`
					legacy = []byte(prefix + strings.Repeat(
						"<", KimiThinkingReplayCacheMaxBytesPerEntry-len(prefix)-len(suffix),
					) + suffix)
				}
				client := newFakeKimiThinkingReplayKVClient()
				key := kimiThinkingReplayKVKey("k3", t.Name())
				client.values[key] = bytes.Clone(legacy)
				ctx := homeKimiReplayContext(t.Name(), client)
				content, snapshot, found, err := GetKimiThinkingReplayWithSnapshotRequired(ctx, "k3", t.Name())
				if err != nil || !found || !bytes.Equal(
					content, legacy,
				) || snapshot.generation == "" || !snapshot.loaded || !snapshot.found {
					t.Fatalf(
						"legacy read: found=%t generation=%q content_equal=%t error=%v",
						found,
						snapshot.generation,
						bytes.Equal(content, legacy),
						err,
					)
				}
				if bytes.Equal(client.values[key], legacy) || !bytes.Equal(
					snapshot.raw, client.values[key],
				) ||
					len(snapshot.raw) >
						kimiThinkingReplayCacheMaxSerializedBytes ||
					client.lastCASTTL !=
						kimiThinkingReplayCacheTTL ||
					client.lastExpireTTL !=
						kimiThinkingReplayCacheTTL {
					t.Fatal("legacy value was not migrated to a bounded exact-generation snapshot")
				}
				content[0] = 'x'
				if !bytes.Equal(snapshot.raw, client.values[key]) {
					t.Fatal("caller mutated the stored snapshot")
				}
				next := kimiReplayTestContent("next")
				if replaced, err := ReplaceKimiThinkingReplayIfUnchanged(
					ctx, "k3", t.Name(), snapshot, next,
				); err != nil || !replaced {
					t.Fatalf("replace migrated snapshot: replaced=%t error=%v", replaced, err)
				}
				if replaced, err := ReplaceKimiThinkingReplayIfUnchanged(
					ctx, "k3", t.Name(), snapshot, legacy,
				); err != nil || replaced {
					t.Fatalf("stale legacy writer: replaced=%t error=%v", replaced, err)
				}
				if deleted, err := DeleteKimiThinkingReplayIfUnchanged(
					ctx, "k3", t.Name(), snapshot,
				); err != nil || deleted {
					t.Fatalf("stale legacy delete: deleted=%t error=%v", deleted, err)
				}
				got, current, found, err := GetKimiThinkingReplayWithSnapshotRequired(ctx, "k3", t.Name())
				if err != nil || !found || !bytes.Equal(got, next) ||
					current.generation == snapshot.generation {
					t.Fatalf("new generation changed: found=%t error=%v", found, err)
				}
			},
		)
	}
}

func TestKimiThinkingReplayLegacyMigrationErrorsAndConflicts(t *testing.T) {
	for _, mode := range []string{
		"CAS error", "new generation", "continuous conflict", "invalid array", "generationless object",
	} {
		t.Run(
			mode, func(t *testing.T) {
				legacy := kimiReplayTestContent("legacy")
				client := &legacyKimiMigrationKV{
					fakeKimiThinkingReplayKVClient: newFakeKimiThinkingReplayKVClient(),
				}
				key := kimiThinkingReplayKVKey("k3", t.Name())
				client.values[key] = bytes.Clone(legacy)
				next := kimiReplayTestContent("concurrent")
				nextRaw, err := marshalKimiThinkingReplayHomeValue("current-generation", false, next)
				if err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "CAS error":
					client.casErr = homekv.ErrNotConnected
				case "new generation":
					client.beforeCAS = func() {
						client.mu.Lock()
						defer client.mu.Unlock()
						client.values[key] = bytes.Clone(nextRaw)
					}
				case "continuous conflict":
					client.forceConflict = true
				case "invalid array":
					client.values[key] = []byte(`[]`)
				case "generationless object":
					client.values[key] = []byte(`{"content":[{"type":"thinking","thinking":"fixture"}]}`)
				}
				before := bytes.Clone(client.values[key])
				content, snapshot, found, err := GetKimiThinkingReplayWithSnapshotRequired(
					homeKimiReplayContext(
						t.Name(), client,
					), "k3", t.Name(),
				)
				if mode == "new generation" {
					if err != nil || !found || !bytes.Equal(
						content, next,
					) || snapshot.generation != "current-generation" || !bytes.Equal(
						client.values[key], nextRaw,
					) || client.casCalls.Load() != 1 {
						t.Fatalf(
							"concurrent generation was replaced or lost: found=%t generation=%q calls=%d error=%v",
							found,
							snapshot.generation,
							client.casCalls.Load(),
							err,
						)
					}
					return
				}
				if err == nil || found || len(content) != 0 || snapshot.loaded || snapshot.found ||
					snapshot.generation != "" ||
					len(snapshot.raw) != 0 ||
					client.expireCount != 0 ||
					!bytes.Equal(
						client.values[key], before,
					) {
					t.Fatalf(
						"failed migration changed state: found=%t calls=%d expire=%d error=%v", found,
						client.casCalls.Load(), client.expireCount, err,
					)
				}
				switch mode {
				case "CAS error":
					if !errors.Is(err, homekv.ErrNotConnected) || client.casCalls.Load() != 1 {
						t.Fatalf("migration lost CAS error: calls=%d error=%v", client.casCalls.Load(), err)
					}
				case "continuous conflict":
					if !strings.Contains(err.Error(), "concurrent") || client.casCalls.Load() != 4 {
						t.Fatalf(
							"migration conflict was not explicit and bounded: calls=%d error=%v",
							client.casCalls.Load(), err,
						)
					}
				default:
					if client.casCalls.Load() != 0 {
						t.Fatal("invalid legacy content triggered a write")
					}
				}
			},
		)
	}
}

func TestKimiThinkingReplayConcurrentLegacyMigrationSharesGeneration(t *testing.T) {
	client := newFakeKimiThinkingReplayKVClient()
	content := kimiReplayTestContent("legacy")
	key := kimiThinkingReplayKVKey("k3", t.Name())
	client.values[key] = bytes.Clone(content)
	ctx := homeKimiReplayContext(t.Name(), client)
	type result struct {
		content  []byte
		snapshot KimiThinkingReplaySnapshot
		found    bool
		err      error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			value, snapshot, found, err := GetKimiThinkingReplayWithSnapshotRequired(ctx, "k3", t.Name())
			results <- result{value, snapshot, found, err}
		}()
	}
	var generation string
	for range 2 {
		got := <-results
		if got.err != nil || !got.found || !bytes.Equal(
			got.content, content,
		) || got.snapshot.generation == "" || !bytes.Equal(got.snapshot.raw, client.values[key]) {
			t.Fatalf(
				"concurrent legacy read: found=%t generation=%q error=%v",
				got.found,
				got.snapshot.generation,
				got.err,
			)
		}
		if generation != "" && generation != got.snapshot.generation {
			t.Fatal("concurrent readers observed different migration generations")
		}
		generation = got.snapshot.generation
	}
}
