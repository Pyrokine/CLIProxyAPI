package home

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func HashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func CurrentKVClient() (*Client, bool, error) {
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
