package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

type apiKeyFirstSelector struct{}

func (*apiKeyFirstSelector) Pick(
	_ context.Context,
	_ string,
	_ string,
	_ coreexecutor.Options,
	auths []*auth.Auth,
) (*auth.Auth, error) {
	for _, candidate := range auths {
		if candidate.AuthKind() == auth.AuthKindAPIKey {
			return candidate, nil
		}
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[0], nil
}

type captureExecutor struct {
	calls        atomic.Int32
	refreshCalls atomic.Int32
	request      *http.Request
	body         []byte
	selectedAuth *auth.Auth
	responseBody io.ReadCloser
	statusCode   int
	headers      http.Header
}

func (*captureExecutor) Identifier() string { return "codex" }

func (*captureExecutor) Execute(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (*captureExecutor) ExecuteStream(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *captureExecutor) Refresh(_ context.Context, credential *auth.Auth) (*auth.Auth, error) {
	e.refreshCalls.Add(1)
	return credential, nil
}

func (*captureExecutor) CountTokens(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (*captureExecutor) PrepareRequest(req *http.Request, credential *auth.Auth) error {
	token, _ := credential.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (e *captureExecutor) HttpRequest(
	_ context.Context,
	credential *auth.Auth,
	req *http.Request,
) (*http.Response, error) {
	e.calls.Add(1)
	e.request = req.Clone(req.Context())
	e.selectedAuth = credential.Clone()
	body, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		return nil, errRead
	}
	e.body = body
	statusCode := e.statusCode
	if statusCode == 0 {
		statusCode = http.StatusCreated
	}
	headers := e.headers.Clone()
	if headers == nil {
		headers = http.Header{
			"Connection":          []string{"X-Connection-Secret"},
			"Content-Type":        []string{"application/sdp"},
			"Location":            []string{"/v1/live/call-123"},
			"Set-Cookie":          []string{"session=secret"},
			"X-Connection-Secret": []string{"secret"},
			"X-Live-Session":      []string{"live-session-123"},
		}
	}
	return &http.Response{
		StatusCode: statusCode,
		Header:     headers,
		Body:       e.responseBody,
	}, nil
}

type homeDispatcher struct {
	model string
	calls atomic.Int32
}

func (*homeDispatcher) HeartbeatOK() bool { return true }

func (d *homeDispatcher) RPopAuth(
	_ context.Context,
	model string,
	_ string,
	_ http.Header,
	_ int,
) ([]byte, error) {
	d.model = model
	d.calls.Add(1)
	return json.Marshal(
		map[string]any{
			"model":      model,
			"provider":   "codex",
			"auth_index": "home-codex-live",
			"auth": map[string]any{
				"id":       "home-codex-live",
				"provider": "codex",
				"status":   "active",
				"metadata": map[string]any{"access_token": "home-live-token"},
			},
			"concurrency": map[string]any{
				"accounted":     true,
				"credential_id": "home-codex-live",
				"model":         model,
			},
		},
	)
}

func (*homeDispatcher) AbortAmbiguousDispatch() {}

type trackedResponseBody struct {
	io.Reader
	closed atomic.Bool
	closes atomic.Int32
}

func (b *trackedResponseBody) Close() error {
	b.closes.Add(1)
	b.closed.Store(true)
	return nil
}

func registerCredential(t *testing.T, manager *auth.Manager, credential *auth.Auth) {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), credential); errRegister != nil {
		t.Fatalf("register %s: %v", credential.ID, errRegister)
	}
}

func multipartBody(boundary, sdp, session string) string {
	body := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"sdp\"\r\n" +
		"Content-Type: application/sdp\r\n\r\n" +
		sdp + "\r\n"
	if session != "" {
		body += "--" + boundary + "\r\n" +
			"Content-Disposition: form-data; name=\"session\"\r\n" +
			"Content-Type: application/json\r\n\r\n" +
			session + "\r\n"
	}
	return body + "--" + boundary + "--\r\n"
}

func newLocalLiveHandler(t *testing.T, executor *captureExecutor) *Handler {
	t.Helper()
	manager := auth.NewManager(nil, &apiKeyFirstSelector{}, nil)
	manager.RegisterExecutor(executor)
	registerCredential(
		t, manager, &auth.Auth{
			ID:         "codex-api-key",
			Provider:   "codex",
			Status:     auth.StatusActive,
			Attributes: map[string]string{auth.AttributeAPIKey: "must-not-be-used"},
		},
	)
	registerCredential(
		t, manager, &auth.Auth{
			ID:       "codex-oauth",
			Provider: "codex",
			Status:   auth.StatusActive,
			Metadata: map[string]any{
				"access_token": "oauth-token",
				"account_id":   "account-123",
			},
		},
	)
	return NewHandler(manager, nil)
}

func TestHandlerRewritesLiveCallAndSchedulesOAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	responseBody := &trackedResponseBody{Reader: strings.NewReader("v=0\r\na=ice-lite\r\n")}
	executor := &captureExecutor{responseBody: responseBody}
	handler := newLocalLiveHandler(t, executor)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	const boundary = "codex-realtime-call-boundary"
	body := multipartBody(boundary, "v=0\r\na=setup:actpass", `{"model":"gpt-live-1-codex"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(body))
	req = req.WithContext(internallogging.WithRequestID(req.Context(), "1234abcd"))
	req.Header.Set("Authorization", "Bearer downstream-api-key")
	req.Header.Set("Cookie", "downstream=secret")
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Originator", "Codex Desktop")
	req.Header.Set("Thread-Id", "thread-123")
	req.Header.Set("Session-Id", "session-123")
	req.Header.Set("OpenAI-Alpha", "quicksilver=v2")
	req.Header.Set("X-Oai-Attestation", "attestation-token")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if executor.request == nil || executor.selectedAuth == nil {
		t.Fatal("Codex executor did not receive a live request")
	}
	if executor.selectedAuth.ID != "codex-oauth" {
		t.Fatalf("selected auth = %q, want codex-oauth", executor.selectedAuth.ID)
	}
	if got := executor.request.URL.String(); got != upstreamCallURL {
		t.Fatalf("upstream URL = %q, want %q", got, upstreamCallURL)
	}
	var upstreamPayload struct {
		SDP     string         `json:"sdp"`
		Session map[string]any `json:"session"`
	}
	if errUnmarshal := json.Unmarshal(executor.body, &upstreamPayload); errUnmarshal != nil {
		t.Fatalf("unmarshal upstream body: %v; body=%s", errUnmarshal, executor.body)
	}
	if upstreamPayload.SDP != "v=0\r\na=setup:actpass" {
		t.Fatalf("upstream sdp = %q", upstreamPayload.SDP)
	}
	if got := upstreamPayload.Session["model"]; got != "gpt-live-1-codex" {
		t.Fatalf("upstream session model = %#v", got)
	}
	if got := executor.request.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := executor.request.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want OAuth token", got)
	}
	if got := executor.request.Header.Get("Chatgpt-Account-Id"); got != "account-123" {
		t.Fatalf("Chatgpt-Account-Id = %q, want account-123", got)
	}
	for header, want := range map[string]string{
		"OpenAI-Alpha":      "quicksilver=v2",
		"Originator":        "Codex Desktop",
		"Session-Id":        "session-123",
		"Thread-Id":         "thread-123",
		"X-Oai-Attestation": "attestation-token",
	} {
		if got := executor.request.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	for _, blocked := range []string{"Cookie", "Host", "Proxy-Authorization"} {
		if got := executor.request.Header.Get(blocked); got != "" {
			t.Errorf("blocked request header %s leaked as %q", blocked, got)
		}
	}
	if got := recorder.Body.String(); got != "v=0\r\na=ice-lite\r\n" {
		t.Fatalf("response body = %q", got)
	}
	if got := recorder.Header().Get("Location"); got != "/v1/live/call-123" {
		t.Fatalf("Location = %q, want live call location", got)
	}
	for _, blocked := range []string{"Connection", "Set-Cookie", "X-Connection-Secret", "X-Live-Session"} {
		if got := recorder.Header().Get(blocked); got != "" {
			t.Errorf("blocked response header %s leaked as %q", blocked, got)
		}
	}
	if traceID := recorder.Header().Get(internallogging.CPATraceIDHeader); !regexp.MustCompile(`^\d{14}-[a-f0-9]+-1234abcd$`).MatchString(traceID) {
		t.Fatalf("CPA trace ID = %q", traceID)
	}
	if !responseBody.closed.Load() {
		t.Fatal("upstream response body was not closed")
	}
	stored, ok := handler.sessions.peek("call-123")
	if !ok || stored.authID != "codex-oauth" || stored.model != "gpt-live-1-codex" {
		t.Fatalf("stored live session = %#v, ok=%t", stored, ok)
	}
}

func TestMediaCredentialNameUsesSafeIdentity(t *testing.T) {
	for name, testCase := range map[string]struct {
		selected *auth.Auth
		index    string
		want     string
	}{
		"label": {
			selected: &auth.Auth{Label: "Voice credential", FileName: "/auths/codex-user.json", ID: "secret-id"},
			index:    "auth-index",
			want:     "Voice credential",
		},
		"file basename": {
			selected: &auth.Auth{FileName: "/auths/codex-user.json", ID: "secret-id"},
			index:    "auth-index",
			want:     "codex-user.json",
		},
		"opaque index": {
			selected: &auth.Auth{ID: "secret-id"},
			index:    "auth-index",
			want:     "auth-index",
		},
		"nil auth": {
			index: "auth-index",
			want:  "auth-index",
		},
	} {
		t.Run(
			name, func(t *testing.T) {
				if got := mediaCredentialName(testCase.selected, testCase.index); got != testCase.want {
					t.Fatalf("mediaCredentialName() = %q, want %q", got, testCase.want)
				}
			},
		)
	}
}

func TestHandlerUsesLiveModelForHomeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	dispatcher := &homeDispatcher{}
	registry := executionregistry.New()
	manager.PublishHomeDispatch(dispatcher, registry, 1)
	responseBody := &trackedResponseBody{Reader: strings.NewReader("v=0\r\n")}
	executor := &captureExecutor{responseBody: responseBody}
	manager.RegisterExecutor(executor)

	handler := NewHandler(manager, nil)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	const boundary = "home-live-boundary"
	body := multipartBody(boundary, "v=0", `{"model":"future-live-model"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if dispatcher.model != "future-live-model" {
		t.Fatalf("Home dispatch model = %q, want future-live-model", dispatcher.model)
	}
	if executor.selectedAuth == nil || executor.selectedAuth.ID != "home-codex-live" {
		t.Fatalf("selected Home auth = %#v", executor.selectedAuth)
	}
	if !responseBody.closed.Load() {
		t.Fatal("Home upstream response body was not closed")
	}
	stored, ok := handler.sessions.peek("call-123")
	if !ok || stored.homeSelection == nil || !stored.homeSelection.Retained() || !stored.homeSelection.Active() {
		t.Fatalf("stored Home live session = %#v, ok=%t", stored, ok)
	}
	if errDrain := registry.Drain(context.Background()); errDrain != nil {
		t.Fatalf("Drain() error = %v", errDrain)
	}
	if stored.homeSelection.Active() {
		t.Fatal("Home live selection remained active after drain")
	}
}

func TestHomeLiveSessionExpiryReleasesSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	registry := executionregistry.New()
	manager.PublishHomeDispatch(&homeDispatcher{}, registry, 1)
	manager.RegisterExecutor(
		&captureExecutor{
			responseBody: &trackedResponseBody{Reader: strings.NewReader("v=0\r\n")},
		},
	)

	handler := NewHandler(manager, nil)
	handler.sessions.lifetime = 20 * time.Millisecond
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	const boundary = "expiring-home-live-boundary"
	body := multipartBody(boundary, "v=0", `{"model":"gpt-live-1-codex"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	stored, ok := handler.sessions.peek("call-123")
	if !ok || stored.homeSelection == nil || !stored.homeSelection.Active() {
		t.Fatalf("stored Home live session = %#v, ok=%t", stored, ok)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, stillStored := handler.sessions.peek("call-123")
		if !stillStored && !stored.homeSelection.Active() {
			if errDrain := registry.Drain(context.Background()); errDrain != nil {
				t.Fatalf("Drain() error = %v", errDrain)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expired Home live session remained active")
}

func TestHandlerRejectsOversizedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/live", bytes.NewReader(bytes.Repeat([]byte{'x'}, maxBodySize+1))),
	)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestHandlerRejectsOversizedUpstreamResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &captureExecutor{
		responseBody: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, maxBodySize+1))),
	}
	handler := newLocalLiveHandler(t, executor)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"model":"gpt-live-1-codex"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "response body too large") {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestHandlerDoesNotStoreInvalidLocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &captureExecutor{
		responseBody: io.NopCloser(strings.NewReader("v=0\r\n")),
		headers: http.Header{
			"Content-Type": []string{"application/sdp"},
			"Location":     []string{"/v1/live/bad/call"},
		},
	}
	handler := newLocalLiveHandler(t, executor)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	if _, ok := handler.sessions.peek("call"); ok {
		t.Fatal("invalid Location created a live session")
	}
}

func TestHandlerConfigProviderReturnsLatestSnapshot(t *testing.T) {
	first := &config.Config{}
	second := &config.Config{}
	current := first
	handler := NewHandler(nil, func() *config.Config { return current })

	if got := handler.configSnapshot(); got != first {
		t.Fatalf("first config snapshot = %p, want %p", got, first)
	}
	current = second
	if got := handler.configSnapshot(); got != second {
		t.Fatalf("second config snapshot = %p, want %p", got, second)
	}
}

func TestPrepareCallRequestRewritesMultipart(t *testing.T) {
	const boundary = "live-model-boundary"
	body := multipartBody(boundary, "v=0-offer", `{"model":"future-live-model","instructions":"hi"}`)

	encoded, contentType, model, errPrepare := prepareCallRequest(
		[]byte(body), "multipart/form-data; boundary="+boundary,
	)
	if errPrepare != nil {
		t.Fatalf("prepareCallRequest() error = %v", errPrepare)
	}
	if contentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", contentType)
	}
	if model != "future-live-model" {
		t.Fatalf("model = %q, want future-live-model", model)
	}
	var payload struct {
		SDP     string         `json:"sdp"`
		Session map[string]any `json:"session"`
	}
	if errUnmarshal := json.Unmarshal(encoded, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal encoded body: %v", errUnmarshal)
	}
	if payload.SDP != "v=0-offer" || payload.Session["instructions"] != "hi" {
		t.Fatalf("encoded payload = %#v", payload)
	}
}

func TestPrepareCallRequestModelPriority(t *testing.T) {
	_, _, model, errPrepare := prepareCallRequest(
		[]byte(`{"model":"top-level","session":{"model":"session-model"}}`),
		"application/json",
	)
	if errPrepare != nil {
		t.Fatalf("prepareCallRequest() error = %v", errPrepare)
	}
	if model != "session-model" {
		t.Fatalf("model = %q, want session-model", model)
	}

	_, _, model, errPrepare = prepareCallRequest([]byte(`{}`), "application/json")
	if errPrepare != nil {
		t.Fatalf("prepareCallRequest() error = %v", errPrepare)
	}
	if model != defaultLiveModel {
		t.Fatalf("default model = %q, want %q", model, defaultLiveModel)
	}
}

func TestPrepareCallRequestRejectsInvalidMultipart(t *testing.T) {
	const boundary = "invalid-live-boundary"
	body := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"session\"\r\n\r\n" +
		`{"model":"gpt-live-1-codex"}` + "\r\n" +
		"--" + boundary + "--\r\n"

	if _, _, _, errPrepare := prepareCallRequest(
		[]byte(body), "multipart/form-data; boundary="+boundary,
	); errPrepare == nil {
		t.Fatal("prepareCallRequest() accepted multipart body without sdp")
	}
}

func TestPrepareCallRequestRejectsInvalidSessionJSON(t *testing.T) {
	const boundary = "invalid-session-boundary"
	body := multipartBody(boundary, "v=0", `{"model":`)
	if _, _, _, errPrepare := prepareCallRequest(
		[]byte(body), "multipart/form-data; boundary="+boundary,
	); errPrepare == nil {
		t.Fatal("prepareCallRequest() accepted invalid session JSON")
	}
}

func TestRewriteCallRequestModelRejectsNullObjects(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{"session":null}`,
	} {
		t.Run(body, func(t *testing.T) {
			_, _, err := rewriteCallRequestModel([]byte(body), "application/json", defaultLiveModel)
			if err == nil {
				t.Fatalf("rewriteCallRequestModel(%s) accepted a null object", body)
			}
		})
	}
}

func TestHeadersForLoggingRedactsAttestation(t *testing.T) {
	source := http.Header{
		"Authorization":     []string{"Bearer oauth-token"},
		"X-Oai-Attestation": []string{"attestation-token"},
	}

	got := headersForLogging(source)
	if value := got.Get("X-Oai-Attestation"); value != "[REDACTED]" {
		t.Fatalf("logged X-Oai-Attestation = %q, want redacted", value)
	}
	if value := source.Get("X-Oai-Attestation"); value != "attestation-token" {
		t.Fatalf("source X-Oai-Attestation changed to %q", value)
	}
}

func TestCallIDFromLocation(t *testing.T) {
	for location, want := range map[string]string{
		"rtc_0":                    "rtc_0",
		"/v1/live/rtc_1":           "rtc_1",
		"/v1/realtime/calls/rtc_2": "rtc_2",
		"/v1/realtime?intent=quicksilver&call_id=rtc_3":    "rtc_3",
		"https://example.test/v1/live/rtc_4":               "rtc_4",
		"https://example.test/v1/realtime/calls/rtc_5?x=1": "rtc_5",
	} {
		if got := callIDFromLocation(location); got != want {
			t.Errorf("callIDFromLocation(%q) = %q, want %q", location, got, want)
		}
	}
	for _, location := range []string{"", "/v1/live/", "/v1/live/bad/call", "/v1/other/rtc_1", "/v1/live/bad%20id"} {
		if got := callIDFromLocation(location); got != "" {
			t.Errorf("callIDFromLocation(%q) = %q, want empty", location, got)
		}
	}
}

type fakeMediaRelay struct {
	clientOffer   string
	upstreamOffer string
	route         mediaSessionRoute
	session       *fakeMediaSession
	err           error
}

func (r *fakeMediaRelay) NewSession(
	_ context.Context,
	clientOffer string,
	route mediaSessionRoute,
) (mediaRelaySession, string, error) {
	r.clientOffer = clientOffer
	r.route = route
	return r.session, r.upstreamOffer, r.err
}

type fakeMediaSession struct {
	upstreamAnswer string
	callIDAtAccept string
	downstreamSDP  string
	closeHandler   func(string)
	callID         string
	closeReason    string
	closed         atomic.Bool
	closeCount     atomic.Int32
	err            error
	closeErr       error
	closeStarted   chan struct{}
	closeAllowed   chan struct{}
}

func (s *fakeMediaSession) AcceptUpstreamAnswer(_ context.Context, answer string) (string, error) {
	s.upstreamAnswer = answer
	s.callIDAtAccept = s.callID
	return s.downstreamSDP, s.err
}

func (s *fakeMediaSession) SetCallID(callID string) {
	s.callID = callID
}

func (s *fakeMediaSession) SetCloseHandler(handler func(string)) {
	s.closeHandler = handler
}

func (s *fakeMediaSession) Close() error {
	return s.CloseWithReason("closed")
}

func (s *fakeMediaSession) CloseWithReason(reason string) error {
	if s.closed.CompareAndSwap(false, true) {
		s.closeReason = reason
		if s.closeStarted != nil {
			close(s.closeStarted)
			<-s.closeAllowed
		}
		s.closeCount.Add(1)
	}
	return s.closeErr
}

type failingHTTPWriter struct {
	header http.Header
	status int
}

func (w *failingHTTPWriter) Header() http.Header {
	return w.header
}

func (*failingHTTPWriter) Write([]byte) (int, error) {
	return 0, errors.New("downstream write failed")
}

func (w *failingHTTPWriter) WriteHeader(statusCode int) {
	w.status = statusCode
}

func installFakeMediaRelay(handler *Handler, relay mediaRelayFactory) {
	handler.mediaRelayMu.Lock()
	handler.mediaRelay = relay
	handler.mediaRelayErr = nil
	handler.mediaRelayMu.Unlock()
}

func TestHandlerRelaysWebRTCMediaSDP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseBody := &trackedResponseBody{Reader: strings.NewReader("v=0\r\no=upstream-answer\r\n")}
	executor := &captureExecutor{responseBody: responseBody}
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerCredential(
		t, manager, &auth.Auth{
			ID:       "codex-oauth",
			Provider: "codex",
			Status:   auth.StatusActive,
			Label:    "Voice credential",
			ProxyURL: "socks5://credential-proxy.example:1080",
			Metadata: map[string]any{"access_token": "oauth-token"},
		},
	)
	runtimeConfig := &config.Config{}
	runtimeConfig.ProxyURL = "http://global-proxy.example:8080"
	handler := NewHandler(manager, func() *config.Config { return runtimeConfig })
	mediaSession := &fakeMediaSession{downstreamSDP: "v=0\r\no=downstream-answer\r\n"}
	mediaRelay := &fakeMediaRelay{
		upstreamOffer: "v=0\r\no=gateway-offer\r\n",
		session:       mediaSession,
	}
	installFakeMediaRelay(handler, mediaRelay)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	const boundary = "media-relay-boundary"
	body := multipartBody(boundary, "v=0\r\no=desktop-offer\r\n", `{"model":"gpt-live-1-codex"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if mediaRelay.clientOffer != "v=0\r\no=desktop-offer\r\n" {
		t.Fatalf("media client offer = %q", mediaRelay.clientOffer)
	}
	if mediaRelay.route.proxyURL != "socks5://credential-proxy.example:1080" {
		t.Fatalf("media proxy URL = %q, want credential override", mediaRelay.route.proxyURL)
	}
	if mediaRelay.route.credential != "Voice credential" || mediaRelay.route.authIndex == "" {
		t.Fatalf("media credential route = %#v", mediaRelay.route)
	}
	var upstreamPayload struct {
		SDP string `json:"sdp"`
	}
	if errUnmarshal := json.Unmarshal(executor.body, &upstreamPayload); errUnmarshal != nil {
		t.Fatalf("unmarshal upstream body: %v", errUnmarshal)
	}
	if upstreamPayload.SDP != mediaRelay.upstreamOffer {
		t.Fatalf("upstream SDP = %q, want gateway offer", upstreamPayload.SDP)
	}
	if mediaSession.upstreamAnswer != "v=0\r\no=upstream-answer\r\n" {
		t.Fatalf("accepted upstream answer = %q", mediaSession.upstreamAnswer)
	}
	if mediaSession.callID != "call-123" {
		t.Fatalf("media call ID = %q, want call-123", mediaSession.callID)
	}
	if mediaSession.callIDAtAccept != "call-123" {
		t.Fatalf("media call ID at answer acceptance = %q, want call-123", mediaSession.callIDAtAccept)
	}
	if got := recorder.Body.String(); got != mediaSession.downstreamSDP {
		t.Fatalf("downstream SDP = %q, want %q", got, mediaSession.downstreamSDP)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/sdp" {
		t.Fatalf("Content-Type = %q, want application/sdp", got)
	}
	if mediaSession.closed.Load() {
		t.Fatal("retained media session was closed before session completion")
	}
	if mediaSession.closeHandler == nil {
		t.Fatal("media session close handler was not installed")
	}
	mediaSession.closeHandler("test_closed")
	if !mediaSession.closed.Load() || mediaSession.closeReason != "test_closed" {
		t.Fatalf("completed media session close state = %t/%q", mediaSession.closed.Load(), mediaSession.closeReason)
	}
	if mediaSession.closeCount.Load() != 1 {
		t.Fatalf("media close count = %d, want 1", mediaSession.closeCount.Load())
	}
	if _, ok := handler.sessions.peek("call-123"); ok {
		t.Fatal("completed media session remained stored")
	}
}

func TestLiveAdmissionWaitsForMediaClose(t *testing.T) {
	for _, ending := range []string{"complete", "expire", "replacement", "shutdown"} {
		t.Run(ending, func(t *testing.T) {
			responseBody := &trackedResponseBody{Reader: strings.NewReader("v=0\r\no=upstream-answer\r\n")}
			handler := newLocalLiveHandler(t, &captureExecutor{responseBody: responseBody})
			admission := executionregistry.NewAdmission()
			handler.SetInferenceAdmission(admission)
			media := &fakeMediaSession{
				downstreamSDP: "v=0\r\no=downstream-answer\r\n",
				closeStarted:  make(chan struct{}),
				closeAllowed:  make(chan struct{}),
			}
			defer close(media.closeAllowed)
			installFakeMediaRelay(handler, &fakeMediaRelay{session: media, upstreamOffer: "v=0\r\n"})
			router := gin.New()
			router.POST("/v1/live", handler.Handle)
			ctx, releaseHTTP, err := admission.Acquire(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"sdp":"v=0\r\no=desktop-offer\r\n","model":"gpt-live-1-codex"}`)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			releaseHTTP()
			if response.Code != http.StatusCreated {
				t.Fatalf("bootstrap status=%d body=%s", response.Code, response.Body.String())
			}
			session, ok := handler.sessions.peek("call-123")
			if !ok {
				t.Fatal("bootstrap did not retain the media session")
			}
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				switch ending {
				case "complete":
					handler.sessions.complete(session, "test_complete")
				case "expire":
					handler.sessions.expire(session.callID, session.token)
				case "replacement":
					handler.sessions.put(session.callID, liveSession{authID: "replacement"})
				case "shutdown":
					_ = handler.Close()
				}
			}()
			select {
			case <-media.closeStarted:
			case <-time.After(time.Second):
				t.Fatal("media close did not start")
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			_, err = admission.Drain(waitCtx, "update", time.Now().Add(time.Second))
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("drain completed before media close returned: %v", err)
			}
			media.closeAllowed <- struct{}{}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("session close did not finish")
			}
			confirmation, err := admission.Drain(context.Background(), "next-update", time.Now().Add(time.Second))
			if err != nil || !admission.Confirm(confirmation) {
				t.Fatalf("finished media kept its lease: %v", err)
			}
			admission.Resume("next-update")
			_ = handler.Close()
		})
	}
}

func TestHandlerClosesUnretainedMediaSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseBody := &trackedResponseBody{Reader: strings.NewReader("v=0\r\no=upstream-answer\r\n")}
	executor := &captureExecutor{responseBody: responseBody, statusCode: http.StatusUnauthorized}
	handler := newLocalLiveHandler(t, executor)
	mediaSession := &fakeMediaSession{downstreamSDP: "v=0\r\no=downstream-answer\r\n"}
	installFakeMediaRelay(
		handler, &fakeMediaRelay{
			upstreamOffer: "v=0\r\no=gateway-offer\r\n",
			session:       mediaSession,
		},
	)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	const boundary = "media-error-boundary"
	body := multipartBody(boundary, "v=0\r\no=desktop-offer\r\n", `{"model":"gpt-live-1-codex"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	if !mediaSession.closed.Load() || mediaSession.closeReason != "request_not_retained" {
		t.Fatalf("unretained media close state = %t/%q", mediaSession.closed.Load(), mediaSession.closeReason)
	}
	if _, ok := handler.sessions.peek("call-123"); ok {
		t.Fatal("failed request stored its media session")
	}
}

func TestHandlerClosesMediaWhenResponseWriteFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseBody := &trackedResponseBody{Reader: strings.NewReader("v=0\r\no=upstream-answer\r\n")}
	executor := &captureExecutor{responseBody: responseBody}
	handler := newLocalLiveHandler(t, executor)
	mediaSession := &fakeMediaSession{downstreamSDP: "v=0\r\no=downstream-answer\r\n"}
	installFakeMediaRelay(
		handler, &fakeMediaRelay{
			upstreamOffer: "v=0\r\no=gateway-offer\r\n",
			session:       mediaSession,
		},
	)
	router := gin.New()
	router.POST("/v1/live", handler.Handle)

	const boundary = "response-write-error-boundary"
	body := multipartBody(boundary, "v=0\r\no=desktop-offer\r\n", `{"model":"gpt-live-1-codex"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	writer := &failingHTTPWriter{header: make(http.Header)}
	router.ServeHTTP(writer, req)

	if writer.status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", writer.status, http.StatusCreated)
	}
	if !mediaSession.closed.Load() || mediaSession.closeReason != "response_write_failed" {
		t.Fatalf("response failure media close state = %t/%q", mediaSession.closed.Load(), mediaSession.closeReason)
	}
	if _, ok := handler.sessions.peek("call-123"); ok {
		t.Fatal("response write failure retained a stored session")
	}
}

