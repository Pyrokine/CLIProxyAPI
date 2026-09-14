package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

type fakeKimiThinkingReplayKVClient struct {
	mu            sync.Mutex
	values        map[string][]byte
	enabled       bool
	heartbeatOK   bool
	getErr        error
	casErr        error
	expireErr     error
	expireOK      bool
	expireCount   int
	lastCASTTL    time.Duration
	lastExpireTTL time.Duration
}

func newFakeKimiThinkingReplayKVClient() *fakeKimiThinkingReplayKVClient {
	return &fakeKimiThinkingReplayKVClient{
		values:      make(map[string][]byte),
		enabled:     true,
		heartbeatOK: true,
		expireOK:    true,
	}
}

func (c *fakeKimiThinkingReplayKVClient) Enabled() bool {
	return c.enabled
}

func (c *fakeKimiThinkingReplayKVClient) HeartbeatOK() bool {
	return c.heartbeatOK
}

func (c *fakeKimiThinkingReplayKVClient) KVGet(
	_ context.Context,
	key string,
) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return nil, false, c.getErr
	}
	value, ok := c.values[key]
	return append([]byte(nil), value...), ok, nil
}

func (c *fakeKimiThinkingReplayKVClient) KVSet(
	_ context.Context,
	key string,
	value []byte,
	_ homekv.KVSetOptions,
) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeKimiThinkingReplayKVClient) KVDel(
	_ context.Context,
	keys ...string,
) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var deleted int64
	for _, key := range keys {
		if _, ok := c.values[key]; ok {
			delete(c.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *fakeKimiThinkingReplayKVClient) KVCompareAndSwap(
	_ context.Context,
	key string,
	expected []byte,
	expectedExists bool,
	value []byte,
	ttl time.Duration,
) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastCASTTL = ttl
	if c.casErr != nil {
		return false, c.casErr
	}
	current, exists := c.values[key]
	if exists != expectedExists || (exists && !bytes.Equal(current, expected)) {
		return false, nil
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeKimiThinkingReplayKVClient) KVExpire(
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
	return c.expireOK, nil
}

type fakeKimiThinkingReplayKVWithoutCAS struct {
	client *fakeKimiThinkingReplayKVClient
}

func (c *fakeKimiThinkingReplayKVWithoutCAS) Enabled() bool {
	return c.client.Enabled()
}

func (c *fakeKimiThinkingReplayKVWithoutCAS) HeartbeatOK() bool {
	return c.client.HeartbeatOK()
}

func (c *fakeKimiThinkingReplayKVWithoutCAS) KVGet(
	ctx context.Context,
	key string,
) ([]byte, bool, error) {
	return c.client.KVGet(ctx, key)
}

func (c *fakeKimiThinkingReplayKVWithoutCAS) KVSet(
	ctx context.Context,
	key string,
	value []byte,
	opts homekv.KVSetOptions,
) (bool, error) {
	return c.client.KVSet(ctx, key, value, opts)
}

func (c *fakeKimiThinkingReplayKVWithoutCAS) KVDel(
	ctx context.Context,
	keys ...string,
) (int64, error) {
	return c.client.KVDel(ctx, keys...)
}

func (c *fakeKimiThinkingReplayKVWithoutCAS) KVExpire(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (bool, error) {
	return c.client.KVExpire(ctx, key, ttl)
}

func localKimiReplayContext(scopeID string) context.Context {
	return homekv.WithClientScope(
		context.Background(), homekv.ClientScope{
			ID:      scopeID,
			Enabled: false,
		},
	)
}

func homeKimiReplayContext(
	scopeID string,
	client homekv.KVClient,
) context.Context {
	return homekv.WithClientScope(
		context.Background(), homekv.ClientScope{
			Client:  client,
			ID:      scopeID,
			Enabled: true,
		},
	)
}

func kimiReplayTestContent(label string) []byte {
	return []byte(fmt.Sprintf(
		`[{"type":"thinking","thinking":"%s","signature":"sig-%s"},{"type":"tool_use","id":"tool-%s","name":"shell","input":{"value":%q}}]`,
		label,
		label,
		label,
		label,
	))
}

func TestKimiThinkingReplayLocalRoundTripAndCopyIsolation(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := localKimiReplayContext("runtime-a")
	const model, session = "k3", "session-a"

	_, snapshot, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil || found {
		t.Fatalf("initial read = found %v, err %v", found, errGet)
	}
	content := kimiReplayTestContent("first")
	written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		snapshot,
		content,
	)
	if errReplace != nil || !written {
		t.Fatalf("replace = %v, %v", written, errReplace)
	}

	got, _, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil || !found || !bytes.Equal(got, content) {
		t.Fatalf("round trip = %q, found %v, err %v", got, found, errGet)
	}
	got[0] = '!'
	gotAgain, _, foundAgain, errAgain := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errAgain != nil || !foundAgain || !bytes.Equal(gotAgain, content) {
		t.Fatalf("cached content was aliased: %q, found %v, err %v", gotAgain, foundAgain, errAgain)
	}
}

func TestKimiThinkingReplayRejectsStaleLocalMutation(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := localKimiReplayContext("runtime-a")
	const model, session = "k3", "stale-local"

	_, firstSnapshot, _, errFirst := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	_, staleSnapshot, _, errStale := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errFirst != nil || errStale != nil {
		t.Fatalf("snapshot reads failed: %v, %v", errFirst, errStale)
	}
	current := kimiReplayTestContent("current")
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		firstSnapshot,
		current,
	); errReplace != nil || !written {
		t.Fatalf("current replace = %v, %v", written, errReplace)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		staleSnapshot,
		kimiReplayTestContent("stale"),
	); errReplace != nil || written {
		t.Fatalf("stale replace = %v, %v", written, errReplace)
	}
	if deleted, errDelete := DeleteKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		staleSnapshot,
	); errDelete != nil || deleted {
		t.Fatalf("stale delete = %v, %v", deleted, errDelete)
	}
	got, _, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil || !found || !bytes.Equal(got, current) {
		t.Fatalf("newer state changed: %q, found %v, err %v", got, found, errGet)
	}
}

