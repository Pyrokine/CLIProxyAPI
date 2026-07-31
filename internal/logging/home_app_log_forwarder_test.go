package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

type stubHomeAppLogClient struct {
	mu          sync.Mutex
	heartbeatOK bool
	err         error
	pushed      [][]byte
}

func (c *stubHomeAppLogClient) HeartbeatOK() bool { return c.heartbeatOK }

func (c *stubHomeAppLogClient) RPushAppLog(_ context.Context, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.pushed = append(c.pushed, bytes.Clone(payload))
	return nil
}

func (c *stubHomeAppLogClient) pushedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pushed)
}

func (c *stubHomeAppLogClient) pushedAt(index int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if index < 0 || index >= len(c.pushed) {
		return nil
	}
	return bytes.Clone(c.pushed[index])
}

func TestHomeAppLogForwarderPausesGlobalLogsWithMultipleHomeServices(t *testing.T) {
	clientA := &stubHomeAppLogClient{heartbeatOK: true}
	clientB := &stubHomeAppLogClient{heartbeatOK: true}
	forwarderA := startHomeAppLogForwarder(4, clientA)
	forwarderB := startHomeAppLogForwarder(4, clientB)
	t.Cleanup(forwarderA.Stop)
	t.Cleanup(forwarderB.Stop)

	log.WithField("request_id", "multi-home").Info("must not cross Home services")
	time.Sleep(50 * time.Millisecond)
	if clientA.pushedCount() != 0 || clientB.pushedCount() != 0 {
		t.Fatalf(
			"multi-service pushes = service A %d, service B %d",
			clientA.pushedCount(),
			clientB.pushedCount(),
		)
	}

	forwarderB.Stop()
	log.WithField("request_id", "single-home").Info("single Home service log")
	deadline := time.Now().Add(time.Second)
	for clientA.pushedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if clientA.pushedCount() != 1 || clientB.pushedCount() != 0 {
		t.Fatalf(
			"single-service pushes = service A %d, service B %d",
			clientA.pushedCount(),
			clientB.pushedCount(),
		)
	}
}

func TestHomeAppLogScopeCoversPeerStartupAndShutdownWindows(t *testing.T) {
	clientA := &stubHomeAppLogClient{heartbeatOK: true}
	clientB := &stubHomeAppLogClient{heartbeatOK: true}
	releaseScopeA := BeginHomeAppLogScope()
	t.Cleanup(releaseScopeA)
	forwarderA := startHomeAppLogForwarder(8, clientA)
	t.Cleanup(forwarderA.Stop)

	releaseScopeB := BeginHomeAppLogScope()
	log.Info("peer Home service is starting")
	time.Sleep(30 * time.Millisecond)
	if clientA.pushedCount() != 0 {
		t.Fatalf("startup-window pushes to service A = %d, want 0", clientA.pushedCount())
	}

	forwarderB := startHomeAppLogForwarder(8, clientB)
	forwarderB.Stop()
	log.Info("peer Home service is shutting down")
	time.Sleep(30 * time.Millisecond)
	if clientA.pushedCount() != 0 || clientB.pushedCount() != 0 {
		t.Fatalf(
			"shutdown-window pushes = service A %d, service B %d",
			clientA.pushedCount(),
			clientB.pushedCount(),
		)
	}

	releaseScopeB()
	log.Info("single Home service resumed")
	deadline := time.Now().Add(time.Second)
	for clientA.pushedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if clientA.pushedCount() != 1 {
		t.Fatalf("single-service pushes after peer shutdown = %d, want 1", clientA.pushedCount())
	}
}

func TestHomeAppLogForwarderClientsAreIsolated(t *testing.T) {
	globalClient := &stubHomeAppLogClient{heartbeatOK: true}
	previous := currentHomeAppLogClient
	currentHomeAppLogClient = func() homeAppLogClient { return globalClient }
	t.Cleanup(func() { currentHomeAppLogClient = previous })

	clientA := &stubHomeAppLogClient{heartbeatOK: true}
	clientB := &stubHomeAppLogClient{heartbeatOK: true}
	forwarderA := &HomeAppLogForwarder{client: clientA}
	forwarderB := &HomeAppLogForwarder{client: clientB}
	forwarderNil := &HomeAppLogForwarder{}
	forwarderNil.client = nil
	forwarderA.enabled.Store(true)
	forwarderB.enabled.Store(true)
	forwarderNil.enabled.Store(false)

	forwarderA.forward(homeAppLogPayload{Line: "service-a"})
	forwarderB.forward(homeAppLogPayload{Line: "service-b"})
	if clientA.pushedCount() != 1 || clientB.pushedCount() != 1 || globalClient.pushedCount() != 0 {
		t.Fatalf(
			"pushed counts = service A %d, service B %d, global %d",
			clientA.pushedCount(),
			clientB.pushedCount(),
			globalClient.pushedCount(),
		)
	}
}