func TestWriteSelectionErrorMapsContextStatuses(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "canceled", err: context.Canceled, want: clienterror.StatusClientClosedRequest},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "explicit status",
			err:  &auth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
			want: http.StatusTooManyRequests,
		},
		{name: "fallback", err: errors.New("boom"), want: http.StatusServiceUnavailable},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				writeSelectionError(ctx, test.err)
				if recorder.Code != test.want {
					t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
				}
			},
		)
	}
}

func TestMediaRelayWrapsRawSDPForCodexBackend(t *testing.T) {
	body := []byte("v=0\r\no=raw-offer\r\n")
	clientOffer, errSDP := callRequestSDP(body, "application/sdp")
	if errSDP != nil {
		t.Fatalf("callRequestSDP() error = %v", errSDP)
	}
	if clientOffer != string(body) {
		t.Fatalf("client offer = %q, want original body", clientOffer)
	}
	prepared, contentType, errReplace := replaceCallRequestSDP(
		body,
		"application/sdp",
		"v=0\r\no=gateway-offer\r\n",
	)
	if errReplace != nil {
		t.Fatalf("replaceCallRequestSDP() error = %v", errReplace)
	}
	if contentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", contentType)
	}
	var payload struct {
		SDP string `json:"sdp"`
	}
	if errUnmarshal := json.Unmarshal(prepared, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal prepared request: %v", errUnmarshal)
	}
	if payload.SDP != "v=0\r\no=gateway-offer\r\n" {
		t.Fatalf("upstream SDP = %q", payload.SDP)
	}
}

