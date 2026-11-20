package helps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/google/uuid"
)

func isolateUserIDCache(t *testing.T) {
	t.Helper()
	userIDCacheMu.Lock()
	previous := userIDCache
	userIDCache = make(map[string]userIDCacheEntry)
	userIDCacheMu.Unlock()
	t.Cleanup(
		func() {
			userIDCacheMu.Lock()
			userIDCache = previous
			userIDCacheMu.Unlock()
		},
	)
}

func TestGenerateFakeUserIDUsesClaudeCode220JSONShape(t *testing.T) {
	userID := GenerateFakeUserIDWithSessionID(uuid.NewString())
	if !IsValidUserID(userID) {
		t.Fatalf("user ID %q is not valid", userID)
	}
	var value claudeMetadataUserID
	if err := json.Unmarshal([]byte(userID), &value); err != nil {
		t.Fatalf("unmarshal user ID: %v", err)
	}
	if value.AccountUUID != "" {
		t.Fatalf("account_uuid = %q, want empty", value.AccountUUID)
	}
}

func TestCachedUserIDUsesCachedClaudeSessionID(t *testing.T) {
	isolateUserIDCache(t)
	isolateSessionIDCache(t)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	const key = "api-key-shared-session"
	sessionID, err := CachedSessionIDRequired(ctx, key)
	if err != nil {
		t.Fatalf("cached session ID error = %v", err)
	}
	userID, err := CachedUserIDRequired(ctx, key)
	if err != nil {
		t.Fatalf("cached user ID error = %v", err)
	}
	var value claudeMetadataUserID
	if err := json.Unmarshal([]byte(userID), &value); err != nil {
		t.Fatalf("unmarshal user ID: %v", err)
	}
	if value.SessionID != sessionID {
		t.Fatalf("metadata session_id = %q, header session ID = %q", value.SessionID, sessionID)
	}
}

func TestCachedUserID_ReusesWithinTTL(t *testing.T) {
	isolateUserIDCache(t)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	first, err := CachedUserIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("first cached user ID error = %v", err)
	}
	second, err := CachedUserIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("second cached user ID error = %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("cached user ID = %q then %q, want nonempty identical values", first, second)
	}
}

func TestCachedUserID_ExpiresAfterTTL(t *testing.T) {
	isolateUserIDCache(t)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	expiredID, err := CachedUserIDRequired(ctx, "api-key-expired")
	if err != nil {
		t.Fatalf("initial cached user ID error = %v", err)
	}
	cacheKey := claudeLocalCacheKey(ctx, userIDCacheKey("api-key-expired"))
	userIDCacheMu.Lock()
	userIDCache[cacheKey] = userIDCacheEntry{value: expiredID, expire: time.Now().Add(-time.Minute)}
	userIDCacheMu.Unlock()
	newID, err := CachedUserIDRequired(ctx, "api-key-expired")
	if err != nil {
		t.Fatalf("regenerated user ID error = %v", err)
	}
	if newID == "" || newID == expiredID {
		t.Fatalf("expired user ID = %q, regenerated = %q, want a different nonempty value", expiredID, newID)
	}
}

func TestCachedUserID_IsScopedByAPIKey(t *testing.T) {
	isolateUserIDCache(t)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	first, err := CachedUserIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("first API key user ID error = %v", err)
	}
	second, err := CachedUserIDRequired(ctx, "api-key-2")
	if err != nil {
		t.Fatalf("second API key user ID error = %v", err)
	}
	if first == second {
		t.Fatalf("different API keys have the same user ID: %q", first)
	}
}

