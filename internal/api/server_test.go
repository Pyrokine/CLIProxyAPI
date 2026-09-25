package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
	claudemodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/claude/models"
	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

func newTestServer(t *testing.T) *Server {
	return newTestServerWithOptions(t)
}

func newTestServerWithOptions(t *testing.T, opts ...ServerOption) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	t.Setenv("WRITABLE_PATH", tmpDir)
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		APIKeys:                []string{"test-key"},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	server := NewServer(cfg, authManager, accessManager, configPath, opts...)
	t.Cleanup(func() { _ = server.mgmt.Close() })
	return server
}

func TestClaudeModelListCloakingConfigHotReload(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-claude-model-list-cloaking-hot-reload"
	modelID := "gpt-model-list-hot-reload"
	modelRegistry.RegisterClient(
		clientID, "claude", []*registry.ModelInfo{
			{
				ID: modelID, Object: "model", DisplayName: "Model List Hot Reload",
			},
		},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	server := newTestServer(t)
	assertModelID := func(want string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("User-Agent", "claude-cli/test")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		for _, model := range gjson.Get(recorder.Body.String(), "data").Array() {
			if model.Get("id").String() == want {
				return
			}
		}
		t.Fatalf("model %q not found in response: %s", want, recorder.Body.String())
	}

	assertModelID(claudemodels.ClientModelID(modelID))
	updatedCfg := *server.configSnapshot()
	updatedCfg.ClaudeCode.DisableCloakingModelList = true
	server.UpdateClients(&updatedCfg)
	assertModelID(modelID)
}

const artifactTestPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

type imageArtifactIntegrationExecutor struct{}

func (*imageArtifactIntegrationExecutor) Identifier() string { return "image-artifact-integration" }

func (*imageArtifactIntegrationExecutor) Execute(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{
		Payload: []byte(`{"created":1,"data":[{"b64_json":"` + artifactTestPNGBase64 + `"}]}`),
	}, nil
}

func (*imageArtifactIntegrationExecutor) ExecuteStream(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (*imageArtifactIntegrationExecutor) Refresh(_ context.Context, current *auth.Auth) (*auth.Auth, error) {
	return current, nil
}

func (*imageArtifactIntegrationExecutor) CountTokens(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*imageArtifactIntegrationExecutor) HttpRequest(
	context.Context,
	*auth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestServerStopContinuesResourceCleanupAfterHTTPShutdownError(t *testing.T) {
	server := newTestServer(t)
	server.quotaScheduler.Stop()
	server.quotaScheduler = quota.NewScheduler(quota.Config{Enabled: true, Interval: 60}, nil)
	server.quotaScheduler.Start()
	server.imageArtifacts.ApplyCacheConfig(artifact.CacheConfig{CleanupInterval: time.Hour})
	server.imageArtifacts.StartCleaner()

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	server.server = &http.Server{
		Handler: http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {
				close(requestStarted)
				<-releaseRequest
			},
		),
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.server.Serve(listener)
	}()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, errGet := http.Get("http://" + listener.Addr().String())
		if errGet == nil {
			_ = response.Body.Close()
		}
	}()
	<-requestStarted

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	errStop := server.Stop(ctx)
	close(releaseRequest)
	<-clientDone
	<-serveDone
	if !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want deadline exceeded", errStop)
	}
	if cancelField := reflect.ValueOf(server.quotaScheduler).Elem().FieldByName("cancel"); !cancelField.IsNil() {
		t.Fatal("quota scheduler remained active after HTTP shutdown error")
	}
	if cleanerStop := reflect.ValueOf(server.imageArtifacts).Elem().FieldByName("cleanerStop"); !cleanerStop.IsNil() {
		t.Fatal("artifact cleaner remained active after HTTP shutdown error")
	}
}

func TestServerBeginShutdownClosesRuntimeStateDuringConfigUpdate(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	queue := redisqueue.NewQueue()
	server := newTestServerWithOptions(t, WithUsageQueue(queue))

	next, errClone := proxyconfig.Clone(server.configSnapshot())
	if errClone != nil {
		t.Fatalf("clone config: %v", errClone)
	}
	next.RequestLog = true
	updateStarted := make(chan struct{})
	releaseUpdate := make(chan struct{})
	server.loggerToggle = func(bool) {
		close(updateStarted)
		<-releaseUpdate
	}
	updateDone := make(chan struct{})
	go func() {
		server.UpdateClients(next)
		close(updateDone)
	}()
	<-updateStarted

	beginDone := make(chan struct{})
	go func() {
		server.BeginShutdown()
		close(beginDone)
	}()
	select {
	case <-beginDone:
	case <-time.After(time.Second):
		t.Fatal("BeginShutdown() blocked on the active config update")
	}
	if queue.Enabled() {
		t.Fatal("BeginShutdown() did not close the usage queue")
	}
	if server.managementRoutesEnabled.load() {
		t.Fatal("BeginShutdown() did not close management routes")
	}
	close(releaseUpdate)
	select {
	case <-updateDone:
	case <-time.After(2 * time.Second):
		t.Fatal("config update did not complete")
	}
	select {
	case <-beginDone:
	case <-time.After(2 * time.Second):
		t.Fatal("BeginShutdown() did not complete")
	}
	if queue.Enabled() {
		t.Fatal("usage queue was enabled after BeginShutdown()")
	}

	retry, errClone := proxyconfig.Clone(next)
	if errClone != nil {
		t.Fatalf("clone retry config: %v", errClone)
	}
	retry.Home.Enabled = true
	server.UpdateClients(retry)
	if queue.Enabled() {
		t.Fatal("post-shutdown config update re-enabled the usage queue")
	}
	if server.configSnapshot() == retry {
		t.Fatal("post-shutdown config update was published")
	}
}

