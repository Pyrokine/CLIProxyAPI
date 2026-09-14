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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	xaiReasoningReplayCacheTTL            = 1 * time.Hour
	xaiReasoningReplayCacheMaxEntries     = 10240
	xaiReasoningReplayCacheEvictBatchSize = 128
	xaiReasoningReplayCacheMaxItemBytes   = 256 * 1024
	xaiReasoningReplayCacheMaxEntryBytes  = 1024 * 1024
)

type xaiReasoningReplayEntry struct {
	Items     [][]byte
	Timestamp time.Time
}

var (
	xaiReasoningReplayMu      sync.Mutex
	xaiReasoningReplayEntries = make(map[string]xaiReasoningReplayEntry)
)

type xaiReasoningReplayKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, opts home.KVSetOptions) (bool, error)
	KVDel(ctx context.Context, keys ...string) (int64, error)
	KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

var currentXAIReasoningReplayKVClient = func() (xaiReasoningReplayKVClient, bool, error) {
	client := home.Current()
	if client == nil {
		return nil, false, nil
	}
	if !client.Enabled() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", home.ErrDisabled)
	}
	if !client.HeartbeatOK() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", home.ErrNotConnected)
	}
	return client, true, nil
}

func xaiReasoningReplayRuntime(
	ctx context.Context,
) (client xaiReasoningReplayKVClient, homeMode bool, scopeID string, err error) {
	if scope, ok := home.ClientScopeFromContext(ctx); ok {
		scopeID = strings.TrimSpace(scope.ID)
		if !scope.Enabled {
			return nil, false, scopeID, nil
		}
		if scope.Client == nil {
			return nil, true, scopeID, fmt.Errorf("home kv store unavailable: %w", home.ErrNotConnected)
		}
		if !scope.Client.Enabled() {
			return nil, true, scopeID, fmt.Errorf("home kv store unavailable: %w", home.ErrDisabled)
		}
		if !scope.Client.HeartbeatOK() {
			return nil, true, scopeID, fmt.Errorf("home kv store unavailable: %w", home.ErrNotConnected)
		}
		return scope.Client, true, scopeID, nil
	}

	client, homeMode, err = currentXAIReasoningReplayKVClient()
	if scoped, ok := client.(interface{ RuntimeScopeID() string }); ok {
		scopeID = strings.TrimSpace(scoped.RuntimeScopeID())
	}
	return client, homeMode, scopeID, err
}

func XAIReasoningReplayItems(modelName, sessionKey string, items [][]byte) bool {
	return cacheXAIReasoningReplayItemsBestEffort(context.Background(), modelName, sessionKey, items)
}

func cacheXAIReasoningReplayItemsBestEffort(
	ctx context.Context,
	modelName string,
	sessionKey string,
	items [][]byte,
) bool {
	return StoreXAIReasoningReplayItems(ctx, modelName, sessionKey, items) == XAIReasoningReplayStored
}

type XAIReasoningReplayStoreStatus int

const (
	xaiReasoningReplayStoreInvalidArgs XAIReasoningReplayStoreStatus = iota
	XAIReasoningReplayStored
	XAIReasoningReplayNoReplayableState
	XAIReasoningReplayStoreBackendError
)

func StoreXAIReasoningReplayItems(
	ctx context.Context,
	modelName string,
	sessionKey string,
	items [][]byte,
) XAIReasoningReplayStoreStatus {
	key := xaiReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return xaiReasoningReplayStoreInvalidArgs
	}
	normalized, ok := normalizeXAIReasoningReplayItems(items)
	if !ok {
		return XAIReasoningReplayNoReplayableState
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := xaiReasoningReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			log.Errorf("home kv best-effort xai reasoning replay set failed prefix=cpa:xai:*: %v", errClient)
			return XAIReasoningReplayStoreBackendError
		}
		raw, errMarshal := json.Marshal(normalized)
		if errMarshal != nil {
			log.Errorf("home kv best-effort xai reasoning replay set failed prefix=cpa:xai:*: %v", errMarshal)
			return XAIReasoningReplayStoreBackendError
		}
		written, errSet := client.KVSet(
			ctx,
			xaiReasoningReplayKVKey(modelName, sessionKey),
			raw,
			home.KVSetOptions{EX: xaiReasoningReplayCacheTTL},
		)
		if errSet != nil {
			log.Errorf("home kv best-effort xai reasoning replay set failed prefix=cpa:xai:*: %v", errSet)
			return XAIReasoningReplayStoreBackendError
		}
		if !written {
			log.Error("home kv best-effort xai reasoning replay set failed prefix=cpa:xai:*: value was not written")
			return XAIReasoningReplayStoreBackendError
		}
		return XAIReasoningReplayStored
	}

	key = xaiReasoningReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	xaiReasoningReplayMu.Lock()
	defer xaiReasoningReplayMu.Unlock()
	xaiReasoningReplayEntries[key] = xaiReasoningReplayEntry{Items: normalized, Timestamp: now}
	if len(xaiReasoningReplayEntries) > xaiReasoningReplayCacheMaxEntries {
		evictOldestXAIReasoningReplayEntries(xaiReasoningReplayCacheEvictBatchSize)
	}
	return XAIReasoningReplayStored
}

func GetXAIReasoningReplayItems(modelName, sessionKey string) ([][]byte, bool) {
	items, ok, err := GetXAIReasoningReplayItemsRequired(context.Background(), modelName, sessionKey)
	if err != nil {
		log.Warnf("xai reasoning replay cache read failed: %v", err)
		return nil, false
	}
	return items, ok
}

