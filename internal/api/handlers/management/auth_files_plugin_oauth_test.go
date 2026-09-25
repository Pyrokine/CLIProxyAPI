package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type managementPluginOAuthHost struct {
	provider      string
	start         func(context.Context, string, string, map[string]any) (pluginapi.AuthLoginStartResponse, error)
	poll          func(context.Context, string, string, map[string]any) (pluginapi.AuthLoginPollResponse, error)
	startMetadata map[string]any
	pollMetadata  map[string]any
}

func (h *managementPluginOAuthHost) RegisteredPlugins() []pluginhost.RegisteredPluginInfo {
	return nil
}

func (h *managementPluginOAuthHost) PluginBusy(string) bool {
	return false
}

func (h *managementPluginOAuthHost) PluginRuntimeIdentity(string) pluginhost.PluginRuntimeIdentity {
	return pluginhost.PluginRuntimeIdentity{}
}

func (h *managementPluginOAuthHost) UnloadPluginWithError(string) (bool, error) {
	return false, nil
}

func (h *managementPluginOAuthHost) UnloadPluginIfCurrentWithError(pluginhost.PluginRuntimeIdentity) (bool, error) {
	return false, nil
}

func (h *managementPluginOAuthHost) HasAuthProvider(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), h.provider)
}

func (h *managementPluginOAuthHost) StartLogin(
	ctx context.Context,
	provider,
	baseURL string,
	metadata ...map[string]any,
) (pluginapi.AuthLoginStartResponse, bool, error) {
	if !h.HasAuthProvider(provider) {
		return pluginapi.AuthLoginStartResponse{}, false, nil
	}
	if len(metadata) > 0 {
		h.startMetadata = cloneOAuthSessionMetadata(metadata[0])
	}
	if h.start == nil {
		return pluginapi.AuthLoginStartResponse{}, true, nil
	}
	response, err := h.start(ctx, provider, baseURL, h.startMetadata)
	return response, true, err
}

func (h *managementPluginOAuthHost) PollLogin(
	ctx context.Context,
	provider,
	state string,
	metadata ...map[string]any,
) (pluginapi.AuthLoginPollResponse, bool, error) {
	if !h.HasAuthProvider(provider) {
		return pluginapi.AuthLoginPollResponse{}, false, nil
	}
	if len(metadata) > 0 {
		h.pollMetadata = cloneOAuthSessionMetadata(metadata[0])
	}
	if h.poll == nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, true, nil
	}
	response, err := h.poll(ctx, provider, state, h.pollMetadata)
	return response, true, err
}

func (h *managementPluginOAuthHost) AuthDataToCoreAuth(
	data pluginapi.AuthData,
	path,
	fileName string,
) *coreauth.Auth {
	return pluginhost.New().AuthDataToCoreAuth(data, path, fileName)
}

type pluginOAuthFileStore struct {
	mu        sync.Mutex
	failAt    int
	saves     int
	persisted map[string][]byte
}

func (s *pluginOAuthFileStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *pluginOAuthFileStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", errors.New("auth is nil")
	}
	path := strings.TrimSpace(auth.Attributes[coreauth.AttributePath])
	if path == "" {
		return "", errors.New("auth path is empty")
	}
	payload := []byte(`{"type":"` + auth.Provider + `"}`)
	if rawStorage, ok := auth.Storage.(interface{ RawJSON() []byte }); ok && len(rawStorage.RawJSON()) > 0 {
		payload = rawStorage.RawJSON()
	} else if auth.Metadata != nil {
		encoded, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", errMarshal
		}
		payload = encoded
	}
	if errWrite := os.WriteFile(path, payload, 0o600); errWrite != nil {
		return path, errWrite
	}
	s.mu.Lock()
	s.saves++
	if s.persisted != nil {
		s.persisted[path] = bytes.Clone(payload)
	}
	shouldFail := s.failAt > 0 && s.saves == s.failAt
	s.mu.Unlock()
	if shouldFail {
		return path, errors.New("injected save failure")
	}
	return path, nil
}

func (s *pluginOAuthFileStore) Delete(_ context.Context, id string) error {
	if errRemove := os.Remove(id); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		return errRemove
	}
	s.mu.Lock()
	delete(s.persisted, id)
	s.mu.Unlock()
	return nil
}

