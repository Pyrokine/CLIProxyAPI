package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

type safeRefreshExecutor struct {
	lastAuthID string
	fail       bool
}

func (e *safeRefreshExecutor) Identifier() string {
	return "refresh-test"
}

func (e *safeRefreshExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.lastAuthID = auth.ID
	if e.fail {
		return nil, errors.New(`token endpoint failed: {"access_token":"secret-response-token"}`)
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "refreshed-secret-token"
	auth.Metadata["refresh_token"] = "persisted-secret-token"
	return auth, nil
}

func (e *safeRefreshExecutor) Execute(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *safeRefreshExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *safeRefreshExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *safeRefreshExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, nil
}

func newSafeRefreshHandler(t *testing.T, executor *safeRefreshExecutor) (*Handler, *coreauth.Auth, *coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	first := &coreauth.Auth{
		ID:       "refresh-first",
		FileName: "shared-refresh.json",
		Provider: "refresh-test",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthIndexSeed: "refresh-test|shared-refresh.json|first",
		},
		Metadata: map[string]any{
			"access_token":  "old-first-secret",
			"refresh_token": "first-refresh-secret",
		},
	}
	second := &coreauth.Auth{
		ID:       "refresh-second",
		FileName: "shared-refresh.json",
		Provider: "refresh-test",
		Status:   coreauth.StatusDisabled,
		Disabled: true,
		Attributes: map[string]string{
			coreauth.AttributeAuthIndexSeed: "refresh-test|shared-refresh.json|second",
		},
		Metadata: map[string]any{
			"access_token":  "old-second-secret",
			"refresh_token": "second-refresh-secret",
		},
	}
	var err error
	first, err = manager.Register(context.Background(), first)
	if err != nil {
		t.Fatalf("register first auth: %v", err)
	}
	second, err = manager.Register(context.Background(), second)
	if err != nil {
		t.Fatalf("register second auth: %v", err)
	}
	return NewHandlerWithoutConfigFilePath(&config.Config{}, manager), first, second
}

func TestRefreshAuthFileReturnsOnlyExactIdentityAndRefreshOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &safeRefreshExecutor{}
	handler, _, second := newSafeRefreshHandler(t, executor)
	second.EnsureIndex()

	engine := gin.New()
	engine.POST("/auth-files/refresh", handler.RefreshAuthFile)
	body := `{"name":"shared-refresh.json","auth_index":"` + second.Index + `"}`
	request := httptest.NewRequest(http.MethodPost, "/auth-files/refresh", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if executor.lastAuthID != second.ID {
		t.Fatalf("refreshed auth = %q, want %q", executor.lastAuthID, second.ID)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, key := range []string{"filename", "provider", "auth_index", "status", "refreshed_at"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("response missing %q: %v", key, payload)
		}
	}
	if len(payload) != 5 {
		t.Fatalf("response contains unexpected fields: %v", payload)
	}
	if payload["filename"] != "shared-refresh.json" || payload["auth_index"] != second.Index {
		t.Fatalf("response identity = %v", payload)
	}
	if payload["status"] != string(coreauth.StatusDisabled) {
		t.Fatalf("status = %v, want disabled", payload["status"])
	}
	for _, secret := range []string{
		"refreshed-secret-token",
		"persisted-secret-token",
		"old-second-secret",
		"metadata",
		"attributes",
	} {
		if strings.Contains(strings.ToLower(response.Body.String()), strings.ToLower(secret)) {
			t.Fatalf("response exposed %q: %s", secret, response.Body.String())
		}
	}
}

func TestRefreshAuthFileRejectsAmbiguousAndSanitizesFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &safeRefreshExecutor{fail: true}
	handler, first, _ := newSafeRefreshHandler(t, executor)
	first.EnsureIndex()
	engine := gin.New()
	engine.POST("/auth-files/refresh", handler.RefreshAuthFile)

	ambiguous := httptest.NewRequest(
		http.MethodPost,
		"/auth-files/refresh",
		strings.NewReader(`{"name":"shared-refresh.json"}`),
	)
	ambiguous.Header.Set("Content-Type", "application/json")
	ambiguousResponse := httptest.NewRecorder()
	engine.ServeHTTP(ambiguousResponse, ambiguous)
	if ambiguousResponse.Code != http.StatusBadRequest {
		t.Fatalf("missing auth_index status = %d, body = %s", ambiguousResponse.Code, ambiguousResponse.Body.String())
	}

	body := `{"name":"shared-refresh.json","auth_index":"` + first.Index + `"}`
	request := httptest.NewRequest(http.MethodPost, "/auth-files/refresh", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failure status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret-response-token") {
		t.Fatalf("failure response exposed provider body: %s", response.Body.String())
	}
	var payload authFileRefreshResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode failure response: %v", err)
	}
	if payload.Error != "credential_refresh_failed" || payload.AuthIndex != first.Index {
		t.Fatalf("failure response = %+v", payload)
	}
	if payload.RefreshedAt.After(time.Now()) {
		t.Fatalf("refreshed_at is in the future: %s", payload.RefreshedAt)
	}
}