func TestManagementConfigCommitUpdatesServerWithoutDeadlock(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodPut, "/v0/management/debug", strings.NewReader(`{"value":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-management-password")
	req.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.engine.ServeHTTP(recorder, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("management config commit deadlocked while updating the server")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if server.cfg == nil || server.cfg.Debug {
		t.Fatalf("server config = %#v, want debug disabled", server.cfg)
	}
	persisted, err := proxyconfig.LoadConfig(server.configFilePath)
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if persisted.Debug {
		t.Fatal("persisted config did not contain the management update")
	}
}

func TestOAuthGETCallbackUsesSessionAuthDirectory(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	server := newTestServer(t)
	authDirAtStart := server.cfg.AuthDir

	authRequest := httptest.NewRequest(http.MethodGet, "/v0/management/codex-auth-url", nil)
	authRequest.Header.Set("Authorization", "Bearer test-management-password")
	authRequest.RemoteAddr = "127.0.0.1:12345"
	authRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(authRecorder, authRequest)
	if authRecorder.Code != http.StatusOK {
		t.Fatalf("auth URL status = %d, body = %s", authRecorder.Code, authRecorder.Body.String())
	}
	var authResponse struct {
		State string `json:"state"`
	}
	if errDecode := json.Unmarshal(authRecorder.Body.Bytes(), &authResponse); errDecode != nil {
		t.Fatalf("decode auth URL response: %v", errDecode)
	}
	if authResponse.State == "" {
		t.Fatal("auth URL response has empty state")
	}

	currentAuthDir := t.TempDir()
	next, errClone := proxyconfig.Clone(server.cfg)
	if errClone != nil {
		t.Fatalf("clone server config: %v", errClone)
	}
	next.AuthDir = currentAuthDir
	server.UpdateClients(next)

	callbackPath := "/codex/callback?state=" + url.QueryEscape(authResponse.State) + "&error=access_denied"
	callbackRequest := httptest.NewRequest(http.MethodGet, callbackPath, nil)
	callbackRequest.RemoteAddr = "203.0.113.20:12345"
	callbackRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(callbackRecorder, callbackRequest)
	if callbackRecorder.Code != http.StatusOK {
		t.Fatalf("callback status = %d, body = %s", callbackRecorder.Code, callbackRecorder.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		statusRequest := httptest.NewRequest(
			http.MethodGet,
			"/v0/management/get-auth-status?state="+url.QueryEscape(authResponse.State),
			nil,
		)
		statusRequest.Header.Set("Authorization", "Bearer test-management-password")
		statusRequest.RemoteAddr = "127.0.0.1:12345"
		statusRecorder := httptest.NewRecorder()
		server.engine.ServeHTTP(statusRecorder, statusRequest)
		if statusRecorder.Code != http.StatusOK {
			t.Fatalf("auth status = %d, body = %s", statusRecorder.Code, statusRecorder.Body.String())
		}
		var statusResponse struct {
			Status string `json:"status"`
		}
		if errDecode := json.Unmarshal(statusRecorder.Body.Bytes(), &statusResponse); errDecode != nil {
			t.Fatalf("decode auth status response: %v", errDecode)
		}
		if statusResponse.Status == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("OAuth waiter did not consume callback from session auth directory %s", authDirAtStart)
		}
		time.Sleep(20 * time.Millisecond)
	}
	callbackFile := filepath.Join(currentAuthDir, ".oauth-codex-"+authResponse.State+".oauth")
	if _, errStat := os.Stat(callbackFile); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("callback file written to current auth directory: %v", errStat)
	}
}

func TestOAuthGETCallbackRejectsUnknownSession(t *testing.T) {
	server := newTestServer(t)
	request := httptest.NewRequest(
		http.MethodGet,
		"/codex/callback?state=missing&code=callback-code",
		nil,
	)
	request.RemoteAddr = "203.0.113.21:12345"
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("callback status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	if recorder.Body.String() != oauthCallbackFailureHTML {
		t.Fatalf("callback body = %q, want failure page", recorder.Body.String())
	}
}

func TestServerUpdateClientsPreservesLocalManagementPassword(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	const localPassword = "runtime-local-password"
	server := newTestServerWithOptions(t, WithLocalManagementPassword(localPassword))
	t.Cleanup(func() { _ = server.mgmt.Close() })
	next, err := proxyconfig.Clone(server.cfg)
	if err != nil {
		t.Fatalf("clone server config: %v", err)
	}
	next.Debug = false
	server.UpdateClients(next)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	req.Header.Set("Authorization", "Bearer "+localPassword)
	req.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !server.managementRoutesEnabled.load() {
		t.Fatal("management routes disabled after config update with local password")
	}
}

func TestRefreshModelClientsDoesNotPublishConfig(t *testing.T) {
	server := newTestServer(t)
	originalCfg := server.cfg
	originalState := server.ManagementHandler().ConfigState()
	next, errClone := proxyconfig.Clone(originalCfg)
	if errClone != nil {
		t.Fatalf("clone config: %v", errClone)
	}
	next.Debug = !originalCfg.Debug

	server.RefreshModelClients(next)

	if server.cfg != originalCfg {
		t.Fatal("model client refresh replaced the server config")
	}
	currentState := server.ManagementHandler().ConfigState()
	if currentState.Config != originalState.Config || currentState.Generation != originalState.Generation {
		t.Fatalf("management config state changed: before=%#v after=%#v", originalState, currentState)
	}
}

func TestModelCatalogCommitKeepsCanonicalServerConfig(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	var (
		server             *Server
		modelRuntimeCalls  int
		modelRuntimeConfig *proxyconfig.Config
	)
	server = newTestServerWithOptions(
		t,
		WithManagementConfigCommitted(
			func(ctx context.Context, commit ManagementConfigCommit) error {
				server.UpdateClientsForConfigCommit(ctx, commit.Config, commit.Generation)
				return nil
			},
		),
		WithModelCatalogRuntimeSync(
			func(cfg *proxyconfig.Config) error {
				modelRuntimeCalls++
				modelRuntimeConfig = cfg
				return nil
			},
		),
	)

	req := httptest.NewRequest(
		http.MethodPut,
		"/v0/management/model-prices",
		strings.NewReader(`{"prices":{"canonical-model":{"prompt":3,"completion":4,"cache":1}}}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-management-password")
	req.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if modelRuntimeCalls != 1 {
		t.Fatalf("model runtime calls = %d, want 1", modelRuntimeCalls)
	}
	state := server.ManagementHandler().ConfigState()
	if state.Config != server.cfg {
		t.Fatalf("management config = %p, server config = %p", state.Config, server.cfg)
	}
	if modelRuntimeConfig != server.cfg {
		t.Fatalf("model runtime config = %p, server config = %p", modelRuntimeConfig, server.cfg)
	}
}

func TestManagementConfigCommitUsesConfiguredCallback(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	var (
		server *Server
		calls  int
	)
	server = newTestServerWithOptions(
		t,
		WithManagementConfigCommitted(
			func(ctx context.Context, commit ManagementConfigCommit) error {
				calls++
				server.UpdateClientsForConfigCommit(ctx, commit.Config, commit.Generation)
				return nil
			},
		),
	)

	req := httptest.NewRequest(http.MethodPut, "/v0/management/debug", strings.NewReader(`{"value":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-management-password")
	req.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls != 1 {
		t.Fatalf("management config callback calls = %d, want 1", calls)
	}
	if server.cfg == nil || server.cfg.Debug {
		t.Fatalf("server config = %#v, want debug disabled", server.cfg)
	}
}

func TestServerUpdateClientsPublishesRequestLoggingGateInSafeOrder(t *testing.T) {
	server := newTestServer(t)
	type toggleRecord struct {
		enabled bool
		gate    bool
	}
	var records []toggleRecord
	server.loggerToggle = func(enabled bool) {
		records = append(
			records, toggleRecord{
				enabled: enabled,
				gate:    server.requestLoggingRuntimeEnabled.Load(),
			},
		)
	}

	enabledConfig, errClone := proxyconfig.Clone(server.cfg)
	if errClone != nil {
		t.Fatalf("clone enabled config: %v", errClone)
	}
	enabledConfig.RequestLog = true
	server.UpdateClients(enabledConfig)
	records = nil

	commercialConfig, errClone := proxyconfig.Clone(server.cfg)
	if errClone != nil {
		t.Fatalf("clone commercial config: %v", errClone)
	}
	commercialConfig.CommercialMode = true
	server.UpdateClients(commercialConfig)
	if len(records) != 1 || records[0].enabled || records[0].gate {
		t.Fatalf("commercial-mode toggle records = %#v", records)
	}
	if server.requestLoggingRuntimeEnabled.Load() {
		t.Fatal("request logging runtime gate remained enabled in commercial mode")
	}

	records = nil
	normalConfig, errClone := proxyconfig.Clone(server.cfg)
	if errClone != nil {
		t.Fatalf("clone normal config: %v", errClone)
	}
	normalConfig.CommercialMode = false
	server.UpdateClients(normalConfig)
	if len(records) != 1 || !records[0].enabled || records[0].gate {
		t.Fatalf("normal-mode toggle records = %#v", records)
	}
	if !server.requestLoggingRuntimeEnabled.Load() {
		t.Fatal("request logging runtime gate was not enabled after logger activation")
	}
}

func TestServerUpdateClientsDoesNotPublishManagementCommit(t *testing.T) {
	server := newTestServer(t)
	next, err := proxyconfig.Clone(server.cfg)
	if err != nil {
		t.Fatalf("clone server config: %v", err)
	}
	next.Debug = false

	done := make(chan struct{})
	go func() {
		server.UpdateClients(next)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("external server config update re-entered the management commit callback")
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/debug", nil)
	server.mgmt.GetDebug(ginCtx)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"debug":true}` {
		t.Fatalf("management config response = %d %s", recorder.Code, recorder.Body.String())
	}
	if _, err = os.Stat(server.configFilePath); !os.IsNotExist(err) {
		t.Fatalf("external server config update persisted management config: %v", err)
	}
}

func TestImageGenerationResponseURLDownloadsCreatorArtifact(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}, "other-key": {}}},
		},
	)

	executor := &imageArtifactIntegrationExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	upstreamAuth := &auth.Auth{
		ID:       "image-artifact-integration-auth",
		Provider: executor.Identifier(),
		Status:   auth.StatusActive,
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), upstreamAuth); err != nil {
		t.Fatalf("Register image auth: %v", err)
	}
	const modelName = "image-artifact-integration-model"
	registry.GetGlobalRegistry().RegisterClient(
		upstreamAuth.ID,
		upstreamAuth.Provider,
		[]*registry.ModelInfo{{ID: modelName, Type: registry.OpenAIImageModelType}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(upstreamAuth.ID)
		},
	)

	generateReq := httptest.NewRequest(
		http.MethodPost,
		"https://cpa.test/v1/images/generations",
		strings.NewReader(`{"model":"`+modelName+`","prompt":"draw","response_format":"b64_json"}`),
	)
	generateReq.Header.Set("Authorization", "Bearer valid-key")
	generateReq.Header.Set("Content-Type", "application/json")
	generateReq.RemoteAddr = "10.0.0.1:12345"
	generateRec := httptest.NewRecorder()
	server.engine.ServeHTTP(generateRec, generateReq)
	if generateRec.Code != http.StatusOK {
		t.Fatalf("generation status = %d, want 200; body=%s", generateRec.Code, generateRec.Body.String())
	}

	data, err := base64.StdEncoding.DecodeString(artifactTestPNGBase64)
	if err != nil {
		t.Fatalf("DecodeString test PNG: %v", err)
	}
	sum := sha256.Sum256(data)
	wantSHA256 := hex.EncodeToString(sum[:])
	responseBody := generateRec.Body.Bytes()
	if got := gjson.GetBytes(responseBody, "data.0.b64_json").String(); got != artifactTestPNGBase64 {
		t.Fatalf("generation b64_json differs from provider output; body=%s", responseBody)
	}
	if got := gjson.GetBytes(responseBody, "data.0.sha256").String(); got != wantSHA256 {
		t.Fatalf("generation sha256 = %q, want %q; body=%s", got, wantSHA256, responseBody)
	}
	artifactID := gjson.GetBytes(responseBody, "data.0.artifact_id").String()
	if err = artifact.ValidateID(artifactID); err != nil {
		t.Fatalf("generation artifact_id = %q: %v", artifactID, err)
	}
	contentURL := gjson.GetBytes(responseBody, "data.0.content_url").String()
	parsedContentURL, err := url.Parse(contentURL)
	if err != nil {
		t.Fatalf("Parse content URL: %v", err)
	}
	if parsedContentURL.Scheme != "https" || parsedContentURL.Host != "cpa.test" || parsedContentURL.Query().Get("access") == "" {
		t.Fatalf("generation content_url = %q, want authenticated CPA HTTPS URL", contentURL)
	}

	otherReq := httptest.NewRequest(http.MethodGet, parsedContentURL.RequestURI(), nil)
	otherReq.Header.Set("Authorization", "Bearer other-key")
	otherReq.RemoteAddr = "10.0.0.2:12345"
	otherRec := httptest.NewRecorder()
	server.engine.ServeHTTP(otherRec, otherReq)
	if otherRec.Code != http.StatusForbidden {
		t.Fatalf("other principal download status = %d, want 403; body=%s", otherRec.Code, otherRec.Body.String())
	}

	downloadReq := httptest.NewRequest(http.MethodGet, parsedContentURL.RequestURI(), nil)
	downloadReq.Header.Set("Authorization", "Bearer valid-key")
	downloadReq.RemoteAddr = "10.0.0.1:12345"
	downloadRec := httptest.NewRecorder()
	server.engine.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusOK {
		t.Fatalf("creator download status = %d, want 200; body=%s", downloadRec.Code, downloadRec.Body.String())
	}
	if got := downloadRec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("creator download Content-Type = %q, want image/png", got)
	}
	if got := downloadRec.Header().Get("ETag"); got != `"`+wantSHA256+`"` {
		t.Fatalf("creator download ETag = %q, want sha256 ETag", got)
	}
	if !bytes.Equal(downloadRec.Body.Bytes(), data) {
		t.Fatal("creator download body differs from generated image")
	}
}

