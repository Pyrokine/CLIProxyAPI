package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRedisProtocol_SUBSCRIBE_ErrorsReceivesErrorEvents(t *testing.T) {
	const managementPassword = "test-management-password"
	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	queue := redisqueue.NewQueue()
	t.Cleanup(queue.Close)
	server := newTestServerWithOptions(t, WithUsageQueue(queue))
	if !server.managementRoutesEnabled.load() {
		t.Fatal("expected managementRoutesEnabled to be true")
	}

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)
	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })
	reader := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if errWrite := writeTestRESPCommand(conn, "AUTH", managementPassword); errWrite != nil {
		t.Fatalf("failed to write AUTH command: %v", errWrite)
	}
	if msg, errRead := readTestRESPSimpleString(reader); errRead != nil {
		t.Fatalf("failed to read AUTH response: %v", errRead)
	} else if msg != "OK" {
		t.Fatalf("unexpected AUTH response: %q", msg)
	}
	queue.Enqueue([]byte("retained-usage"))
	if errWrite := writeTestRESPCommand(conn, "RPOP", "errors", "1"); errWrite != nil {
		t.Fatal(errWrite)
	}
	if response, errRead := readTestRESPError(reader); errRead != nil || response != "ERR unsupported queue" {
		t.Fatalf("RPOP errors response = %q, error = %v", response, errRead)
	}
	if retained := queue.PopOldest(1); len(retained) != 1 || string(retained[0]) != "retained-usage" {
		t.Fatalf("RPOP errors consumed usage records: %q", retained)
	}
	if errWrite := writeTestRESPCommand(conn, "SUBSCRIBE", "errors"); errWrite != nil {
		t.Fatalf("failed to write SUBSCRIBE command: %v", errWrite)
	}
	if errSubscribe := readTestRESPSubscribe(reader, "errors"); errSubscribe != nil {
		t.Fatalf("failed to read subscribe response: %v", errSubscribe)
	}

	queue.EnqueueError([]byte(`{"auth_index":"auth-1","status_code":401}`))
	message, errMessage := readRESPArrayOfBulkStrings(reader)
	if errMessage != nil {
		t.Fatalf("failed to read error message: %v", errMessage)
	}
	if len(message) != 3 || string(message[0]) != "message" || string(message[1]) != "errors" ||
		string(message[2]) != `{"auth_index":"auth-1","status_code":401}` {
		t.Fatalf("unexpected error message: %q", message)
	}

	manager := server.handlers.AuthManager
	registered, errRegister := manager.Register(
		auth.WithSkipPersist(t.Context()), &auth.Auth{
			ID: "redis-error-event", Provider: "codex", Status: auth.StatusActive,
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	manager.MarkResult(
		t.Context(), auth.Result{
			AuthID: registered.ID, AuthGeneration: registered.Generation, Provider: "codex", Model: "gpt-5",
			Error: &auth.Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":"quota"}`},
		},
	)
	message, errMessage = readRESPArrayOfBulkStrings(reader)
	if errMessage != nil || len(message) != 3 || string(message[0]) != "message" || string(message[1]) != "errors" {
		t.Fatalf("Manager error event message = %q, error = %v", message, errMessage)
	}
	var event struct {
		AuthID     string `json:"auth_id"`
		AuthIndex  string `json:"auth_index"`
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
		AuthStatus struct {
			Status      auth.Status `json:"status"`
			Unavailable bool        `json:"unavailable"`
		} `json:"auth_status"`
	}
	if errDecode := json.Unmarshal(message[2], &event); errDecode != nil {
		t.Fatal(errDecode)
	}
	if event.AuthID != registered.ID || event.AuthIndex != registered.Index ||
		event.StatusCode != http.StatusTooManyRequests || event.Body != `{"error":"quota"}` ||
		event.AuthStatus.Status != auth.StatusError || !event.AuthStatus.Unavailable {
		t.Fatalf("Manager state was not delivered through its instance queue: %+v", event)
	}
	if errWrite := writeTestRESPCommand(conn, "UNSUBSCRIBE", "errors"); errWrite != nil {
		t.Fatal(errWrite)
	}
	remaining, errRead := io.ReadAll(reader)
	if errRead != nil || string(remaining) != "*3\r\n$11\r\nunsubscribe\r\n$6\r\nerrors\r\n:0\r\n" {
		t.Fatalf("error unsubscribe response = %q, error = %v", remaining, errRead)
	}
}