func TestServePluginAuthURLPreservesQueryMetadataAndSessionNonce(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })

	authDir := t.TempDir()
	host := &managementPluginOAuthHost{provider: "custom-sso"}
	host.start = func(
		_ context.Context,
		provider,
		baseURL string,
		metadata map[string]any,
	) (pluginapi.AuthLoginStartResponse, error) {
		if provider != "custom-sso" {
			t.Fatalf("provider = %q", provider)
		}
		if baseURL != "http://127.0.0.1:8317/v0/management/oauth-callback" {
			t.Fatalf("baseURL = %q", baseURL)
		}
		return pluginapi.AuthLoginStartResponse{
			Provider: "custom-sso",
			URL:      "https://login.example.test",
			State:    "custom-state",
			Metadata: map[string]any{
				"exchange": "poll-token",
				"nested":   map[string]any{"step": "one"},
			},
		}, nil
	}
	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: authDir, Port: 8317},
		nil,
		WithPluginHost(nil),
	)
	t.Cleanup(func() { _ = handler.Close() })
	handler.pluginHost = host

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/custom-sso-auth-url?nonce=browser-nonce&single=one&scopes=read&scopes=write",
		nil,
	)
	if !handler.ServePluginAuthURL(ctx) {
		t.Fatal("ServePluginAuthURL() = false")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := host.startMetadata["single"]; got != "one" {
		t.Fatalf("caller metadata single mutated: %#v", got)
	}
	if got, ok := host.startMetadata["scopes"].([]string); !ok || len(got) != 2 || got[0] != "read" || got[1] != "write" {
		t.Fatalf("caller metadata scopes = %#v", host.startMetadata["scopes"])
	}
	session, okSession := getOAuthSessionIntegrationDetails("custom-state")
	if !okSession || session.Source != oauthSessionSourcePlugin || session.AuthDir != authDir {
		t.Fatalf("session = %#v ok=%t", session, okSession)
	}
	if session.Metadata["exchange"] != "poll-token" || !verifyOAuthNonce("custom-state", "browser-nonce") {
		t.Fatalf("session metadata/nonce = %#v", session)
	}
	nested := session.Metadata["nested"].(map[string]any)
	nested["step"] = "mutated"
	fresh, _ := getOAuthSessionIntegrationDetails("custom-state")
	if fresh.Metadata["nested"].(map[string]any)["step"] != "one" {
		t.Fatalf("session metadata was not defensively copied: %#v", fresh.Metadata)
	}
}

func TestPluginOAuthPollPersistsMultipleRecordsOnce(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })

	authDir := t.TempDir()
	pollCalls := 0
	host := &managementPluginOAuthHost{provider: "custom-sso"}
	host.poll = func(
		_ context.Context,
		provider,
		state string,
		metadata map[string]any,
	) (pluginapi.AuthLoginPollResponse, error) {
		pollCalls++
		if provider != "custom-sso" || state != "custom-state" || metadata["exchange"] != "poll-token" {
			t.Fatalf("poll request provider=%q state=%q metadata=%#v", provider, state, metadata)
		}
		return pluginapi.AuthLoginPollResponse{
			Status: pluginapi.AuthLoginStatusSuccess,
			Auths: []pluginapi.AuthData{
				{
					FileName:    "custom-a.json",
					StorageJSON: []byte(`{"type":"custom-sso","account":"a"}`),
				},
				{
					Provider:    "custom-sso",
					FileName:    "custom-b.json",
					StorageJSON: []byte(`{"type":"custom-sso","account":"b"}`),
				},
			},
		}, nil
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	t.Cleanup(func() { _ = handler.Close() })
	handler.pluginHost = host
	handler.tokenStore = &pluginOAuthFileStore{}
	if errRegister := registerPluginOAuthSession(
		"custom-state",
		"custom-sso",
		"browser-nonce",
		authDir,
		map[string]any{"exchange": "poll-token"},
	); errRegister != nil {
		t.Fatalf("registerPluginOAuthSession() error = %v", errRegister)
	}

	wrongNonce := performOAuthSessionRequest(
		t,
		http.MethodGet,
		"/?state=custom-state&nonce=wrong",
		handler.GetAuthStatus,
	)
	if wrongNonce.Code != http.StatusUnauthorized || pollCalls != 0 {
		t.Fatalf("wrong nonce status=%d pollCalls=%d", wrongNonce.Code, pollCalls)
	}

	response := performOAuthSessionRequest(
		t,
		http.MethodGet,
		"/?state=custom-state&nonce=browser-nonce",
		handler.GetAuthStatus,
	)
	payload := decodeOAuthSessionResponse(t, response)
	if payload["status"] != "ok" || payload["phase"] != "success" || pollCalls != 1 {
		t.Fatalf("response=%#v pollCalls=%d", payload, pollCalls)
	}
	for _, fileName := range []string{"custom-a.json", "custom-b.json"} {
		if _, errStat := os.Stat(filepath.Join(authDir, fileName)); errStat != nil {
			t.Fatalf("credential %s was not persisted: %v", fileName, errStat)
		}
	}

	response = performOAuthSessionRequest(
		t,
		http.MethodGet,
		"/?state=custom-state&nonce=browser-nonce",
		handler.GetAuthStatus,
	)
	payload = decodeOAuthSessionResponse(t, response)
	if payload["status"] != "ok" || pollCalls != 1 {
		t.Fatalf("completed response=%#v pollCalls=%d", payload, pollCalls)
	}
}