func TestImageArtifactContentRequiresCreatorPrincipalAndServesStoredImage(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}, "other-key": {}}},
		},
	)
	data, err := base64.StdEncoding.DecodeString(artifactTestPNGBase64)
	if err != nil {
		t.Fatalf("DecodeString test PNG: %v", err)
	}
	record, err := server.imageArtifacts.PutBytes(context.Background(), data, "image/png")
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	urlRecorder := httptest.NewRecorder()
	urlContext, _ := gin.CreateTestContext(urlRecorder)
	urlContext.Request = httptest.NewRequest(http.MethodPost, "https://cpa.test/v1/images/generations", nil)
	urlContext.Set("apiKey", "valid-key")
	contentURL, err := server.ImageArtifactContentURL(urlContext, toImageArtifactRecord(record))
	if err != nil {
		t.Fatalf("ImageArtifactContentURL: %v", err)
	}
	parsedContentURL, err := url.Parse(contentURL)
	if err != nil {
		t.Fatalf("Parse content URL: %v", err)
	}
	if parsedContentURL.Query().Get("access") == "" {
		t.Fatalf("content URL missing creator access token: %s", contentURL)
	}
	contentPath := parsedContentURL.RequestURI()

	unauthenticated := httptest.NewRequest(http.MethodGet, contentPath, nil)
	unauthenticated.RemoteAddr = "10.0.0.1:12345"
	unauthenticatedRec := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthenticatedRec, unauthenticated)
	if unauthenticatedRec.Code != http.StatusUnauthorized {
		t.Fatalf(
			"unauthenticated status = %d, want 401; body=%s", unauthenticatedRec.Code, unauthenticatedRec.Body.String(),
		)
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
		apiKey string
		want   int
	}{
		{
			name: "missing creator token", method: http.MethodGet, path: parsedContentURL.Path, apiKey: "valid-key",
			want: http.StatusForbidden,
		},
		{
			name: "different principal", method: http.MethodGet, path: contentPath, apiKey: "other-key",
			want: http.StatusForbidden,
		},
		{
			name: "creator principal", method: http.MethodGet, path: contentPath, apiKey: "valid-key",
			want: http.StatusOK,
		},
		{
			name: "creator principal head", method: http.MethodHead, path: contentPath, apiKey: "valid-key",
			want: http.StatusOK,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("Authorization", "Bearer "+tc.apiKey)
				req.RemoteAddr = "10.0.0.1:12345"
				rec := httptest.NewRecorder()
				server.engine.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
				}
				if tc.want != http.StatusOK {
					return
				}
				if got := rec.Header().Get("Content-Type"); got != "image/png" {
					t.Fatalf("Content-Type = %q, want image/png", got)
				}
				if got := rec.Header().Get("ETag"); got != `"`+record.SHA256+`"` {
					t.Fatalf("ETag = %q, want sha256 ETag", got)
				}
				if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
					t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
				}
				if got := rec.Header().Get("Content-Disposition"); !strings.Contains(
					got, "inline",
				) || !strings.Contains(got, record.ID) {
					t.Fatalf("Content-Disposition = %q, want inline filename", got)
				}
				if tc.method == http.MethodGet && !bytes.Equal(rec.Body.Bytes(), data) {
					t.Fatal("response body differs from stored image")
				}
			},
		)
	}
}

func TestImageArtifactContentURLRequiresStoreAndSigner(t *testing.T) {
	signer, err := artifact.NewAccessSigner("instance", sha256.Sum256([]byte("test-key")))
	if err != nil {
		t.Fatalf("NewAccessSigner: %v", err)
	}
	server := &Server{imageArtifactSigner: signer}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "https://cpa.test/v1/images/generations", nil)
	ginCtx.Set("apiKey", "valid-key")

	if _, err = server.ImageArtifactContentURL(ginCtx, toImageArtifactRecord(artifact.Record{})); err == nil {
		t.Fatal("content URL was generated without an artifact store")
	}
}

