package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	var called bool
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Method != http.MethodPost {
					t.Fatalf("method = %s, want POST", r.Method)
				}
				if r.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
					t.Fatalf("path = %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
					t.Fatalf("Authorization = %q", got)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if body["redeem_request_id"] == "" {
					t.Fatalf("missing redeem_request_id")
				}
				w.WriteHeader(http.StatusOK)
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
	if !called {
		t.Fatalf("Codex reset credit endpoint was not called")
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
