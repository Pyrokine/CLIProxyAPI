package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	// claudeThinkingReplayCacheTTL limits how long signed assistant turns stay replayable.
	claudeThinkingReplayCacheTTL = time.Hour

	// claudeThinkingReplayCacheMaxEntries bounds process memory used by replay continuity.
	claudeThinkingReplayCacheMaxEntries = 10240

	// claudeThinkingReplayCacheEvictBatchSize leaves headroom after reaching capacity.
	claudeThinkingReplayCacheEvictBatchSize = 128

	// claudeThinkingReplayCacheMaxBytesPerSession bounds all cached assistant turns for one session.
	claudeThinkingReplayCacheMaxBytesPerSession = 8 << 20

	// claudeThinkingReplayCacheMaxTurnsPerSession bounds assistant turns retained for one session.
	claudeThinkingReplayCacheMaxTurnsPerSession = 64

	// claudeThinkingReplayCacheMaxBlocksPerTurn prevents pathological content arrays.
	claudeThinkingReplayCacheMaxBlocksPerTurn = 512

	// claudeThinkingReplayCacheMaxTotalBytes bounds aggregate in-process replay content.
	claudeThinkingReplayCacheMaxTotalBytes = 256 << 20

	claudeThinkingReplayCacheMaxSerializedBytes = claudeThinkingReplayCacheMaxBytesPerSession + 1024
)

type claudeThinkingReplayEntry struct {
	Contents   [][]byte
	Timestamp  time.Time
	Generation string
	Deleted    bool
}

// ClaudeThinkingReplaySnapshot identifies the exact replay generation read for one request.
// Its fields are intentionally opaque outside this package.
type ClaudeThinkingReplaySnapshot = KimiThinkingReplaySnapshot

type claudeThinkingReplayHomeValue struct {
	Generation string            `json:"generation"`
	Deleted    bool              `json:"deleted,omitempty"`
	Contents   []json.RawMessage `json:"contents,omitempty"`
}

var (
	claudeThinkingReplayMu         sync.Mutex
	claudeThinkingReplayEntries    = make(map[string]claudeThinkingReplayEntry)
	claudeThinkingReplayTotalBytes int
)

// GetClaudeThinkingReplayWithSnapshotRequired retrieves all replayable turns and the exact cache state read.
func GetClaudeThinkingReplayWithSnapshotRequired(
	ctx context.Context,
	modelFamily,
	sessionKey string,
) ([][]byte, ClaudeThinkingReplaySnapshot, bool, error) {
	key := claudeThinkingReplayCacheKey(modelFamily, sessionKey)
	if key == "" {
		return nil, ClaudeThinkingReplaySnapshot{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := kimiThinkingReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return nil, ClaudeThinkingReplaySnapshot{}, false, errClient
		}
		kvKey := claudeThinkingReplayKVKey(modelFamily, sessionKey)
		raw, errRead := readOrReserveClaudeThinkingReplayHomeValue(ctx, client, kvKey)
		if errRead != nil {
			return nil, ClaudeThinkingReplaySnapshot{}, false, errRead
		}
		snapshot := ClaudeThinkingReplaySnapshot{
			raw:    append([]byte(nil), raw...),
			loaded: true,
			found:  true,
		}
		contents, generation, deleted, okDecode := decodeClaudeThinkingReplayHomeValue(raw)
		if !okDecode {
			return nil, ClaudeThinkingReplaySnapshot{}, false, fmt.Errorf("invalid Claude thinking replay content")
		}
		snapshot.generation = generation
		refreshed, errExpire := client.KVExpire(ctx, kvKey, claudeThinkingReplayCacheTTL)
		if errExpire != nil {
			return nil, ClaudeThinkingReplaySnapshot{}, false, errExpire
		}
		if !refreshed {
			return nil, ClaudeThinkingReplaySnapshot{}, false, fmt.Errorf(
				"claude thinking replay state expired during read",
			)
		}
		if deleted {
			return nil, snapshot, false, nil
		}
		return cloneClaudeThinkingReplayContents(contents), snapshot, true, nil
	}
	if errClient != nil {
		return nil, ClaudeThinkingReplaySnapshot{}, false, errClient
	}

	key = kimiThinkingReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	claudeThinkingReplayMu.Lock()
	defer claudeThinkingReplayMu.Unlock()
	entry, ok := claudeThinkingReplayEntries[key]
	if !ok || now.Sub(entry.Timestamp) > claudeThinkingReplayCacheTTL {
		if ok {
			claudeThinkingReplayTotalBytes -= claudeThinkingReplayEntryBytes(entry.Contents)
			delete(claudeThinkingReplayEntries, key)
		}
		entry = reserveClaudeThinkingReplayLocalLocked(key, now)
	}
	entry.Timestamp = now
	claudeThinkingReplayEntries[key] = entry
	snapshot := ClaudeThinkingReplaySnapshot{
		generation: entry.Generation,
		loaded:     true,
		found:      true,
	}
	if entry.Deleted {
		return nil, snapshot, false, nil
	}
	return cloneClaudeThinkingReplayContents(entry.Contents), snapshot, true, nil
}

