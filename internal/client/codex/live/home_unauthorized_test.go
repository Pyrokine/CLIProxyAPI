package live

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func captureLiveFailureLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	hooks, level := logger.ReplaceHooks(make(log.LevelHooks)), logger.GetLevel()
	logger.SetLevel(log.WarnLevel)
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() { logger.ReplaceHooks(hooks); logger.SetLevel(level) })
	return hook
}

func assertLiveFailureLog(t *testing.T, hook *logtest.Hook, prefix, body string, count int) {
	t.Helper()
	found := 0
	for _, entry := range hook.AllEntries() {
		if !strings.HasPrefix(entry.Message, prefix) {
			continue
		}
		found++
		if entry.Message != prefix+logging.SafeDiagnosticForLog(body) || entry.Data["status"] != http.StatusUnauthorized {
			t.Fatalf("unexpected diagnostic: %q %v", entry.Message, entry.Data)
		}
		if strings.Contains(entry.Message, "synthetic-warning-secret") {
			t.Fatal("warning retained credential")
		}
	}
	if found != count {
		t.Fatalf("warning count=%d, want %d", found, count)
	}
}

type liveUnauthorizedUsage struct{ records chan coreusage.Record }

func (p liveUnauthorizedUsage) HandleUsage(_ context.Context, record coreusage.Record) {
	if p.records != nil && record.AuthID == "home-codex-live" && record.Fail.StatusCode == http.StatusUnauthorized {
		p.records <- record
	}
}

func captureLiveUnauthorizedUsage(t *testing.T) chan coreusage.Record {
	t.Helper()
	records := make(chan coreusage.Record, 8)
	const name = "live-unauthorized-test"
	coreusage.RegisterNamedPlugin(name, liveUnauthorizedUsage{records: records})
	t.Cleanup(func() { coreusage.RegisterNamedPlugin(name, liveUnauthorizedUsage{}) })
	return records
}

func assertLiveUnauthorizedUsage(t *testing.T, records <-chan coreusage.Record, body string) {
	t.Helper()
	select {
	case record := <-records:
		if record.Fail.Body != body || record.AuthIndex != "home-codex-live" || record.AccessTokenSHA256 == "" || coreusage.GenerateEnabled(record.Generate) || record.Detail.TotalTokens != 0 {
			t.Fatalf("unauthorized usage body=%q index=%q generate=%v", record.Fail.Body, record.AuthIndex, record.Generate)
		}
	case <-time.After(time.Second):
		t.Fatal("missing unauthorized usage")
	}
	select {
	case <-records:
		t.Fatal("duplicate unauthorized usage")
	case <-time.After(50 * time.Millisecond):
	}
}

type livePartialReader struct{ *strings.Reader }

func (r livePartialReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if r.Len() == 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestReadLimitedBodyPreservesPartialResponse(t *testing.T) {
	body, err := readLimitedBody(livePartialReader{strings.NewReader("partial")})
	if string(body) != "partial" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body=%q err=%v", body, err)
	}
	body, err = readLimitedBody(strings.NewReader(strings.Repeat("x", maxBodySize+1)))
	if len(body) != maxBodySize || !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("size=%d err=%v", len(body), err)
	}
}

