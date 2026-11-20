package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	log "github.com/sirupsen/logrus"
)

// signatureEntry holds a cached thinking signature with timestamp
type signatureEntry struct {
	Signature string
	Timestamp time.Time
}

const (
	// signatureCacheTTL is how long signatures are valid
	signatureCacheTTL = 3 * time.Hour

	// signatureTextHashLen is the length of the hash key (16 hex chars = 64-bit key space)
	signatureTextHashLen = 16

	// minValidSignatureLen is the minimum length for a signature to be considered valid
	minValidSignatureLen = 50

	// cleanupInterval controls how often stale entries are purged
	cleanupInterval = 10 * time.Minute
)

// signatureCache stores signatures by model group -> textHash -> SignatureEntry
var signatureCache sync.Map

// cacheCleanupOnce ensures the background cleanup goroutine starts only once
var cacheCleanupOnce sync.Once

// groupCache is the inner map type
type groupCache struct {
	mu      sync.RWMutex
	entries map[string]signatureEntry
}

// hashText creates a stable, Unicode-safe key from text content
func hashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])[:signatureTextHashLen]
}

// getOrCreateGroupCache gets or creates a cache bucket for a model group
func getOrCreateGroupCache(groupKey string) *groupCache {
	// Start background cleanup on first access
	cacheCleanupOnce.Do(startCacheCleanup)

	if val, ok := signatureCache.Load(groupKey); ok {
		return val.(*groupCache)
	}
	sc := &groupCache{entries: make(map[string]signatureEntry)}
	actual, _ := signatureCache.LoadOrStore(groupKey, sc)
	return actual.(*groupCache)
}

// startCacheCleanup launches a background goroutine that periodically
// removes caches where all entries have expired.
func startCacheCleanup() {
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredCaches()
		}
	}()
}

// purgeExpiredCaches removes caches with no valid (non-expired) entries.
func purgeExpiredCaches() {
	now := time.Now()
	signatureCache.Range(
		func(key, value any) bool {
			sc := value.(*groupCache)
			sc.mu.Lock()
			// Remove expired entries
			for k, entry := range sc.entries {
				if now.Sub(entry.Timestamp) > signatureCacheTTL {
					delete(sc.entries, k)
				}
			}
			isEmpty := len(sc.entries) == 0
			sc.mu.Unlock()
			// Remove cache bucket if empty
			if isEmpty {
				signatureCache.Delete(key)
			}
			return true
		},
	)
	purgeExpiredCodexReasoningReplayCache(now)
	purgeExpiredXAIReasoningReplayCache(now)
	purgeExpiredAntigravityReasoningReplayCache(now)
	purgeExpiredKimiThinkingReplayCache(now)
	purgeExpiredClaudeThinkingReplayCache(now)
}

// StoreSignature stores a thinking signature for a given model group and text.
// Used for Claude models that require signed thinking blocks in multi-turn conversations.
func StoreSignature(modelName, text, signature string) {
	CacheSignatureBestEffort(context.Background(), modelName, text, signature)
}

// CacheSignatureBestEffort 保存当前请求的签名，Home 写入失败时记录错误
//
//goland:noinspection GoNameStartsWithPackageName
func CacheSignatureBestEffort(ctx context.Context, modelName, text, signature string) bool {
	if text == "" || len(signature) < minValidSignatureLen {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, err := home.KVClientFromContext(ctx)
	if homeMode {
		if err != nil {
			log.Errorf("home kv best-effort signature set failed prefix=cpa:signature:*: %v", err)
			return false
		}
		written, err := client.KVSet(
			ctx, signatureKVKey(modelName, text), []byte(signature),
			home.KVSetOptions{EX: signatureCacheTTL},
		)
		if err != nil {
			log.Errorf("home kv best-effort signature set failed prefix=cpa:signature:*: %v", err)
			return false
		}
		if !written {
			log.Warn("home kv best-effort signature set not written prefix=cpa:signature:*")
		}
		return written
	}

	groupKey := signatureLocalGroupKey(ctx, GetModelGroup(modelName))
	textHash := hashText(text)
	sc := getOrCreateGroupCache(groupKey)
	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.entries[textHash] = signatureEntry{
		Signature: signature,
		Timestamp: time.Now(),
	}
	return true
}

// GetCachedSignature retrieves a cached signature for a given model group and text.
// Returns empty string if not found or expired.
func GetCachedSignature(modelName, text string) string {
	signature, err := GetCachedSignatureRequired(context.Background(), modelName, text)
	if err != nil {
		log.Errorf("home kv signature get failed prefix=cpa:signature:*: %v", err)
	}
	return signature
}

type signatureKVError struct {
	cause error
}

func (e signatureKVError) Error() string { return e.cause.Error() }

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e signatureKVError) Unwrap() error       { return e.cause }
func (signatureKVError) StatusCode() int       { return http.StatusServiceUnavailable }
func (signatureKVError) IsRequestScoped() bool { return true }