// ReplaceClaudeThinkingReplayIfUnchanged appends one completed assistant turn if the snapshot is current.
func ReplaceClaudeThinkingReplayIfUnchanged(
	ctx context.Context,
	modelFamily,
	sessionKey string,
	snapshot ClaudeThinkingReplaySnapshot,
	content []byte,
) (bool, error) {
	key := claudeThinkingReplayCacheKey(modelFamily, sessionKey)
	if key == "" {
		return false, nil
	}
	if !validClaudeThinkingReplayContent(content) {
		return false, fmt.Errorf("invalid Claude thinking replay content")
	}
	if !snapshot.loaded {
		return false, fmt.Errorf("claude thinking replay snapshot was not loaded")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := kimiThinkingReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return false, errClient
		}
		contents, _, deleted, okDecode := decodeClaudeThinkingReplayHomeValue(snapshot.raw)
		if !okDecode {
			return false, fmt.Errorf("invalid Claude thinking replay snapshot")
		}
		if deleted {
			contents = nil
		}
		contents = appendClaudeThinkingReplayContent(contents, content)
		raw, errMarshal := marshalClaudeThinkingReplayHomeValue(uuid.NewString(), false, contents)
		if errMarshal != nil {
			return false, errMarshal
		}
		return client.KVCompareAndSwap(
			ctx,
			claudeThinkingReplayKVKey(modelFamily, sessionKey),
			snapshot.raw,
			snapshot.found,
			raw,
			claudeThinkingReplayCacheTTL,
		)
	}
	if errClient != nil {
		return false, errClient
	}

	key = kimiThinkingReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	claudeThinkingReplayMu.Lock()
	defer claudeThinkingReplayMu.Unlock()
	entry, found := claudeThinkingReplayEntries[key]
	if found != snapshot.found || (found && entry.Generation != snapshot.generation) {
		return false, nil
	}
	contents := entry.Contents
	if entry.Deleted {
		contents = nil
	}
	updated := appendClaudeThinkingReplayContent(contents, content)
	claudeThinkingReplayTotalBytes -= claudeThinkingReplayEntryBytes(entry.Contents)
	claudeThinkingReplayTotalBytes += claudeThinkingReplayEntryBytes(updated)
	claudeThinkingReplayEntries[key] = claudeThinkingReplayEntry{
		Contents:   updated,
		Timestamp:  time.Now(),
		Generation: uuid.NewString(),
	}
	enforceClaudeThinkingReplayLimitsLocked()
	return true, nil
}

// DeleteClaudeThinkingReplayIfUnchanged clears replay state only if the snapshot is current.
func DeleteClaudeThinkingReplayIfUnchanged(
	ctx context.Context,
	modelFamily,
	sessionKey string,
	snapshot ClaudeThinkingReplaySnapshot,
) (bool, error) {
	key := claudeThinkingReplayCacheKey(modelFamily, sessionKey)
	if key == "" {
		return false, nil
	}
	if !snapshot.loaded {
		return false, fmt.Errorf("claude thinking replay snapshot was not loaded")
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
		tombstone, errMarshal := marshalClaudeThinkingReplayHomeValue(generation, true, nil)
		if errMarshal != nil {
			return false, errMarshal
		}
		return client.KVCompareAndSwap(
			ctx,
			claudeThinkingReplayKVKey(modelFamily, sessionKey),
			snapshot.raw,
			snapshot.found,
			tombstone,
			claudeThinkingReplayCacheTTL,
		)
	}
	if errClient != nil {
		return false, errClient
	}

	key = kimiThinkingReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	claudeThinkingReplayMu.Lock()
	defer claudeThinkingReplayMu.Unlock()
	entry, found := claudeThinkingReplayEntries[key]
	if found != snapshot.found || (found && entry.Generation != snapshot.generation) {
		return false, nil
	}
	claudeThinkingReplayTotalBytes -= claudeThinkingReplayEntryBytes(entry.Contents)
	claudeThinkingReplayEntries[key] = claudeThinkingReplayEntry{
		Timestamp:  time.Now(),
		Generation: generation,
		Deleted:    true,
	}
	return true, nil
}

