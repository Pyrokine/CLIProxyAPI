package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestFileRequestLoggerHomeClientsAreIsolated(t *testing.T) {
	globalClient := &stubHomeRequestLogClient{heartbeatOK: true}
	previous := currentHomeRequestLogClient
	currentHomeRequestLogClient = func() homeRequestLogClient { return globalClient }
	t.Cleanup(func() { currentHomeRequestLogClient = previous })

	clientA := &stubHomeRequestLogClient{heartbeatOK: true}
	clientB := &stubHomeRequestLogClient{heartbeatOK: true}
	loggerA := NewFileRequestLogger(true, t.TempDir(), "", 0)
	loggerB := NewFileRequestLogger(true, t.TempDir(), "", 0)
	loggerNil := NewFileRequestLogger(true, t.TempDir(), "", 0)
	loggerA.homeClientProvider = func() homeRequestLogClient { return clientA }
	loggerB.homeClientProvider = func() homeRequestLogClient { return clientB }
	loggerNil.homeClientProvider = func() homeRequestLogClient { return nil }

	if errForward := loggerA.forwardRequestLogToHome(
		context.Background(), nil, "request-a", "log-a",
	); errForward != nil {
		t.Fatalf("service A forward error = %v", errForward)
	}
	if errForward := loggerB.forwardRequestLogToHome(
		context.Background(), nil, "request-b", "log-b",
	); errForward != nil {
		t.Fatalf("service B forward error = %v", errForward)
	}
	if errForward := loggerNil.forwardRequestLogToHome(
		context.Background(), nil, "request-nil", "log-nil",
	); errForward == nil {
		t.Fatal("explicit nil Home provider inherited compatibility client")
	}
	if len(clientA.pushed) != 1 || len(clientB.pushed) != 1 || len(globalClient.pushed) != 0 {
		t.Fatalf(
			"pushed counts = service A %d, service B %d, global %d",
			len(clientA.pushed),
			len(clientB.pushed),
			len(globalClient.pushed),
		)
	}
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
		[]byte(`{"input":"hello","api_key":"request-body-secret","max_tokens":256}`),
		http.StatusOK,
		map[string][]string{
			"Content-Type": {"application/json"},
			"Set-Cookie":   {"session=response-secret"},
		},
		[]byte(`{"ok":true,"refresh_token":"response-body-secret"}`),
		nil,
		[]byte("Authorization: Bearer upstream-header-secret\nBody:\n{\"access_token\":\"upstream-body-secret\",\"max_tokens\":128}"),
		[]byte(`{"access_token":"upstream-response-secret"}`),
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
		RequestID  string              `json:"request_id"`
		RequestLog string              `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.RequestID != "req-1" {
		t.Fatalf("request_id = %q, want req-1", got.RequestID)
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
	for _, secret := range []string{
		"request-body-secret",
		"response-body-secret",
		"upstream-header-secret",
		"upstream-body-secret",
		"upstream-response-secret",
	} {
		if bytes.Contains([]byte(got.RequestLog), []byte(secret)) {
			t.Fatalf("request_log contains raw secret %q: %q", secret, got.RequestLog)
		}
	}
	if !bytes.Contains([]byte(got.RequestLog), []byte("max_tokens")) {
		t.Fatalf("request_log removed non-secret max_tokens: %q", got.RequestLog)
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
		[]byte(`{"stream":true,"api_key":"stream-body-secret"}`),
		"req-stream",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}
	writer.WriteChunkAsync([]byte("data: {\"ok\":true}\n\n"))
	if errStatus := writer.WriteStatus(
		http.StatusOK, map[string][]string{"Content-Type": {"text/event-stream"}},
	); errStatus != nil {
		t.Fatalf("WriteStatus error: %v", errStatus)
	}
	if errAPI := writer.WriteAPIRequest([]byte("Authorization: Bearer stream-upstream-secret\nBody:\n{\"refresh_token\":\"stream-upstream-body-secret\"}")); errAPI != nil {
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
		RequestID  string              `json:"request_id"`
		RequestLog string              `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.RequestID != "req-stream" {
		t.Fatalf("request_id = %q, want req-stream", got.RequestID)
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
	for _, secret := range []string{
		"stream-body-secret",
		"stream-upstream-secret",
		"stream-upstream-body-secret",
	} {
		if bytes.Contains([]byte(got.RequestLog), []byte(secret)) {
			t.Fatalf("streaming request_log contains raw secret %q: %q", secret, got.RequestLog)
		}
	}
}

func TestFileStreamingLogWriterOmitsResponseAfterCaptureLimit(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	writer, errLog := logger.LogStreamingRequest(
		"/v1/responses", http.MethodPost, nil, []byte(`{"stream":true}`), "req-file-limit",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}
	part := bytes.Repeat([]byte("x"), maxStreamingResponseBodyBytes/2+1)
	writer.WriteChunkAsync(part)
	writer.WriteChunkAsync(part)
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error: %v", errClose)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("ReadDir error: %v", errRead)
	}
	var logContent []byte
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		logContent, errRead = os.ReadFile(filepath.Join(logsDir, entry.Name()))
		if errRead != nil {
			t.Fatalf("ReadFile error: %v", errRead)
		}
		break
	}
	if !bytes.Contains(logContent, truncatedInMemoryRequestLog) {
		t.Fatalf("streaming log missing capture marker: %q", logContent)
	}
	if bytes.Contains(logContent, bytes.Repeat([]byte("x"), 128)) {
		t.Fatal("streaming log retained response body after capture limit")
	}
}