func TestKimiThinkingReplayGenerationBlocksContentABA(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := localKimiReplayContext("runtime-a")
	const model, session = "k3", "content-aba"

	_, initialSnapshot, _, errInitial := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errInitial != nil {
		t.Fatal(errInitial)
	}
	contentA := kimiReplayTestContent("a")
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		initialSnapshot,
		contentA,
	); errReplace != nil || !written {
		t.Fatalf("first A = %v, %v", written, errReplace)
	}
	_, staleSnapshot, _, errStale := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errStale != nil {
		t.Fatal(errStale)
	}
	_, currentSnapshot, _, errCurrent := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errCurrent != nil {
		t.Fatal(errCurrent)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		currentSnapshot,
		kimiReplayTestContent("b"),
	); errReplace != nil || !written {
		t.Fatalf("B = %v, %v", written, errReplace)
	}
	_, snapshotB, _, errB := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errB != nil {
		t.Fatal(errB)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		snapshotB,
		contentA,
	); errReplace != nil || !written {
		t.Fatalf("second A = %v, %v", written, errReplace)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		staleSnapshot,
		kimiReplayTestContent("stale"),
	); errReplace != nil || written {
		t.Fatalf("stale A snapshot crossed ABA = %v, %v", written, errReplace)
	}
}

func TestKimiThinkingReplayMissTombstoneFencesStaleFirstWriter(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := localKimiReplayContext("runtime-a")
	const model, session = "k3", "stale-first-writer"

	_, staleSnapshot, _, errStale := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	_, clearSnapshot, _, errClear := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errStale != nil || errClear != nil {
		t.Fatalf("snapshot reads failed: %v, %v", errStale, errClear)
	}
	if deleted, errDelete := DeleteKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		clearSnapshot,
	); errDelete != nil || !deleted {
		t.Fatalf("conditional delete = %v, %v", deleted, errDelete)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		staleSnapshot,
		kimiReplayTestContent("stale"),
	); errReplace != nil || written {
		t.Fatalf("stale first write = %v, %v", written, errReplace)
	}
}