func TestHandlerUpdatesMediaRelayConfig(t *testing.T) {
	handler := NewHandler(nil, nil)
	if relay, errRelay := handler.currentMediaRelay(); relay != nil || errRelay != nil {
		t.Fatalf("initial media relay = %#v, error = %v", relay, errRelay)
	}
	enabled := &config.Config{
		Codex: config.CodexConfig{
			LiveMediaRelay: config.CodexLiveMediaRelayConfig{
				Enabled:                 true,
				MaxSessions:             1,
				DisablePrivateRemoteIPs: false,
			},
		},
	}
	if errUpdate := handler.UpdateConfig(enabled); errUpdate != nil {
		t.Fatalf("enable media relay: %v", errUpdate)
	}
	enabledRelay, errRelay := handler.currentMediaRelay()
	if enabledRelay == nil || errRelay != nil {
		t.Fatalf("enabled media relay = %#v, error = %v", enabledRelay, errRelay)
	}
	unchanged := *enabled
	unchanged.Debug = true
	unchanged.ProxyURL = "http://new-proxy.example"
	if errUpdate := handler.UpdateConfig(&unchanged); errUpdate != nil {
		t.Fatalf("apply unrelated config change: %v", errUpdate)
	}
	unchangedRelay, errRelay := handler.currentMediaRelay()
	if unchangedRelay != enabledRelay || errRelay != nil {
		t.Fatalf(
			"unrelated config change rebuilt media relay: before=%#v after=%#v error=%v",
			enabledRelay,
			unchangedRelay,
			errRelay,
		)
	}
	changed := *enabled
	changed.Codex.LiveMediaRelay.MaxSessions = 2
	if errUpdate := handler.UpdateConfig(&changed); errUpdate != nil {
		t.Fatalf("reload media relay: %v", errUpdate)
	}
	changedRelay, errRelay := handler.currentMediaRelay()
	if changedRelay == nil || changedRelay == enabledRelay || errRelay != nil {
		t.Fatalf("changed media relay = %#v, previous=%#v error=%v", changedRelay, enabledRelay, errRelay)
	}
	if errUpdate := handler.UpdateConfig(&config.Config{}); errUpdate != nil {
		t.Fatalf("disable media relay: %v", errUpdate)
	}
	if relay, errRelay := handler.currentMediaRelay(); relay != nil || errRelay != nil {
		t.Fatalf("disabled media relay = %#v, error = %v", relay, errRelay)
	}
}