func TestPluginOAuthSaveFailureRestoresAllTargets(t *testing.T) {
	authDir := t.TempDir()
	firstPath := filepath.Join(authDir, "first.json")
	oldPayload := []byte(`{"type":"custom-sso","value":"old"}`)
	if errWrite := os.WriteFile(firstPath, oldPayload, 0o600); errWrite != nil {
		t.Fatalf("write old credential: %v", errWrite)
	}

	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	t.Cleanup(func() { _ = handler.Close() })
	store := &pluginOAuthFileStore{
		failAt:    2,
		persisted: map[string][]byte{firstPath: bytes.Clone(oldPayload)},
	}
	handler.tokenStore = store
	records := []*coreauth.Auth{
		{
			Provider: "custom-sso",
			FileName: "first.json",
			Metadata: map[string]any{"type": "custom-sso", "value": "new"},
		},
		{
			Provider: "custom-sso",
			FileName: "second.json",
			Metadata: map[string]any{"type": "custom-sso", "value": "new"},
		},
	}
	if errSave := handler.saveClaimedPluginLoginRecords(context.Background(), authDir, records); errSave == nil {
		t.Fatal("saveClaimedPluginLoginRecords() error = nil")
	}
	gotFirst, errRead := os.ReadFile(firstPath)
	if errRead != nil {
		t.Fatalf("read restored credential: %v", errRead)
	}
	if string(gotFirst) != string(oldPayload) {
		t.Fatalf("restored first credential = %s, want %s", gotFirst, oldPayload)
	}
	secondPath := filepath.Join(authDir, "second.json")
	if _, errStat := os.Stat(secondPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("second credential remains after rollback: %v", errStat)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !bytes.Equal(store.persisted[firstPath], oldPayload) {
		t.Fatalf("durable first credential = %s, want restored payload", store.persisted[firstPath])
	}
	if _, exists := store.persisted[secondPath]; exists {
		t.Fatal("second credential remains in durable store after rollback")
	}
}

func TestPluginOAuthSaveFailureRestoresRuntimeAndDurableStore(t *testing.T) {
	authDir := t.TempDir()
	firstPath := filepath.Join(authDir, "first.json")
	oldPayload := []byte(`{"type":"custom-sso","value":"old"}`)
	if errWrite := os.WriteFile(firstPath, oldPayload, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := &pluginOAuthFileStore{
		failAt:    2,
		persisted: map[string][]byte{firstPath: bytes.Clone(oldPayload)},
	}
	manager := coreauth.NewManager(store, nil, nil)
	previous, _, errLoad := authFromFile(authDir, firstPath, oldPayload)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), previous); errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := NewHandler(&config.Config{AuthDir: authDir}, "", manager, withTokenStore(store))
	t.Cleanup(func() { _ = handler.Close() })
	records := []*coreauth.Auth{
		{Provider: "custom-sso", FileName: "first.json", Metadata: map[string]any{"type": "custom-sso", "value": "new"}},
		{Provider: "custom-sso", FileName: "second.json", Metadata: map[string]any{"type": "custom-sso", "value": "new"}},
	}
	if errSave := handler.saveClaimedPluginLoginRecords(context.Background(), authDir, records); errSave == nil ||
		!strings.Contains(errSave.Error(), "injected save failure") || strings.Contains(errSave.Error(), "runtime auth") ||
		strings.Contains(errSave.Error(), "ignored watcher mutation") {
		t.Fatalf("saveClaimedPluginLoginRecords() error = %v", errSave)
	}
	current, exists := manager.GetByID(previous.ID)
	if !exists || current == nil || current.Metadata["value"] != "old" {
		t.Fatalf("runtime credential was not restored: exists=%t", exists)
	}
	first, errRead := os.ReadFile(firstPath)
	if errRead != nil || !bytes.Equal(first, oldPayload) {
		t.Fatalf("local credential = %s, err=%v", first, errRead)
	}
	secondPath := filepath.Join(authDir, "second.json")
	if _, errStat := os.Stat(secondPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("second credential remains locally: %v", errStat)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !bytes.Equal(store.persisted[firstPath], oldPayload) {
		t.Fatal("durable credential was not restored")
	}
	if _, exists := store.persisted[secondPath]; exists {
		t.Fatal("second credential remains in durable store")
	}
}

func TestPluginOAuthRuntimeFailureRemovesNewCredential(t *testing.T) {
	authDir := t.TempDir()
	firstPath := filepath.Join(authDir, "first.json")
	oldPayload := []byte(`{"type":"custom-sso","value":"old"}`)
	if errWrite := os.WriteFile(firstPath, oldPayload, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := &pluginOAuthFileStore{persisted: map[string][]byte{firstPath: bytes.Clone(oldPayload)}}
	manager := coreauth.NewManager(store, nil, nil)
	previous, _, errLoad := authFromFile(authDir, firstPath, oldPayload)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), previous); errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := NewHandler(&config.Config{AuthDir: authDir}, "", manager, withTokenStore(store))
	t.Cleanup(func() { _ = handler.Close() })
	handler.SetPostAuthPersistHook(func(_ context.Context, auth *coreauth.Auth) error {
		if auth.FileName == "second.json" {
			return errors.New("injected runtime failure")
		}
		return nil
	})
	records := []*coreauth.Auth{
		{Provider: "custom-sso", FileName: "first.json", Metadata: map[string]any{"type": "custom-sso", "value": "new"}},
		{Provider: "custom-sso", FileName: "second.json", Metadata: map[string]any{"type": "custom-sso", "value": "new"}},
	}
	if errSave := handler.saveClaimedPluginLoginRecords(context.Background(), authDir, records); errSave == nil ||
		!strings.Contains(errSave.Error(), "injected runtime failure") || strings.Contains(errSave.Error(), "runtime auth") {
		t.Fatalf("saveClaimedPluginLoginRecords() error = %v", errSave)
	}
	current, exists := manager.GetByID(previous.ID)
	if !exists || current == nil || current.Metadata["value"] != "old" {
		t.Fatalf("previous runtime auth was not restored: exists=%t", exists)
	}
	if current, exists := manager.GetByID("second.json"); exists || current != nil {
		t.Fatalf("new runtime auth remains: %#v", current)
	}
	first, errRead := os.ReadFile(firstPath)
	if errRead != nil || !bytes.Equal(first, oldPayload) {
		t.Fatalf("local credential = %s, err=%v", first, errRead)
	}
	secondPath := filepath.Join(authDir, "second.json")
	if _, errStat := os.Stat(secondPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("new local credential remains: %v", errStat)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !bytes.Equal(store.persisted[firstPath], oldPayload) {
		t.Fatal("durable credential was not restored")
	}
	if _, exists := store.persisted[secondPath]; exists {
		t.Fatal("new durable credential remains")
	}
}

func TestPostOAuthCallbackAcceptsPluginProviderAndCancellationRemovesFile(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })

	authDir := t.TempDir()
	if errRegister := registerPluginOAuthSession("custom-state", "custom-sso", "", authDir, nil); errRegister != nil {
		t.Fatalf("registerPluginOAuthSession() error = %v", errRegister)
	}
	body := strings.NewReader(`{"provider":"custom-sso","state":"custom-state","code":"code-1"}`)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/oauth-callback", body)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	t.Cleanup(func() { _ = handler.Close() })
	handler.PostOAuthCallback(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PostOAuthCallback() status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	callbackPath := filepath.Join(authDir, ".oauth-custom-sso-custom-state.oauth")
	if _, errStat := os.Stat(callbackPath); errStat != nil {
		t.Fatalf("plugin callback file missing: %v", errStat)
	}
	if !cancelOAuthSession("custom-state") {
		t.Fatal("cancelOAuthSession() = false")
	}
	if _, errStat := os.Stat(callbackPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("plugin callback file remains after cancellation: %v", errStat)
	}
}

var _ pluginRuntimeHost = (*managementPluginOAuthHost)(nil)
var _ pluginOAuthHost = (*managementPluginOAuthHost)(nil)