func TestKimiThinkingReplayLocalRuntimeScopesAreIsolated(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	const model, session = "k3", "scoped-local"
	ctxA := localKimiReplayContext("runtime-a")
	ctxB := localKimiReplayContext("runtime-b")

	_, snapshot, _, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctxA, model, session)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctxA,
		model,
		session,
		snapshot,
		kimiReplayTestContent("scope-a"),
	); errReplace != nil || !written {
		t.Fatalf("scope A replace = %v, %v", written, errReplace)
	}
	if _, _, found, errReadA := GetKimiThinkingReplayWithSnapshotRequired(
		ctxA, model, session,
	); errReadA != nil || !found {
		t.Fatalf("scope A read = found %v, err %v", found, errReadA)
	}
	if _, _, found, errReadB := GetKimiThinkingReplayWithSnapshotRequired(
		ctxB, model, session,
	); errReadB != nil || found {
		t.Fatalf("scope B read = found %v, err %v", found, errReadB)
	}
}

func TestKimiThinkingReplayHomeStateCrossesRuntimeScopes(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	client := newFakeKimiThinkingReplayKVClient()
	ctxA := homeKimiReplayContext("runtime-a", client)
	ctxB := homeKimiReplayContext("runtime-b", client)
	const model, session = "k3", "home-shared"

	_, snapshot, _, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctxA, model, session)
	if errGet != nil {
		t.Fatal(errGet)
	}
	content := kimiReplayTestContent("home")
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctxA,
		model,
		session,
		snapshot,
		content,
	); errReplace != nil || !written {
		t.Fatalf("home replace = %v, %v", written, errReplace)
	}
	got, _, found, errRead := GetKimiThinkingReplayWithSnapshotRequired(ctxB, model, session)
	if errRead != nil || !found || !bytes.Equal(got, content) {
		t.Fatalf("cross-runtime home read = %q, found %v, err %v", got, found, errRead)
	}
}

func TestKimiThinkingReplayHomeRejectsStaleSnapshot(t *testing.T) {
	client := newFakeKimiThinkingReplayKVClient()
	ctx := homeKimiReplayContext("runtime-a", client)
	const model, session = "k3", "home-stale"

	_, firstSnapshot, _, errFirst := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	_, staleSnapshot, _, errStale := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errFirst != nil || errStale != nil {
		t.Fatalf("snapshot reads failed: %v, %v", errFirst, errStale)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		firstSnapshot,
		kimiReplayTestContent("home-current"),
	); errReplace != nil || !written {
		t.Fatalf("home current replace = %v, %v", written, errReplace)
	}
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		staleSnapshot,
		kimiReplayTestContent("home-stale"),
	); errReplace != nil || written {
		t.Fatalf("home stale replace = %v, %v", written, errReplace)
	}
}

func TestKimiThinkingReplayConfiguredHomeDoesNotFallbackToLocal(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := homekv.WithClientScope(
		context.Background(), homekv.ClientScope{
			ID:      "runtime-a",
			Enabled: true,
		},
	)
	_, _, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctx, "k3", "home-down")
	if found || !errors.Is(errGet, homekv.ErrNotConnected) {
		t.Fatalf("configured Home read = found %v, err %v", found, errGet)
	}
	if len(kimiThinkingReplayEntries) != 0 {
		t.Fatalf("configured Home failure wrote %d local entries", len(kimiThinkingReplayEntries))
	}
}

func TestKimiThinkingReplayConcurrentLocalReplaceHasOneWinner(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := localKimiReplayContext("runtime-a")
	const model, session = "k3", "parallel"
	_, snapshot, _, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil {
		t.Fatal(errGet)
	}

	var winners atomic.Int32
	var wait sync.WaitGroup
	for index := range 16 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
				ctx,
				model,
				session,
				snapshot,
				kimiReplayTestContent(fmt.Sprintf("parallel-%d", index)),
			)
			if errReplace != nil {
				t.Errorf("replace %d: %v", index, errReplace)
				return
			}
			if written {
				winners.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("replace winners = %d, want 1", got)
	}
}

