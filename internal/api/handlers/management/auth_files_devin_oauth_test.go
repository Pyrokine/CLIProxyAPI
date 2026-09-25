package management

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

type fakeDevinOAuthService struct {
	build    func(string, string, string) string
	exchange func(context.Context, string, string) (string, error)
	create   func(context.Context, string) (*coreauth.Auth, error)
}

func (service *fakeDevinOAuthService) BuildAuthorizationURL(redirectURI, challenge, state string) string {
	if service.build != nil {
		return service.build(redirectURI, challenge, state)
	}
	authURL := &url.URL{Scheme: "https", Host: "app.devin.test", Path: "/oauth/authorize"}
	query := authURL.Query()
	query.Set("redirect_uri", redirectURI)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	authURL.RawQuery = query.Encode()
	return authURL.String()
}

func (service *fakeDevinOAuthService) ExchangeCodeForToken(
	ctx context.Context,
	code string,
	verifier string,
) (string, error) {
	if service.exchange != nil {
		return service.exchange(ctx, code, verifier)
	}
	return "test-token", nil
}

func (service *fakeDevinOAuthService) CreateAuthRecord(
	ctx context.Context,
	token string,
) (*coreauth.Auth, error) {
	if service.create != nil {
		return service.create(ctx, token)
	}
	return &coreauth.Auth{
		ID:       "devin-test.json",
		FileName: "devin-test.json",
		Provider: "devin",
		Metadata: map[string]any{
			"type":      "devin",
			"api_key":   "devin-session-token$" + token,
			"auth_kind": "oauth",
		},
	}, nil
}

func isolateDevinOAuthSessions(t *testing.T) {
	t.Helper()
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previous })
}

func withFakeDevinOAuthService(t *testing.T, service devinOAuthService) {
	t.Helper()
	previous := newDevinOAuthService
	newDevinOAuthService = func(*config.Config) devinOAuthService { return service }
	t.Cleanup(func() { newDevinOAuthService = previous })
}

func newDevinOAuthTestRouter(handler *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/devin-auth-url", handler.RequestDevinToken)
	router.POST("/oauth-callback", handler.PostOAuthCallback)
	router.GET("/get-auth-status", handler.GetAuthStatus)
	router.DELETE("/cancel-auth", handler.CancelAuthSession)
	return router
}

func decodeDevinOAuthResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return payload
}