func TestHandlerHomeUnauthorizedPreservesBodyAndUsage(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType string
		readError               bool
	}{
		{"json", "  {\"error\":\"synthetic expired\"}\n", "application/json", false},
		{"text", "synthetic expired\n", "text/plain", false},
		{"empty", "", "text/plain", false},
		{"read error", "synthetic partial error", "text/plain", true},
		{"credential", "access token expired\naccess_token=synthetic-warning-secret", "text/plain", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			hook := captureLiveFailureLogs(t)
			var reader io.Reader = strings.NewReader(test.body)
			if test.readError {
				reader = livePartialReader{strings.NewReader(test.body)}
			}
			responseBody := &trackedResponseBody{Reader: reader}
			executor := &captureExecutor{responseBody: responseBody, statusCode: http.StatusUnauthorized, headers: http.Header{"Content-Type": {test.contentType}}}
			manager := auth.NewManager(nil, nil, nil)
			manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
			dispatcher := &homeDispatcher{}
			registry := executionregistry.New()
			t.Cleanup(func() { _ = registry.Close() })
			manager.PublishHomeDispatch(dispatcher, registry, 1)
			manager.RegisterExecutor(executor)
			handler := NewHandler(manager, nil)
			t.Cleanup(func() { _ = handler.Close() })
			records := captureLiveUnauthorizedUsage(t)
			router := gin.New()
			router.POST("/v1/live", handler.Handle)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"model":"synthetic-live"}`))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, request)
			if test.readError {
				if response.Code != http.StatusBadGateway {
					t.Fatalf("read failure status=%d", response.Code)
				}
			} else if response.Code != http.StatusUnauthorized || response.Body.String() != test.body || response.Header().Get("Content-Type") != test.contentType {
				t.Fatalf("status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
			}
			if executor.calls.Load() != 1 || executor.refreshCalls.Load() != 0 || dispatcher.calls.Load() != 1 || responseBody.closes.Load() != 1 {
				t.Fatalf("request=%d refresh=%d dispatch=%d close=%d", executor.calls.Load(), executor.refreshCalls.Load(), dispatcher.calls.Load(), responseBody.closes.Load())
			}
			if executor.request.Context().Err() != context.Canceled {
				t.Fatal("Home request context was not released")
			}
			assertLiveUnauthorizedUsage(t, records, test.body)
			warningCount := 1
			if test.readError {
				warningCount = 0
			}
			assertLiveFailureLog(t, hook, "codex live upstream request failed: ", test.body, warningCount)
		})
	}
}

func TestHandleSidebandDialErrorPreservesUnauthorizedBody(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		partial    bool
	}{
		{"json", "  {\"error\":\"synthetic expired\"}\n", http.StatusUnauthorized, false},
		{"empty", "", http.StatusUnauthorized, false},
		{"partial", "synthetic partial\n", http.StatusUnauthorized, true},
		{"non unauthorized", "private control diagnostic", http.StatusBadGateway, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(test.body)
			if test.partial {
				reader = livePartialReader{strings.NewReader(test.body)}
			}
			body := &trackedResponseBody{Reader: reader}
			response := &http.Response{StatusCode: test.status, Body: body, Header: http.Header{"Content-Type": {"text/plain"}}}
			cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
			var returned []byte
			router := gin.New()
			router.GET("/handshake", func(c *gin.Context) {
				returned = handleSidebandDialError(c, util.WithGinContext(c.Request.Context(), c), cfg, response, errors.New("synthetic handshake failed"))
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/handshake", nil))
			if recorder.Code != test.status || body.closes.Load() != 1 {
				t.Fatalf("status=%d closes=%d", recorder.Code, body.closes.Load())
			}
			if test.status == http.StatusUnauthorized {
				if recorder.Body.String() != test.body || string(returned) != test.body || recorder.Header().Get("Content-Type") != "text/plain" {
					t.Fatalf("wire body=%q returned=%q", recorder.Body.String(), returned)
				}
			} else if strings.Contains(recorder.Body.String(), test.body) || returned != nil {
				t.Fatal("non-401 handshake body was exposed")
			}
		})
	}
}

func TestHandleSidebandHomeUnauthorizedDoesNotRefresh(t *testing.T) {
	hook := captureLiveFailureLogs(t)
	const failureBody = "  {\"error\":\"access token expired\",\"access_token\":\"synthetic-warning-secret\"}\n"
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, failureBody)
	}))
	defer upstream.Close()
	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	dispatcher := &homeDispatcher{}
	registry := executionregistry.New()
	t.Cleanup(func() { _ = registry.Close() })
	manager.PublishHomeDispatch(dispatcher, registry, 1)
	executor := &captureExecutor{responseBody: &trackedResponseBody{Reader: strings.NewReader("v=0\r\n")}}
	manager.RegisterExecutor(executor)
	handler := NewHandler(manager, nil)
	t.Cleanup(func() { _ = handler.Close() })
	handler.sidebandAPIBaseURL = websocketURL(upstream.URL, "/v1")
	router := gin.New()
	router.POST("/v1/live", handler.Handle)
	router.GET("/v1/live/:call_id", handler.HandleSideband)
	bootstrap := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"model":"synthetic-live"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(bootstrap, request)
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%q", bootstrap.Code, bootstrap.Body.String())
	}
	records := captureLiveUnauthorizedUsage(t)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial(websocketURL(server.URL, "/v1/live/call-123"), nil)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("unauthorized handshake opened websocket")
	}
	if err == nil || response == nil {
		t.Fatalf("handshake response=%v err=%v", response, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusUnauthorized || string(body) != failureBody || response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	if upstreamCalls.Load() != 1 || executor.refreshCalls.Load() != 0 || dispatcher.calls.Load() != 1 {
		t.Fatalf("handshake=%d refresh=%d dispatch=%d", upstreamCalls.Load(), executor.refreshCalls.Load(), dispatcher.calls.Load())
	}
	assertLiveUnauthorizedUsage(t, records, failureBody)
	assertLiveFailureLog(t, hook, "codex live sideband upstream handshake failed: ", failureBody, 1)
}
