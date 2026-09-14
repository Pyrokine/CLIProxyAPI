package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/tidwall/gjson"
)

func TestXAIC03ReplayCachePreservesAssistantWithToolOnlyAnchor(t *testing.T) {
	ClearXAIReasoningReplayCache()
	t.Cleanup(ClearXAIReasoningReplayCache)

	items := [][]byte{
		[]byte(`{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}`),
		[]byte(`{
			"type":"message",
			"role":"assistant",
			"content":[
				{"type":"output_text","text":"visible answer"},
				{"type":"input_text","text":"must drop"}
			]
		}`),
	}
	if !XAIReasoningReplayItems("grok-4.3", "tool-session", items) {
		t.Fatal("tool-only replay batch was rejected")
	}
	cached, ok := GetXAIReasoningReplayItems("grok-4.3", "tool-session")
	if !ok || len(cached) != 2 {
		t.Fatalf("cached replay items = %q, want tool call and assistant message", cached)
	}
	if got := gjson.GetBytes(cached[0], "type").String(); got != "function_call" {
		t.Fatalf("cached.0.type = %q, want function_call", got)
	}
	if got := gjson.GetBytes(cached[1], "content.0.text").String(); got != "visible answer" {
		t.Fatalf("cached assistant text = %q, want visible answer", got)
	}
	if got := len(gjson.GetBytes(cached[1], "content").Array()); got != 1 {
		t.Fatalf("cached assistant content parts = %d, want one safe output_text part", got)
	}
}