func waitForDevinOAuthCompletion(t *testing.T, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, status, completed, ok := getOAuthSessionDetails(state)
		if ok && completed {
			return
		}
		if ok && status != "" {
			t.Fatalf("Devin OAuth failed: %s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Devin OAuth did not complete")
}

func TestDevinRemoteOAuthFlow(t *testing.T) {
	isolateDevinOAuthSessions(t)
	authDir := t.TempDir()
	otherAuthDir := t.TempDir()
	verifierReceived := make(chan string, 1)
	published := make(chan *coreauth.Auth, 1)
	service := &fakeDevinOAuthService{
		exchange: func(ctx context.Context, code, verifier string) (string, error) {
			if err := ctx.Err(); err != nil {
				t.Fatalf("exchange context already cancelled: %v", err)
			}
			if code != "remote-code" {
				t.Errorf("code = %q, want remote-code", code)
			}
			verifierReceived <- verifier
			return "test-token", nil
		},
	}
	withFakeDevinOAuthService(t, service)

	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: authDir, Port: 8317},
		nil,
		withTokenStore(sdkAuth.NewFileTokenStore()),
	)
	t.Cleanup(func() { _ = handler.Close() })
	handler.SetPostAuthPersistHook(func(ctx context.Context, auth *coreauth.Auth) error {
		if err := ctx.Err(); err != nil {
			t.Fatalf("runtime publication received cancelled context: %v", err)
		}
		published <- auth.Clone()
		return nil
	})
	router := newDevinOAuthTestRouter(handler)

	requestContext, cancelRequest := context.WithCancel(context.Background())
	startRecorder := httptest.NewRecorder()
	startRequest := httptest.NewRequest(
		http.MethodGet,
		"/devin-auth-url?is_webui=true&nonce=browser-nonce",
		nil,
	).WithContext(requestContext)
	router.ServeHTTP(startRecorder, startRequest)
	cancelRequest()
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", startRecorder.Code, startRecorder.Body.String())
	}
	if strings.Contains(startRecorder.Body.String(), "test-token") || strings.Contains(startRecorder.Body.String(), "api_key") {
		t.Fatalf("start response exposed credential material: %s", startRecorder.Body.String())
	}
	start := decodeDevinOAuthResponse(t, startRecorder)
	state, _ := start["state"].(string)
	authURLRaw, _ := start["url"].(string)
	if start["status"] != "ok" || !strings.HasPrefix(state, "devin-") || authURLRaw == "" {
		t.Fatalf("invalid start response: %#v", start)
	}
	if _, exists := start["callback_port"]; exists {
		t.Fatalf("plain HTTP flow returned callback_port: %#v", start)
	}
	if !verifyOAuthNonce(state, "browser-nonce") || verifyOAuthNonce(state, "wrong-nonce") {
		t.Fatal("Devin OAuth nonce was not registered correctly")
	}

	authURL, err := url.Parse(authURLRaw)
	if err != nil {
		t.Fatal(err)
	}
	query := authURL.Query()
	if query.Get("state") != state || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL query = %v", query)
	}
	if got := query.Get("redirect_uri"); got != "http://127.0.0.1:8317/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
	if query.Get("code_verifier") != "" {
		t.Fatal("authorization URL exposed PKCE verifier")
	}

	handler.SetConfig(&config.Config{AuthDir: otherAuthDir, Port: 8317})
	callbackBody, err := json.Marshal(map[string]string{
		"provider": "cognition",
		"state":    state,
		"code":     "remote-code",
	})
	if err != nil {
		t.Fatal(err)
	}
	callbackRecorder := httptest.NewRecorder()
	callbackRequest := httptest.NewRequest(http.MethodPost, "/oauth-callback", strings.NewReader(string(callbackBody)))
	callbackRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(callbackRecorder, callbackRequest)
	if callbackRecorder.Code != http.StatusOK {
		t.Fatalf("callback status = %d, body = %s", callbackRecorder.Code, callbackRecorder.Body.String())
	}

	var verifier string
	select {
	case verifier = <-verifierReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("token exchange did not start")
	}
	digest := sha256.Sum256([]byte(verifier))
	if got := base64.RawURLEncoding.EncodeToString(digest[:]); got != query.Get("code_challenge") {
		t.Fatalf("code_challenge = %q, want %q", query.Get("code_challenge"), got)
	}
	waitForDevinOAuthCompletion(t, state)

	credentialPath := filepath.Join(authDir, "devin-test.json")
	credentialData, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatalf("read saved credential: %v", err)
	}
	var credential map[string]any
	if err = json.Unmarshal(credentialData, &credential); err != nil {
		t.Fatalf("decode saved credential: %v", err)
	}
	if credential["api_key"] != "devin-session-token$test-token" || credential["type"] != "devin" {
		t.Fatalf("saved credential = %#v", credential)
	}
	if _, err = os.Stat(filepath.Join(otherAuthDir, "devin-test.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential was written outside captured auth dir: %v", err)
	}
	select {
	case runtimeAuth := <-published:
		if runtimeAuth.Provider != "devin" || runtimeAuth.FileName != "devin-test.json" {
			t.Fatalf("published auth = %#v", runtimeAuth)
		}
	case <-time.After(time.Second):
		t.Fatal("saved credential was not published to runtime hook")
	}

	statusRecorder := httptest.NewRecorder()
	router.ServeHTTP(
		statusRecorder,
		httptest.NewRequest(http.MethodGet, "/get-auth-status?state="+url.QueryEscape(state)+"&nonce=browser-nonce", nil),
	)
	if payload := decodeDevinOAuthResponse(t, statusRecorder); payload["status"] != "ok" {
		t.Fatalf("completed status = %#v", payload)
	}

	replayRecorder := httptest.NewRecorder()
	replayRequest := httptest.NewRequest(http.MethodPost, "/oauth-callback", strings.NewReader(string(callbackBody)))
	replayRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(replayRecorder, replayRequest)
	if replayRecorder.Code == http.StatusOK {
		t.Fatal("completed callback was accepted twice")
	}
}