func TestKimiThinkingReplayContentLimits(t *testing.T) {
	buildBlocks := func(count int) []byte {
		content := bytes.NewBufferString("[")
		for index := range count {
			if index > 0 {
				content.WriteByte(',')
			}
			content.WriteString(`{"type":"text","text":"x"}`)
		}
		content.WriteByte(']')
		return content.Bytes()
	}
	if content := buildBlocks(KimiThinkingReplayCacheMaxBlocksPerEntry); !validKimiThinkingReplayContent(content) {
		t.Fatal("content at block limit was rejected")
	}
	if content := buildBlocks(KimiThinkingReplayCacheMaxBlocksPerEntry + 1); validKimiThinkingReplayContent(content) {
		t.Fatal("content above block limit was accepted")
	}

	prefix := []byte(`[{"type":"text","text":"`)
	suffix := []byte(`"}]`)
	exact := make([]byte, 0, KimiThinkingReplayCacheMaxBytesPerEntry)
	exact = append(exact, prefix...)
	exact = append(
		exact,
		bytes.Repeat(
			[]byte("x"),
			KimiThinkingReplayCacheMaxBytesPerEntry-len(prefix)-len(suffix),
		)...,
	)
	exact = append(exact, suffix...)
	if len(exact) != KimiThinkingReplayCacheMaxBytesPerEntry || !validKimiThinkingReplayContent(exact) {
		t.Fatalf("content at byte limit was rejected: bytes=%d", len(exact))
	}
	raw, errMarshal := marshalKimiThinkingReplayHomeValue("generation", false, exact)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if len(raw) > kimiThinkingReplayCacheMaxSerializedBytes {
		t.Fatalf("serialized content bytes = %d", len(raw))
	}
	decoded, generation, deleted, okDecode := decodeKimiThinkingReplayHomeValue(raw)
	if !okDecode || deleted || generation != "generation" || !bytes.Equal(decoded, exact) {
		t.Fatalf("Home boundary decode = ok %v, deleted %v, generation %q", okDecode, deleted, generation)
	}

	oversized := append(bytes.Clone(exact), ' ')
	if validKimiThinkingReplayContent(oversized) {
		t.Fatal("content above byte limit was accepted")
	}
}

func TestKimiThinkingReplayLocalByteAccounting(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	ctx := localKimiReplayContext("runtime-a")
	const model, session = "k3", "byte-accounting"

	_, snapshot, _, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil {
		t.Fatal(errGet)
	}
	first := kimiReplayTestContent("first")
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		snapshot,
		first,
	); errReplace != nil || !written {
		t.Fatalf("first replace = %v, %v", written, errReplace)
	}
	if kimiThinkingReplayTotalBytes != len(first) {
		t.Fatalf("bytes after first replace = %d, want %d", kimiThinkingReplayTotalBytes, len(first))
	}

	_, snapshot, _, errGet = GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil {
		t.Fatal(errGet)
	}
	second := kimiReplayTestContent("second-longer")
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		snapshot,
		second,
	); errReplace != nil || !written {
		t.Fatalf("second replace = %v, %v", written, errReplace)
	}
	if kimiThinkingReplayTotalBytes != len(second) {
		t.Fatalf("bytes after second replace = %d, want %d", kimiThinkingReplayTotalBytes, len(second))
	}

	_, snapshot, _, errGet = GetKimiThinkingReplayWithSnapshotRequired(ctx, model, session)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if deleted, errDelete := DeleteKimiThinkingReplayIfUnchanged(
		ctx,
		model,
		session,
		snapshot,
	); errDelete != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, errDelete)
	}
	if kimiThinkingReplayTotalBytes != 0 {
		t.Fatalf("bytes after delete = %d, want 0", kimiThinkingReplayTotalBytes)
	}
}