func TestHomeStreamingLogWriterOmitsResponseAfterCaptureLimit(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()
	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	logger.SetHomeEnabled(true)
	writer, errLog := logger.LogStreamingRequest(
		"/v1/responses", http.MethodPost, nil, []byte(`{"stream":true}`), "req-home-limit",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}
	part := bytes.Repeat([]byte("x"), maxStreamingResponseBodyBytes/2+1)
	writer.WriteChunkAsync(part)
	writer.WriteChunkAsync(part)
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error: %v", errClose)
	}
	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}
	var got homeRequestLogPayload
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v", errUnmarshal)
	}
	if !strings.Contains(got.RequestLog, string(truncatedInMemoryRequestLog)) {
		t.Fatalf("home streaming log missing capture marker: %q", got.RequestLog)
	}
	if strings.Contains(got.RequestLog, strings.Repeat("x", 128)) {
		t.Fatal("home streaming log retained response body after capture limit")
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
	writer.WriteChunkAsync([]byte("data: {\"ok\":true}\n\n"))
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
		[]byte(`{"input":"hello","api_key":"forced-request-secret"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"error":"upstream failure","access_token":"forced-response-secret"}`),
		nil,
		[]byte("Authorization: Bearer forced-upstream-secret"),
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
	var logContent []byte
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, errFile := os.ReadFile(logsDir + string(os.PathSeparator) + entry.Name())
		if errFile != nil {
			t.Fatalf("failed to read request log file: %v", errFile)
		}
		logContent = content
		break
	}
	if len(logContent) == 0 {
		t.Fatal("expected local forced error log file when request-log disabled")
	}
	for _, secret := range []string{
		"forced-request-secret",
		"forced-response-secret",
		"forced-upstream-secret",
	} {
		if bytes.Contains(logContent, []byte(secret)) {
			t.Fatalf("forced error log contains raw secret %q: %q", secret, string(logContent))
		}
	}
}