func TestHomeAppLogForwarderForwardsFormattedLogWhenHomeHealthy(t *testing.T) {
	original := currentHomeAppLogClient
	defer func() {
		currentHomeAppLogClient = original
	}()

	stub := &stubHomeAppLogClient{heartbeatOK: true}
	currentHomeAppLogClient = func() homeAppLogClient {
		return stub
	}

	forwarder := &HomeAppLogForwarder{
		formatter: &LogFormatter{},
		queue:     make(chan homeAppLogPayload, 4),
		stop:      make(chan struct{}),
	}
	forwarder.enabled.Store(true)
	forwarder.wg.Add(1)
	go forwarder.run()
	defer forwarder.Stop()

	entry := log.NewEntry(log.StandardLogger())
	entry.Time = time.Date(2026, 5, 29, 8, 0, 0, 0, time.Local)
	entry.Level = log.DebugLevel
	entry.Message = "debug details"
	entry.Data["request_id"] = "req-app-1"

	if errFire := forwarder.Fire(entry); errFire != nil {
		t.Fatalf("Fire error: %v", errFire)
	}

	deadline := time.Now().Add(time.Second)
	for stub.pushedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.pushedCount() != 1 {
		t.Fatalf("pushed records = %d, want 1", stub.pushedCount())
	}

	var got homeAppLogPayload
	if errUnmarshal := json.Unmarshal(stub.pushedAt(0), &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v", errUnmarshal)
	}
	if got.Level != "debug" {
		t.Fatalf("level = %q, want debug", got.Level)
	}
	if got.RequestID != "req-app-1" {
		t.Fatalf("request_id = %q, want req-app-1", got.RequestID)
	}
	if !strings.Contains(got.Line, "debug details") {
		t.Fatalf("line %q missing log message", got.Line)
	}
	if !strings.Contains(got.Line, "[req-app-1]") {
		t.Fatalf("line %q missing matching request id", got.Line)
	}
	if strings.TrimSpace(got.Timestamp) == "" {
		t.Fatal("timestamp empty, want non-empty")
	}
}

func TestHomeAppLogForwarderOmitsPlaceholderRequestID(t *testing.T) {
	entry := log.NewEntry(log.StandardLogger())
	entry.Data["request_id"] = "--------"

	if got := appLogRequestID(entry); got != "" {
		t.Fatalf("request id = %q, want empty for placeholder", got)
	}
}

func TestHomeAppLogForwarderSkipsWhenHomeHeartbeatIsDown(t *testing.T) {
	original := currentHomeAppLogClient
	defer func() {
		currentHomeAppLogClient = original
	}()

	stub := &stubHomeAppLogClient{heartbeatOK: false}
	currentHomeAppLogClient = func() homeAppLogClient {
		return stub
	}

	forwarder := &HomeAppLogForwarder{
		formatter: &LogFormatter{},
		queue:     make(chan homeAppLogPayload, 4),
		stop:      make(chan struct{}),
	}
	forwarder.enabled.Store(true)

	entry := log.NewEntry(log.StandardLogger())
	entry.Time = time.Now()
	entry.Level = log.InfoLevel
	entry.Message = "should stay local"

	if errFire := forwarder.Fire(entry); errFire != nil {
		t.Fatalf("Fire error: %v", errFire)
	}
	if stub.pushedCount() != 0 {
		t.Fatalf("pushed records = %d, want 0", stub.pushedCount())
	}
}

func TestHomeAppLogForwarderDisablesForwardingWhenHomeDoesNotSupportAppLog(t *testing.T) {
	original := currentHomeAppLogClient
	defer func() {
		currentHomeAppLogClient = original
	}()

	stub := &stubHomeAppLogClient{
		heartbeatOK: true,
		err:         errors.New("ERR unsupported key"),
	}
	currentHomeAppLogClient = func() homeAppLogClient {
		return stub
	}

	forwarder := &HomeAppLogForwarder{
		formatter: &LogFormatter{},
		queue:     make(chan homeAppLogPayload, 4),
		stop:      make(chan struct{}),
	}
	forwarder.enabled.Store(true)

	forwarder.forward(homeAppLogPayload{Line: "legacy home cannot receive app logs"})
	if forwarder.enabled.Load() {
		t.Fatal("forwarder still enabled, want disabled after unsupported app-log response")
	}
}
