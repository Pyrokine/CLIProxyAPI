package cache

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// CodexReasoningReplayTurnType identifies an internal turn-boundary marker.
	CodexReasoningReplayTurnType = "cpa_codex_replay_turn"

	codexReasoningReplayCacheTTL              = 1 * time.Hour
	codexReasoningReplayCacheMaxEntries       = 10240
	codexReasoningReplayCacheEvictBatchSize   = 128
	codexReasoningReplayCacheMaxTurnsPerEntry = 256
	codexReasoningReplayCacheMaxBytesPerEntry = 16 << 20

	codexReasoningReplayCacheMaxSerializedBytes = 24 << 20
	maxGPTReasoningSignatureLen                 = 32 * 1024 * 1024
)

type codexReasoningReplayEntry struct {
	Items     [][]byte
	Timestamp time.Time
}

var (
	codexReasoningReplayMu      sync.Mutex
	codexReasoningReplayEntries = make(map[string]codexReasoningReplayEntry)
)

type codexReasoningReplayKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, opts home.KVSetOptions) (bool, error)
	KVCompareAndSwap(
		ctx context.Context,
		key string,
		expected []byte,
		expectedExists bool,
		value []byte,
		ttl time.Duration,
	) (bool, error)
	KVDel(ctx context.Context, keys ...string) (int64, error)
	KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

var currentCodexReasoningReplayKVClient = func() (codexReasoningReplayKVClient, bool, error) {
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

func codexReasoningReplayRuntime(
	ctx context.Context,
) (client codexReasoningReplayKVClient, homeMode bool, scopeID string, err error) {
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
		scopedClient, ok := scope.Client.(codexReasoningReplayKVClient)
		if !ok {
			return nil, true, scopeID, home.ErrCompareAndSwapUnsupported
		}
		return scopedClient, true, scopeID, nil
	}

	client, homeMode, err = currentCodexReasoningReplayKVClient()
	if scoped, ok := client.(interface{ RuntimeScopeID() string }); ok {
		scopeID = strings.TrimSpace(scoped.RuntimeScopeID())
	}
	return client, homeMode, scopeID, err
}

func cacheCodexReasoningReplayItemsBestEffort(
	ctx context.Context,
	modelName string,
	sessionKey string,
	items [][]byte,
) bool {
	key := codexReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return false
	}
	normalized, ok := normalizeCodexReasoningReplayItems(items)
	if !ok {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := codexReasoningReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			log.Errorf("home kv best-effort codex reasoning replay set failed prefix=cpa:codex:*: %v", errClient)
			return false
		}
		raw, errMarshal := json.Marshal(normalized)
		if errMarshal != nil {
			log.Errorf("home kv best-effort codex reasoning replay set failed prefix=cpa:codex:*: %v", errMarshal)
			return false
		}
		written, errSet := client.KVSet(
			ctx,
			codexReasoningReplayKVKey(modelName, sessionKey),
			raw,
			home.KVSetOptions{EX: codexReasoningReplayCacheTTL},
		)
		if errSet != nil {
			log.Errorf("home kv best-effort codex reasoning replay set failed prefix=cpa:codex:*: %v", errSet)
			return false
		}
		return written
	}

	key = codexReasoningReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	codexReasoningReplayMu.Lock()
	defer codexReasoningReplayMu.Unlock()
	codexReasoningReplayEntries[key] = codexReasoningReplayEntry{Items: normalized, Timestamp: now}
	if len(codexReasoningReplayEntries) > codexReasoningReplayCacheMaxEntries {
		evictOldestCodexReasoningReplayEntries(codexReasoningReplayCacheEvictBatchSize)
	}
	return true
}