func TestFileStreamingLogWriterConcurrentCloseWritesAPIWebsocketTimelineSource(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	writer, errLog := logger.LogStreamingRequest(
		"/v1/responses", http.MethodPost, map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"stream":true}`), "req-file-concurrent-close",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}
	source, errSource := logger.NewFileBodySource("api-websocket-timeline")
	if errSource != nil {
		t.Fatalf("NewFileBodySource error: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte(`{"type":"response.done","api_key":"source-secret","visible":"yes"}`)); errAppend != nil {
		t.Fatalf("AppendBytes error: %v", errAppend)
	}
	sourcePaths := source.Paths()
	sourceWriter, ok := writer.(interface {
		WriteAPIWebsocketTimelineSource(*FileBodySource) error
	})
	if !ok {
		t.Fatal("streaming writer does not support api websocket timeline sources")
	}
	if errWrite := sourceWriter.WriteAPIWebsocketTimelineSource(source); errWrite != nil {
		t.Fatalf("WriteAPIWebsocketTimelineSource error: %v", errWrite)
	}
	writer.WriteChunkAsync([]byte("data: {\"ok\":true}\n\n"))
	if errStatus := writer.WriteStatus(
		http.StatusOK, map[string][]string{"Content-Type": {"text/event-stream"}},
	); errStatus != nil {
		t.Fatalf("WriteStatus error: %v", errStatus)
	}

	const closeCount = 8
	start := make(chan struct{})
	errs := make(chan error, closeCount)
	var wg sync.WaitGroup
	wg.Add(closeCount + 1)
	go func() {
		defer wg.Done()
		<-start
		for range 200 {
			writer.WriteChunkAsync([]byte("data: concurrent\n\n"))
		}
	}()
	for range closeCount {
		go func() {
			defer wg.Done()
			<-start
			errs <- writer.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for errClose := range errs {
		if errClose != nil {
			t.Fatalf("Close error: %v", errClose)
		}
	}
	writer.WriteChunkAsync([]byte("data: after-close\n\n"))

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("ReadDir error: %v", errRead)
	}
	var logContent []byte
	logFiles := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		logFiles++
		content, errFile := os.ReadFile(logsDir + string(os.PathSeparator) + entry.Name())
		if errFile != nil {
			t.Fatalf("ReadFile error: %v", errFile)
		}
		logContent = content
	}
	if logFiles != 1 {
		t.Fatalf("log files = %d, want 1", logFiles)
	}
	if !bytes.Contains(logContent, []byte("=== API WEBSOCKET TIMELINE ===")) ||
		!bytes.Contains(logContent, []byte(`"visible": "yes"`)) {
		t.Fatalf("log missing api websocket timeline source: %s", logContent)
	}
	if bytes.Contains(logContent, []byte("source-secret")) {
		t.Fatalf("log contains raw source secret: %s", logContent)
	}
	for _, path := range sourcePaths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("source path %s still exists, stat err=%v", path, errStat)
		}
	}
}

func TestHomeStreamingLogWriterConcurrentCloseWritesAPIWebsocketTimelineSource(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()
	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	logger.SetHomeEnabled(true)
	writer, errLog := logger.LogStreamingRequest(
		"/v1/responses", http.MethodPost, map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"stream":true}`), "req-home-concurrent-close",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}
	source, errSource := logger.NewFileBodySource("api-websocket-timeline")
	if errSource != nil {
		t.Fatalf("NewFileBodySource error: %v", errSource)
	}
	if errAppend := source.AppendBytes([]byte(`{"type":"response.done","api_key":"home-source-secret","visible":"yes"}`)); errAppend != nil {
		t.Fatalf("AppendBytes error: %v", errAppend)
	}
	sourcePaths := source.Paths()
	sourceWriter, ok := writer.(interface {
		WriteAPIWebsocketTimelineSource(*FileBodySource) error
	})
	if !ok {
		t.Fatal("home streaming writer does not support api websocket timeline sources")
	}
	if errWrite := sourceWriter.WriteAPIWebsocketTimelineSource(source); errWrite != nil {
		t.Fatalf("WriteAPIWebsocketTimelineSource error: %v", errWrite)
	}
	writer.WriteChunkAsync([]byte("data: {\"ok\":true}\n\n"))

	const closeCount = 8
	start := make(chan struct{})
	errs := make(chan error, closeCount)
	var wg sync.WaitGroup
	wg.Add(closeCount + 1)
	go func() {
		defer wg.Done()
		<-start
		for range 200 {
			writer.WriteChunkAsync([]byte("data: concurrent\n\n"))
		}
	}()
	for range closeCount {
		go func() {
			defer wg.Done()
			<-start
			errs <- writer.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for errClose := range errs {
		if errClose != nil {
			t.Fatalf("Close error: %v", errClose)
		}
	}
	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}
	var got homeRequestLogPayload
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v", errUnmarshal)
	}
	if !strings.Contains(got.RequestLog, "=== API WEBSOCKET TIMELINE ===") ||
		!strings.Contains(got.RequestLog, `"visible": "yes"`) {
		t.Fatalf("request log missing api websocket timeline source: %s", got.RequestLog)
	}
	if strings.Contains(got.RequestLog, "home-source-secret") {
		t.Fatalf("request log contains raw source secret: %s", got.RequestLog)
	}
	for _, path := range sourcePaths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("source path %s still exists, stat err=%v", path, errStat)
		}
	}
}

