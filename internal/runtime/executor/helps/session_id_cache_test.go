package helps

import (
	"errors"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/google/uuid"
)

func isolateSessionIDCache(t *testing.T) {
	t.Helper()
	sessionIDCacheMu.Lock()
	previous := sessionIDCache
	sessionIDCache = make(map[string]sessionIDCacheEntry)
	sessionIDCacheMu.Unlock()
	t.Cleanup(
		func() {
			sessionIDCacheMu.Lock()
			sessionIDCache = previous
			sessionIDCacheMu.Unlock()
		},
	)
}

func TestCachedSessionIDRequiredHomeReusesKVAcrossLocalCacheReset(t *testing.T) {
	isolateSessionIDCache(t)
	client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
	first, err := CachedSessionIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("first session ID error = %v", err)
	}
	sessionIDCacheMu.Lock()
	clear(sessionIDCache)
	sessionIDCacheMu.Unlock()
	second, err := CachedSessionIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("second session ID error = %v", err)
	}
	if first != second {
		t.Fatalf("session ID = %q then %q, want same Home KV value", first, second)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("session ID %q is not a UUID: %v", first, err)
	}
	if client.setNXCount != 1 || client.lastSetNXTTL != time.Hour {
		t.Fatalf("KVSetNX count/TTL = %d/%s, want 1/1h", client.setNXCount, client.lastSetNXTTL)
	}
	if client.expireCount != 1 || client.lastExpireTTL != time.Hour {
		t.Fatalf("KVExpire count/TTL = %d/%s, want 1/1h", client.expireCount, client.lastExpireTTL)
	}
}

func TestCachedSessionIDRequiredEmptyAPIKeyDoesNotUseHomeKV(t *testing.T) {
	client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
	value, err := CachedSessionIDRequired(ctx, "")
	if err != nil {
		t.Fatalf("empty key session ID error = %v", err)
	}
	if _, err := uuid.Parse(value); err != nil {
		t.Fatalf("session ID %q is not a UUID: %v", value, err)
	}
	if client.getCount != 0 || client.setNXCount != 0 || client.expireCount != 0 {
		t.Fatalf(
			"KV calls get=%d set=%d expire=%d, want all zero",
			client.getCount, client.setNXCount, client.expireCount,
		)
	}
}

func TestCachedSessionIDRequiredHomeKVFailures(t *testing.T) {
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
					client.values[claudeSessionIDKVKey("api-key-1")] = []byte(uuid.NewString())
					client.expireErr = failure
				}
				ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
				if value, err := CachedSessionIDRequired(ctx, "api-key-1"); !errors.Is(err, failure) || value != "" {
					t.Fatalf("session ID=%q error=%v, want empty ID and %v", value, err, failure)
				}
			},
		)
	}
}

func TestCachedSessionIDRequiredHomeRequiresReadAfterSet(t *testing.T) {
	client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
	client.setNXNoPersist = true
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
	if value, err := CachedSessionIDRequired(ctx, "api-key-1"); err == nil || value != "" {
		t.Fatalf("session ID=%q error=%v, want missing-after-set error", value, err)
	}
	if client.setNXCount != 1 || client.getCount != 2 {
		t.Fatalf(
			"KV calls set=%d get=%d, want successful set followed by required read",
			client.setNXCount, client.getCount,
		)
	}
}

func TestCachedSessionIDRequiredNonHomeModeUsesLocalMap(t *testing.T) {
	isolateSessionIDCache(t)
	client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: false})
	first, err := CachedSessionIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("first local session ID error = %v", err)
	}
	second, err := CachedSessionIDRequired(ctx, "api-key-1")
	if err != nil {
		t.Fatalf("second local session ID error = %v", err)
	}
	if first != second {
		t.Fatalf("session ID = %q then %q, want local cache reuse", first, second)
	}
	if client.getCount != 0 || client.setNXCount != 0 || client.expireCount != 0 {
		t.Fatalf(
			"KV calls get=%d set=%d expire=%d, want all zero",
			client.getCount, client.setNXCount, client.expireCount,
		)
	}
}