type bulkRefreshExecutor struct {
	current       atomic.Int32
	maxConcurrent atomic.Int32
	calls         atomic.Int32
	entered       chan struct{}
	release       chan struct{}
}

func (*bulkRefreshExecutor) Identifier() string { return "bulk-refresh-test" }

func (e *bulkRefreshExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.calls.Add(1)
	current := e.current.Add(1)
	defer e.current.Add(-1)
	for {
		maximum := e.maxConcurrent.Load()
		if current <= maximum || e.maxConcurrent.CompareAndSwap(maximum, current) {
			break
		}
	}
	if e.entered != nil {
		e.entered <- struct{}{}
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "refreshed"
	return auth, nil
}

func (*bulkRefreshExecutor) Execute(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (*bulkRefreshExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (*bulkRefreshExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (*bulkRefreshExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newBulkRefreshHandler(t *testing.T, executor *bulkRefreshExecutor, count int) (*Handler, *coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for index := range count {
		_, errRegister := manager.Register(context.Background(), &coreauth.Auth{
			ID:       fmt.Sprintf("bulk-%02d", index),
			FileName: fmt.Sprintf("bulk-%02d.json", index),
			Provider: executor.Identifier(),
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{"refresh_token": fmt.Sprintf("refresh-%02d", index)},
		})
		if errRegister != nil {
			t.Fatalf("register bulk auth: %v", errRegister)
		}
	}
	disabled, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "bulk-disabled",
		FileName: "bulk-disabled.json",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusDisabled,
		Disabled: true,
		Metadata: map[string]any{"refresh_token": "disabled-refresh"},
	})
	if errRegister != nil {
		t.Fatalf("register disabled auth: %v", errRegister)
	}
	if _, errRegister = manager.Register(context.Background(), &coreauth.Auth{
		ID:       "bulk-no-refresh",
		FileName: "bulk-no-refresh.json",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
	}); errRegister != nil {
		t.Fatalf("register no-refresh auth: %v", errRegister)
	}
	cfg := &config.Config{AuthAutoRefreshWorkers: 2}
	manager.SetConfig(cfg)
	return NewHandlerWithoutConfigFilePath(cfg, manager), disabled
}

func TestRefreshAllAuthFilesBoundsWorkersAndSkipsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &bulkRefreshExecutor{
		entered: make(chan struct{}, 6),
		release: make(chan struct{}),
	}
	handler, disabled := newBulkRefreshHandler(t, executor, 6)
	disabled.EnsureIndex()
	router := gin.New()
	router.POST("/auth-files/refresh", handler.RefreshAuthFile)
	request := httptest.NewRequest(http.MethodPost, "/auth-files/refresh", strings.NewReader(`{"all":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(response, request)
		close(done)
	}()
	for range 2 {
		select {
		case <-executor.entered:
		case <-time.After(time.Second):
			t.Fatal("refresh workers did not start")
		}
	}
	close(executor.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bulk refresh did not finish")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload authFileRefreshBatchResponse
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Results) != 6 || executor.calls.Load() != 6 {
		t.Fatalf("results = %d, calls = %d", len(payload.Results), executor.calls.Load())
	}
	if maximum := executor.maxConcurrent.Load(); maximum > 2 {
		t.Fatalf("max concurrent refreshes = %d, want <= 2", maximum)
	}
	for _, result := range payload.Results {
		if !result.Success || result.Error != "" {
			t.Fatalf("bulk result = %+v", result)
		}
		if result.AuthIndex == disabled.Index {
			t.Fatalf("disabled credential was refreshed: %+v", result)
		}
	}
}

func TestRefreshAllAuthFilesCancellationReturnsPerItemResults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &bulkRefreshExecutor{}
	handler, _ := newBulkRefreshHandler(t, executor, 4)
	router := gin.New()
	router.POST("/auth-files/refresh", handler.RefreshAuthFile)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/auth-files/refresh", strings.NewReader(`{"all":true}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload authFileRefreshBatchResponse
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(payload.Results))
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", executor.calls.Load())
	}
	for _, result := range payload.Results {
		if result.Success || result.Error != "credential_refresh_cancelled" || result.AuthIndex == "" {
			t.Fatalf("cancelled result = %+v", result)
		}
	}
}