// ClearClaudeThinkingReplayCache clears all in-process Claude replay state.
func ClearClaudeThinkingReplayCache() {
	claudeThinkingReplayMu.Lock()
	claudeThinkingReplayEntries = make(map[string]claudeThinkingReplayEntry)
	claudeThinkingReplayTotalBytes = 0
	claudeThinkingReplayMu.Unlock()
}

func readOrReserveClaudeThinkingReplayHomeValue(
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
			if len(raw) > claudeThinkingReplayCacheMaxSerializedBytes {
				return nil, fmt.Errorf("claude thinking replay value exceeds size limit")
			}
			return raw, nil
		}
		tombstone, errMarshal := marshalClaudeThinkingReplayHomeValue(uuid.NewString(), true, nil)
		if errMarshal != nil {
			return nil, errMarshal
		}
		swapped, errReserve := client.KVCompareAndSwap(
			ctx,
			key,
			nil,
			false,
			tombstone,
			claudeThinkingReplayCacheTTL,
		)
		if errReserve != nil {
			return nil, errReserve
		}
		if swapped {
			return tombstone, nil
		}
	}
	return nil, fmt.Errorf("could not reserve absent Claude thinking replay state")
}

func marshalClaudeThinkingReplayHomeValue(
	generation string,
	deleted bool,
	contents [][]byte,
) ([]byte, error) {
	value := claudeThinkingReplayHomeValue{Generation: generation, Deleted: deleted}
	if !deleted {
		value.Contents = make([]json.RawMessage, 0, len(contents))
		for _, content := range contents {
			value.Contents = append(value.Contents, append(json.RawMessage(nil), content...))
		}
	}
	return json.Marshal(value)
}

func decodeClaudeThinkingReplayHomeValue(raw []byte) ([][]byte, string, bool, bool) {
	if len(raw) == 0 || len(raw) > claudeThinkingReplayCacheMaxSerializedBytes || !gjson.ValidBytes(raw) {
		return nil, "", false, false
	}
	var value claudeThinkingReplayHomeValue
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil ||
		strings.TrimSpace(value.Generation) == "" {
		return nil, "", false, false
	}
	if value.Deleted {
		return nil, value.Generation, true, true
	}
	contents := make([][]byte, 0, len(value.Contents))
	for _, content := range value.Contents {
		if !validClaudeThinkingReplayContent(content) {
			return nil, "", false, false
		}
		contents = append(contents, append([]byte(nil), content...))
	}
	if len(contents) == 0 || len(contents) > claudeThinkingReplayCacheMaxTurnsPerSession ||
		claudeThinkingReplayEntryBytes(contents) > claudeThinkingReplayCacheMaxBytesPerSession {
		return nil, "", false, false
	}
	return contents, value.Generation, false, true
}

func reserveClaudeThinkingReplayLocalLocked(key string, now time.Time) claudeThinkingReplayEntry {
	if len(claudeThinkingReplayEntries) >= claudeThinkingReplayCacheMaxEntries {
		evictOldestClaudeThinkingReplayEntriesLocked(claudeThinkingReplayCacheEvictBatchSize)
	}
	entry := claudeThinkingReplayEntry{
		Timestamp:  now,
		Generation: uuid.NewString(),
		Deleted:    true,
	}
	claudeThinkingReplayEntries[key] = entry
	enforceClaudeThinkingReplayLimitsLocked()
	return entry
}