func TestDevinOAuthCancellationRejectsLateExchangeResult(t *testing.T) {
	isolateDevinOAuthSessions(t)
	authDir := t.TempDir()
	exchangeStarted := make(chan struct{})
	exchangeReturned := make(chan struct{})
	createCalled := make(chan struct{}, 1)
	service := &fakeDevinOAuthService{
		exchange: func(ctx context.Context, _, _ string) (string, error) {
			close(exchangeStarted)
			<-ctx.Done()
			close(exchangeReturned)
			return "late-token", nil
		},
		create: func(context.Context, string) (*coreauth.Auth, error) {
			createCalled <- struct{}{}
			return nil, errors.New("must not be called")
		},
	}
	withFakeDevinOAuthService(t, service)
	store := &memoryAuthStore{}
	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: authDir, Port: 8317},
		nil,
		withTokenStore(store),
	)
	t.Cleanup(func() { _ = handler.Close() })
	router := newDevinOAuthTestRouter(handler)

	startRecorder := httptest.NewRecorder()
	router.ServeHTTP(
		startRecorder,
		httptest.NewRequest(http.MethodGet, "/devin-auth-url?nonce=cancel-nonce", nil),
	)
	start := decodeDevinOAuthResponse(t, startRecorder)
	state, _ := start["state"].(string)
	callbackBody := fmt.Sprintf(`{"provider":"devin","state":%q,"code":"code"}`, state)
	callbackRecorder := httptest.NewRecorder()
	callbackRequest := httptest.NewRequest(http.MethodPost, "/oauth-callback", strings.NewReader(callbackBody))
	callbackRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(callbackRecorder, callbackRequest)
	if callbackRecorder.Code != http.StatusOK {
		t.Fatalf("callback status = %d, body = %s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	select {
	case <-exchangeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not start")
	}

	cancelRecorder := httptest.NewRecorder()
	router.ServeHTTP(
		cancelRecorder,
		httptest.NewRequest(
			http.MethodDelete,
			"/cancel-auth?state="+url.QueryEscape(state)+"&nonce=cancel-nonce",
			nil,
		),
	)
	cancelResponse := decodeDevinOAuthResponse(t, cancelRecorder)
	if cancelResponse["status"] != "ok" || cancelResponse["cancelled"] != true {
		t.Fatalf("cancel response = %#v", cancelResponse)
	}
	select {
	case <-exchangeReturned:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop token exchange")
	}
	select {
	case <-createCalled:
		t.Fatal("late exchange result created a credential")
	case <-time.After(50 * time.Millisecond):
	}
	if len(store.items) != 0 {
		t.Fatalf("late exchange result was saved: %#v", store.items)
	}
	if _, _, _, ok := getOAuthSessionDetails(state); ok {
		t.Fatal("cancelled OAuth session was recreated")
	}
}

type failingDevinAuthStore struct {
	err error
}

