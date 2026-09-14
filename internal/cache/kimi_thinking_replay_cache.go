package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	// kimiThinkingReplayCacheTTL limits how long signed assistant content stays replayable.
	kimiThinkingReplayCacheTTL = time.Hour

	// kimiThinkingReplayCacheMaxEntries bounds process memory used for replay continuity.
	kimiThinkingReplayCacheMaxEntries = 10240

	// kimiThinkingReplayCacheEvictBatchSize leaves headroom after reaching capacity.
	kimiThinkingReplayCacheEvictBatchSize = 128

	// KimiThinkingReplayCacheMaxBytesPerEntry bounds one complete assistant content array.
	KimiThinkingReplayCacheMaxBytesPerEntry = 8 << 20

	// KimiThinkingReplayCacheMaxBlocksPerEntry prevents pathological content arrays.
	KimiThinkingReplayCacheMaxBlocksPerEntry = 512

	// kimiThinkingReplayCacheMaxTotalBytes bounds aggregate in-process replay content.
	kimiThinkingReplayCacheMaxTotalBytes = 256 << 20

	kimiThinkingReplayCacheMaxSerializedBytes = KimiThinkingReplayCacheMaxBytesPerEntry + 1024
)

type kimiThinkingReplayEntry struct {
	Content    []byte
	Timestamp  time.Time
	Generation string
	Deleted    bool
}

// KimiThinkingReplaySnapshot identifies the exact replay generation read for one request.
// Its fields are intentionally opaque outside this package.
type KimiThinkingReplaySnapshot struct {
	raw        []byte
	generation string
	loaded     bool
	found      bool
}