func appendClaudeThinkingReplayContent(contents [][]byte, content []byte) [][]byte {
	cloned := cloneClaudeThinkingReplayContents(contents)
	for _, existing := range cloned {
		if claudeThinkingReplayJSONEqual(existing, content) {
			return cloned
		}
	}
	cloned = append(cloned, append([]byte(nil), content...))
	for len(cloned) > claudeThinkingReplayCacheMaxTurnsPerSession ||
		claudeThinkingReplayEntryBytes(cloned) > claudeThinkingReplayCacheMaxBytesPerSession {
		cloned = cloned[1:]
	}
	return cloned
}

func cloneClaudeThinkingReplayContents(contents [][]byte) [][]byte {
	cloned := make([][]byte, 0, len(contents))
	for _, content := range contents {
		cloned = append(cloned, append([]byte(nil), content...))
	}
	return cloned
}

func claudeThinkingReplayEntryBytes(contents [][]byte) int {
	total := 0
	for _, content := range contents {
		total += len(content)
	}
	return total
}

func claudeThinkingReplayCacheKey(modelFamily, sessionKey string) string {
	modelFamily = strings.TrimSpace(modelFamily)
	sessionKey = strings.TrimSpace(sessionKey)
	if modelFamily == "" || sessionKey == "" {
		return ""
	}
	return strings.Join([]string{"claude-thinking-replay", modelFamily, sessionKey}, "\x00")
}

func claudeThinkingReplayKVKey(modelFamily, sessionKey string) string {
	return "cpa:claude:thinking-replay:" +
		kimiThinkingReplayHashKeyPart(strings.TrimSpace(modelFamily)) + ":" +
		kimiThinkingReplayHashKeyPart(strings.TrimSpace(sessionKey))
}

func validClaudeThinkingReplayContent(content []byte) bool {
	if len(content) == 0 || len(content) > claudeThinkingReplayCacheMaxBytesPerSession ||
		!gjson.ValidBytes(content) {
		return false
	}
	root := gjson.ParseBytes(content)
	if !root.IsArray() {
		return false
	}
	blocks := root.Array()
	if len(blocks) == 0 || len(blocks) > claudeThinkingReplayCacheMaxBlocksPerTurn {
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

func claudeThinkingReplayJSONEqual(left, right []byte) bool {
	leftCanonical, leftOK := claudeThinkingReplayCanonicalJSON(left)
	rightCanonical, rightOK := claudeThinkingReplayCanonicalJSON(right)
	return leftOK && rightOK && bytes.Equal(leftCanonical, rightCanonical)
}

func claudeThinkingReplayCanonicalJSON(raw []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, false
	}
	canonical, errMarshal := json.Marshal(value)
	return canonical, errMarshal == nil
}

func enforceClaudeThinkingReplayLimitsLocked() {
	for len(claudeThinkingReplayEntries) > claudeThinkingReplayCacheMaxEntries ||
		claudeThinkingReplayTotalBytes > claudeThinkingReplayCacheMaxTotalBytes {
		if len(claudeThinkingReplayEntries) == 0 {
			claudeThinkingReplayTotalBytes = 0
			return
		}
		evictOldestClaudeThinkingReplayEntriesLocked(claudeThinkingReplayCacheEvictBatchSize)
	}
}

func evictOldestClaudeThinkingReplayEntriesLocked(count int) {
	if count <= 0 || len(claudeThinkingReplayEntries) == 0 {
		return
	}
	type candidate struct {
		key       string
		timestamp time.Time
	}
	candidates := make([]candidate, 0, len(claudeThinkingReplayEntries))
	for key, entry := range claudeThinkingReplayEntries {
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
		entry := claudeThinkingReplayEntries[candidates[i].key]
		claudeThinkingReplayTotalBytes -= claudeThinkingReplayEntryBytes(entry.Contents)
		delete(claudeThinkingReplayEntries, candidates[i].key)
	}
}

func purgeExpiredClaudeThinkingReplayCache(now time.Time) {
	claudeThinkingReplayMu.Lock()
	for key, entry := range claudeThinkingReplayEntries {
		if now.Sub(entry.Timestamp) > claudeThinkingReplayCacheTTL {
			claudeThinkingReplayTotalBytes -= claudeThinkingReplayEntryBytes(entry.Contents)
			delete(claudeThinkingReplayEntries, key)
		}
	}
	claudeThinkingReplayMu.Unlock()
}