// AppendCodexReasoningReplayItemsBestEffort appends one completed turn to existing replay state.
func AppendCodexReasoningReplayItemsBestEffort(
	ctx context.Context,
	modelName string,
	sessionKey string,
	items [][]byte,
) bool {
	key := codexReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return false
	}
	normalized, ok := normalizeCodexReasoningReplayItems(items)
	if !ok {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := codexReasoningReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			log.Errorf("home kv best-effort codex reasoning replay append failed prefix=cpa:codex:*: %v", errClient)
			return false
		}
		kvKey := codexReasoningReplayKVKey(modelName, sessionKey)
		const maxCASAttempts = 32
		for range maxCASAttempts {
			if errContext := ctx.Err(); errContext != nil {
				return false
			}
			existingRaw, found, errGet := client.KVGet(ctx, kvKey)
			if errGet != nil {
				log.Errorf("home kv best-effort codex reasoning replay append failed prefix=cpa:codex:*: %v", errGet)
				return false
			}
			if len(existingRaw) > codexReasoningReplayCacheMaxSerializedBytes {
				log.Error("home kv best-effort codex reasoning replay append failed prefix=cpa:codex:*: value exceeds serialized size limit")
				return false
			}
			var existing [][]byte
			if found {
				if errUnmarshal := json.Unmarshal(existingRaw, &existing); errUnmarshal != nil {
					log.Errorf(
						"home kv best-effort codex reasoning replay append failed prefix=cpa:codex:*: %v", errUnmarshal,
					)
					return false
				}
				normalizedExisting, okExisting := normalizeCodexReasoningReplayItems(existing)
				if !okExisting || len(normalizedExisting) != len(existing) {
					log.Error("home kv best-effort codex reasoning replay append failed prefix=cpa:codex:*: existing value is invalid")
					return false
				}
				existing = normalizedExisting
			}
			combined := appendCodexReasoningReplayTurn(existing, normalized)
			raw, errMarshal := json.Marshal(combined)
			if errMarshal != nil {
				log.Errorf(
					"home kv best-effort codex reasoning replay append failed prefix=cpa:codex:*: %v", errMarshal,
				)
				return false
			}
			written, errCAS := client.KVCompareAndSwap(
				ctx,
				kvKey,
				existingRaw,
				found,
				raw,
				codexReasoningReplayCacheTTL,
			)
			if errCAS != nil {
				log.Errorf("home kv best-effort codex reasoning replay append failed prefix=cpa:codex:*: %v", errCAS)
				return false
			}
			if written {
				return true
			}
		}
		log.Warn("home kv best-effort codex reasoning replay append exhausted compare-and-swap attempts")
		return false
	}

	key = codexReasoningReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	codexReasoningReplayMu.Lock()
	entry := codexReasoningReplayEntries[key]
	if now.Sub(entry.Timestamp) > codexReasoningReplayCacheTTL {
		entry.Items = nil
	}
	entry.Items = appendCodexReasoningReplayTurn(entry.Items, normalized)
	entry.Timestamp = now
	codexReasoningReplayEntries[key] = entry
	if len(codexReasoningReplayEntries) > codexReasoningReplayCacheMaxEntries {
		evictOldestCodexReasoningReplayEntries(codexReasoningReplayCacheEvictBatchSize)
	}
	codexReasoningReplayMu.Unlock()
	return true
}

func appendCodexReasoningReplayTurn(existing, turn [][]byte) [][]byte {
	if len(existing) > 0 && strings.TrimSpace(
		gjson.GetBytes(
			existing[0], "type",
		).String(),
	) != CodexReasoningReplayTurnType {
		existing = nil
	}
	turnID := ""
	if len(turn) > 0 && strings.TrimSpace(gjson.GetBytes(turn[0], "type").String()) == CodexReasoningReplayTurnType {
		turnID = strings.TrimSpace(gjson.GetBytes(turn[0], "id").String())
	}
	if turnID != "" {
		for _, item := range existing {
			if strings.TrimSpace(gjson.GetBytes(item, "type").String()) == CodexReasoningReplayTurnType &&
				strings.TrimSpace(gjson.GetBytes(item, "id").String()) == turnID {
				return trimCodexReasoningReplayItems(cloneCodexReasoningReplayItems(existing))
			}
		}
	}
	combined := make([][]byte, 0, len(existing)+len(turn))
	combined = append(combined, cloneCodexReasoningReplayItems(existing)...)
	combined = append(combined, cloneCodexReasoningReplayItems(turn)...)
	return trimCodexReasoningReplayItems(combined)
}

func trimCodexReasoningReplayItems(items [][]byte) [][]byte {
	for {
		turnStarts := []int{0}
		totalBytes := 0
		for index, item := range items {
			totalBytes += len(item)
			if index > 0 && strings.TrimSpace(gjson.GetBytes(item, "type").String()) == CodexReasoningReplayTurnType {
				turnStarts = append(turnStarts, index)
			}
		}
		if len(turnStarts) <= codexReasoningReplayCacheMaxTurnsPerEntry &&
			totalBytes <= codexReasoningReplayCacheMaxBytesPerEntry {
			return items
		}
		if len(turnStarts) <= 1 {
			return nil
		}
		items = items[turnStarts[1]:]
	}
}