func GetXAIReasoningReplayItemsRequired(
	ctx context.Context,
	modelName string,
	sessionKey string,
) ([][]byte, bool, error) {
	key := xaiReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := xaiReasoningReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return nil, false, errClient
		}
		remoteKey := xaiReasoningReplayKVKey(modelName, sessionKey)
		raw, found, errGet := client.KVGet(ctx, remoteKey)
		if errGet != nil || !found {
			return nil, false, errGet
		}
		if len(raw) > xaiReasoningReplayCacheMaxEntryBytes*2 {
			return nil, false, fmt.Errorf("home kv xai reasoning replay value exceeds serialized size limit")
		}
		var items [][]byte
		if errUnmarshal := json.Unmarshal(raw, &items); errUnmarshal != nil {
			return nil, false, errUnmarshal
		}
		normalized, ok := normalizeXAIReasoningReplayItems(items)
		if !ok {
			return nil, false, fmt.Errorf("home kv xai reasoning replay value is invalid")
		}
		if _, errExpire := client.KVExpire(ctx, remoteKey, xaiReasoningReplayCacheTTL); errExpire != nil {
			log.Warnf("home kv xai reasoning replay expire failed prefix=cpa:xai:*: %v", errExpire)
		}
		return normalized, true, nil
	}

	key = xaiReasoningReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	xaiReasoningReplayMu.Lock()
	defer xaiReasoningReplayMu.Unlock()
	entry, ok := xaiReasoningReplayEntries[key]
	if !ok {
		return nil, false, nil
	}
	if now.Sub(entry.Timestamp) > xaiReasoningReplayCacheTTL {
		delete(xaiReasoningReplayEntries, key)
		return nil, false, nil
	}
	entry.Timestamp = now
	xaiReasoningReplayEntries[key] = entry
	return cloneXAIReasoningReplayItems(entry.Items), true, nil
}

func DeleteXAIReasoningReplayItemRequired(ctx context.Context, modelName, sessionKey string) error {
	key := xaiReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := xaiReasoningReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return errClient
		}
		_, errDel := client.KVDel(ctx, xaiReasoningReplayKVKey(modelName, sessionKey))
		return errDel
	}
	key = xaiReasoningReplayLocalCacheKey(scopeID, key)
	xaiReasoningReplayMu.Lock()
	delete(xaiReasoningReplayEntries, key)
	xaiReasoningReplayMu.Unlock()
	return nil
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

func xaiReasoningReplayLocalCacheKey(scopeID, key string) string {
	scopeID = strings.TrimSpace(scopeID)
	if scopeID == "" {
		return key
	}
	return strings.Join([]string{scopeID, key}, "\x00")
}

func xaiReasoningReplayKVKey(modelName, sessionKey string) string {
	return "cpa:xai:reasoning-replay:" + xaiReasoningReplayHashKeyPart(strings.TrimSpace(modelName)) + ":" +
		xaiReasoningReplayHashKeyPart(strings.TrimSpace(sessionKey))
}

func xaiReasoningReplayHashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func normalizeXAIReasoningReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	totalBytes := 0
	hasReplayAnchor := false
	for _, item := range items {
		if len(item) > xaiReasoningReplayCacheMaxItemBytes {
			return nil, false
		}
		normalizedItem, ok := normalizeXAIReasoningReplayItem(item)
		if !ok {
			continue
		}
		totalBytes += len(normalizedItem)
		if totalBytes > xaiReasoningReplayCacheMaxEntryBytes {
			return nil, false
		}
		normalized = append(normalized, normalizedItem)
		switch strings.TrimSpace(gjson.GetBytes(normalizedItem, "type").String()) {
		case "reasoning", "function_call", "custom_tool_call":
			hasReplayAnchor = true
		}
	}
	return normalized, hasReplayAnchor
}

func normalizeXAIReasoningReplayItem(item []byte) ([]byte, bool) {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case "reasoning":
		return normalizeXAIReasoningReplayReasoningItem(itemResult)
	case "message":
		return normalizeXAIReasoningReplayMessageItem(itemResult)
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

func normalizeXAIReasoningReplayMessageItem(itemResult gjson.Result) ([]byte, bool) {
	if !strings.EqualFold(strings.TrimSpace(itemResult.Get("role").String()), "assistant") {
		return nil, false
	}
	content := itemResult.Get("content")
	if !content.IsArray() || len(content.Array()) == 0 {
		return nil, false
	}

	normalized := []byte(`{"type":"message","role":"assistant","content":[]}`)
	for _, part := range content.Array() {
		partType := strings.TrimSpace(part.Get("type").String())
		var nextPart []byte
		switch partType {
		case "output_text":
			text := part.Get("text")
			if text.Type != gjson.String {
				continue
			}
			nextPart = []byte(`{"type":"output_text","text":""}`)
			nextPart, _ = sjson.SetBytes(nextPart, "text", text.String())
		case "refusal":
			refusal := part.Get("refusal")
			if refusal.Type != gjson.String {
				continue
			}
			nextPart = []byte(`{"type":"refusal","refusal":""}`)
			nextPart, _ = sjson.SetBytes(nextPart, "refusal", refusal.String())
		default:
			continue
		}
		updated, err := sjson.SetRawBytes(normalized, "content.-1", nextPart)
		if err != nil {
			return nil, false
		}
		normalized = updated
	}
	if len(gjson.GetBytes(normalized, "content").Array()) == 0 {
		return nil, false
	}
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
	if namespace := strings.TrimSpace(itemResult.Get("namespace").String()); namespace != "" {
		normalized, _ = sjson.SetBytes(normalized, "namespace", namespace)
	}
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
		if now.Sub(entry.Timestamp) > xaiReasoningReplayCacheTTL {
			delete(xaiReasoningReplayEntries, key)
		}
	}
	xaiReasoningReplayMu.Unlock()
}
