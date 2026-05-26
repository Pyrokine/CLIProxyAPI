package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

type stubHomeRequestLogClient struct {
	heartbeatOK bool
	pushed      [][]byte
}

func (c *stubHomeRequestLogClient) HeartbeatOK() bool { return c.heartbeatOK }

func (c *stubHomeRequestLogClient) RPushRequestLog(_ context.Context, payload []byte) error {
	c.pushed = append(c.pushed, bytes.Clone(payload))
	return nil
}

func TestFileRequestLogger_HomeEnabled_ForwardsWhenRequestLogEnabled(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	requestHeaders := map[string][]string{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer secret"},
		"Cookie":        {"session=secret-cookie"},
	}

	errLog := logger.LogRequest(
		"/v1/chat/completions",
		http.MethodPost,
		requestHeaders,
		[]byte(`{"input":"hello"}`),
		http.StatusOK,
		map[string][]string{
			"Content-Type": {"application/json"},
			"Set-Cookie":   {"session=response-secret"},
		},
		[]byte(`{"ok":true}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		"req-1",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequest error: %v", errLog)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no local request log files, got entries: %+v", entries)
	}

	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		Headers    map[string][]string `json:"headers"`
		RequestLog string              `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.Headers == nil || got.Headers["Content-Type"][0] != "application/json" {
		t.Fatalf("headers.content-type = %+v, want application/json", got.Headers["Content-Type"])
	}
	if got.Headers == nil || got.Headers["Authorization"][0] == "Bearer secret" {
		t.Fatalf("headers.authorization = %+v, want masked value", got.Headers["Authorization"])
	}
	if got.Headers == nil || got.Headers["Cookie"][0] == "session=secret-cookie" {
		t.Fatalf("headers.cookie = %+v, want masked value", got.Headers["Cookie"])
	}
	if got.RequestLog == "" {
		t.Fatal("request_log empty, want non-empty")
	}
	if bytes.Contains([]byte(got.RequestLog), []byte("session=secret-cookie")) {
		t.Fatalf("request_log contains raw cookie: %q", got.RequestLog)
	}
	if bytes.Contains([]byte(got.RequestLog), []byte("session=response-secret")) {
		t.Fatalf("request_log contains raw set-cookie: %q", got.RequestLog)
	}
}

func TestFileRequestLogger_HomeEnabled_ForwardsStreamingRequest(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	writer, errLog := logger.LogStreamingRequest(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer stream-secret"},
		},
		[]byte(`{"stream":true}`),
		"req-stream",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}
	writer.WriteChunkAsync([]byte(`data: {"ok":true}\n\n`))
	if errStatus := writer.WriteStatus(
		http.StatusOK, map[string][]string{"Content-Type": {"text/event-stream"}},
	); errStatus != nil {
		t.Fatalf("WriteStatus error: %v", errStatus)
	}
	if errAPI := writer.WriteAPIRequest([]byte("upstream request")); errAPI != nil {
		t.Fatalf("WriteAPIRequest error: %v", errAPI)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error: %v", errClose)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no local streaming log files, got entries: %+v", entries)
	}
	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		Headers    map[string][]string `json:"headers"`
		RequestLog string              `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.Headers == nil || got.Headers["Content-Type"][0] != "application/json" {
		t.Fatalf("headers.content-type = %+v, want application/json", got.Headers["Content-Type"])
	}
	if got.Headers == nil || got.Headers["Authorization"][0] == "Bearer stream-secret" {
		t.Fatalf("headers.authorization = %+v, want masked value", got.Headers["Authorization"])
	}
	if !bytes.Contains([]byte(got.RequestLog), []byte("data: {\"ok\":true}")) {
		t.Fatalf("request_log = %q, want streaming chunk", got.RequestLog)
	}
}

func TestFileRequestLogger_HomeEnabled_ReturnsErrorWhenHomeUnavailable(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	currentHomeRequestLogClient = func() homeRequestLogClient {
		return nil
	}

	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	logger.SetHomeEnabled(true)

	errLog := logger.LogRequest(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusOK,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"ok":true}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		"req-unavailable",
		time.Now(),
		time.Now(),
	)
	if errLog == nil {
		t.Fatal("expected unavailable Home request log client to return an error")
	}
}

func TestFileRequestLogger_HomeEnabled_ReturnsStreamingCloseErrorWhenHeartbeatUnhealthy(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	currentHomeRequestLogClient = func() homeRequestLogClient {
		return &stubHomeRequestLogClient{heartbeatOK: false}
	}

	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	logger.SetHomeEnabled(true)

	writer, errLog := logger.LogStreamingRequest(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"stream":true}`),
		"req-stream-unhealthy",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}
	writer.WriteChunkAsync([]byte(`data: {"ok":true}\n\n`))
	if errClose := writer.Close(); errClose == nil {
		t.Fatal("expected unhealthy Home heartbeat to return a streaming close error")
	}
}

func TestFileRequestLogger_HomeEnabled_DoesNotForwardForcedErrorLogsWhenRequestLogDisabled(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	errLog := logger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"req-2",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions error: %v", errLog)
	}

	if len(stub.pushed) != 0 {
		t.Fatalf("home pushed records = %d, want 0", len(stub.pushed))
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Name() != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected local forced error log file when request-log disabled")
	}
}
