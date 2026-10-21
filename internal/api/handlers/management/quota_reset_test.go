package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestResetQuotaUsesAuthIndex(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID:             "reset-auth-id",
		FileName:       "reset-auth-file.json",
		Provider:       "claude",
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		ModelStates: map[string]*coreauth.ModelState{
			"claude-reset-model": {
				Status:         coreauth.StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: coreauth.QuotaState{
					Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2,
				},
			},
		},
	}
	authIndex := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth record: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v0/management/reset-quota",
		strings.NewReader(`{"auth_index":"`+authIndex+`"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	ginCtx.Request = request

	h.ResetQuota(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["auth_index"] != authIndex {
		t.Fatalf("auth_index = %#v, want %q", payload["auth_index"], authIndex)
	}

	updated, ok := manager.GetByID("reset-auth-id")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after reset")
	}
	if updated.Status != coreauth.StatusActive || updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf(
			"updated auth state = status %q message %q unavailable %v next %v",
			updated.Status,
			updated.StatusMessage,
			updated.Unavailable,
			updated.NextRetryAfter,
		)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() || updated.Quota.BackoffLevel != 0 {
		t.Fatalf("updated auth quota = %+v, want cleared", updated.Quota)
	}
	state := updated.ModelStates["claude-reset-model"]
	if state == nil {
		t.Fatalf("expected model state to remain")
	}
	if state.Status != coreauth.StatusActive || state.StatusMessage != "" || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf(
			"updated model state = status %q message %q unavailable %v next %v",
			state.Status,
			state.StatusMessage,
			state.Unavailable,
			state.NextRetryAfter,
		)
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		t.Fatalf("updated model quota = %+v, want cleared", state.Quota)
	}
}

func TestResetQuotaConsumesCodexResetCredit(t *testing.T) {
	var consumeCalls atomic.Int32
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				consumeCalls.Add(1)
				if r.Method != http.MethodPost {
					t.Fatalf("method = %s, want POST", r.Method)
				}
				if r.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
					t.Fatalf("path = %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
					t.Fatalf("Authorization = %q", got)
				}
				if got := r.UserAgent(); got != "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)" {
					t.Errorf("User-Agent = %q", got)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if body["redeem_request_id"] == "" {
					t.Fatalf("missing redeem_request_id")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":"reset"}`))
			},
		),
	)
	defer server.Close()
	oldURL := codexRateLimitResetCreditsConsumeURL
	codexRateLimitResetCreditsConsumeURL = server.URL + "/backend-api/wham/rate-limit-reset-credits/consume"
	defer func() { codexRateLimitResetCreditsConsumeURL = oldURL }()

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID:             "codex-reset-auth-id",
		FileName:       "codex-reset-auth-file.json",
		Provider:       "codex",
		Metadata:       map[string]any{"access_token": "access-token"},
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
	}
	authIndex := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth record: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	attachCodexResetProbe(t, h, auth, nil)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v0/management/reset-quota",
		strings.NewReader(`{"auth_index":"`+authIndex+`"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	ginCtx.Request = request

	h.ResetQuota(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if calls := consumeCalls.Load(); calls != 1 {
		t.Fatalf("Codex reset credit endpoint calls = %d, want 1", calls)
	}
	updated, ok := manager.GetByID("codex-reset-auth-id")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after reset")
	}
	if updated.Status != coreauth.StatusActive || updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf(
			"updated auth state = status %q message %q unavailable %v next %v", updated.Status, updated.StatusMessage,
			updated.Unavailable, updated.NextRetryAfter,
		)
	}
}

func TestCodexResetRequiresConfirmedResponse(t *testing.T) {
	for _, test := range []struct {
		name           string
		body           string
		upstreamStatus int
		wantStatus     int
	}{
		{name: "already redeemed", body: `{"code":"already_redeemed"}`, wantStatus: http.StatusOK},
		{name: "zero reset windows", body: `{"code":"reset","windows_reset":0}`, wantStatus: http.StatusOK},
		{name: "unconfirmed", body: `{"code":"not_available"}`, wantStatus: http.StatusBadGateway},
		{name: "no credit", body: `{"code":"no_credit"}`, wantStatus: http.StatusBadGateway},
		{name: "nothing to reset", body: `{"code":"nothing_to_reset"}`, wantStatus: http.StatusBadGateway},
		{name: "unknown code", body: `{"code":"unknown"}`, wantStatus: http.StatusBadGateway},
		{name: "upstream unavailable", body: `{"code":"reset"}`, upstreamStatus: http.StatusServiceUnavailable, wantStatus: http.StatusBadGateway},
		{name: "missing code", body: `{}`, wantStatus: http.StatusBadGateway},
		{name: "invalid response", body: `not-json`, wantStatus: http.StatusBadGateway},
		{name: "trailing response data", body: `{"code":"reset"} invalid`, wantStatus: http.StatusBadGateway},
		{name: "multiple response documents", body: `{"code":"reset"}{"code":"no_credit"}`, wantStatus: http.StatusBadGateway},
		{name: "oversized response", body: `{"code":"reset","padding":"` + strings.Repeat("x", 16<<10) + `"}`, wantStatus: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if test.upstreamStatus != 0 {
					w.WriteHeader(test.upstreamStatus)
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			previousURL := codexRateLimitResetCreditsConsumeURL
			codexRateLimitResetCreditsConsumeURL = server.URL
			defer func() { codexRateLimitResetCreditsConsumeURL = previousURL }()

			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{
				ID: "codex-confirmed-reset", Provider: "codex", Status: coreauth.StatusError,
				Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour),
				Metadata: map[string]any{"access_token": "fixture-token"},
				Quota:    coreauth.QuotaState{Exceeded: true, Reason: "quota"},
			}
			authIndex := auth.EnsureIndex()
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
			attachCodexResetProbe(t, h, auth, nil)
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(
				http.MethodPost, "/v0/management/reset-quota", strings.NewReader(`{"auth_index":"`+authIndex+`"}`),
			)
			ginCtx.Request.Header.Set("Content-Type", "application/json")
			h.ResetQuota(ginCtx)
			if recorder.Code != test.wantStatus {
				t.Fatalf("HTTP status = %d, want %d", recorder.Code, test.wantStatus)
			}
			current, ok := manager.GetByID(auth.ID)
			if !ok || current == nil {
				t.Fatal("credential disappeared")
			}
			if current.Quota.Exceeded != (test.wantStatus != http.StatusOK) {
				t.Fatalf("local quota cleared without a confirmed reset: status=%d exceeded=%t", recorder.Code, current.Quota.Exceeded)
			}
		})
	}
}

func TestResetQuotaFailurePreservesCodexCooldown(t *testing.T) {
	for _, test := range []struct {
		name             string
		cancelConsume    bool
		wantStatus       int
		wantConsumeCalls int32
	}{
		{name: "cancelled consume", cancelConsume: true, wantStatus: http.StatusBadGateway},
		{name: "local persistence failure", wantStatus: http.StatusInternalServerError, wantConsumeCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var consumeCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				consumeCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":"reset"}`))
			}))
			defer server.Close()
			previousURL := codexRateLimitResetCreditsConsumeURL
			codexRateLimitResetCreditsConsumeURL = server.URL
			t.Cleanup(func() { codexRateLimitResetCreditsConsumeURL = previousURL })
			manager := coreauth.NewManager(nil, nil, nil)
			next := time.Now().Add(time.Hour)
			auth := &coreauth.Auth{
				ID: "fixture-failed-reset", Provider: "codex", Status: coreauth.StatusError,
				Unavailable: true, NextRetryAfter: next,
				Metadata: map[string]any{"access_token": "fixture-token"},
				Quota:    coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
				ModelStates: map[string]*coreauth.ModelState{
					"fixture-model": {Status: coreauth.StatusError, Unavailable: true,
						Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next}},
				},
			}
			authIndex := auth.EnsureIndex()
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancelConsume {
				cancel()
			} else {
				manager.SetStore(&failingDevinAuthStore{err: errors.New("fixture persistence failed")})
			}
			handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
			attachCodexResetProbe(t, handler, auth, nil)
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/reset-quota",
				strings.NewReader(`{"auth_index":"`+authIndex+`"}`)).WithContext(ctx)
			ginCtx.Request.Header.Set("Content-Type", "application/json")
			handler.ResetQuota(ginCtx)
			if recorder.Code != test.wantStatus || consumeCalls.Load() != test.wantConsumeCalls {
				t.Fatalf("status = %d, calls = %d, want %d/%d; body=%s", recorder.Code,
					consumeCalls.Load(), test.wantStatus, test.wantConsumeCalls, recorder.Body.String())
			}
			current, exists := manager.GetByID(auth.ID)
			if !exists || !current.Quota.Exceeded || !current.Unavailable || !current.NextRetryAfter.Equal(next) ||
				!current.ModelStates["fixture-model"].Quota.Exceeded || !current.ModelStates["fixture-model"].Unavailable {
				t.Fatalf("failed reset changed local cooldown: %#v", current)
			}
		})
	}
}

