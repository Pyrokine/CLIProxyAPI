package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func newAdmissionTestServer() *Server {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, auth.NewManager(nil, nil, nil))
	base.InferenceAdmission = executionregistry.NewAdmission()
	server := &Server{engine: gin.New(), handlers: base, wsRoutes: make(map[string]struct{})}
	server.engine.Use(server.inferenceAdmissionMiddleware())
	return server
}

func TestUpdateAdmissionPreservesSSEAndHealthUntilNaturalCompletion(t *testing.T) {
	server := newAdmissionTestServer()
	started := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	endStream := func() { finishOnce.Do(func() { close(finish) }) }
	server.engine.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = io.WriteString(c.Writer, "data: started\n\n")
		c.Writer.Flush()
		close(started)
		<-finish
		_, _ = io.WriteString(c.Writer, "data: completed\n\n")
	})
	server.engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	server.engine.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	httpServer := httptest.NewServer(server.engine)
	defer func() { endStream(); httpServer.Close() }()
	response, err := httpServer.Client().Post(httpServer.URL+"/v1/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	<-started
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: started\n" {
		t.Fatalf("initial SSE = %q, %v", line, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := server.handlers.InferenceAdmission.Drain(ctx, "attempt", time.Now().Add(time.Minute))
		result <- err
	}()
	paused := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		reply, err := httpServer.Client().Post(httpServer.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_ = reply.Body.Close()
		if reply.StatusCode == http.StatusServiceUnavailable {
			paused = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !paused {
		t.Fatal("new HTTP inference was not rejected")
	}
	select {
	case <-result:
		t.Fatal("SSE was treated as complete before the final write")
	default:
	}
	health, err := httpServer.Client().Get(httpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatal("health query stopped during drain")
	}
	endStream()
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), "data: completed") {
		t.Fatalf("SSE completion = %q, %v", body, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after SSE completion")
	}
	server.handlers.InferenceAdmission.Resume("attempt")
}

func TestUpdateAdmissionIdleResponsesSocketDoesNotBlockAndNewTurnIsRejected(t *testing.T) {
	server := newAdmissionTestServer()
	responses := openai.NewResponsesAPIHandler(server.handlers)
	server.engine.GET("/v1/responses", responses.ResponsesWebsocket)
	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	confirmation, err := server.handlers.InferenceAdmission.Drain(context.Background(), "attempt", time.Now().Add(time.Minute))
	if err != nil || !server.handlers.InferenceAdmission.Confirm(confirmation) {
		t.Fatalf("idle socket blocked drain: %v", err)
	}
	if err := connection.WriteJSON(map[string]any{
		"type": "response.create", "model": "fixture-model", "generate": true,
		"input": []map[string]string{{"role": "user", "content": "synthetic request"}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := connection.ReadMessage()
	if err != nil || !strings.Contains(string(data), executionregistry.ErrInferencePaused.Error()) || !strings.Contains(string(data), "503") {
		t.Fatalf("new WS turn was not rejected before dispatch: %s, %v", data, err)
	}
	if !server.handlers.InferenceAdmission.Confirm(confirmation) {
		t.Fatal("rejected turn changed active work")
	}
	server.handlers.InferenceAdmission.Resume("attempt")
}

func TestUpdateAdmissionCountsUploadBeforeBodyReadAndAllowsItToContinue(t *testing.T) {
	server := newAdmissionTestServer()
	entered := make(chan context.Context, 1)
	continueUpload := make(chan struct{})
	var uploadOnce sync.Once
	endUpload := func() { uploadOnce.Do(func() { close(continueUpload) }) }
	defer endUpload()
	server.engine.POST("/v1/images/edits", func(c *gin.Context) {
		entered <- c.Request.Context()
		<-continueUpload
		ctx, release, err := server.handlers.InferenceAdmission.Acquire(context.Background(), c.Request.Context())
		defer release()
		if err != nil || ctx == nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		server.engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader("synthetic-upload")))
		close(finished)
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drained := make(chan error, 1)
	go func() {
		_, err := server.handlers.InferenceAdmission.Drain(ctx, "attempt", time.Now().Add(time.Minute))
		drained <- err
	}()
	paused := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		_, release, err := server.handlers.InferenceAdmission.Acquire(context.Background(), nil)
		if errors.Is(err, executionregistry.ErrInferencePaused) {
			paused = true
			break
		}
		release()
		time.Sleep(time.Millisecond)
	}
	if !paused {
		t.Fatal("upload was not included in the pause boundary")
	}
	endUpload()
	<-finished
	if response.Code != http.StatusNoContent {
		t.Fatalf("accepted upload lost permission: %d", response.Code)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	server.handlers.InferenceAdmission.Resume("attempt")
}

func TestUpdateAdmissionIncludesDynamicWebsocketHandshakes(t *testing.T) {
	server := newAdmissionTestServer()
	calls := 0
	server.AttachWebsocketRoute("/custom-provider", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	_, err := server.handlers.InferenceAdmission.Drain(context.Background(), "attempt", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/custom-provider", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || calls != 0 {
		t.Fatalf("dynamic WS bypassed drain: status=%d calls=%d", response.Code, calls)
	}
	server.handlers.InferenceAdmission.Resume("attempt")
}
