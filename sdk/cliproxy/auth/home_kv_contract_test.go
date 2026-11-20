package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

type sdkHomeKVClient struct {
	isolatedHomeClient
	calls []string
	err   error
}

func (c *sdkHomeKVClient) KVTTL(context.Context, string) (time.Duration, bool, error) {
	c.calls = append(c.calls, "TTL")
	return time.Second, c.err == nil, c.err
}

func (c *sdkHomeKVClient) KVIncrBy(context.Context, string, int64) (int64, error) {
	c.calls = append(c.calls, "INCRBY")
	return 7, c.err
}

func (c *sdkHomeKVClient) KVMGet(context.Context, ...string) ([][]byte, []bool, error) {
	c.calls = append(c.calls, "MGET")
	return [][]byte{[]byte(c.label)}, []bool{true}, c.err
}

func (c *sdkHomeKVClient) KVMSet(context.Context, map[string][]byte) error {
	c.calls = append(c.calls, "MSET")
	return c.err
}

func TestSDKHomeKVUsesManagerRequestScope(t *testing.T) {
	previous := home.Current()
	home.SetCurrent(home.New(config.HomeConfig{}))
	t.Cleanup(func() { home.SetCurrent(previous) })
	for _, label := range []string{"service-a", "service-b"} {
		client := &sdkHomeKVClient{label: label}
		manager := NewManager(nil, nil, nil)
		manager.SetHomeClientProvider(func() HomeClient { return client })
		ctx := manager.withHomeRefreshClient(t.Context())
		operations, homeMode, err := HomeKVClientFromContext(ctx)
		if err != nil || !homeMode || operations != client {
			t.Fatalf("%s client = (%T, %t, %v)", label, operations, homeMode, err)
		}
		if ttl, found, err := operations.KVTTL(ctx, "key"); ttl != time.Second || !found || err != nil {
			t.Fatalf("TTL = (%s, %t, %v)", ttl, found, err)
		}
		if value, err := operations.KVIncrBy(ctx, "key", 1); value != 7 || err != nil {
			t.Fatalf("INCRBY = (%d, %v)", value, err)
		}
		if values, found, err := operations.KVMGet(ctx, "key"); err != nil ||
			!reflect.DeepEqual(values, [][]byte{[]byte(label)}) || !reflect.DeepEqual(found, []bool{true}) {
			t.Fatalf("MGET = (%q, %v, %v)", values, found, err)
		}
		if err := operations.KVMSet(ctx, map[string][]byte{"key": []byte("value")}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(client.calls, []string{"TTL", "INCRBY", "MGET", "MSET"}) {
			t.Fatalf("request reached wrong client: %v", client.calls)
		}
		sentinel := errors.New("ERR home kv unavailable")
		client.err = sentinel
		if err := operations.KVMSet(ctx, map[string][]byte{"key": nil}); !errors.Is(err, sentinel) {
			t.Fatalf("SDK write hid error: %v", err)
		}
	}
}

func TestSDKHomeKVReportsLocalAndUnavailableModes(t *testing.T) {
	for _, sample := range []struct {
		name  string
		scope home.ClientScope
		mode  bool
		err   error
	}{
		{"local", home.ClientScope{ID: "local"}, false, nil},
		{"missing", home.ClientScope{Enabled: true}, true, home.ErrNotConnected},
		{"disabled", home.ClientScope{Enabled: true, Client: home.New(config.HomeConfig{})}, true, home.ErrDisabled},
		{
			"disconnected", home.ClientScope{Enabled: true, Client: home.New(config.HomeConfig{Enabled: true})},
			true, home.ErrNotConnected,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				client, mode, err := HomeKVClientFromContext(home.WithClientScope(t.Context(), sample.scope))
				if client != nil || mode != sample.mode || !errors.Is(err, sample.err) {
					t.Fatalf(
						"client=%T mode=%t error=%v, want mode=%t error=%v", client, mode, err, sample.mode, sample.err,
					)
				}
			},
		)
	}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Enabled: true, Client: &isolatedHomeClient{}})
	if client, mode, err := HomeKVClientFromContext(ctx); client != nil || !mode || err == nil {
		t.Fatalf("partial SDK client = (%T, %t, %v)", client, mode, err)
	}
}