func TestHandlerClearsICEServersFromBothConfigEntrypoints(t *testing.T) {
	const base = "codex:\n  live-media-relay:\n    enabled: true\n    max-sessions: 2\n"
	const populated = base + "    ice-servers:\n      - urls: [\"turn:relay.example.org:3478?transport=udp\"]\n        username: test-turn-user\n        credential: test-turn-secret\n"
	for _, entrypoint := range []string{"disk", "bytes"} {
		for _, form := range []struct {
			name string
			yaml string
		}{
			{name: "omitted", yaml: base},
			{name: "empty", yaml: base + "    ice-servers: []\n"},
		} {
			t.Run(entrypoint+"/"+form.name, func(t *testing.T) {
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				load := func(raw string) *config.Config {
					t.Helper()
					var cfg *config.Config
					var err error
					if entrypoint == "disk" {
						if errWrite := os.WriteFile(configPath, []byte(raw), 0o600); errWrite != nil {
							t.Fatalf("write config: %v", errWrite)
						}
						cfg, err = config.LoadConfigOptional(configPath, false)
					} else {
						cfg, err = config.ParseConfigBytes([]byte(raw))
					}
					if err != nil {
						t.Fatalf("load config: %v", err)
					}
					return cfg
				}
				current := load(form.yaml)
				handler := NewHandler(nil, func() *config.Config { return current })
				t.Cleanup(func() {
					if errClose := handler.Close(); errClose != nil {
						t.Errorf("close handler: %v", errClose)
					}
				})
				relay := func() *pionMediaRelay {
					t.Helper()
					factory, errRelay := handler.currentMediaRelay()
					concrete, ok := factory.(*pionMediaRelay)
					if errRelay != nil || !ok || concrete == nil {
						t.Fatalf("media relay type = %T, error = %v", factory, errRelay)
					}
					return concrete
				}
				if len(current.Codex.LiveMediaRelay.ICEServers) != 0 || len(relay().configuration.ICEServers) != 0 {
					t.Fatal("initial empty config installed ICE servers")
				}
				current = load(populated)
				if errUpdate := handler.UpdateConfig(current); errUpdate != nil {
					t.Fatalf("install ICE servers: %v", errUpdate)
				}
				previous := relay()
				if servers := previous.configuration.ICEServers; len(servers) != 1 ||
					servers[0].Username != "test-turn-user" || servers[0].Credential != "test-turn-secret" {
					t.Fatalf("populated config did not install the test ICE server: %#v", servers)
				}
				current = load(form.yaml)
				if errUpdate := handler.UpdateConfig(current); errUpdate != nil {
					t.Fatalf("clear ICE servers: %v", errUpdate)
				}
				cleared := relay()
				if len(current.Codex.LiveMediaRelay.ICEServers) != 0 || len(cleared.configuration.ICEServers) != 0 {
					t.Fatal("cleared config retained ICE servers for new sessions")
				}
				if cleared == previous || cleared.limiter != previous.limiter || len(previous.configuration.ICEServers) != 1 {
					t.Fatal("config update did not replace the factory while retaining its limiter and prior configuration")
				}
			})
		}
	}
}