func TestKimiThinkingReplayHomeHealthFailuresDoNotFallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		client  homekv.KVClient
		wantErr error
	}{
		{name: "nil client", wantErr: homekv.ErrNotConnected},
		{
			name: "disabled client",
			client: func() homekv.KVClient {
				client := newFakeKimiThinkingReplayKVClient()
				client.enabled = false
				return client
			}(),
			wantErr: homekv.ErrDisabled,
		},
		{
			name: "disconnected client",
			client: func() homekv.KVClient {
				client := newFakeKimiThinkingReplayKVClient()
				client.heartbeatOK = false
				return client
			}(),
			wantErr: homekv.ErrNotConnected,
		},
		{
			name: "CAS unsupported",
			client: &fakeKimiThinkingReplayKVWithoutCAS{
				client: newFakeKimiThinkingReplayKVClient(),
			},
			wantErr: homekv.ErrCompareAndSwapUnsupported,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				ClearKimiThinkingReplayCache()
				ctx := homeKimiReplayContext("runtime-a", test.client)
				_, snapshot, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					"k3",
					"home-health",
				)
				if found || !errors.Is(errGet, test.wantErr) {
					t.Fatalf("read = found %v, snapshot %+v, err %v", found, snapshot, errGet)
				}
				if snapshot.loaded || len(kimiThinkingReplayEntries) != 0 {
					t.Fatalf(
						"failed Home read left writable/local state: snapshot=%+v entries=%d", snapshot,
						len(kimiThinkingReplayEntries),
					)
				}
			},
		)
	}
}

func TestKimiThinkingReplayHomeOperationErrorsReturnZeroSnapshot(t *testing.T) {
	operationError := errors.New("home operation failed")
	for _, test := range []struct {
		name  string
		setup func(*fakeKimiThinkingReplayKVClient)
	}{
		{name: "get", setup: func(client *fakeKimiThinkingReplayKVClient) { client.getErr = operationError }},
		{
			name:  "reservation CAS",
			setup: func(client *fakeKimiThinkingReplayKVClient) { client.casErr = operationError },
		},
		{
			name: "expire error",
			setup: func(client *fakeKimiThinkingReplayKVClient) {
				raw, errMarshal := marshalKimiThinkingReplayHomeValue(
					"generation",
					false,
					kimiReplayTestContent("expire-error"),
				)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				client.values[kimiThinkingReplayKVKey("k3", "home-operation")] = raw
				client.expireErr = operationError
			},
		},
		{
			name: "expire missing",
			setup: func(client *fakeKimiThinkingReplayKVClient) {
				raw, errMarshal := marshalKimiThinkingReplayHomeValue(
					"generation",
					false,
					kimiReplayTestContent("expire-missing"),
				)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				client.values[kimiThinkingReplayKVKey("k3", "home-operation")] = raw
				client.expireOK = false
			},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				client := newFakeKimiThinkingReplayKVClient()
				test.setup(client)
				ctx := homeKimiReplayContext("runtime-a", client)
				_, snapshot, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					"k3",
					"home-operation",
				)
				if errGet == nil {
					t.Fatal("read error = nil, want Home read failure")
				}
				if found || snapshot.loaded {
					t.Fatalf("read = found %v, snapshot %+v, err %v", found, snapshot, errGet)
				}
			},
		)
	}
}

func TestKimiThinkingReplayInvalidHomeValueDoesNotRefreshTTL(t *testing.T) {
	client := newFakeKimiThinkingReplayKVClient()
	client.values[kimiThinkingReplayKVKey("k3", "invalid-home")] = []byte(`{"generation":"","content":[]}`)
	ctx := homeKimiReplayContext("runtime-a", client)
	_, snapshot, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		"k3",
		"invalid-home",
	)
	if errGet == nil {
		t.Fatal("invalid Home read error = nil")
	}
	// The API returns a zero snapshot on decode failure; this test verifies that error contract.
	// noinspection GoDfaErrorMayBeNotNil
	if found || snapshot.loaded {
		t.Fatalf("invalid Home read = found %v, snapshot %+v, err %v", found, snapshot, errGet)
	}
	if client.expireCount != 0 {
		t.Fatalf("invalid Home value refreshed TTL %d times", client.expireCount)
	}
}

func TestKimiThinkingReplayScopedHomeOverridesGlobalClient(t *testing.T) {
	client := newFakeKimiThinkingReplayKVClient()
	previous := currentKimiThinkingReplayKVClient
	currentKimiThinkingReplayKVClient = func() (kimiThinkingReplayKVClient, bool, error) {
		t.Fatal("global Home client was consulted despite request scope")
		return nil, false, nil
	}
	t.Cleanup(
		func() {
			currentKimiThinkingReplayKVClient = previous
		},
	)
	ctx := homeKimiReplayContext("runtime-a", client)
	_, snapshot, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		"k3",
		"scoped-home",
	)
	if errGet != nil || found || !snapshot.loaded {
		t.Fatalf("scoped Home read = found %v, snapshot %+v, err %v", found, snapshot, errGet)
	}
	if client.lastCASTTL != kimiThinkingReplayCacheTTL ||
		client.lastExpireTTL != kimiThinkingReplayCacheTTL {
		t.Fatalf("Home TTLs = CAS %v, expire %v", client.lastCASTTL, client.lastExpireTTL)
	}
}

