package home

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestAuthDispatchRequestIncludesCount(t *testing.T) {
	req := newAuthDispatchRequest("gpt-5.4", "session-1", http.Header{"User-Agent": {"codex-cli"}}, 2)

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal auth dispatch request: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal auth dispatch request: %v", err)
	}
	if got := int(payload["count"].(float64)); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
}

func TestAuthDispatchRequestFiltersHeaders(t *testing.T) {
	req := newAuthDispatchRequest(
		"gpt-5.4",
		"session-1",
		http.Header{
			"Authorization":          {"Bearer user-key"},
			"Cookie":                 {"session=secret"},
			"X-Api-Key":              {"sk-user-key"},
			"X-Goog-Api-Key":         {"goog-user-key"},
			"X-Internal-Token":       {"internal-secret"},
			"User-Agent":             {"codex-cli"},
			"X-Codex-Client-Version": {"1.2.3"},
		},
		1,
	)

	if req.Headers["authorization"] != "Bearer user-key" {
		t.Fatalf("authorization = %q, want Bearer user-key", req.Headers["authorization"])
	}
	if req.Headers["x-api-key"] != "sk-user-key" {
		t.Fatalf("x-api-key = %q, want sk-user-key", req.Headers["x-api-key"])
	}
	if req.Headers["x-goog-api-key"] != "goog-user-key" {
		t.Fatalf("x-goog-api-key = %q, want goog-user-key", req.Headers["x-goog-api-key"])
	}
	if req.Headers["cookie"] != "" || req.Headers["x-internal-token"] != "" {
		t.Fatalf("unwanted headers were forwarded: %+v", req.Headers)
	}
	if req.Headers["user-agent"] != "codex-cli" {
		t.Fatalf("user-agent = %q, want codex-cli", req.Headers["user-agent"])
	}
	if req.Headers["x-codex-client-version"] != "1.2.3" {
		t.Fatalf("x-codex-client-version = %q, want 1.2.3", req.Headers["x-codex-client-version"])
	}
}

func TestAuthDispatchRequestDefaultsCountToOne(t *testing.T) {
	req := newAuthDispatchRequest("gpt-5.4", "", nil, 0)

	if req.Count != 1 {
		t.Fatalf("count = %d, want 1", req.Count)
	}
}

func TestRedisOptionsHomeTLSDisabled(t *testing.T) {
	client := New(
		config.HomeConfig{
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    6379,
		},
	)

	client.mu.Lock()
	options, err := client.redisOptionsLocked("127.0.0.1:6379")
	client.mu.Unlock()
	if err != nil {
		t.Fatalf("redisOptionsLocked() error = %v", err)
	}

	if options.TLSConfig != nil {
		t.Fatalf("TLSConfig = %#v, want nil", options.TLSConfig)
	}
	if options.Password != "" {
		t.Fatalf("Password = %q, want empty", options.Password)
	}
}

func TestRedisOptionsHomeTLSEnabledUsesSeedHostAsServerName(t *testing.T) {
	client := New(
		config.HomeConfig{
			Enabled: true,
			Host:    "home.example.com",
			Port:    444,
			TLS: config.HomeTLSConfig{
				Enable: true,
			},
		},
	)
	client.homeCfg.Host = "127.0.0.1"

	client.mu.Lock()
	options, err := client.redisOptionsLocked("127.0.0.1:444")
	client.mu.Unlock()
	if err != nil {
		t.Fatalf("redisOptionsLocked() error = %v", err)
	}

	if options.TLSConfig == nil {
		t.Fatal("TLSConfig is nil")
	}
	if options.TLSConfig.ServerName != "home.example.com" {
		t.Fatalf("ServerName = %q, want home.example.com", options.TLSConfig.ServerName)
	}
	if options.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS 1.2", options.TLSConfig.MinVersion)
	}
}

func TestRedisOptionsHomeTLSEnabledUsesExplicitServerName(t *testing.T) {
	client := New(
		config.HomeConfig{
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    444,
			TLS: config.HomeTLSConfig{
				Enable:             true,
				ServerName:         "home.example.com",
				InsecureSkipVerify: true,
			},
		},
	)

	client.mu.Lock()
	options, err := client.redisOptionsLocked("127.0.0.1:444")
	client.mu.Unlock()
	if err != nil {
		t.Fatalf("redisOptionsLocked() error = %v", err)
	}

	if options.TLSConfig == nil {
		t.Fatal("TLSConfig is nil")
	}
	if options.TLSConfig.ServerName != "home.example.com" {
		t.Fatalf("ServerName = %q, want home.example.com", options.TLSConfig.ServerName)
	}
	if !options.TLSConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify = false, want true")
	}
}

func TestRefreshClusterNodesDisabledSkipsRedisCommand(t *testing.T) {
	client := New(
		config.HomeConfig{
			Enabled:                 true,
			Host:                    "127.0.0.1",
			Port:                    1,
			DisableClusterDiscovery: true,
		},
	)

	switched, err := client.refreshClusterNodes(context.Background())
	if err != nil {
		t.Fatalf("refreshClusterNodes() error = %v", err)
	}
	if switched {
		t.Fatal("refreshClusterNodes() switched = true, want false")
	}
	if client.cmd != nil || client.sub != nil {
		t.Fatalf("redis clients were initialized when cluster discovery was disabled")
	}
}

func TestFailoverAfterReconnectFailureDisabledDoesNotSwitchToClusterNode(t *testing.T) {
	client := New(
		config.HomeConfig{
			Enabled:                 true,
			Host:                    "seed.example.com",
			Port:                    8327,
			DisableClusterDiscovery: true,
		},
	)
	client.mu.Lock()
	client.clusterNodes = []clusterNode{{IP: "other.example.com", Port: 8327}}
	client.reconnectFailures = homeReconnectFailoverThreshold - 1
	client.mu.Unlock()

	switched, addr := client.failoverAfterReconnectFailure()
	if switched {
		t.Fatalf("failoverAfterReconnectFailure() switched to %s, want no switch", addr)
	}
	if got, _ := client.addr(); got != "seed.example.com:8327" {
		t.Fatalf("addr() = %q, want seed.example.com:8327", got)
	}
}