type kimiThinkingReplayHomeValue struct {
	Generation string          `json:"generation"`
	Deleted    bool            `json:"deleted,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
}

var (
	kimiThinkingReplayMu         sync.Mutex
	kimiThinkingReplayEntries    = make(map[string]kimiThinkingReplayEntry)
	kimiThinkingReplayTotalBytes int
)

type kimiThinkingReplayKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVCompareAndSwap(
		ctx context.Context,
		key string,
		expected []byte,
		expectedExists bool,
		value []byte,
		ttl time.Duration,
	) (bool, error)
	KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

var currentKimiThinkingReplayKVClient = func() (kimiThinkingReplayKVClient, bool, error) {
	client := homekv.Current()
	if client == nil {
		return nil, false, nil
	}
	if !client.Enabled() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", homekv.ErrDisabled)
	}
	if !client.HeartbeatOK() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", homekv.ErrNotConnected)
	}
	return client, true, nil
}

func kimiThinkingReplayRuntime(
	ctx context.Context,
) (client kimiThinkingReplayKVClient, homeMode bool, scopeID string, err error) {
	if scope, ok := homekv.ClientScopeFromContext(ctx); ok {
		scopeID = strings.TrimSpace(scope.ID)
		if !scope.Enabled {
			return nil, false, scopeID, nil
		}
		if scope.Client == nil {
			return nil, true, scopeID, fmt.Errorf("home kv store unavailable: %w", homekv.ErrNotConnected)
		}
		if !scope.Client.Enabled() {
			return nil, true, scopeID, fmt.Errorf("home kv store unavailable: %w", homekv.ErrDisabled)
		}
		if !scope.Client.HeartbeatOK() {
			return nil, true, scopeID, fmt.Errorf("home kv store unavailable: %w", homekv.ErrNotConnected)
		}
		scopedClient, ok := scope.Client.(kimiThinkingReplayKVClient)
		if !ok {
			return nil, true, scopeID, fmt.Errorf(
				"home kv store unavailable: %w",
				homekv.ErrCompareAndSwapUnsupported,
			)
		}
		return scopedClient, true, scopeID, nil
	}

	client, homeMode, err = currentKimiThinkingReplayKVClient()
	if scoped, ok := client.(interface{ RuntimeScopeID() string }); ok {
		scopeID = strings.TrimSpace(scoped.RuntimeScopeID())
	}
	return client, homeMode, scopeID, err
}

// GetKimiThinkingReplayWithSnapshotRequired retrieves replay content and the exact cache state read.
func GetKimiThinkingReplayWithSnapshotRequired(
	ctx context.Context,
	modelFamily,
	sessionKey string,
) ([]byte, KimiThinkingReplaySnapshot, bool, error) {
	key := kimiThinkingReplayCacheKey(modelFamily, sessionKey)
	if key == "" {
		return nil, KimiThinkingReplaySnapshot{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := kimiThinkingReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return nil, KimiThinkingReplaySnapshot{}, false, errClient
		}
		kvKey := kimiThinkingReplayKVKey(modelFamily, sessionKey)
		raw, errRead := readOrReserveKimiThinkingReplayHomeValue(ctx, client, kvKey)
		if errRead != nil {
			return nil, KimiThinkingReplaySnapshot{}, false, errRead
		}
		snapshot := KimiThinkingReplaySnapshot{
			raw:    append([]byte(nil), raw...),
			loaded: true,
			found:  true,
		}
		content, generation, deleted, okDecode := decodeKimiThinkingReplayHomeValue(raw)
		if !okDecode {
			return nil, KimiThinkingReplaySnapshot{}, false, fmt.Errorf("invalid kimi thinking replay content")
		}
		snapshot.generation = generation
		refreshed, errExpire := client.KVExpire(ctx, kvKey, kimiThinkingReplayCacheTTL)
		if errExpire != nil {
			return nil, KimiThinkingReplaySnapshot{}, false, errExpire
		}
		if !refreshed {
			return nil, KimiThinkingReplaySnapshot{}, false, fmt.Errorf(
				"kimi thinking replay state expired during read",
			)
		}
		if deleted {
			return nil, snapshot, false, nil
		}
		return content, snapshot, true, nil
	}
	if errClient != nil {
		return nil, KimiThinkingReplaySnapshot{}, false, errClient
	}

	key = kimiThinkingReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	kimiThinkingReplayMu.Lock()
	defer kimiThinkingReplayMu.Unlock()
	entry, ok := kimiThinkingReplayEntries[key]
	if !ok || now.Sub(entry.Timestamp) > kimiThinkingReplayCacheTTL {
		if ok {
			kimiThinkingReplayTotalBytes -= len(entry.Content)
			delete(kimiThinkingReplayEntries, key)
		}
		entry = reserveKimiThinkingReplayLocalLocked(key, now)
	}
	entry.Timestamp = now
	kimiThinkingReplayEntries[key] = entry
	snapshot := KimiThinkingReplaySnapshot{
		generation: entry.Generation,
		loaded:     true,
		found:      true,
	}
	if entry.Deleted {
		return nil, snapshot, false, nil
	}
	return append([]byte(nil), entry.Content...), snapshot, true, nil
}

// ReplaceKimiThinkingReplayIfUnchanged stores completed content only if the request snapshot is current.
func ReplaceKimiThinkingReplayIfUnchanged(
	ctx context.Context,
	modelFamily,
	sessionKey string,
	snapshot KimiThinkingReplaySnapshot,
	content []byte,
) (bool, error) {
	key := kimiThinkingReplayCacheKey(modelFamily, sessionKey)
	if key == "" {
		return false, nil
	}
	if !validKimiThinkingReplayContent(content) {
		return false, fmt.Errorf("invalid kimi thinking replay content")
	}
	if !snapshot.loaded {
		return false, fmt.Errorf("kimi thinking replay snapshot was not loaded")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cloned := append([]byte(nil), content...)
	generation := uuid.NewString()
	client, homeMode, scopeID, errClient := kimiThinkingReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return false, errClient
		}
		raw, errMarshal := marshalKimiThinkingReplayHomeValue(generation, false, cloned)
		if errMarshal != nil {
			return false, errMarshal
		}
		return client.KVCompareAndSwap(
			ctx,
			kimiThinkingReplayKVKey(modelFamily, sessionKey),
			snapshot.raw,
			snapshot.found,
			raw,
			kimiThinkingReplayCacheTTL,
		)
	}
	if errClient != nil {
		return false, errClient
	}

	key = kimiThinkingReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	kimiThinkingReplayMu.Lock()
	defer kimiThinkingReplayMu.Unlock()
	entry, found := kimiThinkingReplayEntries[key]
	if found != snapshot.found || (found && entry.Generation != snapshot.generation) {
		return false, nil
	}
	kimiThinkingReplayTotalBytes -= len(entry.Content)
	kimiThinkingReplayTotalBytes += len(cloned)
	kimiThinkingReplayEntries[key] = kimiThinkingReplayEntry{
		Content:    cloned,
		Timestamp:  time.Now(),
		Generation: generation,
	}
	enforceKimiThinkingReplayLimitsLocked()
	return true, nil
}

// DeleteKimiThinkingReplayIfUnchanged clears replay state only if the request snapshot is current.
func DeleteKimiThinkingReplayIfUnchanged(
	ctx context.Context,
	modelFamily,
	sessionKey string,
	snapshot KimiThinkingReplaySnapshot,
) (bool, error) {
	key := kimiThinkingReplayCacheKey(modelFamily, sessionKey)
	if key == "" {
		return false, nil
	}
	if !snapshot.loaded {
		return false, fmt.Errorf("kimi thinking replay snapshot was not loaded")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	generation := uuid.NewString()
	client, homeMode, scopeID, errClient := kimiThinkingReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return false, errClient
		}
		tombstone, errMarshal := marshalKimiThinkingReplayHomeValue(generation, true, nil)
		if errMarshal != nil {
			return false, errMarshal
		}
		return client.KVCompareAndSwap(
			ctx,
			kimiThinkingReplayKVKey(modelFamily, sessionKey),
			snapshot.raw,
			snapshot.found,
			tombstone,
			kimiThinkingReplayCacheTTL,
		)
	}
	if errClient != nil {
		return false, errClient
	}

	key = kimiThinkingReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	kimiThinkingReplayMu.Lock()
	defer kimiThinkingReplayMu.Unlock()
	entry, found := kimiThinkingReplayEntries[key]
	if found != snapshot.found || (found && entry.Generation != snapshot.generation) {
		return false, nil
	}
	kimiThinkingReplayTotalBytes -= len(entry.Content)
	kimiThinkingReplayEntries[key] = kimiThinkingReplayEntry{
		Timestamp:  time.Now(),
		Generation: generation,
		Deleted:    true,
	}
	return true, nil
}

// ClearKimiThinkingReplayCache clears all in-process Kimi replay state.
func ClearKimiThinkingReplayCache() {
	kimiThinkingReplayMu.Lock()
	kimiThinkingReplayEntries = make(map[string]kimiThinkingReplayEntry)
	kimiThinkingReplayTotalBytes = 0
	kimiThinkingReplayMu.Unlock()
}

func readOrReserveKimiThinkingReplayHomeValue(
	ctx context.Context,
	client kimiThinkingReplayKVClient,
	key string,
) ([]byte, error) {
	for range 4 {
		raw, found, errGet := client.KVGet(ctx, key)
		if errGet != nil {
			return nil, errGet
		}
		if found {
			if len(raw) > kimiThinkingReplayCacheMaxSerializedBytes {
				return nil, fmt.Errorf("kimi thinking replay value exceeds size limit")
			}
			return raw, nil
		}
		tombstone, errMarshal := marshalKimiThinkingReplayHomeValue(uuid.NewString(), true, nil)
		if errMarshal != nil {
			return nil, errMarshal
		}
		swapped, errReserve := client.KVCompareAndSwap(
			ctx,
			key,
			nil,
			false,
			tombstone,
			kimiThinkingReplayCacheTTL,
		)
		if errReserve != nil {
			return nil, errReserve
		}
		if swapped {
			return tombstone, nil
		}
	}
	return nil, fmt.Errorf("could not reserve absent kimi thinking replay state")
}

func marshalKimiThinkingReplayHomeValue(
	generation string,
	deleted bool,
	content []byte,
) ([]byte, error) {
	value := kimiThinkingReplayHomeValue{Generation: generation, Deleted: deleted}
	if !deleted {
		value.Content = append(json.RawMessage(nil), content...)
	}
	return json.Marshal(value)
}

func decodeKimiThinkingReplayHomeValue(raw []byte) ([]byte, string, bool, bool) {
	if len(raw) == 0 || len(raw) > kimiThinkingReplayCacheMaxSerializedBytes || !gjson.ValidBytes(raw) {
		return nil, "", false, false
	}
	var value kimiThinkingReplayHomeValue
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil || strings.TrimSpace(value.Generation) == "" {
		return nil, "", false, false
	}
	if value.Deleted {
		return nil, value.Generation, true, true
	}
	if !validKimiThinkingReplayContent(value.Content) {
		return nil, "", false, false
	}
	return append([]byte(nil), value.Content...), value.Generation, false, true
}

func reserveKimiThinkingReplayLocalLocked(key string, now time.Time) kimiThinkingReplayEntry {
	if len(kimiThinkingReplayEntries) >= kimiThinkingReplayCacheMaxEntries {
		evictOldestKimiThinkingReplayEntriesLocked(kimiThinkingReplayCacheEvictBatchSize)
	}
	entry := kimiThinkingReplayEntry{
		Timestamp:  now,
		Generation: uuid.NewString(),
		Deleted:    true,
	}
	kimiThinkingReplayEntries[key] = entry
	enforceKimiThinkingReplayLimitsLocked()
	return entry
}

func kimiThinkingReplayCacheKey(modelFamily, sessionKey string) string {
	modelFamily = strings.TrimSpace(modelFamily)
	sessionKey = strings.TrimSpace(sessionKey)
	if modelFamily == "" || sessionKey == "" {
		return ""
	}
	return strings.Join([]string{"kimi-thinking-replay", modelFamily, sessionKey}, "\x00")
}

func kimiThinkingReplayLocalCacheKey(scopeID, key string) string {
	scopeID = strings.TrimSpace(scopeID)
	if scopeID == "" {
		return key
	}
	return strings.Join([]string{scopeID, key}, "\x00")
}

func kimiThinkingReplayKVKey(modelFamily, sessionKey string) string {
	return "cpa:kimi:thinking-replay:" +
		kimiThinkingReplayHashKeyPart(strings.TrimSpace(modelFamily)) + ":" +
		kimiThinkingReplayHashKeyPart(strings.TrimSpace(sessionKey))
}

func kimiThinkingReplayHashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validKimiThinkingReplayContent(content []byte) bool {
	if len(content) == 0 || len(content) > KimiThinkingReplayCacheMaxBytesPerEntry || !gjson.ValidBytes(content) {
		return false
	}
	root := gjson.ParseBytes(content)
	if !root.IsArray() {
		return false
	}
	blocks := root.Array()
	if len(blocks) == 0 || len(blocks) > KimiThinkingReplayCacheMaxBlocksPerEntry {
		return false
	}
	for _, block := range blocks {
		if !block.IsObject() || block.Get("type").Type != gjson.String ||
			strings.TrimSpace(block.Get("type").String()) == "" {
			return false
		}
	}
	return true
}

func enforceKimiThinkingReplayLimitsLocked() {
	for len(kimiThinkingReplayEntries) > kimiThinkingReplayCacheMaxEntries ||
		kimiThinkingReplayTotalBytes > kimiThinkingReplayCacheMaxTotalBytes {
		if len(kimiThinkingReplayEntries) == 0 {
			kimiThinkingReplayTotalBytes = 0
			return
		}
		evictOldestKimiThinkingReplayEntriesLocked(kimiThinkingReplayCacheEvictBatchSize)
	}
}

func evictOldestKimiThinkingReplayEntriesLocked(count int) {
	if count <= 0 || len(kimiThinkingReplayEntries) == 0 {
		return
	}
	type candidate struct {
		key       string
		timestamp time.Time
	}
	candidates := make([]candidate, 0, len(kimiThinkingReplayEntries))
	for key, entry := range kimiThinkingReplayEntries {
		candidates = append(candidates, candidate{key: key, timestamp: entry.Timestamp})
	}
	sort.Slice(
		candidates, func(i, j int) bool {
			return candidates[i].timestamp.Before(candidates[j].timestamp)
		},
	)
	if count > len(candidates) {
		count = len(candidates)
	}
	for i := 0; i < count; i++ {
		entry := kimiThinkingReplayEntries[candidates[i].key]
		kimiThinkingReplayTotalBytes -= len(entry.Content)
		delete(kimiThinkingReplayEntries, candidates[i].key)
	}
}

func purgeExpiredKimiThinkingReplayCache(now time.Time) {
	kimiThinkingReplayMu.Lock()
	for key, entry := range kimiThinkingReplayEntries {
		if now.Sub(entry.Timestamp) > kimiThinkingReplayCacheTTL {
			kimiThinkingReplayTotalBytes -= len(entry.Content)
			delete(kimiThinkingReplayEntries, key)
		}
	}
	kimiThinkingReplayMu.Unlock()
}