// GetCachedSignatureRequired 读取签名并延长有效期，Home 错误继续返回调用方
func GetCachedSignatureRequired(ctx context.Context, modelName, text string) (string, error) {
	groupKey := GetModelGroup(modelName)
	missing := ""
	if groupKey == "gemini" {
		missing = "skip_thought_signature_validator"
	}
	if text == "" {
		return missing, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, err := home.KVClientFromContext(ctx)
	if homeMode {
		if err != nil {
			return "", signatureKVError{cause: err}
		}
		key := signatureKVKey(modelName, text)
		raw, found, err := client.KVGet(ctx, key)
		if err != nil {
			return "", signatureKVError{cause: err}
		}
		if !found {
			return missing, nil
		}
		if _, err := client.KVExpire(ctx, key, signatureCacheTTL); err != nil {
			return "", signatureKVError{cause: err}
		}
		return string(raw), nil
	}
	val, ok := signatureCache.Load(signatureLocalGroupKey(ctx, groupKey))
	if !ok {
		return missing, nil
	}
	sc := val.(*groupCache)

	textHash := hashText(text)
	now := time.Now()
	sc.mu.Lock()
	entry, exists := sc.entries[textHash]
	if !exists {
		sc.mu.Unlock()
		return missing, nil
	}
	if now.Sub(entry.Timestamp) > signatureCacheTTL {
		delete(sc.entries, textHash)
		sc.mu.Unlock()
		return missing, nil
	}

	// Refresh TTL on access (sliding expiration).
	entry.Timestamp = now
	sc.entries[textHash] = entry
	sc.mu.Unlock()

	return entry.Signature, nil
}

// ClearSignatureCache clears signature cache for a specific model group or all groups.
func ClearSignatureCache(modelName string) {
	if modelName == "" {
		signatureCache.Range(
			func(key, _ any) bool {
				signatureCache.Delete(key)
				return true
			},
		)
		return
	}
	groupKey := GetModelGroup(modelName)
	signatureCache.Range(
		func(key, _ any) bool {
			if name := key.(string); name == groupKey || strings.HasSuffix(name, "\x00"+groupKey) {
				signatureCache.Delete(key)
			}
			return true
		},
	)
}

// DeleteCachedSignatureRequired 删除当前请求作用域内的一项签名
func DeleteCachedSignatureRequired(ctx context.Context, modelName, text string) error {
	if text == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, homeMode, err := home.KVClientFromContext(ctx)
	if homeMode {
		if err != nil {
			return err
		}
		_, err := client.KVDel(ctx, signatureKVKey(modelName, text))
		return err
	}
	if value, ok := signatureCache.Load(signatureLocalGroupKey(ctx, GetModelGroup(modelName))); ok {
		sc := value.(*groupCache)
		sc.mu.Lock()
		delete(sc.entries, hashText(text))
		sc.mu.Unlock()
	}
	return nil
}

func signatureKVKey(modelName, text string) string {
	return "cpa:signature:" + GetModelGroup(modelName) + ":" + home.HashKeyPart(text)
}

func signatureLocalGroupKey(ctx context.Context, group string) string {
	if scope, ok := home.ClientScopeFromContext(ctx); ok {
		return scope.ID + "\x00" + group
	}
	return group
}

// hasValidSignature checks if a signature is valid (non-empty and long enough)
func hasValidSignature(modelName, signature string) bool {
	return (signature != "" && len(signature) >= minValidSignatureLen) ||
		(signature == "skip_thought_signature_validator" && GetModelGroup(modelName) == "gemini")
}

func GetModelGroup(modelName string) string {
	if strings.Contains(modelName, "gpt") {
		return "gpt"
	} else if strings.Contains(modelName, "claude") {
		return "claude"
	} else if strings.Contains(modelName, "gemini") {
		return "gemini"
	}
	return modelName
}