func GetCodexReasoningReplayItemsRequired(
	ctx context.Context,
	modelName string,
	sessionKey string,
) ([][]byte, bool, error) {
	key := codexReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := codexReasoningReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return nil, false, errClient
		}
		remoteKey := codexReasoningReplayKVKey(modelName, sessionKey)
		raw, found, errGet := client.KVGet(ctx, remoteKey)
		if errGet != nil || !found {
			return nil, false, errGet
		}
		if len(raw) > codexReasoningReplayCacheMaxSerializedBytes {
			return nil, false, fmt.Errorf("home kv codex reasoning replay value exceeds serialized size limit")
		}
		var items [][]byte
		if errUnmarshal := json.Unmarshal(raw, &items); errUnmarshal != nil {
			return nil, false, errUnmarshal
		}
		normalized, okNormalize := normalizeCodexReasoningReplayItems(items)
		if !okNormalize || len(normalized) != len(items) {
			return nil, false, fmt.Errorf("home kv codex reasoning replay value is invalid")
		}
		if _, errExpire := client.KVExpire(ctx, remoteKey, codexReasoningReplayCacheTTL); errExpire != nil {
			return nil, false, errExpire
		}
		return normalized, true, nil
	}

	key = codexReasoningReplayLocalCacheKey(scopeID, key)
	cacheCleanupOnce.Do(startCacheCleanup)
	now := time.Now()
	codexReasoningReplayMu.Lock()
	defer codexReasoningReplayMu.Unlock()
	entry, ok := codexReasoningReplayEntries[key]
	if !ok {
		return nil, false, nil
	}
	if now.Sub(entry.Timestamp) > codexReasoningReplayCacheTTL {
		delete(codexReasoningReplayEntries, key)
		return nil, false, nil
	}
	entry.Timestamp = now
	codexReasoningReplayEntries[key] = entry
	return cloneCodexReasoningReplayItems(entry.Items), true, nil
}

func DeleteCodexReasoningReplayItemRequired(ctx context.Context, modelName, sessionKey string) error {
	key := codexReasoningReplayCacheKey(modelName, sessionKey)
	if key == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, scopeID, errClient := codexReasoningReplayRuntime(ctx)
	if homeMode {
		if errClient != nil {
			return errClient
		}
		_, errDel := client.KVDel(ctx, codexReasoningReplayKVKey(modelName, sessionKey))
		return errDel
	}
	key = codexReasoningReplayLocalCacheKey(scopeID, key)
	codexReasoningReplayMu.Lock()
	delete(codexReasoningReplayEntries, key)
	codexReasoningReplayMu.Unlock()
	return nil
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

func codexReasoningReplayKVKey(modelName, sessionKey string) string {
	return "cpa:codex:reasoning-replay:" + codexReasoningReplayHashKeyPart(strings.TrimSpace(modelName)) + ":" +
		codexReasoningReplayHashKeyPart(strings.TrimSpace(sessionKey))
}

func codexReasoningReplayHashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func codexReasoningReplayLocalCacheKey(scopeID, key string) string {
	scopeID = strings.TrimSpace(scopeID)
	if scopeID == "" {
		return key
	}
	return strings.Join([]string{scopeID, key}, "\x00")
}

func normalizeCodexReasoningReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	for _, item := range items {
		normalizedItem, ok := normalizeCodexReasoningReplayItem(item)
		if ok {
			normalized = append(normalized, normalizedItem)
		}
	}
	normalized = trimCodexReasoningReplayItems(normalized)
	return normalized, len(normalized) > 0
}

func normalizeCodexReasoningReplayItem(item []byte) ([]byte, bool) {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case CodexReasoningReplayTurnType:
		return normalizeCodexReasoningReplayTurn(itemResult)
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

func normalizeCodexReasoningReplayTurn(itemResult gjson.Result) ([]byte, bool) {
	turnID := strings.TrimSpace(itemResult.Get("id").String())
	if turnID == "" {
		return nil, false
	}
	normalized := []byte(`{"type":"` + CodexReasoningReplayTurnType + `"}`)
	normalized, _ = sjson.SetBytes(normalized, "id", turnID)
	if fingerprint := strings.TrimSpace(itemResult.Get("assistant_fingerprint").String()); fingerprint != "" {
		normalized, _ = sjson.SetBytes(normalized, "assistant_fingerprint", fingerprint)
	}
	if fingerprint := strings.TrimSpace(itemResult.Get("request_fingerprint").String()); fingerprint != "" {
		normalized, _ = sjson.SetBytes(normalized, "request_fingerprint", fingerprint)
	}
	if callIDs := itemResult.Get("call_ids"); callIDs.IsArray() {
		for _, callIDResult := range callIDs.Array() {
			if callID := strings.TrimSpace(callIDResult.String()); callID != "" {
				normalized, _ = sjson.SetBytes(normalized, "call_ids.-1", callID)
			}
		}
	}
	return normalized, true
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
		if now.Sub(entry.Timestamp) > codexReasoningReplayCacheTTL {
			delete(codexReasoningReplayEntries, key)
		}
	}
	codexReasoningReplayMu.Unlock()
}
