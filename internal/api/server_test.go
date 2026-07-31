package api

import (
	"bytes"
	"context"
	"encoding/base64"
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
	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
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
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
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

const artifactTestPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

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
	server.server = &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-releaseRequest
	})}
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
	if server.managementRoutesEnabled.Load() {
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
	if !server.managementRoutesEnabled.Load() {
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
			func(_ context.Context, commit ManagementConfigCommit) error {
				server.UpdateClientsForConfigCommit(commit.Config, commit.Generation)
				return nil
			},
		),
		WithModelCatalogRuntimeSync(
			func(cfg *proxyconfig.Config) {
				modelRuntimeCalls++
				modelRuntimeConfig = cfg
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
			func(_ context.Context, commit ManagementConfigCommit) error {
				calls++
				server.UpdateClientsForConfigCommit(commit.Config, commit.Generation)
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
		records = append(records, toggleRecord{
			enabled: enabled,
			gate:    server.requestLoggingRuntimeEnabled.Load(),
		})
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

func TestImageArtifactContentRequiresAuthAndServesStoredImage(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
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

	unauthenticated := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+record.ID+"/content", nil)
	unauthenticated.RemoteAddr = "10.0.0.1:12345"
	unauthenticatedRec := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthenticatedRec, unauthenticated)
	if unauthenticatedRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401; body=%s", unauthenticatedRec.Code, unauthenticatedRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+record.ID+"/content", nil)
	req.Header.Set("Authorization", "Bearer valid-key")
	req.RemoteAddr = "10.0.0.1:12345"
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200; body=%s", rec.Code, rec.Body.String())
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
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "inline") || !strings.Contains(got, record.ID) {
		t.Fatalf("Content-Disposition = %q, want inline filename", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("response body differs from stored image")
	}
}

func TestImageArtifactContentRejectsInvalidAndMissingIDs(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)

	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/v1/artifacts/../secret.png/content", want: http.StatusNotFound},
		{path: "/v1/artifacts/img_nothex.png/content", want: http.StatusBadRequest},
		{path: "/v1/artifacts/img_" + strings.Repeat("a", 64) + ".png/content", want: http.StatusNotFound},
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

	for i := 0; i < oauthCallbackRateLimitMax; i++ {
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

	for i := 0; i < oauthCallbackRateLimitMax; i++ {
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
	server.cfg.TLS.TrustForwardedProto = true

	for i := 0; i < oauthCallbackRateLimitMax; i++ {
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

func TestRegisteredManagementRouteKeysIncludeFixedPluginRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	server := newTestServer(t)
	keys := server.registeredManagementRouteKeys()
	for _, key := range []string{
		"GET /v0/management/plugins",
		"PATCH /v0/management/plugins/:id/enabled",
		"PUT /v0/management/plugins/:id/config",
		"PATCH /v0/management/plugins/:id/config",
		"GET /v0/management/plugin-store",
		"POST /v0/management/plugin-store/:id/install",
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
	server.managementRoutesEnabled.Store(true)

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
	server.managementRoutesEnabled.Store(true)

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

func TestPluginResourceExpiredSessionCookieRequiresManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := newTestServer(t)
	server.pluginHost = pluginhost.New()
	server.mgmt.SetLocalPassword("test-management-password")
	server.managementRoutesEnabled.Store(true)

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
	server.managementRoutesEnabled.Store(true)

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
	if err := os.WriteFile(filepath.Join(staticDir, "management.html"), []byte("<html>management app</html>"), 0o600); err != nil {
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
			for _, want := range []string{"Example API key detected", "Open Management", `href="/management.html?safe-mode=configure"`} {
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
	if got := rr.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, pluginhost.SupportPluginHeader) {
		t.Fatalf("Access-Control-Expose-Headers = %q, want %s", got, pluginhost.SupportPluginHeader)
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
		SDKConfig: proxyconfig.SDKConfig{
			RequestLog: false,
		},
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

// --- authMiddleware tests ---

func setupAuthMiddlewareEngine(manager *sdkaccess.Manager) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(authMiddleware(manager, nil, false))
	engine.GET(
		"/test", func(c *gin.Context) {
			apiKey, _ := c.Get("apiKey")
			c.JSON(http.StatusOK, gin.H{"apiKey": apiKey})
		},
	)
	return engine
}

func TestAuthMiddleware_NilManager_LoopbackAllowed(t *testing.T) {
	engine := setupAuthMiddlewareEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for loopback with nil manager, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAuthMiddleware_NilManager_NonLoopbackRejected(t *testing.T) {
	engine := setupAuthMiddlewareEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for non-loopback with nil manager, got %d", rr.Code)
	}
}

func TestAuthMiddleware_NilManager_IPv6LoopbackAllowed(t *testing.T) {
	engine := setupAuthMiddlewareEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "[::1]:12345"
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for IPv6 loopback with nil manager, got %d", rr.Code)
	}
}

func setupRequireTLSEngine(cfg *proxyconfig.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(requireTLSForAuth(cfg))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	return engine
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

func TestAuthMiddleware_ValidAPIKey(t *testing.T) {
	manager := sdkaccess.NewManager()
	manager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{keys: map[string]struct{}{"valid-key": {}}},
		},
	)
	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, nil, false))
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
	engine.Use(authMiddleware(manager, nil, false))
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
	for i := 0; i < 10; i++ {
		rl.RecordFailure("10.0.0.99")
	}

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	engine.Use(authMiddleware(manager, rl, false))
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