func TestImageArtifactContentURLTrustsForwardedProtoOnlyFromTrustedProxy(t *testing.T) {
	server := newTestServer(t)
	server.cfg.TLS.TrustForwardedProto = true
	server.cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	record := toImageArtifactRecord(
		artifact.Record{
			ID: "img_" + strings.Repeat("a", sha256.Size*2) + ".png",
		},
	)

	for _, tc := range []struct {
		name       string
		remoteAddr string
		forwarded  string
		wantScheme string
	}{
		{
			name:       "untrusted peer",
			remoteAddr: "203.0.113.42:12345",
			wantScheme: "http",
		},
		{
			name:       "trusted proxy",
			remoteAddr: "127.0.0.1:12345",
			forwarded:  "198.51.100.42",
			wantScheme: "https",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ginCtx, _ := gin.CreateTestContext(recorder)
				ginCtx.Request = httptest.NewRequest(
					http.MethodPost,
					"http://cpa.test/v1/images/generations",
					nil,
				)
				ginCtx.Request.RemoteAddr = tc.remoteAddr
				ginCtx.Request.Header.Set("X-Forwarded-Proto", "https")
				if tc.forwarded != "" {
					ginCtx.Request.Header.Set("X-Forwarded-For", tc.forwarded)
				}
				ginCtx.Set("apiKey", "valid-key")

				contentURL, err := server.ImageArtifactContentURL(ginCtx, record)
				if err != nil {
					t.Fatalf("ImageArtifactContentURL: %v", err)
				}
				parsed, err := url.Parse(contentURL)
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if parsed.Scheme != tc.wantScheme {
					t.Fatalf("scheme = %q, want %q", parsed.Scheme, tc.wantScheme)
				}
			},
		)
	}
}

func TestArtifactCapabilityDisabledWhenSigningKeyCannotLoad(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writable := t.TempDir()
	t.Setenv("WRITABLE_PATH", writable)
	if err := os.WriteFile(filepath.Join(writable, "management-state"), []byte("blocked"), 0o600); err != nil {
		t.Fatalf("WriteFile management-state blocker: %v", err)
	}
	cfg := &proxyconfig.Config{
		APIKeys:                []string{"test-key"},
		AuthDir:                filepath.Join(writable, "auth"),
		UsageStatisticsEnabled: false,
	}
	server := NewServer(
		cfg,
		auth.NewManager(nil, nil, nil),
		sdkaccess.NewManager(),
		filepath.Join(writable, "config.yaml"),
	)
	t.Cleanup(func() { _ = server.mgmt.Close() })
	if server.imageArtifacts != nil || server.imageArtifactSigner != nil {
		t.Fatalf(
			"artifact capability remained enabled: store=%p signer=%p", server.imageArtifacts,
			server.imageArtifactSigner,
		)
	}
}

func TestImageArtifactContentRejectsInvalidAndMissingIDs(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)

	missingID := "img_" + strings.Repeat("a", 64) + ".png"
	missingToken, err := server.imageArtifactSigner.Sign("valid-key", missingID)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/v1/artifacts/../secret.png/content", want: http.StatusNotFound},
		{path: "/v1/artifacts/img_nothex.png/content", want: http.StatusBadRequest},
		{
			path: "/v1/artifacts/" + missingID + "/content?access=" + url.QueryEscape(missingToken),
			want: http.StatusNotFound,
		},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer valid-key")
		req.RemoteAddr = "10.0.0.1:12345"
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("GET %s status = %d, want %d; body=%s", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestPutConfigYAMLReportsArtifactCleanupFailureAfterCommit(t *testing.T) {
	server := newTestServer(t)
	server.imageArtifacts.StopCleaner()
	t.Cleanup(server.imageArtifacts.CloseCleaner)
	root := server.imageArtifacts.Root()
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll artifact root: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatalf("MkdirAll artifact parent: %v", err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile artifact root blocker: %v", err)
	}

	next, err := proxyconfig.Clone(server.cfg)
	if err != nil {
		t.Fatalf("Clone config: %v", err)
	}
	next.ImageArtifactCache.RetentionDays++
	body, err := yaml.Marshal(next)
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", bytes.NewReader(body))
	ginCtx.Request.Header.Set("Content-Type", "application/yaml")
	server.mgmt.PutConfigYAML(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if cleanupError := gjson.GetBytes(recorder.Body.Bytes(), "cleanup_error").String(); cleanupError == "" {
		t.Fatalf("response missing cleanup_error: %s", recorder.Body.String())
	}
	if server.cfg.ImageArtifactCache.RetentionDays != next.ImageArtifactCache.RetentionDays {
		t.Fatalf(
			"runtime config retention_days = %d, want %d", server.cfg.ImageArtifactCache.RetentionDays,
			next.ImageArtifactCache.RetentionDays,
		)
	}
}

func TestPutImageArtifactCacheReportsCallbackCleanupFailure(t *testing.T) {
	server := newTestServer(t)
	server.imageArtifacts.StopCleaner()
	t.Cleanup(server.imageArtifacts.CloseCleaner)
	root := server.imageArtifacts.Root()
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll artifact root: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatalf("MkdirAll artifact parent: %v", err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile artifact root blocker: %v", err)
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPut,
		"/image-artifact-cache",
		strings.NewReader(`{"retention_days":5,"max_total_size_mb":1}`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	server.mgmt.PutImageArtifactCache(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if cleanupError := gjson.GetBytes(recorder.Body.Bytes(), "cleanup_error").String(); cleanupError == "" {
		t.Fatalf("response missing cleanup_error: %s", recorder.Body.String())
	}
	if server.cfg.ImageArtifactCache.RetentionDays != 5 || server.cfg.ImageArtifactCache.MaxTotalSizeMB != 1 {
		t.Fatalf("runtime config cache = %#v", server.cfg.ImageArtifactCache)
	}
}

func TestOAuthCallbackRateLimiterCleansStaleKeys(t *testing.T) {
	limiter := newCallbackRateLimiter()
	start := time.Now()

	if !limiter.allow("203.0.113.10", start) {
		t.Fatal("first request should be allowed")
	}
	if len(limiter.hits) != 1 {
		t.Fatalf("hits length = %d, want 1", len(limiter.hits))
	}
	if !limiter.allow("203.0.113.11", start.Add(oauthCallbackCleanupInterval+oauthCallbackRateLimitWindow)) {
		t.Fatal("new request should be allowed")
	}
	if _, exists := limiter.hits["203.0.113.10"]; exists {
		t.Fatal("stale callback rate-limit entry was not cleaned")
	}
}

func TestOAuthCallbackRateLimit(t *testing.T) {
	server := newTestServer(t)

	for i := range oauthCallbackRateLimitMax {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/callback?state=missing&code=x", nil)
		req.RemoteAddr = "203.0.113.10:12345"
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("callback request %d status = %d, want 409", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/anthropic/callback?state=missing&code=x", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("callback over limit status = %d, want 429", rec.Code)
	}
}

func TestOAuthCallbackRateLimitIgnoresXRealIP(t *testing.T) {
	server := newTestServer(t)
	server.cfg.TLS.TrustForwardedProto = true

	for i := range oauthCallbackRateLimitMax {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/callback?state=missing&code=x", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Real-IP", fmt.Sprintf("20.0.0.%d", i+1))
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("callback request %d status = %d, want 409", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/anthropic/callback?state=missing&code=x", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Real-IP", "20.0.0.99")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("callback over limit with spoofed X-Real-IP status = %d, want 429", rec.Code)
	}
}

func TestOAuthCallbackRateLimitUsesTrustedXRealIP(t *testing.T) {
	server := newTestServer(t)
	server.cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}

	for i := range oauthCallbackRateLimitMax {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/callback?state=missing&code=x", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Real-IP", fmt.Sprintf("20.0.0.%d", i+1))
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("callback request %d status = %d, want 409", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/anthropic/callback?state=missing&code=x", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Real-IP", "20.0.0.99")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("callback with new trusted X-Real-IP status = %d, want 409", rec.Code)
	}
}

func TestDevinCallbackAliasUsesOAuthCallbackHandler(t *testing.T) {
	server := newTestServer(t)
	for _, path := range []string{"/callback", "/devin/callback"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path+"?state=missing&code=callback-code", nil)
		request.RemoteAddr = "127.0.0.1:12345"
		server.engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s status = %d, want %d body=%s", path, recorder.Code, http.StatusConflict, recorder.Body.String())
		}
		if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", path, cacheControl)
		}
	}
}

