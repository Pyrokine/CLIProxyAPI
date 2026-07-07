package cache

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	AntigravityReasoningReplayCacheTTL            = 1 * time.Hour
	AntigravityReasoningReplayCacheMaxEntries     = 10240
	AntigravityReasoningReplayCacheEvictBatchSize = 128
	AntigravityReasoningReplayCacheMaxItemBytes   = 256 * 1024
	AntigravityReasoningReplayCacheMaxEntryBytes  = 1024 * 1024
	minAntigravityThoughtSignatureReplayLen       = 16
)

type antigravityReasoningReplayEntry struct {
	Items     [][]byte
	Timestamp time.Time
}

var (
	antigravityReasoningReplayMu      sync.Mutex
	antigravityReasoningReplayEntries = make(map[string]antigravityReasoningReplayEntry)
)

func CacheAntigravityReasoningReplayItem(modelName, sessionKey string, item []byte) bool {
	return CacheAntigravityReasoningReplayItems(modelName, sessionKey, [][]byte{item})
}

func CacheAntigravityReasoningReplayItems(modelName, sessionKey string, items [][]byte) bool {
	key := antigravityReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return false
	}
	normalized, ok := normalizeAntigravityReasoningReplayItems(items)
	if !ok {
		return false
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	antigravityReasoningReplayMu.Lock()
	defer antigravityReasoningReplayMu.Unlock()
	antigravityReasoningReplayEntries[key] = antigravityReasoningReplayEntry{Items: normalized, Timestamp: now}
	if len(antigravityReasoningReplayEntries) > AntigravityReasoningReplayCacheMaxEntries {
		evictOldestAntigravityReasoningReplayEntries(AntigravityReasoningReplayCacheEvictBatchSize)
	}
	return true
}

func GetAntigravityReasoningReplayItem(modelName, sessionKey string) ([]byte, bool) {
	items, ok := GetAntigravityReasoningReplayItems(modelName, sessionKey)
	if !ok || len(items) == 0 {
		return nil, false
	}
	return items[0], true
}

func GetAntigravityReasoningReplayItems(modelName, sessionKey string) ([][]byte, bool) {
	key := antigravityReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return nil, false
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	antigravityReasoningReplayMu.Lock()
	defer antigravityReasoningReplayMu.Unlock()
	entry, ok := antigravityReasoningReplayEntries[key]
	if !ok {
		return nil, false
	}
	if now.Sub(entry.Timestamp) > AntigravityReasoningReplayCacheTTL {
		delete(antigravityReasoningReplayEntries, key)
		return nil, false
	}
	entry.Timestamp = now
	antigravityReasoningReplayEntries[key] = entry
	return cloneAntigravityReasoningReplayItems(entry.Items), true
}

func DeleteAntigravityReasoningReplayItem(modelName, sessionKey string) {
	key := antigravityReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return
	}
	antigravityReasoningReplayMu.Lock()
	delete(antigravityReasoningReplayEntries, key)
	antigravityReasoningReplayMu.Unlock()
}

func ClearAntigravityReasoningReplayCache() {
	antigravityReasoningReplayMu.Lock()
	antigravityReasoningReplayEntries = make(map[string]antigravityReasoningReplayEntry)
	antigravityReasoningReplayMu.Unlock()
}

func antigravityReasoningReplayCacheKey(modelName, sessionKey string) string {
	modelName = strings.TrimSpace(modelName)
	sessionKey = strings.TrimSpace(sessionKey)
	if modelName == "" || sessionKey == "" {
		return ""
	}
	return strings.Join([]string{"antigravity-reasoning-replay", modelName, sessionKey}, "\x00")
}

func normalizeAntigravityReasoningReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	totalBytes := 0
	for _, item := range items {
		if len(item) > AntigravityReasoningReplayCacheMaxItemBytes {
			return nil, false
		}
		normalizedItem, ok := normalizeAntigravityReasoningReplayItem(item)
		if ok {
			totalBytes += len(normalizedItem)
			if totalBytes > AntigravityReasoningReplayCacheMaxEntryBytes {
				return nil, false
			}
			normalized = append(normalized, normalizedItem)
		}
	}
	return normalized, len(normalized) > 0
}

func normalizeAntigravityReasoningReplayItem(item []byte) ([]byte, bool) {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case "thought_signature":
		return normalizeAntigravityThoughtSignatureReplayItem(itemResult)
	case "function_call_part":
		return normalizeAntigravityFunctionCallPartReplayItem(itemResult)
	default:
		return nil, false
	}
}

func normalizeAntigravityThoughtSignatureReplayItem(itemResult gjson.Result) ([]byte, bool) {
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if sig == "" {
		sig = strings.TrimSpace(itemResult.Get("thought_signature").String())
	}
	if sig == "" || len(sig) < minAntigravityThoughtSignatureReplayLen {
		return nil, false
	}
	normalized := []byte(`{"type":"thought_signature"}`)
	normalized, _ = sjson.SetBytes(normalized, "thoughtSignature", sig)
	if contentIndex := itemResult.Get("contentIndex"); contentIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "contentIndex", contentIndex.Int())
	}
	if partIndex := itemResult.Get("partIndex"); partIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "partIndex", partIndex.Int())
	}
	return normalized, true
}

func normalizeAntigravityFunctionCallPartReplayItem(itemResult gjson.Result) ([]byte, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	if name == "" || !args.Exists() {
		fc := itemResult.Get("functionCall")
		if fc.Exists() {
			if callID == "" {
				callID = strings.TrimSpace(fc.Get("id").String())
			}
			if name == "" {
				name = strings.TrimSpace(fc.Get("name").String())
			}
			if !args.Exists() {
				args = fc.Get("args")
			}
		}
	}
	if name == "" || !args.Exists() {
		return nil, false
	}
	normalized := []byte(`{"type":"function_call_part"}`)
	if callID != "" {
		normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
	}
	normalized, _ = sjson.SetBytes(normalized, "name", name)
	if args.Type == gjson.String {
		normalized, _ = sjson.SetBytes(normalized, "args", args.String())
	} else {
		normalized, _ = sjson.SetRawBytes(normalized, "args", []byte(args.Raw))
	}
	if sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String()); sig != "" {
		normalized, _ = sjson.SetBytes(normalized, "thoughtSignature", sig)
	}
	if contentIndex := itemResult.Get("contentIndex"); contentIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "contentIndex", contentIndex.Int())
	}
	if partIndex := itemResult.Get("partIndex"); partIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "partIndex", partIndex.Int())
	}
	return normalized, true
}

func cloneAntigravityReasoningReplayItems(items [][]byte) [][]byte {
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, append([]byte(nil), item...))
	}
	return cloned
}

func evictOldestAntigravityReasoningReplayEntries(count int) {
	if count <= 0 || len(antigravityReasoningReplayEntries) == 0 {
		return
	}
	type candidate struct {
		key       string
		timestamp time.Time
	}
	candidates := make([]candidate, 0, len(antigravityReasoningReplayEntries))
	for key, entry := range antigravityReasoningReplayEntries {
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
		delete(antigravityReasoningReplayEntries, candidates[i].key)
	}
}

func purgeExpiredAntigravityReasoningReplayCache(now time.Time) {
	antigravityReasoningReplayMu.Lock()
	for key, entry := range antigravityReasoningReplayEntries {
		if now.Sub(entry.Timestamp) > AntigravityReasoningReplayCacheTTL {
			delete(antigravityReasoningReplayEntries, key)
		}
	}
	antigravityReasoningReplayMu.Unlock()
}