func TestFileRequestLogger_AllSourcesWritesAndCleansFileBackedSections(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)

	apiRequestSource, errRequestSource := logger.NewFileBodySource("api-request")
	if errRequestSource != nil {
		t.Fatalf("NewFileBodySource api request: %v", errRequestSource)
	}
	if errAppend := apiRequestSource.AppendBytes([]byte("=== API REQUEST 1 ===\nBody:\n")); errAppend != nil {
		t.Fatalf("append api request header: %v", errAppend)
	}
	if errAppend := apiRequestSource.AppendBytes([]byte(`{"access_token":"source-request-secret"}`)); errAppend != nil {
		t.Fatalf("append api request body: %v", errAppend)
	}
	if errAppend := apiRequestSource.AppendBytes([]byte("\n\n")); errAppend != nil {
		t.Fatalf("append api request terminator: %v", errAppend)
	}

	apiResponseSource, errResponseSource := logger.NewFileBodySource("api-response")
	if errResponseSource != nil {
		t.Fatalf("NewFileBodySource api response: %v", errResponseSource)
	}
	if errAppend := apiResponseSource.AppendBytes([]byte("=== API RESPONSE 1 ===\nBody:\n")); errAppend != nil {
		t.Fatalf("append api response header: %v", errAppend)
	}
	if errAppend := apiResponseSource.AppendBytes([]byte(`{"refresh_token":"source-response-secret"}`)); errAppend != nil {
		t.Fatalf("append api response body: %v", errAppend)
	}

	requestSourcePaths := apiRequestSource.Paths()
	responseSourcePaths := apiResponseSource.Paths()
	errLog := logger.LogRequestWithOptionsAndAllSources(
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
		apiRequestSource,
		nil,
		apiResponseSource,
		nil,
		nil,
		nil,
		nil,
		false,
		"req-source",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndAllSources error: %v", errLog)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	var logContent []byte
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, errFile := os.ReadFile(logsDir + string(os.PathSeparator) + entry.Name())
		if errFile != nil {
			t.Fatalf("failed to read request log file: %v", errFile)
		}
		logContent = content
		break
	}
	if len(logContent) == 0 {
		t.Fatal("expected request log file")
	}
	if !bytes.Contains(logContent, []byte("=== API REQUEST 1 ===")) {
		t.Fatalf("log missing api request source section: %q", string(logContent))
	}
	if !bytes.Contains(logContent, []byte("=== API RESPONSE 1 ===")) {
		t.Fatalf("log missing api response source section: %q", string(logContent))
	}
	for _, secret := range []string{"source-request-secret", "source-response-secret"} {
		if bytes.Contains(logContent, []byte(secret)) {
			t.Fatalf("request log contains raw source secret %q: %q", secret, string(logContent))
		}
	}
	for _, path := range append(requestSourcePaths, responseSourcePaths...) {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("source path %s still exists after log write, stat err=%v", path, errStat)
		}
	}
}