func TestSessionStoreCloseAllReleasesMediaAndResources(t *testing.T) {
	store := newSessionStore()
	mediaSession := &fakeMediaSession{}
	stored := store.put("call-close-all", liveSession{media: mediaSession})
	var resourceClosed atomic.Bool
	stored.resources.add(
		func() error {
			resourceClosed.Store(true)
			return nil
		},
	)

	if errClose := store.closeAll("test_shutdown"); errClose != nil {
		t.Fatalf("closeAll() error = %v", errClose)
	}

	if !mediaSession.closed.Load() || mediaSession.closeReason != "test_shutdown" {
		t.Fatalf("closeAll media close state = %t/%q", mediaSession.closed.Load(), mediaSession.closeReason)
	}
	if !resourceClosed.Load() {
		t.Fatal("closeAll() did not close session resources")
	}
	if _, ok := store.peek("call-close-all"); ok {
		t.Fatal("closeAll() retained a session")
	}
}

func TestHandlerCloseReturnsSessionResourceErrors(t *testing.T) {
	resourceErr := errors.New("resource close failed")
	mediaErr := errors.New("media close failed")
	handler := NewHandler(nil, nil)
	mediaSession := &fakeMediaSession{closeErr: mediaErr}
	stored := handler.sessions.put("call-close-error", liveSession{media: mediaSession})
	stored.resources.add(
		func() error {
			return resourceErr
		},
	)

	errClose := handler.Close()
	if !errors.Is(errClose, resourceErr) || !errors.Is(errClose, mediaErr) {
		t.Fatalf("Close() error = %v, want resource and media errors", errClose)
	}
	if _, ok := handler.sessions.peek("call-close-error"); ok {
		t.Fatal("Close() retained a failed session")
	}
}
