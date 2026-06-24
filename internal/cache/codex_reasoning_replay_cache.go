package cache

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	CodexReasoningReplayCacheTTL            = 1 * time.Hour
	CodexReasoningReplayCacheMaxEntries     = 10240
	CodexReasoningReplayCacheEvictBatchSize = 128
	maxGPTReasoningSignatureLen             = 32 * 1024 * 1024
)

type codexReasoningReplayEntry struct {
	Items     [][]byte
	Timestamp time.Time
}

var (
	codexReasoningReplayMu      sync.Mutex
	codexReasoningReplayEntries = make(map[string]codexReasoningReplayEntry)
)

func CacheCodexReasoningReplayItem(modelName, sessionKey string, item []byte) bool {
	return CacheCodexReasoningReplayItems(modelName, sessionKey, [][]byte{item})
}

func CacheCodexReasoningReplayItems(modelName, sessionKey string, items [][]byte) bool {
	key := codexReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return false
	}
	normalized, ok := normalizeCodexReasoningReplayItems(items)
	if !ok {
		return false
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	codexReasoningReplayMu.Lock()
	defer codexReasoningReplayMu.Unlock()
	codexReasoningReplayEntries[key] = codexReasoningReplayEntry{Items: normalized, Timestamp: now}
	if len(codexReasoningReplayEntries) > CodexReasoningReplayCacheMaxEntries {
		evictOldestCodexReasoningReplayEntries(CodexReasoningReplayCacheEvictBatchSize)
	}
	return true
}

func GetCodexReasoningReplayItem(modelName, sessionKey string) ([]byte, bool) {
	items, ok := GetCodexReasoningReplayItems(modelName, sessionKey)
	if !ok || len(items) == 0 {
		return nil, false
	}
	return items[0], true
}

func GetCodexReasoningReplayItems(modelName, sessionKey string) ([][]byte, bool) {
	key := codexReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return nil, false
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	codexReasoningReplayMu.Lock()
	defer codexReasoningReplayMu.Unlock()
	entry, ok := codexReasoningReplayEntries[key]
	if !ok {
		return nil, false
	}
	if now.Sub(entry.Timestamp) > CodexReasoningReplayCacheTTL {
		delete(codexReasoningReplayEntries, key)
		return nil, false
	}
	entry.Timestamp = now
	codexReasoningReplayEntries[key] = entry
	return cloneCodexReasoningReplayItems(entry.Items), true
}

func DeleteCodexReasoningReplayItem(modelName, sessionKey string) {
	key := codexReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return
	}
	codexReasoningReplayMu.Lock()
	delete(codexReasoningReplayEntries, key)
	codexReasoningReplayMu.Unlock()
}

func ClearCodexReasoningReplayCache() {
	codexReasoningReplayMu.Lock()
	codexReasoningReplayEntries = make(map[string]codexReasoningReplayEntry)
	codexReasoningReplayMu.Unlock()
}

func codexReasoningReplayCacheKey(modelName, sessionKey string) string {
	modelName = strings.TrimSpace(modelName)
	sessionKey = strings.TrimSpace(sessionKey)
	if modelName == "" || sessionKey == "" {
		return ""
	}
	return strings.Join([]string{"codex-reasoning-replay", modelName, sessionKey}, "\x00")
}

func normalizeCodexReasoningReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	for _, item := range items {
		normalizedItem, ok := normalizeCodexReasoningReplayItem(item)
		if ok {
			normalized = append(normalized, normalizedItem)
		}
	}
	return normalized, len(normalized) > 0
}

func normalizeCodexReasoningReplayItem(item []byte) ([]byte, bool) {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case "reasoning":
		return normalizeCodexReasoningReplayReasoningItem(itemResult)
	case "function_call":
		return normalizeCodexReasoningReplayFunctionCallItem(itemResult)
	case "custom_tool_call":
		return normalizeCodexReasoningReplayCustomToolCallItem(itemResult)
	default:
		return nil, false
	}
}

