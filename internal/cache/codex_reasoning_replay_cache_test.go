package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/tidwall/gjson"
)

type fakeCodexReasoningReplayKVClient struct {
	mu            sync.Mutex
	values        map[string][]byte
	getErr        error
	setErr        error
	delErr        error
	expireErr     error
	getCount      int
	setCount      int
	delCount      int
	expireCount   int
	lastSetTTL    time.Duration
	lastExpireTTL time.Duration
}

func newFakeCodexReasoningReplayKVClient() *fakeCodexReasoningReplayKVClient {
	return &fakeCodexReasoningReplayKVClient{values: make(map[string][]byte)}
}

func (c *fakeCodexReasoningReplayKVClient) Enabled() bool {
	return true
}

func (c *fakeCodexReasoningReplayKVClient) HeartbeatOK() bool {
	return true
}

func (c *fakeCodexReasoningReplayKVClient) KVGet(
	_ context.Context,
	key string,
) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCount++
	if c.getErr != nil {
		return nil, false, c.getErr
	}
	value, ok := c.values[key]
	if !ok {
		return nil, false, nil
	}
	return bytes.Clone(value), true, nil
}

func (c *fakeCodexReasoningReplayKVClient) KVSet(
	_ context.Context,
	key string,
	value []byte,
	opts home.KVSetOptions,
) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCount++
	c.lastSetTTL = opts.EX
	if c.setErr != nil {
		return false, c.setErr
	}
	c.values[key] = bytes.Clone(value)
	return true, nil
}

func (c *fakeCodexReasoningReplayKVClient) KVCompareAndSwap(
	_ context.Context,
	key string,
	expected []byte,
	expectedExists bool,
	value []byte,
	ttl time.Duration,
) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCount++
	c.lastSetTTL = ttl
	if c.setErr != nil {
		return false, c.setErr
	}
	current, exists := c.values[key]
	if exists != expectedExists || (exists && !bytes.Equal(current, expected)) {
		return false, nil
	}
	c.values[key] = bytes.Clone(value)
	return true, nil
}

