package home

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

type testNetConnWrapper struct {
	net.Conn
	closeCalled bool
}

func (c *testNetConnWrapper) Close() error {
	c.closeCalled = true
	return nil
}

func (c *testNetConnWrapper) NetConn() net.Conn {
	return c.Conn
}

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

func TestCloseDetachedClientsClosesUnderlyingTLSConnAndUntracks(t *testing.T) {
	raw, peer := net.Pipe()
	defer func() { _ = raw.Close() }()
	defer func() { _ = peer.Close() }()

	transport := &testNetConnWrapper{Conn: raw}
	tlsConn := tls.Client(transport, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "home.example.com"})
	client := New(config.HomeConfig{Enabled: true})
	dispatchConn := &homeDispatchConn{Conn: tlsConn, client: client}
	client.connections = map[*homeDispatchConn]struct{}{dispatchConn: {}}

	closeDetachedClients(nil, nil, []*homeDispatchConn{dispatchConn})
	dispatchConn.untrack()
	dispatchConn.untrack()

	if got := dispatchConn.NetConn(); got != tlsConn {
		t.Fatalf("NetConn() = %T, want original TLS connection", got)
	}
	if transport.closeCalled {
		t.Fatal("TLS wrapper Close was called instead of the underlying transport")
	}
	client.mu.Lock()
	remaining := len(client.connections)
	client.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("tracked connections = %d, want 0", remaining)
	}
	if errDeadline := peer.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		if !errors.Is(errDeadline, io.ErrClosedPipe) {
			t.Fatalf("set peer deadline: %v", errDeadline)
		}
	} else {
		var buffer [1]byte
		if _, errRead := peer.Read(buffer[:]); errRead == nil {
			t.Fatal("underlying transport remained open")
		}
	}
}

func TestHomeOperationTimeoutOverrideIsTestScopedAndPreservedAcrossLifetime(t *testing.T) {
	production := New(config.HomeConfig{Enabled: true, Host: "127.0.0.1", Port: 6379})
	options, err := production.redisOptionsLocked("127.0.0.1:6379")
	if err != nil {
		t.Fatalf("redisOptionsLocked() error = %v", err)
	}
	if production.testOperationTimeout != 0 || options.DialTimeout != homeRedisOperationTimeout ||
		options.ReadTimeout != homeRedisOperationTimeout || options.WriteTimeout != homeRedisOperationTimeout {
		t.Fatalf(
			"production timeouts = override:%s dial:%s read:%s write:%s",
			production.testOperationTimeout,
			options.DialTimeout,
			options.ReadTimeout,
			options.WriteTimeout,
		)
	}
	_, heartbeat := production.subscriptionParameters()
	if want := (config.CredentialConcurrencyConfig{}).WithDefaults().CPAHeartbeatTimeout; heartbeat != want {
		t.Fatalf("production heartbeat timeout = %s, want %s", heartbeat, want)
	}

	production.testOperationTimeout = 20 * time.Millisecond
	testOptions, err := production.redisOptionsLocked("127.0.0.1:6379")
	if err != nil {
		t.Fatalf("redisOptionsLocked(test override) error = %v", err)
	}
	if testOptions.DialTimeout != 20*time.Millisecond || testOptions.ReadTimeout != 20*time.Millisecond ||
		testOptions.WriteTimeout != 20*time.Millisecond {
		t.Fatalf(
			"test timeouts = dial:%s read:%s write:%s, want 20ms",
			testOptions.DialTimeout,
			testOptions.ReadTimeout,
			testOptions.WriteTimeout,
		)
	}
	_, heartbeat = production.subscriptionParameters()
	if heartbeat != 20*time.Millisecond {
		t.Fatalf("test heartbeat timeout = %s, want 20ms", heartbeat)
	}
	next := production.NewLifetime()
	if next == nil || next.testOperationTimeout != 20*time.Millisecond {
		t.Fatalf("NewLifetime() test timeout = %#v", next)
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

func TestAuthDispatchRequestRetryConstraints(t *testing.T) {
	constraints := AuthDispatchConstraints{
		RetryRound: 2, ExcludedAuthIDs: []string{"synthetic-a"}, PinnedAuthID: " synthetic-b ",
		CredentialPolicy: "codex_alpha_search_v1", ParentSessionID: " parent-session ",
	}
	request := newAuthDispatchRequest("synthetic-model", "synthetic-session", nil, 1, constraints)
	constraints.ExcludedAuthIDs[0] = "changed"
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["retry_round"] != float64(2) || payload["pinned_auth_id"] != "synthetic-b" ||
		payload["credential_policy"] != "codex_alpha_search_v1" || payload["parent_session_id"] != "parent-session" ||
		payload["excluded_auth_ids"].([]any)[0] != "synthetic-a" {
		t.Fatalf("wire=%s", raw)
	}
}