func TestCachedUserID_RenewsTTLOnHit(t *testing.T) {
	isolateUserIDCache(t)
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
	const key = "api-key-renew"
	id, err := CachedUserIDRequired(ctx, key)
	if err != nil {
		t.Fatalf("initial cached user ID error = %v", err)
	}
	cacheKey := claudeLocalCacheKey(ctx, userIDCacheKey(key))
	soon := time.Now()
	userIDCacheMu.Lock()
	userIDCache[cacheKey] = userIDCacheEntry{value: id, expire: soon.Add(2 * time.Second)}
	userIDCacheMu.Unlock()
	refreshed, err := CachedUserIDRequired(ctx, key)
	if err != nil || refreshed != id {
		t.Fatalf("refreshed user ID=%q error=%v, want %q", refreshed, err, id)
	}
	userIDCacheMu.RLock()
	entry := userIDCache[cacheKey]
	userIDCacheMu.RUnlock()
	if entry.expire.Sub(soon) < 30*time.Minute {
		t.Fatalf("TTL did not renew: %s remaining", entry.expire.Sub(soon))
	}
}

func TestCachedUserIDRequiredHomeReusesKVAcrossLocalCacheReset(t *testing.T) {
	isolateUserIDCache(t)
	client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
	first, err := CachedUserIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("first Home user ID error = %v", err)
	}
	userIDCacheMu.Lock()
	clear(userIDCache)
	userIDCacheMu.Unlock()
	second, err := CachedUserIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("second Home user ID error = %v", err)
	}
	if first != second || !IsValidUserID(first) {
		t.Fatalf("Home user ID = %q then %q, want same valid value", first, second)
	}
	if client.setNXCount != 2 || client.lastSetNXTTL != time.Hour {
		t.Fatalf("KVSetNX count/TTL = %d/%s, want 2/1h for session and user", client.setNXCount, client.lastSetNXTTL)
	}
	if client.expireCount != 1 || client.lastExpireTTL != time.Hour {
		t.Fatalf("KVExpire count/TTL = %d/%s, want 1/1h", client.expireCount, client.lastExpireTTL)
	}
}

func TestCachedUserIDRequiredEmptyAPIKeyDoesNotUseHomeKV(t *testing.T) {
	client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
	value, err := CachedUserIDRequired(ctx, "")
	if err != nil || !IsValidUserID(value) {
		t.Fatalf("empty API key user ID=%q error=%v, want valid user ID", value, err)
	}
	if client.getCount != 0 || client.setNXCount != 0 || client.expireCount != 0 {
		t.Fatalf(
			"KV calls get=%d set=%d expire=%d, want all zero",
			client.getCount, client.setNXCount, client.expireCount,
		)
	}
}

func TestCachedUserIDRequiredHomeKVFailures(t *testing.T) {
	for _, name := range []string{"get", "set", "expire"} {
		t.Run(
			name, func(t *testing.T) {
				client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
				failure := errors.New(name + " failed")
				switch name {
				case "get":
					client.getErr = failure
				case "set":
					client.setNXErr = failure
				case "expire":
					client.values[claudeUserIDKVKey("api-key-1")] = []byte(
						GenerateFakeUserIDWithSessionID(uuid.NewString()),
					)
					client.expireErr = failure
				}
				ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
				if value, err := CachedUserIDRequired(ctx, "api-key-1"); !errors.Is(err, failure) || value != "" {
					t.Fatalf("user ID=%q error=%v, want empty ID and %v", value, err, failure)
				}
			},
		)
	}
}

func TestCachedUserIDRequiredHomeRequiresReadAfterSet(t *testing.T) {
	for _, missing := range []string{"session", "user"} {
		t.Run(
			missing, func(t *testing.T) {
				client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
				client.setNXNoPersist = true
				if missing == "user" {
					client.values[claudeSessionIDKVKey("api-key-1")] = []byte(uuid.NewString())
				}
				ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
				value, err := CachedUserIDRequired(ctx, "api-key-1")
				if err == nil || value != "" || !strings.Contains(err.Error(), missing+" id missing after set") {
					t.Fatalf("user ID=%q error=%v, want missing %s after successful set", value, err, missing)
				}
			},
		)
	}
}