func (c *fakeCodexReasoningReplayKVClient) KVDel(
	_ context.Context,
	keys ...string,
) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delCount++
	if c.delErr != nil {
		return 0, c.delErr
	}
	var deleted int64
	for _, key := range keys {
		if _, ok := c.values[key]; ok {
			delete(c.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *fakeCodexReasoningReplayKVClient) KVExpire(
	_ context.Context,
	_ string,
	ttl time.Duration,
) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireCount++
	c.lastExpireTTL = ttl
	if c.expireErr != nil {
		return false, c.expireErr
	}
	return true, nil
}

func useFakeCodexReasoningReplayKVClient(
	t *testing.T,
	client *fakeCodexReasoningReplayKVClient,
	homeMode bool,
	errClient error,
) {
	t.Helper()
	previous := currentCodexReasoningReplayKVClient
	currentCodexReasoningReplayKVClient = func() (codexReasoningReplayKVClient, bool, error) {
		return client, homeMode, errClient
	}
	t.Cleanup(
		func() {
			currentCodexReasoningReplayKVClient = previous
		},
	)
}

func validCodexReasoningReplayEncryptedContentForTest(seed byte) string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for index := 9; index < len(payload); index++ {
		payload[index] = seed + byte(index)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func validCodexReasoningReplayItemForTest(seed byte) []byte {
	return []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` +
		validCodexReasoningReplayEncryptedContentForTest(seed) + `"}`)
}

func mustCodexReasoningReplayJSON(t *testing.T, items [][]byte) []byte {
	t.Helper()
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal replay items: %v", err)
	}
	return raw
}

func TestCodexReasoningReplayRequiredHomeReadAndSlidingExpire(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	client := newFakeCodexReasoningReplayKVClient()
	key := codexReasoningReplayKVKey("gpt-5.4", "session-home")
	item := validCodexReasoningReplayItemForTest(3)
	client.values[key] = mustCodexReasoningReplayJSON(t, [][]byte{item})
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	items, found, err := GetCodexReasoningReplayItemsRequired(
		context.Background(),
		"gpt-5.4",
		"session-home",
	)
	if err != nil {
		t.Fatalf("GetCodexReasoningReplayItemsRequired() error = %v", err)
	}
	if !found || len(items) != 1 || string(items[0]) != string(item) {
		t.Fatalf("GetCodexReasoningReplayItemsRequired() = %q, %v, want item, true", items, found)
	}
	if client.expireCount != 1 || client.lastExpireTTL != codexReasoningReplayCacheTTL {
		t.Fatalf(
			"KVExpire count/ttl = %d/%v, want 1/%v",
			client.expireCount,
			client.lastExpireTTL,
			codexReasoningReplayCacheTTL,
		)
	}
}

func TestCodexReasoningReplayRequiredHomeFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		client *fakeCodexReasoningReplayKVClient
	}{
		{
			name: "get",
			client: &fakeCodexReasoningReplayKVClient{
				values: make(map[string][]byte),
				getErr: errors.New("get failed"),
			},
		},
		{
			name: "expire",
			client: &fakeCodexReasoningReplayKVClient{
				values: map[string][]byte{
					codexReasoningReplayKVKey("gpt-5.4", "session-home"): mustCodexReasoningReplayJSON(
						t,
						[][]byte{validCodexReasoningReplayItemForTest(4)},
					),
				},
				expireErr: errors.New("expire failed"),
			},
		},
		{
			name: "delete",
			client: &fakeCodexReasoningReplayKVClient{
				values: make(map[string][]byte),
				delErr: errors.New("delete failed"),
			},
		},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				useFakeCodexReasoningReplayKVClient(t, testCase.client, true, nil)
				if testCase.name == "delete" {
					if err := DeleteCodexReasoningReplayItemRequired(
						context.Background(),
						"gpt-5.4",
						"session-home",
					); err == nil {
						t.Fatal("DeleteCodexReasoningReplayItemRequired() error = nil, want error")
					}
					return
				}
				if _, _, err := GetCodexReasoningReplayItemsRequired(
					context.Background(),
					"gpt-5.4",
					"session-home",
				); err == nil {
					t.Fatal("GetCodexReasoningReplayItemsRequired() error = nil, want error")
				}
			},
		)
	}
}

func TestCodexReasoningReplayAppendPreservesCumulativeTurnsInHome(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	client := newFakeCodexReasoningReplayKVClient()
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	first := [][]byte{
		[]byte(`{"type":"` + CodexReasoningReplayTurnType + `","id":"turn-1","assistant_fingerprint":"answer-1"}`),
		validCodexReasoningReplayItemForTest(11),
	}
	second := [][]byte{
		[]byte(`{"type":"` + CodexReasoningReplayTurnType + `","id":"turn-2","call_ids":["call-2"]}`),
		validCodexReasoningReplayItemForTest(12),
	}
	for _, turn := range [][][]byte{first, second, second} {
		if !AppendCodexReasoningReplayItemsBestEffort(
			context.Background(),
			"gpt-5.4",
			"session-home-append",
			turn,
		) {
			t.Fatal("append failed")
		}
	}

	items, found, err := GetCodexReasoningReplayItemsRequired(
		context.Background(),
		"gpt-5.4",
		"session-home-append",
	)
	if err != nil || !found {
		t.Fatalf("get cumulative turns = found %v err %v", found, err)
	}
	if len(items) != 4 {
		t.Fatalf("cumulative item count = %d, want 4: %q", len(items), items)
	}
	if got := gjson.GetBytes(items[0], "id").String(); got != "turn-1" {
		t.Fatalf("first turn id = %q, want turn-1", got)
	}
	if got := gjson.GetBytes(items[2], "id").String(); got != "turn-2" {
		t.Fatalf("second turn id = %q, want turn-2", got)
	}
}