func TestXAIC03ReplayCacheRejectsAssistantWithoutReplayAnchor(t *testing.T) {
	ClearXAIReasoningReplayCache()
	t.Cleanup(ClearXAIReasoningReplayCache)

	if XAIReasoningReplayItems(
		"grok-4.3", "message-only", [][]byte{
			[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible answer"}]}`),
		},
	) {
		t.Fatal("assistant-only replay batch was accepted without encrypted reasoning or tool call")
	}
	if _, ok := GetXAIReasoningReplayItems("grok-4.3", "message-only"); ok {
		t.Fatal("assistant-only replay batch was stored")
	}
}

func TestXAIC03ReplayCachePreservesFunctionNamespace(t *testing.T) {
	ClearXAIReasoningReplayCache()
	t.Cleanup(ClearXAIReasoningReplayCache)

	if !XAIReasoningReplayItems(
		"grok-4.3", "namespace-session", [][]byte{
			[]byte(`{"type":"function_call","call_id":"call_1","namespace":"ops","name":"run","arguments":"{}"}`),
		},
	) {
		t.Fatal("namespaced function call was rejected")
	}
	cached, ok := GetXAIReasoningReplayItems("grok-4.3", "namespace-session")
	if !ok || len(cached) != 1 {
		t.Fatalf("cached replay items = %q, want one function call", cached)
	}
	if got := gjson.GetBytes(cached[0], "namespace").String(); got != "ops" {
		t.Fatalf("cached namespace = %q, want ops; item=%s", got, cached[0])
	}
}

type xaiC03ReplayKVBackend struct {
	mu              sync.Mutex
	values          map[string][]byte
	setErr          error
	setReturnsFalse bool
	deleteErr       error
	expireErr       error
	expireCalls     int
	lastSetTTL      time.Duration
	lastExpireTTL   time.Duration
}

type xaiC03ReplayKVClient struct {
	backend *xaiC03ReplayKVBackend
}

func (*xaiC03ReplayKVClient) Enabled() bool { return true }

func (*xaiC03ReplayKVClient) HeartbeatOK() bool { return true }

func (c *xaiC03ReplayKVClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.backend.mu.Lock()
	defer c.backend.mu.Unlock()
	value, ok := c.backend.values[key]
	return append([]byte(nil), value...), ok, nil
}

func (c *xaiC03ReplayKVClient) KVSet(
	_ context.Context,
	key string,
	value []byte,
	opts home.KVSetOptions,
) (bool, error) {
	c.backend.mu.Lock()
	defer c.backend.mu.Unlock()
	c.backend.lastSetTTL = opts.EX
	if c.backend.setErr != nil {
		return false, c.backend.setErr
	}
	if c.backend.setReturnsFalse {
		return false, nil
	}
	c.backend.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *xaiC03ReplayKVClient) KVDel(_ context.Context, keys ...string) (int64, error) {
	c.backend.mu.Lock()
	defer c.backend.mu.Unlock()
	if c.backend.deleteErr != nil {
		return 0, c.backend.deleteErr
	}
	var deleted int64
	for _, key := range keys {
		if _, ok := c.backend.values[key]; ok {
			delete(c.backend.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *xaiC03ReplayKVClient) KVExpire(_ context.Context, _ string, ttl time.Duration) (bool, error) {
	c.backend.mu.Lock()
	defer c.backend.mu.Unlock()
	c.backend.expireCalls++
	c.backend.lastExpireTTL = ttl
	if c.backend.expireErr != nil {
		return false, c.backend.expireErr
	}
	return true, nil
}

func useXAIC03ReplayKVClient(t *testing.T, client xaiReasoningReplayKVClient) {
	t.Helper()
	original := currentXAIReasoningReplayKVClient
	currentXAIReasoningReplayKVClient = func() (xaiReasoningReplayKVClient, bool, error) {
		return client, true, nil
	}
	t.Cleanup(
		func() {
			currentXAIReasoningReplayKVClient = original
		},
	)
}

func newXAIC03ReplayKVBackend() *xaiC03ReplayKVBackend {
	return &xaiC03ReplayKVBackend{values: make(map[string][]byte)}
}

func TestXAIC03ReplayCacheHomeKVCrossInstanceReadWrite(t *testing.T) {
	backend := newXAIC03ReplayKVBackend()
	writer := &xaiC03ReplayKVClient{backend: backend}
	reader := &xaiC03ReplayKVClient{backend: backend}
	active := xaiReasoningReplayKVClient(writer)
	original := currentXAIReasoningReplayKVClient
	currentXAIReasoningReplayKVClient = func() (xaiReasoningReplayKVClient, bool, error) {
		return active, true, nil
	}
	t.Cleanup(
		func() {
			currentXAIReasoningReplayKVClient = original
		},
	)

	items := [][]byte{
		[]byte(`{"type":"function_call","call_id":"call_1","namespace":"ops","name":"run","arguments":"{}"}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`),
	}
	status := StoreXAIReasoningReplayItems(context.Background(), "grok-4.3", "shared-session", items)
	if status != XAIReasoningReplayStored {
		t.Fatalf("StoreXAIReasoningReplayItems() status = %v, want stored", status)
	}

	ClearXAIReasoningReplayCache()
	active = reader
	cached, ok, err := GetXAIReasoningReplayItemsRequired(context.Background(), "grok-4.3", "shared-session")
	if err != nil || !ok || len(cached) != 2 {
		t.Fatalf("cross-instance get = items %q ok %v err %v", cached, ok, err)
	}
	if got := gjson.GetBytes(cached[0], "namespace").String(); got != "ops" {
		t.Fatalf("cross-instance namespace = %q, want ops; item=%s", got, cached[0])
	}
	if backend.lastSetTTL != xaiReasoningReplayCacheTTL || backend.lastExpireTTL != xaiReasoningReplayCacheTTL {
		t.Fatalf(
			"Home KV set/expire TTL = %v/%v, want %v",
			backend.lastSetTTL,
			backend.lastExpireTTL,
			xaiReasoningReplayCacheTTL,
		)
	}
}

func TestXAIC03ReplayCacheLocalStateIsolatedByServiceScope(t *testing.T) {
	ClearXAIReasoningReplayCache()
	t.Cleanup(ClearXAIReasoningReplayCache)

	ctxA := home.WithClientScope(context.Background(), home.ClientScope{ID: "service-a"})
	ctxB := home.WithClientScope(context.Background(), home.ClientScope{ID: "service-b"})
	if status := StoreXAIReasoningReplayItems(
		ctxA, "grok-4.3", "shared-session", [][]byte{
			[]byte(`{"type":"function_call","call_id":"call_a","name":"service_a","arguments":"{}"}`),
		},
	); status != XAIReasoningReplayStored {
		t.Fatalf("store service A status = %v, want stored", status)
	}
	if status := StoreXAIReasoningReplayItems(
		ctxB, "grok-4.3", "shared-session", [][]byte{
			[]byte(`{"type":"function_call","call_id":"call_b","name":"service_b","arguments":"{}"}`),
		},
	); status != XAIReasoningReplayStored {
		t.Fatalf("store service B status = %v, want stored", status)
	}

	itemsA, okA, errA := GetXAIReasoningReplayItemsRequired(ctxA, "grok-4.3", "shared-session")
	itemsB, okB, errB := GetXAIReasoningReplayItemsRequired(ctxB, "grok-4.3", "shared-session")
	if errA != nil || !okA || len(itemsA) != 1 || gjson.GetBytes(itemsA[0], "name").String() != "service_a" {
		t.Fatalf("service A replay = %q ok %v err %v", itemsA, okA, errA)
	}
	if errB != nil || !okB || len(itemsB) != 1 || gjson.GetBytes(itemsB[0], "name").String() != "service_b" {
		t.Fatalf("service B replay = %q ok %v err %v", itemsB, okB, errB)
	}
}

func TestXAIC03ReplayCacheHomeKVUsesRequestScopedClient(t *testing.T) {
	backendA := newXAIC03ReplayKVBackend()
	backendB := newXAIC03ReplayKVBackend()
	ctxA := home.WithClientScope(
		context.Background(), home.ClientScope{
			Client: &xaiC03ReplayKVClient{backend: backendA}, ID: "service-a", Enabled: true,
		},
	)
	ctxB := home.WithClientScope(
		context.Background(), home.ClientScope{
			Client: &xaiC03ReplayKVClient{backend: backendB}, ID: "service-b", Enabled: true,
		},
	)
	items := [][]byte{[]byte(`{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"}`)}
	if status := StoreXAIReasoningReplayItems(
		ctxA, "grok-4.3", "shared-session", items,
	); status != XAIReasoningReplayStored {
		t.Fatalf("store service A status = %v, want stored", status)
	}
	if cached, ok, err := GetXAIReasoningReplayItemsRequired(ctxB, "grok-4.3", "shared-session"); err != nil || ok {
		t.Fatalf("service B replay = %q ok %v err %v, want isolated miss", cached, ok, err)
	}
	if cached, ok, err := GetXAIReasoningReplayItemsRequired(
		ctxA, "grok-4.3", "shared-session",
	); err != nil || !ok || len(cached) != 1 {
		t.Fatalf("service A replay = %q ok %v err %v", cached, ok, err)
	}
}

func TestXAIC03ReplayCacheHomeReplacementUsesRequestScopedClient(t *testing.T) {
	backend := newXAIC03ReplayKVBackend()
	oldClient := &xaiC03ReplayKVClient{backend: backend}
	newClient := &xaiC03ReplayKVClient{backend: backend}
	oldCtx := home.WithClientScope(
		context.Background(), home.ClientScope{
			Client: oldClient, ID: "service", Enabled: true,
		},
	)
	newCtx := home.WithClientScope(
		context.Background(), home.ClientScope{
			Client: newClient, ID: "service", Enabled: true,
		},
	)
	items := [][]byte{[]byte(`{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"}`)}
	if status := StoreXAIReasoningReplayItems(
		oldCtx, "grok-4.3", "replacement-session", items,
	); status != XAIReasoningReplayStored {
		t.Fatalf("old lifetime store status = %v, want stored", status)
	}
	if cached, ok, err := GetXAIReasoningReplayItemsRequired(
		newCtx, "grok-4.3", "replacement-session",
	); err != nil || !ok || len(cached) != 1 {
		t.Fatalf("new lifetime replay = %q ok %v err %v", cached, ok, err)
	}

	gapCtx := home.WithClientScope(context.Background(), home.ClientScope{ID: "service", Enabled: true})
	if status := StoreXAIReasoningReplayItems(
		gapCtx, "grok-4.3", "gap-session", items,
	); status != XAIReasoningReplayStoreBackendError {
		t.Fatalf("replacement gap store status = %v, want backend error", status)
	}
	localCtx := home.WithClientScope(context.Background(), home.ClientScope{ID: "service"})
	if cached, ok, err := GetXAIReasoningReplayItemsRequired(localCtx, "grok-4.3", "gap-session"); err != nil || ok {
		t.Fatalf("replacement gap local replay = %q ok %v err %v, want no fallback write", cached, ok, err)
	}
}

func TestXAIC03ReplayCacheHomeKVReadValidatesStoredValue(t *testing.T) {
	backend := newXAIC03ReplayKVBackend()
	client := &xaiC03ReplayKVClient{backend: backend}
	ctx := home.WithClientScope(
		context.Background(), home.ClientScope{
			Client: client, ID: "service", Enabled: true,
		},
	)
	remoteKey := xaiReasoningReplayKVKey("grok-4.3", "invalid-session")
	oversized, errMarshal := json.Marshal(
		[][]byte{
			[]byte(`{"type":"function_call","call_id":"call_1","name":"run","arguments":"` +
				strings.Repeat("x", xaiReasoningReplayCacheMaxItemBytes) + `"}`),
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal oversized value: %v", errMarshal)
	}
	entryItems := make([][]byte, 0, 5)
	for i := range 5 {
		entryItems = append(
			entryItems, []byte(fmt.Sprintf(
				`{"type":"function_call","call_id":"call_%d","name":"run","arguments":"%s"}`,
				i,
				strings.Repeat("x", 220*1024),
			)),
		)
	}
	oversizedEntry, errMarshal := json.Marshal(entryItems)
	if errMarshal != nil {
		t.Fatalf("marshal oversized entry: %v", errMarshal)
	}
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "malformed JSON", raw: []byte(`{"broken"`)},
		{name: "oversized item", raw: oversized},
		{name: "oversized entry", raw: oversizedEntry},
		{
			name: "invalid signature",
			raw:  []byte(`["eyJ0eXBlIjoicmVhc29uaW5nIiwiZW5jcnlwdGVkX2NvbnRlbnQiOiJpbnZhbGlkIn0="]`),
		},
		{name: "unknown item", raw: []byte(`["eyJ0eXBlIjoidW5rbm93biJ9"]`)},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				backend.mu.Lock()
				backend.values[remoteKey] = append([]byte(nil), test.raw...)
				backend.expireCalls = 0
				backend.mu.Unlock()
				cached, ok, err := GetXAIReasoningReplayItemsRequired(ctx, "grok-4.3", "invalid-session")
				if err == nil || ok || cached != nil {
					t.Fatalf("invalid Home value replay = %q ok %v err %v", cached, ok, err)
				}
				if backend.expireCalls != 0 {
					t.Fatalf("invalid Home value expire calls = %d, want 0", backend.expireCalls)
				}
			},
		)
	}
}

func TestXAIC03ReplayCacheHomeKVExpireFailureStillReturnsState(t *testing.T) {
	backend := newXAIC03ReplayKVBackend()
	client := &xaiC03ReplayKVClient{backend: backend}
	useXAIC03ReplayKVClient(t, client)

	items := [][]byte{[]byte(`{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"}`)}
	status := StoreXAIReasoningReplayItems(context.Background(), "grok-4.3", "expire-session", items)
	if status != XAIReasoningReplayStored {
		t.Fatalf("StoreXAIReasoningReplayItems() status = %v, want stored", status)
	}
	backend.expireErr = errors.New("expire failed")
	cached, ok, err := GetXAIReasoningReplayItemsRequired(context.Background(), "grok-4.3", "expire-session")
	if err != nil || !ok || len(cached) != 1 {
		t.Fatalf("get after expire failure = items %q ok %v err %v", cached, ok, err)
	}
	if backend.expireCalls != 1 {
		t.Fatalf("expire calls = %d, want 1", backend.expireCalls)
	}
}

func TestXAIC03ReplayCacheHomeKVSetFailureReportsBackendError(t *testing.T) {
	backend := newXAIC03ReplayKVBackend()
	backend.setErr = errors.New("set failed")
	useXAIC03ReplayKVClient(t, &xaiC03ReplayKVClient{backend: backend})

	items := [][]byte{[]byte(`{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"}`)}
	status := StoreXAIReasoningReplayItems(context.Background(), "grok-4.3", "set-session", items)
	if status != XAIReasoningReplayStoreBackendError {
		t.Fatalf("StoreXAIReasoningReplayItems() status = %v, want backend error", status)
	}
}

func TestXAIC03ReplayCacheHomeKVSetNotWrittenReportsBackendError(t *testing.T) {
	backend := newXAIC03ReplayKVBackend()
	backend.setReturnsFalse = true
	useXAIC03ReplayKVClient(t, &xaiC03ReplayKVClient{backend: backend})

	items := [][]byte{[]byte(`{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"}`)}
	status := StoreXAIReasoningReplayItems(context.Background(), "grok-4.3", "set-session", items)
	if status != XAIReasoningReplayStoreBackendError {
		t.Fatalf("StoreXAIReasoningReplayItems() status = %v, want backend error", status)
	}
}

func TestXAIC03ReplayCacheHomeKVDeleteFailureIsReturned(t *testing.T) {
	backend := newXAIC03ReplayKVBackend()
	backend.deleteErr = errors.New("delete failed")
	useXAIC03ReplayKVClient(t, &xaiC03ReplayKVClient{backend: backend})

	err := DeleteXAIReasoningReplayItemRequired(context.Background(), "grok-4.3", "delete-session")
	if !errors.Is(err, backend.deleteErr) {
		t.Fatalf("DeleteXAIReasoningReplayItemRequired() error = %v, want %v", err, backend.deleteErr)
	}
}
