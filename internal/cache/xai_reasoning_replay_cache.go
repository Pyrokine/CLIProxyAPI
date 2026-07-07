package cache

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	XAIReasoningReplayCacheTTL            = 1 * time.Hour
	XAIReasoningReplayCacheMaxEntries     = 10240
	XAIReasoningReplayCacheEvictBatchSize = 128
	XAIReasoningReplayCacheMaxItemBytes   = 256 * 1024
	XAIReasoningReplayCacheMaxEntryBytes  = 1024 * 1024
)

type xaiReasoningReplayEntry struct {
	Items     [][]byte
	Timestamp time.Time
}

var (
	xaiReasoningReplayMu      sync.Mutex
	xaiReasoningReplayEntries = make(map[string]xaiReasoningReplayEntry)
)

func CacheXAIReasoningReplayItem(modelName, sessionKey string, item []byte) bool {
	return CacheXAIReasoningReplayItems(modelName, sessionKey, [][]byte{item})
}

func CacheXAIReasoningReplayItems(modelName, sessionKey string, items [][]byte) bool {
	key := xaiReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return false
	}
	normalized, ok := normalizeXAIReasoningReplayItems(items)
	if !ok {
		return false
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	xaiReasoningReplayMu.Lock()
	defer xaiReasoningReplayMu.Unlock()
	xaiReasoningReplayEntries[key] = xaiReasoningReplayEntry{Items: normalized, Timestamp: now}
	if len(xaiReasoningReplayEntries) > XAIReasoningReplayCacheMaxEntries {
		evictOldestXAIReasoningReplayEntries(XAIReasoningReplayCacheEvictBatchSize)
	}
	return true
}

func GetXAIReasoningReplayItem(modelName, sessionKey string) ([]byte, bool) {
	items, ok := GetXAIReasoningReplayItems(modelName, sessionKey)
	if !ok || len(items) == 0 {
		return nil, false
	}
	return items[0], true
}

func GetXAIReasoningReplayItems(modelName, sessionKey string) ([][]byte, bool) {
	key := xaiReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return nil, false
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	xaiReasoningReplayMu.Lock()
	defer xaiReasoningReplayMu.Unlock()
	entry, ok := xaiReasoningReplayEntries[key]
	if !ok {
		return nil, false
	}
	if now.Sub(entry.Timestamp) > XAIReasoningReplayCacheTTL {
		delete(xaiReasoningReplayEntries, key)
		return nil, false
	}
	entry.Timestamp = now
	xaiReasoningReplayEntries[key] = entry
	return cloneXAIReasoningReplayItems(entry.Items), true
}

func DeleteXAIReasoningReplayItem(modelName, sessionKey string) {
	key := xaiReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return
	}
	xaiReasoningReplayMu.Lock()
	delete(xaiReasoningReplayEntries, key)
	xaiReasoningReplayMu.Unlock()
}

func ClearXAIReasoningReplayCache() {
	xaiReasoningReplayMu.Lock()
	xaiReasoningReplayEntries = make(map[string]xaiReasoningReplayEntry)
	xaiReasoningReplayMu.Unlock()
}

func xaiReasoningReplayCacheKey(modelName, sessionKey string) string {
	modelName = strings.TrimSpace(modelName)
	sessionKey = strings.TrimSpace(sessionKey)
	if modelName == "" || sessionKey == "" {
		return ""
	}
	return strings.Join([]string{"xai-reasoning-replay", modelName, sessionKey}, "\x00")
}

func normalizeXAIReasoningReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	totalBytes := 0
	for _, item := range items {
		if len(item) > XAIReasoningReplayCacheMaxItemBytes {
			return nil, false
		}
		normalizedItem, ok := normalizeXAIReasoningReplayItem(item)
		if ok {
			totalBytes += len(normalizedItem)
			if totalBytes > XAIReasoningReplayCacheMaxEntryBytes {
				return nil, false
			}
			normalized = append(normalized, normalizedItem)
		}
	}
	return normalized, len(normalized) > 0
}

func normalizeXAIReasoningReplayItem(item []byte) ([]byte, bool) {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case "reasoning":
		return normalizeXAIReasoningReplayReasoningItem(itemResult)
	case "function_call":
		return normalizeXAIReasoningReplayFunctionCallItem(itemResult)
	case "custom_tool_call":
		return normalizeXAIReasoningReplayCustomToolCallItem(itemResult)
	default:
		return nil, false
	}
}

func normalizeXAIReasoningReplayReasoningItem(itemResult gjson.Result) ([]byte, bool) {
	encryptedContentResult := itemResult.Get("encrypted_content")
	if encryptedContentResult.Type != gjson.String {
		return nil, false
	}
	encryptedContent := encryptedContentResult.String()
	if encryptedContent != strings.TrimSpace(encryptedContent) {
		return nil, false
	}
	if _, err := signature.InspectGrokEncryptedContent(encryptedContent); err != nil {
		return nil, false
	}

	normalized := []byte(`{"type":"reasoning","summary":[],"content":null}`)
	normalized, _ = sjson.SetBytes(normalized, "encrypted_content", encryptedContent)
	return normalized, true
}

func normalizeXAIReasoningReplayFunctionCallItem(itemResult gjson.Result) ([]byte, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	arguments := itemResult.Get("arguments")
	if callID == "" || name == "" || arguments.Type != gjson.String {
		return nil, false
	}

	normalized := []byte(`{"type":"function_call"}`)
	normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
	normalized, _ = sjson.SetBytes(normalized, "name", name)
	normalized, _ = sjson.SetBytes(normalized, "arguments", arguments.String())
	return normalized, true
}

func normalizeXAIReasoningReplayCustomToolCallItem(itemResult gjson.Result) ([]byte, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	input := itemResult.Get("input")
	if callID == "" || name == "" || !input.Exists() {
		return nil, false
	}

	normalized := []byte(`{"type":"custom_tool_call","status":"completed"}`)
	if status := strings.TrimSpace(itemResult.Get("status").String()); status != "" {
		normalized, _ = sjson.SetBytes(normalized, "status", status)
	}
	normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
	normalized, _ = sjson.SetBytes(normalized, "name", name)
	if input.Type == gjson.String {
		normalized, _ = sjson.SetBytes(normalized, "input", input.String())
	} else {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte(input.Raw))
	}
	return normalized, true
}

func cloneXAIReasoningReplayItems(items [][]byte) [][]byte {
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, append([]byte(nil), item...))
	}
	return cloned
}

func evictOldestXAIReasoningReplayEntries(count int) {
	if count <= 0 || len(xaiReasoningReplayEntries) == 0 {
		return
	}
	type candidate struct {
		key       string
		timestamp time.Time
	}
	candidates := make([]candidate, 0, len(xaiReasoningReplayEntries))
	for key, entry := range xaiReasoningReplayEntries {
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
		delete(xaiReasoningReplayEntries, candidates[i].key)
	}
}

func purgeExpiredXAIReasoningReplayCache(now time.Time) {
	xaiReasoningReplayMu.Lock()
	for key, entry := range xaiReasoningReplayEntries {
		if now.Sub(entry.Timestamp) > XAIReasoningReplayCacheTTL {
			delete(xaiReasoningReplayEntries, key)
		}
	}
	xaiReasoningReplayMu.Unlock()
}