func TestCodexReasoningReplayAppendHomeCASPreservesConcurrentTurns(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	client := newFakeCodexReasoningReplayKVClient()
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	const turnCount = 16
	var waitGroup sync.WaitGroup
	for turn := range turnCount {
		waitGroup.Add(1)
		go func(turnID int) {
			defer waitGroup.Done()
			items := [][]byte{
				[]byte(fmt.Sprintf(`{"type":"%s","id":"turn-%d"}`, CodexReasoningReplayTurnType, turnID)),
				validCodexReasoningReplayItemForTest(byte(30 + turnID)),
			}
			if !AppendCodexReasoningReplayItemsBestEffort(
				context.Background(),
				"gpt-5.4",
				"session-home-concurrent",
				items,
			) {
				t.Errorf("append turn %d failed", turnID)
			}
		}(turn)
	}
	waitGroup.Wait()

	items, found, err := GetCodexReasoningReplayItemsRequired(
		context.Background(),
		"gpt-5.4",
		"session-home-concurrent",
	)
	if err != nil || !found {
		t.Fatalf("get concurrent turns = found %v err %v", found, err)
	}
	if len(items) != turnCount*2 {
		t.Fatalf("concurrent cumulative item count = %d, want %d", len(items), turnCount*2)
	}
}

func TestCodexReasoningReplayAppendBoundsTurnsPerEntry(t *testing.T) {
	items := make([][]byte, 0, (codexReasoningReplayCacheMaxTurnsPerEntry+1)*2)
	for turn := 0; turn <= codexReasoningReplayCacheMaxTurnsPerEntry; turn++ {
		items = append(
			items,
			[]byte(fmt.Sprintf(`{"type":"%s","id":"turn-%d"}`, CodexReasoningReplayTurnType, turn)),
			validCodexReasoningReplayItemForTest(byte(50+turn)),
		)
	}

	trimmed := trimCodexReasoningReplayItems(items)
	if len(trimmed) != codexReasoningReplayCacheMaxTurnsPerEntry*2 {
		t.Fatalf(
			"trimmed item count = %d, want %d",
			len(trimmed),
			codexReasoningReplayCacheMaxTurnsPerEntry*2,
		)
	}
	if firstID := gjson.GetBytes(trimmed[0], "id").String(); firstID != "turn-1" {
		t.Fatalf("first retained turn = %q, want turn-1", firstID)
	}
}

func TestCodexReasoningReplayLocalCacheScopesByRuntime(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	firstContext := home.WithClientScope(
		context.Background(),
		home.ClientScope{ID: "service-a", Enabled: false},
	)
	secondContext := home.WithClientScope(
		context.Background(),
		home.ClientScope{ID: "service-b", Enabled: false},
	)
	first := validCodexReasoningReplayItemForTest(61)
	second := validCodexReasoningReplayItemForTest(62)
	if !cacheCodexReasoningReplayItemsBestEffort(firstContext, "gpt-5.4", "shared-session", [][]byte{first}) {
		t.Fatal("first scoped cache write failed")
	}
	if !cacheCodexReasoningReplayItemsBestEffort(secondContext, "gpt-5.4", "shared-session", [][]byte{second}) {
		t.Fatal("second scoped cache write failed")
	}

	firstItems, firstFound, firstErr := GetCodexReasoningReplayItemsRequired(
		firstContext,
		"gpt-5.4",
		"shared-session",
	)
	secondItems, secondFound, secondErr := GetCodexReasoningReplayItemsRequired(
		secondContext,
		"gpt-5.4",
		"shared-session",
	)
	if firstErr != nil || secondErr != nil || !firstFound || !secondFound {
		t.Fatalf(
			"scoped reads = first(%v, %v) second(%v, %v)",
			firstFound,
			firstErr,
			secondFound,
			secondErr,
		)
	}
	if string(firstItems[0]) != string(first) || string(secondItems[0]) != string(second) {
		t.Fatalf("runtime scopes shared replay state: first=%q second=%q", firstItems, secondItems)
	}
}