func normalizeCodexReasoningReplayReasoningItem(itemResult gjson.Result) ([]byte, bool) {
	encryptedContentResult := itemResult.Get("encrypted_content")
	if encryptedContentResult.Type != gjson.String {
		return nil, false
	}
	encryptedContent := encryptedContentResult.String()
	if encryptedContent != strings.TrimSpace(encryptedContent) {
		return nil, false
	}
	if err := inspectGPTReasoningSignature(encryptedContent); err != nil {
		return nil, false
	}

	normalized := []byte(`{"type":"reasoning","summary":[],"content":null}`)
	normalized, _ = sjson.SetBytes(normalized, "encrypted_content", encryptedContent)
	return normalized, true
}

func normalizeCodexReasoningReplayFunctionCallItem(itemResult gjson.Result) ([]byte, bool) {
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

func normalizeCodexReasoningReplayCustomToolCallItem(itemResult gjson.Result) ([]byte, bool) {
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

func IsValidGPTReasoningEncryptedContent(rawSignature string) bool {
	return inspectGPTReasoningSignature(rawSignature) == nil
}

func inspectGPTReasoningSignature(rawSignature string) error {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return fmt.Errorf("empty GPT reasoning signature")
	}
	if len(sig) > maxGPTReasoningSignatureLen {
		return fmt.Errorf("GPT reasoning signature exceeds maximum length (%d bytes)", maxGPTReasoningSignatureLen)
	}
	if index, r, ok := firstInvalidGPTReasoningSignatureChar(sig); ok {
		return fmt.Errorf(
			"invalid GPT reasoning signature: contains non-base64url character U+%04X at byte %d", r, index,
		)
	}
	if !strings.HasPrefix(sig, "gAAAA") {
		return fmt.Errorf("invalid GPT reasoning signature: expected gAAAA prefix")
	}

	decoded, err := decodeGPTReasoningSignature(sig)
	if err != nil {
		return err
	}
	if len(decoded) < 73 {
		return fmt.Errorf("invalid GPT reasoning signature: decoded payload too short")
	}
	if decoded[0] != 0x80 {
		return fmt.Errorf("invalid GPT reasoning signature: expected version 0x80, got 0x%02x", decoded[0])
	}

	ciphertextLen := len(decoded) - 1 - 8 - 16 - 32
	if ciphertextLen <= 0 || ciphertextLen%16 != 0 {
		return fmt.Errorf(
			"invalid GPT reasoning signature: ciphertext length %d is not a positive AES block multiple", ciphertextLen,
		)
	}
	return nil
}

func decodeGPTReasoningSignature(sig string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(sig); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(sig); err == nil {
		return decoded, nil
	}
	return nil, fmt.Errorf("invalid GPT reasoning signature: base64url decode failed")
}

func firstInvalidGPTReasoningSignatureChar(sig string) (int, rune, bool) {
	for index, r := range sig {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '=':
		default:
			return index, r, true
		}
	}
	return 0, 0, false
}

func cloneCodexReasoningReplayItems(items [][]byte) [][]byte {
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, append([]byte(nil), item...))
	}
	return cloned
}

func evictOldestCodexReasoningReplayEntries(count int) {
	if count <= 0 || len(codexReasoningReplayEntries) == 0 {
		return
	}
	type candidate struct {
		key       string
		timestamp time.Time
	}
	candidates := make([]candidate, 0, len(codexReasoningReplayEntries))
	for key, entry := range codexReasoningReplayEntries {
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
		delete(codexReasoningReplayEntries, candidates[i].key)
	}
}

func purgeExpiredCodexReasoningReplayCache(now time.Time) {
	codexReasoningReplayMu.Lock()
	for key, entry := range codexReasoningReplayEntries {
		if now.Sub(entry.Timestamp) > CodexReasoningReplayCacheTTL {
			delete(codexReasoningReplayEntries, key)
		}
	}
	codexReasoningReplayMu.Unlock()
}