func TestResetCooldownClearsLocalCodexStateWithoutConsumingCredits(t *testing.T) {
	previousURL := codexRateLimitResetCreditsConsumeURL
	codexRateLimitResetCreditsConsumeURL = "http://127.0.0.1:0/reset-credit-must-not-be-called"
	t.Cleanup(func() { codexRateLimitResetCreditsConsumeURL = previousURL })

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID: "codex-local-cooldown", Provider: "codex",
		Metadata: map[string]any{"access_token": "test-token"},
		Status:   coreauth.StatusError, Unavailable: true, NextRetryAfter: next,
		Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
		ModelStates: map[string]*coreauth.ModelState{
			"codex-model": {
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: next,
				Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			},
		},
	}
	authIndex := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/routing/cooldown/reset",
		strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	h.ResetCooldown(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Status    string   `json:"status"`
		AuthIndex string   `json:"auth_index"`
		Models    []string `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.AuthIndex != authIndex || len(response.Models) != 1 || response.Models[0] != "codex-model" {
		t.Fatalf("response = %+v", response)
	}
	updated, exists := manager.GetByID(auth.ID)
	if !exists || updated.Quota.Exceeded || updated.ModelStates["codex-model"].Quota.Exceeded {
		t.Fatalf("local quota cooldown was not cleared: %#v", updated)
	}
}

func TestResetQuotaDoesNotAcceptAuthIDOrFileName(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "reset-auth-id-only",
		FileName: "reset-auth-file-only.json",
		Provider: "claude",
		Status:   coreauth.StatusError,
	}
	authIndex := auth.EnsureIndex()
	if authIndex == auth.ID || authIndex == auth.FileName {
		t.Fatalf("auth_index unexpectedly matches id or file name: %q", authIndex)
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth record: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{name: "auth id field ignored", body: `{"auth_id":"reset-auth-id-only"}`, wantCode: http.StatusBadRequest},
		{name: "id field ignored", body: `{"id":"reset-auth-id-only"}`, wantCode: http.StatusBadRequest},
		{
			name: "file name is not an index", body: `{"auth_index":"reset-auth-file-only.json"}`,
			wantCode: http.StatusNotFound,
		},
		{name: "auth id is not an index", body: `{"auth_index":"reset-auth-id-only"}`, wantCode: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ginCtx, _ := gin.CreateTestContext(recorder)
				request := httptest.NewRequest(
					http.MethodPost,
					"/v0/management/reset-quota",
					strings.NewReader(tt.body),
				)
				request.Header.Set("Content-Type", "application/json")
				ginCtx.Request = request

				h.ResetQuota(ginCtx)

				if recorder.Code != tt.wantCode {
					t.Fatalf("status = %d, want %d body=%s", recorder.Code, tt.wantCode, recorder.Body.String())
				}
			},
		)
	}
}