func TestRegisteredManagementRouteKeysIncludeFixedPluginRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	server := newTestServer(t)
	keys := server.registeredManagementRouteKeys()
	for _, key := range []string{
		"GET /v0/management/plugins",
		"PATCH /v0/management/plugins/:id/enabled",
		"PUT /v0/management/plugins/:id/config",
		"PATCH /v0/management/plugins/:id/config",
		"GET /v0/management/plugins/:id/quota",
		"POST /v0/management/plugins/:id/quota",
		"DELETE /v0/management/plugins/:id/quota",
		"POST /v0/management/plugins/:id/quota/reset",
		"GET /v0/management/quota/providers",
		"POST /v0/management/quota/fetch",
		"POST /v0/management/quota/reset",
		"GET /v0/management/plugin-store",
		"POST /v0/management/plugin-store/:id/install",
		"POST /v0/management/gemini-api-key",
		"POST /v0/management/claude-api-key",
		"POST /v0/management/codex-api-key",
		"POST /v0/management/openai-compatibility",
		"POST /v0/management/vertex-api-key",
	} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("registeredManagementRouteKeys() missing %s", key)
		}
	}
}

func TestPluginResourcePrefixRequiresManagementAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.store(true)

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Management-Key", "test-management-password")
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("authenticated missing route status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("authenticated missing route set cookies: %#v", cookies)
	}
}

func TestPluginResourceSessionCookieSkipsManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.store(true)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	if !server.issuePluginResourceSession(c) {
		t.Fatal("issuePluginResourceSession() = false")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != pluginResourceSessionCookie {
		t.Fatalf("cookies = %#v, want one plugin resource session cookie", cookies)
	}
	if cookies[0].Path != "/v0/resource/plugins/" || !cookies[0].HttpOnly {
		t.Fatalf("cookie attributes = %#v, want resource path and HttpOnly", cookies[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.AddCookie(cookies[0])
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cookie-authenticated missing route status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginResourceSessionCookieTrustsForwardedProtoOnlyFromTrustedProxy(t *testing.T) {
	server := newTestServer(t)
	server.cfg.TLS.TrustForwardedProto = true
	server.cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}

	for _, tc := range []struct {
		name       string
		remoteAddr string
		forwarded  string
		wantSecure bool
	}{
		{
			name:       "untrusted peer",
			remoteAddr: "203.0.113.42:12345",
		},
		{
			name:       "trusted proxy",
			remoteAddr: "127.0.0.1:12345",
			forwarded:  "198.51.100.42",
			wantSecure: true,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ginCtx, _ := gin.CreateTestContext(recorder)
				ginCtx.Request = httptest.NewRequest(
					http.MethodGet,
					"http://cpa.test/v0/resource/plugins/resource/status",
					nil,
				)
				ginCtx.Request.RemoteAddr = tc.remoteAddr
				ginCtx.Request.Header.Set("X-Forwarded-Proto", "https")
				if tc.forwarded != "" {
					ginCtx.Request.Header.Set("X-Forwarded-For", tc.forwarded)
				}

				server.setPluginResourceSessionCookie(ginCtx, "session", 60)
				cookies := recorder.Result().Cookies()
				if len(cookies) != 1 {
					t.Fatalf("cookies = %#v, want one session cookie", cookies)
				}
				if cookies[0].Secure != tc.wantSecure {
					t.Fatalf("Secure = %t, want %t", cookies[0].Secure, tc.wantSecure)
				}
			},
		)
	}
}

func TestPluginResourceExpiredSessionCookieRequiresManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.store(true)

	server.pluginResourceSessionMu.Lock()
	server.pluginResourceSessionTokens["expired"] = pluginResourceSession{
		IdleExpiresAt:     time.Now().Add(-time.Minute),
		AbsoluteExpiresAt: time.Now().Add(pluginResourceSessionAbsoluteTTL),
	}
	server.pluginResourceSessionMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.AddCookie(&http.Cookie{Name: pluginResourceSessionCookie, Value: "expired"})
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired cookie status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginResourceAbsoluteExpiredSessionCookieRequiresManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.store(true)

	server.pluginResourceSessionMu.Lock()
	server.pluginResourceSessionTokens["absolute-expired"] = pluginResourceSession{
		IdleExpiresAt:     time.Now().Add(pluginResourceSessionTTL),
		AbsoluteExpiresAt: time.Now().Add(-time.Minute),
	}
	server.pluginResourceSessionMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.AddCookie(&http.Cookie{Name: pluginResourceSessionCookie, Value: "absolute-expired"})
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("absolute expired cookie status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPluginResourceSessionRenewalIsCappedByAbsoluteExpiry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	now := time.Now()
	absoluteExpiresAt := now.Add(time.Minute)
	server.pluginResourceSessionMu.Lock()
	server.pluginResourceSessionTokens["near-absolute"] = pluginResourceSession{
		IdleExpiresAt:     now.Add(pluginResourceSessionTTL),
		AbsoluteExpiresAt: absoluteExpiresAt,
	}
	server.pluginResourceSessionMu.Unlock()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/resource/status", nil)
	c.Request.AddCookie(&http.Cookie{Name: pluginResourceSessionCookie, Value: "near-absolute"})

	if !server.pluginResourceSessionAuthenticated(c) {
		t.Fatal("pluginResourceSessionAuthenticated() = false, want true")
	}
	server.pluginResourceSessionMu.Lock()
	session := server.pluginResourceSessionTokens["near-absolute"]
	server.pluginResourceSessionMu.Unlock()
	if session.IdleExpiresAt.After(absoluteExpiresAt) {
		t.Fatalf("idle expiry = %s, want not after absolute expiry %s", session.IdleExpiresAt, absoluteExpiresAt)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != pluginResourceSessionCookie {
		t.Fatalf("renewal cookies = %#v, want session cookie", cookies)
	}
	if cookies[0].MaxAge > int(time.Minute.Seconds()) {
		t.Fatalf("renewed cookie MaxAge = %d, want capped by absolute expiry", cookies[0].MaxAge)
	}
}

func TestExampleAPIKeySafeModeShowsWarningAndKeepsManagement(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	if err := os.WriteFile(
		filepath.Join(staticDir, "management.html"), []byte("<html>management app</html>"), 0o600,
	); err != nil {
		t.Fatalf("failed to write management asset: %v", err)
	}

	server := newTestServerWithOptions(t, WithExampleAPIKeySafeMode())
	cfg := *server.cfg
	cfg.APIKeys = []string{"your-api-key-1"}
	server.UpdateClients(&cfg)

	t.Run(
		"root warning page includes management link", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
			}
			body := rr.Body.String()
			for _, want := range []string{
				"Example API key detected", "Open Management", `href="/management.html?safe-mode=configure"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("warning page missing %q: %s", want, body)
				}
			}
		},
	)

	t.Run(
		"management html defaults to warning page", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "Example API key detected") {
				t.Fatalf("management.html did not show warning page: %s", rr.Body.String())
			}
		},
	)

	t.Run(
		"management html head stops at warning page", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodHead, "/management.html", nil)
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
			}
			if rr.Body.Len() != 0 {
				t.Fatalf("HEAD body length = %d, want 0", rr.Body.Len())
			}
			if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		},
	)

	t.Run(
		"management button query opens control panel", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/management.html?safe-mode=configure", nil)
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "management app") {
				t.Fatalf("management panel body missing: %s", rr.Body.String())
			}
		},
	)

	t.Run(
		"proxy endpoints are blocked", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
			}
			if got := rr.Header().Get("X-CPA-SAFE-MODE"); got != "example-api-key" {
				t.Fatalf("X-CPA-SAFE-MODE = %q, want example-api-key", got)
			}
			if !strings.Contains(rr.Body.String(), "unsafe_example_api_key") {
				t.Fatalf("body missing safe-mode error: %s", rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "/management.html?safe-mode=configure") {
				t.Fatalf("body missing management link in message: %s", rr.Body.String())
			}
		},
	)

	t.Run(
		"management endpoints still work", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Authorization", "Bearer test-management-key")
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
			}
		},
	)

	t.Run(
		"safe mode clears after key update", func(t *testing.T) {
			nextCfg := cfg
			nextCfg.APIKeys = []string{"real-key"}
			server.UpdateClients(&nextCfg)

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer real-key")
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "unsafe_example_api_key") {
				t.Fatalf("proxy endpoint still blocked after key update: %s", rr.Body.String())
			}
		},
	)
}

func TestPluginResourceCORSRequiresAllowlistForCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(corsMiddleware(nil))
	engine.GET("/v0/resource/plugins/demo/status", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodOptions, "/v0/resource/plugins/demo/status", nil)
	req.Header.Set("Origin", "https://panel.example.com")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("plugin resource ACAO = %q, want *", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("plugin resource ACAC = %q, want empty", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got != "authorization" {
		t.Fatalf("plugin resource ACAH = %q, want requested header", got)
	}
	exposedHeaders := rr.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposedHeaders, pluginhost.SupportPluginHeader) {
		t.Fatalf("Access-Control-Expose-Headers = %q, want %s", exposedHeaders, pluginhost.SupportPluginHeader)
	}
	if !strings.Contains(exposedHeaders, "ETag") {
		t.Fatalf("Access-Control-Expose-Headers = %q, want ETag", exposedHeaders)
	}
	if !strings.Contains(exposedHeaders, internallogging.CPATraceIDHeader) {
		t.Fatalf("Access-Control-Expose-Headers = %q, want %s", exposedHeaders, internallogging.CPATraceIDHeader)
	}
}

func TestPluginResourceCORSAllowsCredentialsForAllowlistedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(corsMiddleware([]string{"https://panel.example.com"}))
	engine.GET("/v0/resource/plugins/demo/status", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.GET("/v0/management/config", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodOptions, "/v0/resource/plugins/demo/status", nil)
	req.Header.Set("Origin", "https://panel.example.com")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://panel.example.com" {
		t.Fatalf("plugin resource ACAO = %q, want request origin", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("plugin resource ACAC = %q, want true", got)
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	req.Header.Set("Origin", "https://panel.example.com")
	rr = httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://panel.example.com" {
		t.Fatalf("management ACAO = %q, want allowlisted origin", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("management ACAC = %q, want empty", got)
	}
}

func TestManagementBodyLimitMiddlewareUsesImportLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(managementBodyLimitMiddleware())
	engine.POST(
		"/v0/management/usage/import", func(c *gin.Context) {
			_, errRead := io.ReadAll(c.Request.Body)
			if errRead != nil {
				c.String(http.StatusRequestEntityTooLarge, errRead.Error())
				return
			}
			c.Status(http.StatusNoContent)
		},
	)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v0/management/usage/import",
		strings.NewReader(strings.Repeat("a", (10<<20)+1)),
	)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestNoStoreMiddleware_SetsCacheControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(noStoreMiddleware())
	engine.GET(
		"/test", func(c *gin.Context) {
			c.String(http.StatusOK, "ok")
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", got)
	}
}

func TestDefaultRequestLoggerFactory_UsesResolvedLogDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	// Force ResolveLogDirectory to fall back to auth-dir/logs by making ./logs not a writable directory.
	if errWriteFile := os.WriteFile(
		filepath.Join(tmpDir, "logs"), []byte("not-a-directory"), 0o644,
	); errWriteFile != nil {
		t.Fatalf("failed to create blocking logs file: %v", errWriteFile)
	}

	configDir := filepath.Join(tmpDir, "config")
	if errMkdirConfig := os.MkdirAll(configDir, 0o755); errMkdirConfig != nil {
		t.Fatalf("failed to create config dir: %v", errMkdirConfig)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	authDir := filepath.Join(tmpDir, "auth")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}

	cfg := &proxyconfig.Config{
		RequestLog:        false,
		AuthDir:           authDir,
		ErrorLogsMaxFiles: 10,
	}

	logger := defaultRequestLoggerFactory(cfg, configPath)
	fileLogger, ok := logger.(*internallogging.FileRequestLogger)
	if !ok {
		t.Fatalf("expected *FileRequestLogger, got %T", logger)
	}

	errLog := fileLogger.LogRequestWithOptions(
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
		"issue-1711",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("failed to write forced error request log: %v", errLog)
	}

	authLogsDir := filepath.Join(authDir, "logs")
	authEntries, errReadAuthDir := os.ReadDir(authLogsDir)
	if errReadAuthDir != nil {
		t.Fatalf("failed to read auth logs dir %s: %v", authLogsDir, errReadAuthDir)
	}
	foundErrorLogInAuthDir := false
	for _, entry := range authEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			foundErrorLogInAuthDir = true
			break
		}
	}
	if !foundErrorLogInAuthDir {
		t.Fatalf("expected forced error log in auth fallback dir %s, got entries: %+v", authLogsDir, authEntries)
	}

	configLogsDir := filepath.Join(configDir, "logs")
	configEntries, errReadConfigDir := os.ReadDir(configLogsDir)
	if errReadConfigDir != nil && !os.IsNotExist(errReadConfigDir) {
		t.Fatalf("failed to inspect config logs dir %s: %v", configLogsDir, errReadConfigDir)
	}
	for _, entry := range configEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			t.Fatalf("unexpected forced error log in config dir %s", configLogsDir)
		}
	}
}

func TestAttachWebsocketRouteUsesLoopbackPolicyWhenAuthDisabled(t *testing.T) {
	server := newTestServer(t)
	handlerCalls := 0
	server.AttachWebsocketRoute(
		"/v1/ws", http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				handlerCalls++
				w.WriteHeader(http.StatusNoContent)
			},
		),
	)

	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "loopback peer",
			remoteAddr: "127.0.0.1:12345",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "remote peer with valid key",
			remoteAddr: "203.0.113.11:12345",
			headers:    map[string]string{"Authorization": "Bearer test-key"},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				callsBefore := handlerCalls
				req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
				req.RemoteAddr = tt.remoteAddr
				for name, value := range tt.headers {
					req.Header.Set(name, value)
				}
				recorder := httptest.NewRecorder()
				server.engine.ServeHTTP(recorder, req)

				if recorder.Code != tt.wantStatus {
					t.Fatalf("status = %d, want %d: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
				}
				called := handlerCalls == callsBefore+1
				wantCalled := tt.wantStatus == http.StatusNoContent
				if called != wantCalled {
					t.Fatalf("handler called = %v, want %v", called, wantCalled)
				}
			},
		)
	}
}

func TestManagementConfigSnapshotRemainsStableForRequest(t *testing.T) {
	server := newTestServer(t)
	gin.SetMode(gin.TestMode)
	contextRecorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(contextRecorder)
	requestContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	requestContext.Request.RemoteAddr = "203.0.113.10:12345"

	initial := server.managementConfigSnapshot(requestContext)
	if initial == nil {
		t.Fatal("initial management config snapshot is nil")
	}
	next, err := proxyconfig.Clone(initial)
	if err != nil {
		t.Fatalf("clone config: %v", err)
	}
	next.TLS.RequireForAuth = true
	server.mgmt.SetConfig(next)

	if got := server.managementConfigSnapshot(requestContext); got != initial {
		t.Fatal("management config snapshot changed during one request")
	}
	requireTLSForAuthRequest(server.managementConfigSnapshot)(requestContext)
	if requestContext.IsAborted() {
		t.Fatal("request used the newer TLS policy after capturing the older management config")
	}

	freshRecorder := httptest.NewRecorder()
	freshContext, _ := gin.CreateTestContext(freshRecorder)
	freshContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	freshContext.Request.RemoteAddr = "203.0.113.10:12345"
	requireTLSForAuthRequest(server.managementConfigSnapshot)(freshContext)
	if !freshContext.IsAborted() || freshRecorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("fresh request status = %d, want %d", freshRecorder.Code, http.StatusMisdirectedRequest)
	}
}

func TestAttachWebsocketRouteAppliesAuthenticationUpdates(t *testing.T) {
	server := newTestServer(t)
	server.AttachWebsocketRoute(
		"/v1/ws", http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
		),
	)

	request := func(apiKey string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
		req.RemoteAddr = "203.0.113.20:12345"
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder.Code
	}
	setWebsocketAuth := func(enabled bool) {
		t.Helper()
		next := *server.configSnapshot()
		next.WebsocketAuth = enabled
		server.UpdateClients(&next)
	}

	if got := request("test-key"); got != http.StatusUnauthorized {
		t.Fatalf("ws-auth=false status = %d, want %d", got, http.StatusUnauthorized)
	}

	setWebsocketAuth(true)
	if got := request(""); got != http.StatusUnauthorized {
		t.Fatalf("ws-auth=true without key status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request("test-key"); got != http.StatusNoContent {
		t.Fatalf("ws-auth=true with valid key status = %d, want %d", got, http.StatusNoContent)
	}

	setWebsocketAuth(false)
	if got := request("test-key"); got != http.StatusUnauthorized {
		t.Fatalf("ws-auth=false after update status = %d, want %d", got, http.StatusUnauthorized)
	}
}

func TestAttachWebsocketRouteAppliesTLSUpdates(t *testing.T) {
	server := newTestServer(t)
	server.AttachWebsocketRoute(
		"/v1/ws", http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
		),
	)

	updateTLSRequirement := func(required bool) {
		t.Helper()
		next := *server.configSnapshot()
		next.WebsocketAuth = true
		next.TLS.RequireForAuth = required
		server.UpdateClients(&next)
	}
	request := func(secure bool) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
		req.RemoteAddr = "203.0.113.20:12345"
		req.Header.Set("Authorization", "Bearer test-key")
		if secure {
			req.TLS = &tls.ConnectionState{}
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder.Code
	}

	updateTLSRequirement(false)
	if got := request(false); got != http.StatusNoContent {
		t.Fatalf("TLS not required status = %d, want %d", got, http.StatusNoContent)
	}
	updateTLSRequirement(true)
	if got := request(false); got != http.StatusMisdirectedRequest {
		t.Fatalf("TLS required over HTTP status = %d, want %d", got, http.StatusMisdirectedRequest)
	}
	if got := request(true); got != http.StatusNoContent {
		t.Fatalf("TLS required over HTTPS status = %d, want %d", got, http.StatusNoContent)
	}
	updateTLSRequirement(false)
	if got := request(false); got != http.StatusNoContent {
		t.Fatalf("TLS disabled after update status = %d, want %d", got, http.StatusNoContent)
	}
}

func TestAuthPolicyReadLockEndsBeforeRequestHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{}
	engine := gin.New()
	authEntered := make(chan struct{})
	releaseAuth := make(chan struct{})
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	requestDone := make(chan struct{})
	writerAcquired := make(chan struct{})

	engine.Use(server.authPolicyReadLockMiddleware())
	engine.Use(
		func(c *gin.Context) {
			close(authEntered)
			<-releaseAuth
			c.Next()
		},
	)
	engine.Use(authPolicyUnlockMiddleware())
	engine.GET(
		"/test", func(c *gin.Context) {
			close(handlerEntered)
			<-releaseHandler
			c.Status(http.StatusNoContent)
		},
	)

	go func() {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/test", nil))
		close(requestDone)
	}()
	<-authEntered
	go func() {
		server.authPolicyMu.Lock()
		close(writerAcquired)
		server.authPolicyMu.Unlock()
	}()
	select {
	case <-writerAcquired:
		t.Fatal("config publication acquired the policy lock while authentication was in progress")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseAuth)
	<-handlerEntered
	select {
	case <-writerAcquired:
	case <-time.After(time.Second):
		t.Fatal("request handler retained the authentication policy lock")
	}
	close(releaseHandler)
	<-requestDone
}

// --- authMiddleware tests ---

func setupAuthMiddlewareEngine(manager *sdkaccess.Manager) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(authMiddleware(manager, nil, func() *proxyconfig.Config { return &proxyconfig.Config{} }))
	engine.GET(
		"/test", func(c *gin.Context) {
			apiKey, _ := c.Get("apiKey")
			c.JSON(http.StatusOK, gin.H{"apiKey": apiKey})
		},
	)
	return engine
}

func TestAuthMiddleware_NilManagerRestrictsToLoopback(t *testing.T) {
	engine := setupAuthMiddlewareEngine(nil)
	tests := []struct {
		name       string
		remoteAddr string
		host       string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "IPv4 loopback",
			remoteAddr: "127.0.0.1:12345",
			wantStatus: http.StatusOK,
		},
		{
			name:       "IPv6 loopback",
			remoteAddr: "[::1]:12345",
			wantStatus: http.StatusOK,
		},
		{
			name:       "loopback proxy without trusted proxy config",
			remoteAddr: "127.0.0.1:12345",
			headers: map[string]string{
				"X-Forwarded-For": "198.51.100.10",
				"X-Real-IP":       "198.51.100.10",
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "private network peer",
			remoteAddr: "10.0.0.10:12345",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "public peer",
			remoteAddr: "203.0.113.10:12345",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "spoofed forwarding headers",
			remoteAddr: "203.0.113.11:12345",
			host:       "127.0.0.1",
			headers: map[string]string{
				"X-Forwarded-For":   "127.0.0.1",
				"X-Real-IP":         "::1",
				"X-Forwarded-Host":  "localhost",
				"X-Forwarded-Proto": "https",
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed remote address",
			remoteAddr: "not-a-remote-address",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/test", nil)
				req.RemoteAddr = tt.remoteAddr
				if tt.host != "" {
					req.Host = tt.host
				}
				for name, value := range tt.headers {
					req.Header.Set(name, value)
				}
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, req)

				if recorder.Code != tt.wantStatus {
					t.Fatalf("status = %d, want %d: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
				}
			},
		)
	}
}

func setupRequireTLSEngine(cfg *proxyconfig.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(requireTLSForAuth(func() *proxyconfig.Config { return cfg }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	return engine
}

func TestRequireTLSForAuthAppliesConfigUpdates(t *testing.T) {
	cfg := &proxyconfig.Config{}
	engine := setupRequireTLSEngine(cfg)
	request := func() int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "203.0.113.10:12345"
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		return recorder.Code
	}

	if got := request(); got != http.StatusOK {
		t.Fatalf("initial status = %d, want %d", got, http.StatusOK)
	}
	cfg.TLS.RequireForAuth = true
	if got := request(); got != http.StatusMisdirectedRequest {
		t.Fatalf("enabled status = %d, want %d", got, http.StatusMisdirectedRequest)
	}
	cfg.TLS.RequireForAuth = false
	if got := request(); got != http.StatusOK {
		t.Fatalf("disabled status = %d, want %d", got, http.StatusOK)
	}
}

func TestRequireTLSForAuthRejectsSpoofedLoopbackHost(t *testing.T) {
	engine := setupRequireTLSEngine(&proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true}})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMisdirectedRequest)
	}
}

func TestRequireTLSForAuthAllowsLoopbackRemoteAddr(t *testing.T) {
	engine := setupRequireTLSEngine(&proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true}})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Host = "example.com"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestRequireTLSForAuthRejectsForwardedClientThroughTrustedLoopback(t *testing.T) {
	cfg := &proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true}}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	engine := setupRequireTLSEngine(cfg)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.40")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMisdirectedRequest)
	}
}

func TestRequireTLSForAuthDoesNotTreatConfiguredProxyAsDirectLoopback(t *testing.T) {
	cfg := &proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true}}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	engine := setupRequireTLSEngine(cfg)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMisdirectedRequest)
	}
}

func TestRequireTLSForAuthRejectsForwardedProtoFromUntrustedPeer(t *testing.T) {
	cfg := &proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true, TrustForwardedProto: true}}
	engine := setupRequireTLSEngine(cfg)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "203.0.113.42:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMisdirectedRequest)
	}
}

func TestRequireTLSForAuthAcceptsForwardedProtoFromTrustedProxy(t *testing.T) {
	cfg := &proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true, TrustForwardedProto: true}}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	engine := setupRequireTLSEngine(cfg)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.42")
	req.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

func TestRequireTLSForAuthRejectsDuplicateForwardedProtoFromTrustedProxy(t *testing.T) {
	cfg := &proxyconfig.Config{TLS: proxyconfig.TLSConfig{RequireForAuth: true, TrustForwardedProto: true}}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	engine := setupRequireTLSEngine(cfg)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.42")
	req.Header.Add("X-Forwarded-Proto", "https")
	req.Header.Add("X-Forwarded-Proto", "http")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMisdirectedRequest)
	}
}

// stubProvider implements sdkaccess.Provider for testing.
type stubProvider struct {
	keys map[string]struct{}
}

func (s *stubProvider) Identifier() string { return "stub" }

func (s *stubProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	key := strings.TrimPrefix(authHeader, "Bearer ")
	if _, ok := s.keys[key]; ok {
		return &sdkaccess.Result{
			Provider:  "stub",
			Principal: key,
		}, nil
	}
	return nil, sdkaccess.NewInvalidCredentialError()
}

type internalErrorProvider struct{}

func (*internalErrorProvider) Identifier() string { return "internal-error" }

func (*internalErrorProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, sdkaccess.NewInternalAuthError("provider unavailable", errors.New("test failure"))
}

func TestAuthMiddleware_ValidAPIKey(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, nil, func() *proxyconfig.Config { return &proxyconfig.Config{} }))
	engine.GET(
		"/test", func(c *gin.Context) {
			apiKey, _ := c.Get("apiKey")
			c.JSON(http.StatusOK, gin.H{"apiKey": apiKey})
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer valid-key")
	req.RemoteAddr = "10.0.0.1:9999"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "valid-key") {
		t.Fatalf("expected apiKey in context, body: %s", rr.Body.String())
	}
}

func TestAuthMiddleware_InvalidAPIKey(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, nil, func() *proxyconfig.Config { return &proxyconfig.Config{} }))
	engine.GET(
		"/test", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{})
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer bad-key")
	req.RemoteAddr = "10.0.0.1:9999"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", got)
	}
	if !strings.Contains(rr.Body.String(), "Unauthorized") {
		t.Fatalf("expected generic Unauthorized error, got %s", rr.Body.String())
	}
}

func TestAuthMiddlewareUsesCurrentQueryAuthPolicyForAccountRateLimit(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{"valid-key": {}}}})
	rateLimiter := access.NewAuthRateLimiter()
	defer rateLimiter.Stop()

	cfg := &proxyconfig.Config{}
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, rateLimiter, func() *proxyconfig.Config { return cfg }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := func(index int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/test?key=victim-key", nil)
		req.RemoteAddr = fmt.Sprintf("203.0.113.%d:9999", index+1)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want %d", index, recorder.Code, http.StatusUnauthorized)
		}
	}

	for i := range 10 {
		request(i)
	}
	accountHash := access.HashAccountKey("victim-key")
	if rateLimiter.IsAccountLimited(accountHash) {
		t.Fatal("disabled query authentication counted query credentials against the account")
	}

	cfg.AllowQueryAuth = true
	for i := 10; i < 20; i++ {
		request(i)
	}
	if !rateLimiter.IsAccountLimited(accountHash) {
		t.Fatal("enabled query authentication did not count query credentials against the account")
	}
}

func TestAuthMiddleware_RateLimited(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)

	rl := access.NewAuthRateLimiter()
	defer rl.Stop()

	// Trigger lockout for the IP.
	for range 10 {
		rl.RecordFailure("10.0.0.99")
	}

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, rl, func() *proxyconfig.Config { return &proxyconfig.Config{} }))
	engine.GET(
		"/test", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{})
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer valid-key")
	req.RemoteAddr = "10.0.0.99:9999"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAuthMiddlewareTrustedProxyRateLimitIsolatesClients(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{"valid-key": {}}}})
	rateLimiter := access.NewAuthRateLimiter()
	defer rateLimiter.Stop()
	cfg := &proxyconfig.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, rateLimiter, func() *proxyconfig.Config { return cfg }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := func(clientIP, key string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "127.0.0.1:9999"
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Real-IP", clientIP)
		req.Header.Set("Authorization", "Bearer "+key)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		return recorder.Code
	}

	for i := range 10 {
		if got := request("198.51.100.10", fmt.Sprintf("wrong-%d", i)); got != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d status = %d, want 401", i+1, got)
		}
	}
	if got := request("198.51.100.10", "valid-key"); got != http.StatusTooManyRequests {
		t.Fatalf("blocked client status = %d, want 429", got)
	}
	if got := request("198.51.100.11", "valid-key"); got != http.StatusNoContent {
		t.Fatalf("other client status = %d, want 204", got)
	}
	if got := request("198.51.100.10", "valid-key"); got != http.StatusTooManyRequests {
		t.Fatalf("other client success cleared blocked client, status = %d", got)
	}
}

func TestAuthMiddlewareTrustedLoopbackWithoutForwardingHeadersIsNotDirect(t *testing.T) {
	cfg := &proxyconfig.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(nil, nil, func() *proxyconfig.Config { return cfg }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("trusted loopback peer status = %d, want 401", recorder.Code)
	}
}

func TestAuthMiddlewareForwardedLoopbackIsNotDirect(t *testing.T) {
	cfg := &proxyconfig.Config{}
	cfg.RemoteManagement.TrustedProxies = []string{"127.0.0.1/32"}
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(nil, nil, func() *proxyconfig.Config { return cfg }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("forwarded loopback status = %d, want 401", recorder.Code)
	}
}

func TestAuthMiddlewareInternalErrorDoesNotTriggerRateLimit(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{&internalErrorProvider{}})
	rateLimiter := access.NewAuthRateLimiter()
	defer rateLimiter.Stop()
	cfg := &proxyconfig.Config{}

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, rateLimiter, func() *proxyconfig.Config { return cfg }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for i := range 10 {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "203.0.113.10:9999"
		req.Header.Set("Authorization", "Bearer valid-key")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("internal error request %d status = %d, want 500", i+1, recorder.Code)
		}
	}

	manager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{"valid-key": {}}}})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "203.0.113.10:9999"
	req.Header.Set("Authorization", "Bearer valid-key")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("recovered provider status = %d, want 204: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAuthMiddlewareEmptyProviderDoesNotClearRateLimitState(t *testing.T) {
	manager := sdkaccess.NewManager()
	rateLimiter := access.NewAuthRateLimiter()
	defer rateLimiter.Stop()
	for range 9 {
		rateLimiter.RecordFailure("127.0.0.1")
	}

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, rateLimiter, func() *proxyconfig.Config { return &proxyconfig.Config{} }))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("direct loopback status = %d, want 204", recorder.Code)
	}

	rateLimiter.RecordFailure("127.0.0.1")
	if !rateLimiter.IsLimited("127.0.0.1") {
		t.Fatal("empty provider request cleared existing rate-limit state")
	}
}