func TestKimiThinkingReplayRejectsUnloadedSnapshotAndInvalidContent(t *testing.T) {
	ctx := localKimiReplayContext("runtime-a")
	if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		"k3",
		"unloaded",
		KimiThinkingReplaySnapshot{},
		kimiReplayTestContent("valid"),
	); errReplace == nil || written {
		t.Fatalf("unloaded replace = %v, %v", written, errReplace)
	}
	if deleted, errDelete := DeleteKimiThinkingReplayIfUnchanged(
		ctx,
		"k3",
		"unloaded",
		KimiThinkingReplaySnapshot{},
	); errDelete == nil || deleted {
		t.Fatalf("unloaded delete = %v, %v", deleted, errDelete)
	}
	_, snapshot, _, errGet := GetKimiThinkingReplayWithSnapshotRequired(ctx, "k3", "invalid")
	if errGet != nil {
		t.Fatal(errGet)
	}
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{name: "non-array", content: []byte(`{"not":"an array"}`)},
		{name: "untyped block", content: []byte(`[{"text":"missing type"}]`)},
		{name: "primitive block", content: []byte(`["invalid"]`)},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if written, errReplace := ReplaceKimiThinkingReplayIfUnchanged(
					ctx,
					"k3",
					"invalid",
					snapshot,
					test.content,
				); errReplace == nil || written {
					t.Fatalf("invalid replace = %v, %v", written, errReplace)
				}
			},
		)
	}
}

func TestKimiThinkingReplayLatestReservationSurvivesEntryEviction(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	oldTimestamp := time.Now().Add(-time.Hour)
	for index := range kimiThinkingReplayCacheMaxEntries {
		key := fmt.Sprintf("old-%05d", index)
		kimiThinkingReplayEntries[key] = kimiThinkingReplayEntry{
			Timestamp:  oldTimestamp,
			Generation: key,
			Deleted:    true,
		}
	}
	ctx := localKimiReplayContext("runtime-a")
	_, snapshot, found, errGet := GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		"k3",
		"latest-reservation",
	)
	if errGet != nil || found || !snapshot.loaded {
		t.Fatalf("latest reservation = found %v, snapshot %+v, err %v", found, snapshot, errGet)
	}
	key := kimiThinkingReplayLocalCacheKey(
		"runtime-a",
		kimiThinkingReplayCacheKey("k3", "latest-reservation"),
	)
	if _, ok := kimiThinkingReplayEntries[key]; !ok {
		t.Fatal("latest reservation was evicted")
	}
	if len(kimiThinkingReplayEntries) > kimiThinkingReplayCacheMaxEntries {
		t.Fatalf("entry count = %d", len(kimiThinkingReplayEntries))
	}
}

func TestPurgeExpiredKimiThinkingReplayCache(t *testing.T) {
	ClearKimiThinkingReplayCache()
	t.Cleanup(ClearKimiThinkingReplayCache)
	content := kimiReplayTestContent("expired")
	key := kimiThinkingReplayCacheKey("k3", "expired")
	kimiThinkingReplayEntries[key] = kimiThinkingReplayEntry{
		Content:    content,
		Timestamp:  time.Now().Add(-kimiThinkingReplayCacheTTL - time.Minute),
		Generation: "expired-generation",
	}
	kimiThinkingReplayTotalBytes = len(content)

	purgeExpiredKimiThinkingReplayCache(time.Now())
	if len(kimiThinkingReplayEntries) != 0 || kimiThinkingReplayTotalBytes != 0 {
		t.Fatalf(
			"expired cache remains: entries=%d bytes=%d",
			len(kimiThinkingReplayEntries),
			kimiThinkingReplayTotalBytes,
		)
	}
}
