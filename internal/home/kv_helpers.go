package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func HashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// KVClientFromContext 优先使用请求所属的 Home，未指定作用域时沿用当前客户端
func KVClientFromContext(ctx context.Context) (KVClient, bool, error) {
	if scope, ok := ClientScopeFromContext(ctx); ok {
		if !scope.Enabled {
			return nil, false, nil
		}
		if scope.Client == nil {
			return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrNotConnected)
		}
		if !scope.Client.Enabled() {
			return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrDisabled)
		}
		if !scope.Client.HeartbeatOK() {
			return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrNotConnected)
		}
		return scope.Client, true, nil
	}
	client, homeMode, err := currentKVClient()
	if err != nil || !homeMode {
		return nil, homeMode, err
	}
	return client, true, nil
}

func currentKVClient() (*Client, bool, error) {
	client := Current()
	if client == nil {
		return nil, false, nil
	}
	if !client.Enabled() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrDisabled)
	}
	if !client.HeartbeatOK() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrNotConnected)
	}
	return client, true, nil
}
