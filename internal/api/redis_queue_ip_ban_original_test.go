package api

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
)

func TestRedisProtocol_IPBan_MirrorsManagementPolicy(t *testing.T) {
	testRedisOriginalIPBan(t, "missing-auth")
}

func TestRedisProtocol_AUTH_IPBan_BlocksCorrectPasswordDuringBan(t *testing.T) {
	testRedisOriginalIPBan(t, "auth")
}

func TestRedisProtocol_LOCALHOST_AUTH_IPBan_BlocksCorrectPasswordDuringBan(t *testing.T) {
	testRedisOriginalIPBan(t, "loopback")
}

func testRedisOriginalIPBan(t *testing.T, mode string) {
	t.Helper()
	const managementPassword = "test-management-password"
	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	redisqueue.SetEnabled(false)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })
	server := newTestServer(t)
	t.Cleanup(server.quotaScheduler.Stop)
	cfg := *server.cfg
	cfg.RemoteManagement.AllowRemote = true
	server.mgmt.SetConfig(&cfg)
	if !server.managementRoutesEnabled.load() {
		t.Fatal("management routes were not enabled for the synthetic credential")
	}
	var conn net.Conn
	if mode == "loopback" {
		addr, stop := startRedisMuxListener(t, server)
		t.Cleanup(stop)
		var err error
		conn, err = net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		client, peer := net.Pipe()
		conn = client
		done := make(chan struct{})
		go func() {
			server.handleRedisConnection(
				&redisTestConn{
					Conn: peer, remoteAddr: &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 1234},
				}, nil,
			)
			close(done)
		}()
		t.Cleanup(
			func() {
				_ = client.Close()
				_ = peer.Close()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("RESP auth fixture did not stop")
				}
			},
		)
	}
	t.Cleanup(func() { _ = conn.Close() })
	reader := bufio.NewReader(conn)
	exchange := func(args ...string) string {
		t.Helper()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := writeTestRESPCommand(conn, args...); err != nil {
			t.Fatal(err)
		}
		line, err := readTestRESPLine(reader)
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	original := []string{"AUTH", "wrong-password"}
	want := "-" + redisUnauthorizedMessage
	if mode == "missing-auth" {
		original, want = []string{"LPOP", "queue"}, "-"+redisNoAuthMessage
	}
	for range 5 {
		if line := exchange(original...); line != want {
			t.Fatalf("original repeated request: %q, want %q", line, want)
		}
	}
	if mode != "missing-auth" {
		if line := exchange("AUTH", managementPassword); line != "+OK" {
			t.Fatalf("correct credential before the adopted ten-failure threshold: %q", line)
		}
	}
	for i := range 9 {
		if line := exchange("AUTH", fmt.Sprintf("wrong-password-%d", i)); line != "-"+redisUnauthorizedMessage {
			t.Fatalf("distinct counted failure %d: %q", i+2, line)
		}
	}
	if mode == "missing-auth" {
		if line := exchange("LPOP", "queue"); line != "-"+redisAuthRateLimitErrorMessage {
			t.Fatalf("missing AUTH after the adopted threshold: %q", line)
		}
	}
	if line := exchange("AUTH", managementPassword); line != "-"+redisAuthRateLimitErrorMessage {
		t.Fatalf("correct credential during IP ban: %q", line)
	}
}
