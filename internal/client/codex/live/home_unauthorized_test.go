package live

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		if entry.Message != prefix+logging.SafeDiagnosticForLog(body) ||
			entry.Data["status"] != http.StatusUnauthorized {
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
	if p.records != nil && record.AuthID == "home-codex-live" &&
		record.Fail.StatusCode == http.StatusUnauthorized {
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
		if record.Fail.Body != body || record.AuthIndex != "home-codex-live" ||
			record.AccessTokenSHA256 == "" ||
			coreusage.GenerateEnabled(record.Generate) ||
			record.Detail.TotalTokens != 0 {
			t.Fatalf(
				"unauthorized usage body=%q index=%q generate=%v",
				record.Fail.Body,
				record.AuthIndex,
				record.Generate,
			)
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

// noinspection GoDirectComparisonOfErrors
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
		t.Run(
			test.name, func(t *testing.T) {
				hook := captureLiveFailureLogs(t)
				var reader io.Reader = strings.NewReader(test.body)
				if test.readError {
					reader = livePartialReader{strings.NewReader(test.body)}
				}
				responseBody := &trackedResponseBody{Reader: reader}
				executor := &captureExecutor{
					responseBody: responseBody, statusCode: http.StatusUnauthorized,
					headers: http.Header{"Content-Type": {test.contentType}},
				}
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
				request := httptest.NewRequest(
					http.MethodPost, "/v1/live", strings.NewReader(`{"model":"synthetic-live"}`),
				)
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(response, request)
				if test.readError {
					if response.Code != http.StatusBadGateway {
						t.Fatalf("read failure status=%d", response.Code)
					}
				} else if response.Code !=
					http.StatusUnauthorized ||
					response.Body.String() !=
						test.body ||
					response.Header().Get("Content-Type") !=
						test.contentType {
					t.Fatalf("status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
				}
				if executor.calls.Load() != 1 || executor.refreshCalls.Load() != 0 ||
					dispatcher.calls.Load() != 1 ||
					responseBody.closes.Load() != 1 {
					t.Fatalf(
						"request=%d refresh=%d dispatch=%d close=%d", executor.calls.Load(),
						executor.refreshCalls.Load(), dispatcher.calls.Load(), responseBody.closes.Load(),
					)
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
			},
		)
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
		t.Run(
			test.name, func(t *testing.T) {
				var reader io.Reader = strings.NewReader(test.body)
				if test.partial {
					reader = livePartialReader{strings.NewReader(test.body)}
				}
				body := &trackedResponseBody{Reader: reader}
				response := &http.Response{
					StatusCode: test.status, Body: body, Header: http.Header{"Content-Type": {"text/plain"}},
				}
				cfg := &config.Config{RequestLog: true}
				var returned []byte
				router := gin.New()
				router.GET(
					"/handshake", func(c *gin.Context) {
						returned = handleSidebandDialError(
							c, util.WithGinContext(c.Request.Context(), c), cfg, response,
							errors.New("synthetic handshake failed"),
						)
					},
				)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/handshake", nil))
				if recorder.Code != test.status || body.closes.Load() != 1 {
					t.Fatalf("status=%d closes=%d", recorder.Code, body.closes.Load())
				}
				if test.status == http.StatusUnauthorized {
					if recorder.Body.String() != test.body || string(returned) != test.body ||
						recorder.Header().Get("Content-Type") != "text/plain" {
						t.Fatalf("wire body=%q returned=%q", recorder.Body.String(), returned)
					}
				} else if strings.Contains(recorder.Body.String(), test.body) || returned != nil {
					t.Fatal("non-401 handshake body was exposed")
				}
			},
		)
	}
}

func TestHandleSidebandDialErrorCopiesSafeHeaders(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(
			http.StatusText(status), func(t *testing.T) {
				const upstreamBody = "private synthetic handshake diagnostic\n"
				body := &trackedResponseBody{Reader: strings.NewReader(upstreamBody)}
				headers := make(http.Header)
				for name, value := range map[string]string{
					"Content-Type": "text/plain", "Retry-After": "17",
					"X-Request-Id": "synthetic-request", "OpenAI-Request-Id": "synthetic-openai",
					"Authorization": "Bearer synthetic-private", "Set-Cookie": "synthetic-private=1",
					"X-Upstream-Private": "synthetic-private", logging.CPATraceIDHeader: "synthetic-upstream-trace",
				} {
					headers.Set(name, value)
				}
				response := &http.Response{StatusCode: status, Body: body, Header: headers}
				var returned []byte
				router := gin.New()
				router.GET(
					"/handshake", func(c *gin.Context) {
						c.Header(logging.CPATraceIDHeader, "synthetic-local-trace")
						returned = handleSidebandDialError(
							c, c.Request.Context(), nil, response, errors.New("synthetic rejected handshake"),
						)
					},
				)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/handshake", nil))
				if recorder.Code != status || body.closes.Load() != 1 {
					t.Fatalf("status=%d closes=%d", recorder.Code, body.closes.Load())
				}
				for _, name := range []string{"Retry-After", "X-Request-Id", "OpenAI-Request-Id"} {
					if recorder.Header().Get(name) != headers.Get(name) {
						t.Errorf(
							"safe header %s=%q, want %q",
							name,
							recorder.Header().Get(name),
							headers.Get(name),
						)
					}
				}
				for _, name := range []string{"Authorization", "Set-Cookie", "X-Upstream-Private"} {
					if len(recorder.Header().Values(name)) != 0 {
						t.Errorf("private header %s was exposed", name)
					}
				}
				if recorder.Header().Get(logging.CPATraceIDHeader) != "synthetic-local-trace" {
					t.Fatalf("CPA trace overwritten: %q", recorder.Header().Get(logging.CPATraceIDHeader))
				}
				if status == http.StatusUnauthorized {
					if recorder.Body.String() != upstreamBody || string(returned) != upstreamBody ||
						recorder.Header().Get("Content-Type") != "text/plain" {
						t.Fatalf(
							"unauthorized body=%q returned=%q headers=%v", recorder.Body.String(), returned,
							recorder.Header(),
						)
					}
				} else if strings.Contains(
					recorder.Body.String(), upstreamBody,
				) || returned != nil || !strings.Contains(
					recorder.Body.String(), "Codex live sideband upstream unavailable",
				) {
					t.Fatalf("non-401 safe response changed: body=%q returned=%q", recorder.Body.String(), returned)
				}
			},
		)
	}
}

func TestHomeHangupUnauthorizedKeepsBodyAndReadFailure(t *testing.T) {
	const upstreamBody = `{"error":{"message":"access token expired"}}`
	for _, partial := range []bool{false, true} {
		t.Run(
			strconv.FormatBool(partial), func(t *testing.T) {
				cfg := &config.Config{
					Home: config.HomeConfig{Enabled: true}, RequestLog: true,
				}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				dispatcher, registry := &homeDispatcher{}, executionregistry.New()
				t.Cleanup(func() { _ = registry.Close() })
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				var reader io.Reader = strings.NewReader(upstreamBody)
				if partial {
					reader = livePartialReader{strings.NewReader(upstreamBody)}
				}
				body := &trackedResponseBody{Reader: reader}
				executor := &captureExecutor{statusCode: http.StatusUnauthorized, responseBody: body}
				manager.RegisterExecutor(executor)
				handler := NewHandler(manager, func() *config.Config { return cfg })
				t.Cleanup(func() { _ = handler.Close() })
				handler.sessions.put(
					"call-123",
					liveSession{authID: "home-codex-live", model: defaultLiveModel},
				)
				records := captureLiveUnauthorizedUsage(t)
				router := gin.New()
				var apiResponse []byte
				router.Use(
					func(c *gin.Context) {
						c.Next()
						raw, _ := c.Get("API_RESPONSE")
						if buffer, ok := raw.(*logging.BoundedLogBuffer); ok {
							apiResponse = []byte(buffer.String())
						}
					},
				)
				router.POST("/v1/realtime/calls/:call_id/hangup", handler.HandleHangup)
				response := httptest.NewRecorder()
				router.ServeHTTP(
					response, httptest.NewRequest(http.MethodPost, "/v1/realtime/calls/call-123/hangup", nil),
				)
				wantStatus := http.StatusUnauthorized
				if partial {
					wantStatus = http.StatusBadGateway
				}
				if response.Code != wantStatus || (!partial && response.Body.String() != upstreamBody) {
					t.Fatalf("hangup status=%d body=%q", response.Code, response.Body.String())
				}
				if partial && (!strings.Contains(
					string(apiResponse), upstreamBody,
				) || !strings.Contains(string(apiResponse), io.ErrUnexpectedEOF.Error())) {
					t.Fatalf("hangup read failure lost original log evidence: %q", apiResponse)
				}
				if executor.calls.Load() != 1 || executor.refreshCalls.Load() != 0 ||
					dispatcher.calls.Load() != 1 ||
					body.closes.Load() != 1 ||
					executor.request.Header.Get("Authorization") != "Bearer home-live-token" {
					t.Fatal("hangup changed Home request identity, retries or response release")
				}
				assertLiveUnauthorizedUsage(t, records, upstreamBody)
			},
		)
	}
}

func TestHandlerReportsUnauthorizedBeforeEarlyReturn(t *testing.T) {
	const upstreamBody = `{"error":{"message":"access token expired"}}`
	for _, bindFailure := range []bool{false, true} {
		t.Run(
			strconv.FormatBool(bindFailure), func(t *testing.T) {
				cfg := &config.Config{
					Home: config.HomeConfig{Enabled: true}, RequestLog: true,
				}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				registry := executionregistry.New()
				t.Cleanup(func() { _ = registry.Close() })
				manager.PublishHomeDispatch(&homeDispatcher{}, registry, 1)
				var reader io.Reader = livePartialReader{strings.NewReader(upstreamBody)}
				if bindFailure {
					reader = strings.NewReader(upstreamBody)
				}
				body := &trackedResponseBody{Reader: reader}
				executor := &captureExecutor{statusCode: http.StatusUnauthorized, responseBody: body}
				if bindFailure {
					executor.beforeReturn = func() { _ = registry.Close() }
				}
				manager.RegisterExecutor(executor)
				handler := NewHandler(manager, func() *config.Config { return cfg })
				t.Cleanup(func() { _ = handler.Close() })
				records := captureLiveUnauthorizedUsage(t)
				router := gin.New()
				var apiResponse []byte
				router.Use(
					func(c *gin.Context) {
						c.Next()
						raw, _ := c.Get("API_RESPONSE")
						if buffer, ok := raw.(*logging.BoundedLogBuffer); ok {
							apiResponse = []byte(buffer.String())
						}
					},
				)
				router.POST("/v1/live", handler.Handle)
				request := httptest.NewRequest(
					http.MethodPost, "/v1/live", strings.NewReader(`{"model":"gpt-live-1-codex","sdp":"v=0"}`),
				)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				wantStatus, wantBody := http.StatusBadGateway, upstreamBody
				if bindFailure {
					wantStatus, wantBody = http.StatusServiceUnavailable, "upstream unauthorized"
				}
				if response.Code != wantStatus || executor.calls.Load() != 1 ||
					executor.refreshCalls.Load() != 0 ||
					body.closes.Load() != 1 {
					t.Fatalf(
						"early return status=%d calls=%d refresh=%d closes=%d",
						response.Code,
						executor.calls.Load(),
						executor.refreshCalls.Load(),
						body.closes.Load(),
					)
				}
				if !bindFailure && (!strings.Contains(
					string(apiResponse), upstreamBody,
				) || !strings.Contains(string(apiResponse), io.ErrUnexpectedEOF.Error())) {
					t.Fatalf("early return lost original response/read failure: %q", apiResponse)
				}
				assertLiveUnauthorizedUsage(t, records, wantBody)
			},
		)
	}
}

func TestHandleDirectWebsocketForwardsUnauthorizedHomeHandshakeWithoutRefresh(t *testing.T) {
	for _, sample := range []struct {
		name, body string
		partial    bool
	}{
		{"response body", `{"error":{"message":"access token expired"}}`, false},
		{"response body with read error", `{"error":{"message":"access token expired"}}`, true},
		{"empty response body", "", false},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				authorization := make(chan string, 1)
				upstream := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							authorization <- r.Header.Get("Authorization")
							w.Header().Set("Content-Type", "application/json")
							if sample.partial {
								w.Header().Set("Content-Length", strconv.Itoa(len(sample.body)+1))
							}
							w.WriteHeader(http.StatusUnauthorized)
							_, _ = io.WriteString(w, sample.body)
						},
					),
				)
				defer upstream.Close()
				cfg := &config.Config{
					Home: config.HomeConfig{Enabled: true}, RequestLog: true,
				}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				dispatcher, registry := &homeDispatcher{}, executionregistry.New()
				t.Cleanup(func() { _ = registry.Close() })
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				executor := &captureExecutor{}
				manager.RegisterExecutor(executor)
				handler := NewHandler(manager, func() *config.Config { return cfg })
				t.Cleanup(func() { _ = handler.Close() })
				handler.sidebandAPIBaseURL = websocketURL(upstream.URL, "/v1")
				records := captureLiveUnauthorizedUsage(t)
				timeline := make(chan []byte, 1)
				router := gin.New()
				router.Use(
					func(c *gin.Context) {
						c.Next()
						raw, _ := c.Get("API_WEBSOCKET_TIMELINE")
						var value []byte
						if buffer, ok := raw.(*logging.BoundedLogBuffer); ok {
							value = []byte(buffer.String())
						}
						timeline <- value
					},
				)
				router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
				downstream := httptest.NewServer(router)
				defer downstream.Close()
				connection, response, err := websocket.DefaultDialer.Dial(
					websocketURL(
						downstream.URL, "/v1/realtime?model=gpt-realtime",
					), nil,
				)
				if connection != nil {
					_ = connection.Close()
				}
				if err == nil || response == nil {
					t.Fatalf("401 handshake response=%v error=%v", response, err)
				}
				defer func() { _ = response.Body.Close() }()
				body, readErr := io.ReadAll(response.Body)
				if readErr != nil || response.StatusCode != 401 || string(body) != sample.body ||
					executor.refreshCalls.Load() != 0 ||
					dispatcher.calls.Load() != 1 {
					t.Fatalf("401 handshake status=%d body=%q error=%v", response.StatusCode, body, readErr)
				}
				select {
				case value := <-authorization:
					if value != "Bearer home-live-token" {
						t.Fatalf("handshake changed original token: %q", value)
					}
				case <-time.After(time.Second):
					t.Fatal("missing upstream handshake")
				}
				if sample.partial {
					select {
					case raw := <-timeline:
						if !strings.Contains(string(raw), sample.body) {
							t.Fatalf("websocket log lost original body: %q", raw)
						}
					case <-time.After(time.Second):
						t.Fatal("missing websocket rejection timeline")
					}
				}
				assertLiveUnauthorizedUsage(t, records, sample.body)
			},
		)
	}
}

func TestHandleSidebandHomeUnauthorizedDoesNotRefresh(t *testing.T) {
	hook := captureLiveFailureLogs(t)
	const failureBody = "  {\"error\":\"access token expired\",\"access_token\":\"synthetic-warning-secret\"}\n"
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, failureBody)
			},
		),
	)
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
	if err != nil || response.StatusCode != http.StatusUnauthorized || string(body) != failureBody ||
		response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	if upstreamCalls.Load() != 1 || executor.refreshCalls.Load() != 0 || dispatcher.calls.Load() != 1 {
		t.Fatalf(
			"handshake=%d refresh=%d dispatch=%d", upstreamCalls.Load(), executor.refreshCalls.Load(),
			dispatcher.calls.Load(),
		)
	}
	assertLiveUnauthorizedUsage(t, records, failureBody)
	assertLiveFailureLog(t, hook, "codex live sideband upstream handshake failed: ", failureBody, 1)
}
