package api

import (
	"bufio"
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
)

func openAuthenticatedRedisConnection(
	t *testing.T,
	server *Server,
	managementPassword string,
) (net.Conn, <-chan struct{}) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	routeDone := make(chan struct{})
	go func() {
		server.routeMuxConnection(
			&redisTestConn{
				Conn:       serverConn,
				remoteAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345},
			},
			nil,
		)
		close(routeDone)
	}()

	reader := bufio.NewReader(clientConn)
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
	if errWrite := writeTestRESPCommand(clientConn, "AUTH", managementPassword); errWrite != nil {
		t.Fatalf("failed to write AUTH command: %v", errWrite)
	}
	if response, errRead := readTestRESPSimpleString(reader); errRead != nil {
		t.Fatalf("failed to read AUTH response: %v", errRead)
	} else if response != "OK" {
		t.Fatalf("unexpected AUTH response: %q", response)
	}
	return clientConn, routeDone
}

func requireRedisConnectionClosed(t *testing.T, conn net.Conn, routeDone <-chan struct{}) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, errRead := bufio.NewReader(conn).ReadByte(); errRead == nil {
		t.Fatal("authenticated Redis connection remained open")
	} else {
		var netErr net.Error
		if errors.As(errRead, &netErr) && netErr.Timeout() {
			t.Fatalf("Redis connection was not closed: %v", errRead)
		}
	}
	select {
	case <-routeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Redis route goroutine remained active")
	}
}

func TestServerStopClosesAuthenticatedIdleRedisConnection(t *testing.T) {
	const managementPassword = "test-management-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	queue := redisqueue.NewQueue()
	server := newTestServerWithOptions(t, WithUsageQueue(queue))
	queue.SetEnabled(true)

	clientConn, routeDone := openAuthenticatedRedisConnection(t, server, managementPassword)
	defer func() { _ = clientConn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errStop := server.Stop(ctx); errStop != nil {
		t.Fatalf("Stop() error = %v", errStop)
	}
	requireRedisConnectionClosed(t, clientConn, routeDone)
}

func TestServerStopTimeoutClosesRedisDuringBlockedConfigPublication(t *testing.T) {
	const managementPassword = "test-management-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	queue := redisqueue.NewQueue()
	server := newTestServerWithOptions(t, WithUsageQueue(queue))
	queue.SetEnabled(true)
	clientConn, routeDone := openAuthenticatedRedisConnection(t, server, managementPassword)
	defer func() { _ = clientConn.Close() }()

	next, errClone := config.Clone(server.configSnapshot())
	if errClone != nil {
		t.Fatalf("clone config: %v", errClone)
	}
	next.RequestLog = true
	next.ImageArtifactCache.RetentionDays = 1
	publicationStarted := make(chan struct{})
	releasePublication := make(chan struct{})
	server.loggerToggle = func(bool) {
		close(publicationStarted)
		<-releasePublication
	}
	publicationDone := make(chan struct{})
	go func() {
		server.UpdateClients(next)
		close(publicationDone)
	}()
	<-publicationStarted

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	errStop := server.Stop(ctx)
	if !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want deadline exceeded", errStop)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("Stop() ignored context while config publication was blocked: %v", elapsed)
	}
	requireRedisConnectionClosed(t, clientConn, routeDone)
	if queue.Enabled() || server.managementRoutesEnabled.load() {
		t.Fatal("runtime state reopened during timed-out shutdown")
	}

	close(releasePublication)
	select {
	case <-publicationDone:
	case <-time.After(2 * time.Second):
		t.Fatal("config publication did not finish after release")
	}
	if queue.Enabled() || server.managementRoutesEnabled.load() {
		t.Fatal("completed config publication reopened closed runtime state")
	}
	if cleanerStop := reflect.ValueOf(server.imageArtifacts).Elem().FieldByName("cleanerStop"); !cleanerStop.IsNil() {
		t.Fatal("completed config publication restarted closed artifact cleaner")
	}
}
