package helps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type requestScopedClaudeKV struct {
	home.KVClient
	err error
}

func (*requestScopedClaudeKV) Enabled() bool     { return true }
func (*requestScopedClaudeKV) HeartbeatOK() bool { return true }
func (c *requestScopedClaudeKV) KVGet(context.Context, string) ([]byte, bool, error) {
	return nil, false, c.err
}

func TestClaudeRequiredKVUsesRequestOwnerAndErrors(t *testing.T) {
	previous := home.Current()
	home.ClearCurrent()
	t.Cleanup(func() { home.SetCurrent(previous) })
	sentinel := errors.New("ERR home kv unavailable")
	ctx := home.WithClientScope(
		t.Context(), home.ClientScope{
			Client: &requestScopedClaudeKV{err: sentinel}, ID: "request-service", Enabled: true,
		},
	)
	for name, call := range claudeRequiredKVCalls(t) {
		t.Run(
			name, func(t *testing.T) {
				if err := call(ctx); !errors.Is(err, sentinel) {
					t.Fatalf("Required error = %v, want request-scoped Home error", err)
				}
			},
		)
	}
}

func TestClaudeRequiredKVExplicitLocalIgnoresOtherHome(t *testing.T) {
	previous := home.Current()
	home.SetCurrent(home.New(config.HomeConfig{}))
	t.Cleanup(func() { home.SetCurrent(previous) })
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: "local-service", Enabled: false})
	for name, call := range claudeRequiredKVCalls(t) {
		t.Run(
			name, func(t *testing.T) {
				if err := call(ctx); err != nil {
					t.Fatalf("local request used another Home: %v", err)
				}
			},
		)
	}
}

type scopedClaudeIDKV struct {
	*fakeClaudeDeviceProfileKVClient
}

func (*scopedClaudeIDKV) Enabled() bool     { return true }
func (*scopedClaudeIDKV) HeartbeatOK() bool { return true }
func (*scopedClaudeIDKV) KVDel(context.Context, ...string) (int64, error) {
	return 0, nil
}
func (c *scopedClaudeIDKV) KVSet(ctx context.Context, key string, value []byte, opts home.KVSetOptions) (bool, error) {
	if opts.NX {
		return c.KVSetNX(ctx, key, value, opts.EX)
	}
	return c.fakeClaudeDeviceProfileKVClient.KVSet(ctx, key, value, opts)
}

func TestClaudeRequiredKVPreservesSetWinnerAndExpiryFailures(t *testing.T) {
	const key = "cpa:test:key"
	for _, name := range []string{"created", "existing", "set-error", "missing-winner", "expire-error"} {
		t.Run(
			name, func(t *testing.T) {
				client := &scopedClaudeIDKV{newFakeClaudeDeviceProfileKVClient()}
				sentinel := errors.New("ERR home kv unavailable")
				sessionID := "11111111-1111-4111-8111-111111111111"
				userID := GenerateFakeUserIDWithSessionID(sessionID)
				switch name {
				case "existing", "expire-error":
					client.values[claudeSessionIDKVKey(key)] = []byte(sessionID)
					client.values[claudeUserIDKVKey(key)] = []byte(userID)
				case "set-error":
					client.setNXErr = sentinel
				case "missing-winner":
					client.setNXResult = false
				}
				if name == "expire-error" {
					client.expireErr = sentinel
				}
				ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, Enabled: true})
				for kind, call := range map[string]func(context.Context, string) (string, error){
					"session": CachedSessionIDRequired, "user": CachedUserIDRequired,
				} {
					value, err := call(ctx, key)
					switch name {
					case "set-error", "expire-error":
						if !errors.Is(err, sentinel) || value != "" {
							t.Fatalf("%s value=%q error=%v", kind, value, err)
						}
					case "missing-winner":
						if err == nil || !strings.Contains(err.Error(), "missing after set") || value != "" {
							t.Fatalf("%s missing winner value=%q error=%v", kind, value, err)
						}
					default:
						if err != nil || value == "" || kind == "user" && !IsValidUserID(value) {
							t.Fatalf("%s value=%q error=%v", kind, value, err)
						}
						if name == "existing" &&
							(kind == "session" && value != sessionID || kind == "user" && value != userID) {
							t.Fatalf("%s replaced existing value: %q", kind, value)
						}
					}
				}
				if name == "created" && (client.setNXCount != 2 || client.lastSetNXTTL != time.Hour) {
					t.Fatalf("NX writes=%d TTL=%s", client.setNXCount, client.lastSetNXTTL)
				}
				if name == "existing" && (client.expireCount != 2 || client.lastExpireTTL != time.Hour) {
					t.Fatalf("expiry updates=%d TTL=%s", client.expireCount, client.lastExpireTTL)
				}
			},
		)
	}
}

func TestClaudeRequiredKVLocalCachesKeepServiceOwnership(t *testing.T) {
	for kind, call := range map[string]func(context.Context, string) (string, error){
		"session": CachedSessionIDRequired, "user": CachedUserIDRequired,
	} {
		var previous string
		for _, id := range []string{"service-a", "service-b"} {
			ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: id})
			first, err := call(ctx, t.Name()+kind)
			if err != nil || first == "" || first == previous {
				t.Fatalf("%s %s value=%q previous=%q error=%v", kind, id, first, previous, err)
			}
			second, err := call(ctx, t.Name()+kind)
			if err != nil || second != first {
				t.Fatalf("%s %s repeated value=%q, want %q; error=%v", kind, id, second, first, err)
			}
			previous = first
		}
	}
}

func claudeRequiredKVCalls(t *testing.T) map[string]func(context.Context) error {
	t.Helper()
	return map[string]func(context.Context) error{
		"session": func(ctx context.Context) error {
			_, err := CachedSessionIDRequired(ctx, "key")
			return err
		},
		"user": func(ctx context.Context) error {
			_, err := CachedUserIDRequired(ctx, "key")
			return err
		},
		"profile": func(ctx context.Context) error {
			_, err := ResolveClaudeDeviceProfileRequired(ctx, nil, "key", nil, nil)
			return err
		},
		"credential-pool": func(ctx context.Context) error {
			_, err := EnsureClaudeCredentialDevicePoolRequired(
				ctx, &cliproxyauth.Auth{
					ID: "request-scoped-auth", Provider: "claude",
					Metadata: map[string]any{"access_token": "synthetic"},
				},
			)
			return err
		},
	}
}