func (*failingDevinAuthStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (store *failingDevinAuthStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", store.err
}
func (*failingDevinAuthStore) Delete(context.Context, string) error { return nil }

func TestCompleteDevinOAuthFailures(t *testing.T) {
	tests := []struct {
		name         string
		payload      oauthCallbackFilePayload
		exchange     func(context.Context, string, string) (string, error)
		create       func(context.Context, string) (*coreauth.Auth, error)
		store        coreauth.Store
		wantStatus   string
		wantMissing  bool
		preserveFile bool
	}{
		{
			name:       "denied",
			payload:    oauthCallbackFilePayload{State: "test-state", Error: "access_denied"},
			wantStatus: "Devin authorization denied",
		},
		{
			name:         "missing code",
			payload:      oauthCallbackFilePayload{State: "test-state"},
			wantStatus:   "Invalid OAuth callback payload",
			preserveFile: true,
		},
		{
			name:    "exchange error is redacted",
			payload: oauthCallbackFilePayload{State: "test-state", Code: "code"},
			exchange: func(context.Context, string, string) (string, error) {
				return "", errors.New("secret-upstream-response")
			},
			wantStatus: "Failed to exchange authorization code",
		},
		{
			name:    "empty token",
			payload: oauthCallbackFilePayload{State: "test-state", Code: "code"},
			exchange: func(context.Context, string, string) (string, error) {
				return "", nil
			},
			wantStatus: "Failed to exchange authorization code",
		},
		{
			name:    "record creation error",
			payload: oauthCallbackFilePayload{State: "test-state", Code: "code"},
			create: func(context.Context, string) (*coreauth.Auth, error) {
				return nil, errors.New("profile failed")
			},
			wantStatus: "Failed to create Devin credential",
		},
		{
			name:    "cancel during record creation",
			payload: oauthCallbackFilePayload{State: "test-state", Code: "code"},
			create: func(context.Context, string) (*coreauth.Auth, error) {
				cancelOAuthSession("test-state")
				return (&fakeDevinOAuthService{}).CreateAuthRecord(context.Background(), "token")
			},
			wantMissing: true,
		},
		{
			name:       "save error",
			payload:    oauthCallbackFilePayload{State: "test-state", Code: "code"},
			store:      &failingDevinAuthStore{err: errors.New("disk unavailable")},
			wantStatus: "Failed to save credential",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateDevinOAuthSessions(t)
			authDir := t.TempDir()
			registerOAuthSessionForAuthDir("test-state", "devin", "", authDir)
			data, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			callbackPath := filepath.Join(authDir, ".oauth-devin-test-state.oauth")
			if err = os.WriteFile(callbackPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			service := &fakeDevinOAuthService{exchange: test.exchange, create: test.create}
			store := test.store
			if store == nil {
				store = sdkAuth.NewFileTokenStore()
			}
			handler := NewHandlerWithoutConfigFilePath(
				&config.Config{AuthDir: authDir},
				nil,
				withTokenStore(store),
			)
			t.Cleanup(func() { _ = handler.Close() })
			handler.completeDevinOAuth(
				context.Background(),
				authDir,
				"test-state",
				"verifier",
				service,
				nil,
			)

			_, status, _, ok := getOAuthSessionDetails("test-state")
			if test.wantMissing {
				if ok {
					t.Fatalf("cancelled session still exists with status %q", status)
				}
			} else if !ok || status != test.wantStatus {
				t.Fatalf("status = %q, exists = %v; want %q", status, ok, test.wantStatus)
			}
			if strings.Contains(status, "secret-upstream-response") {
				t.Fatalf("session status exposed upstream response: %q", status)
			}
			_, statErr := os.Stat(callbackPath)
			if test.preserveFile {
				if statErr != nil {
					t.Fatalf("invalid callback evidence was removed: %v", statErr)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("consumed callback file remains: %v", statErr)
			}
			if _, err = os.Stat(filepath.Join(authDir, "devin-test.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("credential saved for failed flow: %v", err)
			}
		})
	}
}

func TestDevinManagementCallbackModes(t *testing.T) {
	httpConfig := &config.Config{Port: 8317}
	httpURI, forwarder, err := devinManagementCallback(httpConfig)
	if err != nil {
		t.Fatal(err)
	}
	if httpURI != "http://127.0.0.1:8317/callback" || forwarder != nil {
		t.Fatalf("HTTP callback = %q, %#v", httpURI, forwarder)
	}

	tlsConfig := &config.Config{Port: 9443}
	tlsConfig.TLS.Enable = true
	tlsURI, forwarder, err := devinManagementCallback(tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	if forwarder == nil || forwarder.port == 0 {
		t.Fatalf("TLS callback forwarder = %#v", forwarder)
	}
	t.Cleanup(func() { stopCallbackForwarderInstance(forwarder.port, forwarder) })
	if tlsURI != fmt.Sprintf("http://127.0.0.1:%d/callback", forwarder.port) {
		t.Fatalf("TLS callback URI = %q", tlsURI)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Get(tlsURI + "?code=code&state=state")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("forwarder status = %d", response.StatusCode)
	}
	wantLocation := "https://127.0.0.1:9443/callback?code=code&state=state"
	if location := response.Header.Get("Location"); location != wantLocation {
		t.Fatalf("forwarder location = %q, want %q", location, wantLocation)
	}

	stopCallbackForwarderInstance(forwarder.port, forwarder)
	select {
	case <-forwarder.done:
	case <-time.After(time.Second):
		t.Fatal("ephemeral callback forwarder did not stop")
	}
	callbackForwardersMu.Lock()
	registered := callbackForwarders[forwarder.port]
	callbackForwardersMu.Unlock()
	if registered != nil {
		t.Fatal("stopped ephemeral callback forwarder remained registered")
	}
}

func TestPostOAuthCallbackRejectsDevinProviderMismatch(t *testing.T) {
	isolateDevinOAuthSessions(t)
	authDir := t.TempDir()
	registerOAuthSessionForAuthDir("devin-state", "devin", "", authDir)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	t.Cleanup(func() { _ = handler.Close() })
	router := newDevinOAuthTestRouter(handler)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/oauth-callback",
		strings.NewReader(`{"provider":"xai","state":"devin-state","code":"code"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("provider mismatch status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(authDir, ".oauth-devin-devin-state.oauth")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider mismatch wrote callback file: %v", err)
	}
}

func TestDevinOAuthSuccessDoesNotSaveWithCancelledClaimContext(t *testing.T) {
	isolateDevinOAuthSessions(t)
	authDir := t.TempDir()
	registerOAuthSessionForAuthDir("test-state", "devin", "", authDir)
	callbackData, err := json.Marshal(oauthCallbackFilePayload{State: "test-state", Code: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(authDir, ".oauth-devin-test-state.oauth"), callbackData, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	savedWithCancelledContext := false
	store := &contextRecordingDevinStore{onSave: func(ctx context.Context) {
		mu.Lock()
		savedWithCancelledContext = ctx.Err() != nil
		mu.Unlock()
	}}
	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: authDir},
		nil,
		withTokenStore(store),
	)
	t.Cleanup(func() { _ = handler.Close() })
	handler.completeDevinOAuth(
		context.Background(),
		authDir,
		"test-state",
		"verifier",
		&fakeDevinOAuthService{},
		nil,
	)
	_, status, completed, ok := getOAuthSessionDetails("test-state")
	if !ok || !completed || status != "" {
		t.Fatalf("session = status %q completed %v exists %v", status, completed, ok)
	}
	mu.Lock()
	cancelled := savedWithCancelledContext
	mu.Unlock()
	if cancelled {
		t.Fatal("save claim cancelled the persistence context")
	}
}

type contextRecordingDevinStore struct {
	onSave func(context.Context)
}

func (*contextRecordingDevinStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (store *contextRecordingDevinStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if store.onSave != nil {
		store.onSave(ctx)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return auth.Attributes[coreauth.AttributePath], nil
}
func (*contextRecordingDevinStore) Delete(context.Context, string) error { return nil }
