package management

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func registerCooldownTestAuth(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) *coreauth.Auth {
	t.Helper()
	authDir := t.TempDir()
	path := filepath.Join(authDir, auth.ID+".json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"test"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["path"] = path
	auth.FileName = filepath.Base(path)
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return registered
}

func requestAuthFileList(t *testing.T, handler *Handler) []byte {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/auth-files", handler.ListAuthFiles)
	request := httptest.NewRequest(http.MethodGet, "/auth-files", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

func TestListAuthFilesPublishesReadOnlyCooldownSnapshot(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{})
	now := time.Now()
	credentialRetryAt := now.Add(1500 * time.Millisecond)
	credential := registerCooldownTestAuth(t, manager, &coreauth.Auth{
		ID:             "credential-cooldown",
		Provider:       "test",
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: credentialRetryAt,
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: credentialRetryAt,
			BackoffLevel:  2,
		},
		LastError: &coreauth.Error{
			Code:       "rate_limit",
			Message:    "Bearer secret-upstream-value",
			HTTPStatus: http.StatusTooManyRequests,
		},
	})
	modelRetryAt := now.Add(2500 * time.Millisecond)
	model := registerCooldownTestAuth(t, manager, &coreauth.Auth{
		ID:       "model-cooldown",
		Provider: "test",
		Status:   coreauth.StatusActive,
		ModelStates: map[string]*coreauth.ModelState{
			"model-a": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: modelRetryAt,
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: modelRetryAt,
					BackoffLevel:  1,
				},
				LastError: &coreauth.Error{Code: "rate_limit", HTTPStatus: http.StatusTooManyRequests},
			},
			"expired": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(-time.Second),
			},
		},
	})
	registerCooldownTestAuth(t, manager, &coreauth.Auth{ID: "known-empty", Provider: "test", Status: coreauth.StatusActive})

	credentialBefore, _ := manager.GetByID(credential.ID)
	modelBefore, _ := manager.GetByID(model.ID)
	body := requestAuthFileList(t, NewHandlerWithoutConfigFilePath(&config.Config{}, manager))

	var payload struct {
		ObservedAt time.Time `json:"observed_at"`
		Files      []struct {
			ID        string                  `json:"id"`
			Cooldowns []coreauth.CooldownView `json:"cooldowns"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.ObservedAt.IsZero() || len(payload.Files) != 3 {
		t.Fatalf("snapshot = observed_at:%s files:%d", payload.ObservedAt, len(payload.Files))
	}
	byID := make(map[string][]coreauth.CooldownView, len(payload.Files))
	for _, file := range payload.Files {
		byID[file.ID] = file.Cooldowns
	}
	cooldownJSON, errMarshal := json.Marshal(byID)
	if errMarshal != nil {
		t.Fatalf("marshal cooldown projection: %v", errMarshal)
	}
	if strings.Contains(string(cooldownJSON), "secret-upstream-value") {
		t.Fatalf("cooldown projection exposed raw error: %s", cooldownJSON)
	}
	credentialViews := byID[credential.ID]
	if len(credentialViews) != 1 || credentialViews[0].Scope != "credential" ||
		credentialViews[0].Reason != "quota" || credentialViews[0].HTTPStatus != http.StatusTooManyRequests ||
		credentialViews[0].BackoffLevel == nil || *credentialViews[0].BackoffLevel != 2 {
		t.Fatalf("credential cooldowns = %#v", credentialViews)
	}
	wantCredentialSeconds := int64(math.Ceil(credentialRetryAt.Sub(payload.ObservedAt).Seconds()))
	if credentialViews[0].RemainingSeconds != wantCredentialSeconds {
		t.Fatalf("credential remaining_seconds = %d, want %d", credentialViews[0].RemainingSeconds, wantCredentialSeconds)
	}
	modelViews := byID[model.ID]
	if len(modelViews) != 1 || modelViews[0].Scope != "model" || modelViews[0].ModelKey != "model-a" {
		t.Fatalf("model cooldowns = %#v", modelViews)
	}
	if empty, ok := byID["known-empty"]; !ok || empty == nil || len(empty) != 0 {
		t.Fatalf("known-empty cooldowns = %#v, present=%v", empty, ok)
	}

	credentialAfter, _ := manager.GetByID(credential.ID)
	modelAfter, _ := manager.GetByID(model.ID)
	if !credentialAfter.NextRetryAfter.Equal(credentialBefore.NextRetryAfter) ||
		!credentialAfter.Quota.NextRecoverAt.Equal(credentialBefore.Quota.NextRecoverAt) ||
		!modelAfter.ModelStates["model-a"].NextRetryAfter.Equal(modelBefore.ModelStates["model-a"].NextRetryAfter) {
		t.Fatal("ListAuthFiles mutated cooldown state")
	}
}

func TestListAuthFilesDiskFallbackOmitsUnknownCooldownContract(t *testing.T) {
	authDir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(authDir, "disk.json"), []byte(`{"type":"test"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	body := requestAuthFileList(t, NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil))
	var payload map[string]any
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if _, exists := payload["observed_at"]; exists {
		t.Fatalf("disk fallback published observed_at: %#v", payload)
	}
	files, _ := payload["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("disk files = %#v", files)
	}
	entry, _ := files[0].(map[string]any)
	if _, exists := entry["cooldowns"]; exists {
		t.Fatalf("disk fallback published cooldowns: %#v", entry)
	}
}

func TestListAuthFilesUsesNullCooldownsWhenHomeOwnsState(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	registerCooldownTestAuth(t, manager, &coreauth.Auth{ID: "home-owned", Provider: "test", Status: coreauth.StatusActive})

	body := requestAuthFileList(t, NewHandlerWithoutConfigFilePath(&config.Config{}, manager))
	var payload struct {
		Files []struct {
			Cooldowns json.RawMessage `json:"cooldowns"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Files) != 1 || string(payload.Files[0].Cooldowns) != "null" {
		t.Fatalf("home cooldowns = %s", payload.Files[0].Cooldowns)
	}
}

func TestListAuthFilesReconcilesCooldownWithoutChangingAuthState(t *testing.T) {
	now := time.Now().UTC()
	expired := now.Add(-10 * time.Minute)
	future := now.Add(10 * time.Minute)
	for _, tc := range []struct {
		name            string
		auth            *coreauth.Auth
		wantStatus      coreauth.Status
		wantUnavailable bool
		wantMessage     string
		wantRetry       bool
	}{
		{
			name: "expired credential quota",
			auth: &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "credential_quota", Unavailable: true,
				NextRetryAfter: expired, Quota: coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: expired}},
			wantStatus: coreauth.StatusActive,
		},
		{
			name: "expired model cooldown",
			auth: &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "rate limit exceeded", Unavailable: true,
				NextRetryAfter: expired, ModelStates: map[string]*coreauth.ModelState{"model": {
					Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: expired,
				}}},
			wantStatus: coreauth.StatusActive,
		},
		{
			name: "expired access token",
			auth: &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "token expired", Unavailable: true,
				NextRetryAfter: expired, Metadata: map[string]any{"type": "codex", "access_token": "expired", "expired": expired.Format(time.RFC3339)}},
			wantStatus: coreauth.StatusError, wantUnavailable: true, wantMessage: "token expired",
		},
		{
			name: "terminal unauthorized",
			auth: &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "unauthorized", Unavailable: true,
				NextRetryAfter: expired, LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized}},
			wantStatus: coreauth.StatusError, wantUnavailable: true, wantMessage: "unauthorized",
		},
		{
			name: "refresh retry with valid access token",
			auth: &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "credential_quota", Unavailable: true,
				NextRetryAfter: expired, NextRefreshAfter: future, LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized},
				Quota:    coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: expired},
				Metadata: map[string]any{"type": "codex", "access_token": "valid", "expired": future.Format(time.RFC3339)}},
			wantStatus: coreauth.StatusActive,
		},
		{
			name: "one model remains schedulable",
			auth: &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "model cooldown", Unavailable: true,
				ModelStates: map[string]*coreauth.ModelState{
					"cooling": {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: future},
					"ready":   {Status: coreauth.StatusActive},
				}},
			wantStatus: coreauth.StatusActive,
		},
		{
			name: "all models blocked",
			auth: &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "model failure", Unavailable: true,
				ModelStates: map[string]*coreauth.ModelState{
					"cooling": {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: future},
					"blocked": {Status: coreauth.StatusError, Unavailable: true},
				}},
			wantStatus: coreauth.StatusError, wantUnavailable: true, wantMessage: "model failure",
		},
		{
			name:       "active auth with inactive retry timestamp",
			auth:       &coreauth.Auth{Status: coreauth.StatusActive, NextRetryAfter: future},
			wantStatus: coreauth.StatusActive,
		},
		{
			name:       "active credential cooldown",
			auth:       &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "rate limited", Unavailable: true, NextRetryAfter: future},
			wantStatus: coreauth.StatusError, wantUnavailable: true, wantMessage: "rate limited", wantRetry: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			tc.auth.ID, tc.auth.Provider = "credential", "codex"
			registerCooldownTestAuth(t, manager, tc.auth)
			before, _ := manager.GetByID(tc.auth.ID)
			body := requestAuthFileList(t, NewHandlerWithoutConfigFilePath(&config.Config{}, manager))
			var payload struct {
				Files []struct {
					Status         coreauth.Status `json:"status"`
					StatusMessage  string          `json:"status_message"`
					Unavailable    bool            `json:"unavailable"`
					NextRetryAfter *time.Time      `json:"next_retry_after"`
				} `json:"files"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || len(payload.Files) != 1 {
				t.Fatalf("decode auth file response: %v, %s", err, body)
			}
			file := payload.Files[0]
			if file.Status != tc.wantStatus || file.StatusMessage != tc.wantMessage || file.Unavailable != tc.wantUnavailable ||
				(file.NextRetryAfter != nil) != tc.wantRetry {
				t.Fatalf("auth file state = %+v, want status=%s, message=%q, unavailable=%t, retry=%t",
					file, tc.wantStatus, tc.wantMessage, tc.wantUnavailable, tc.wantRetry)
			}
			after, _ := manager.GetByID(tc.auth.ID)
			if after.Status != before.Status || after.Unavailable != before.Unavailable ||
				!after.NextRetryAfter.Equal(before.NextRetryAfter) {
				t.Fatalf("listing mutated auth state: before=%+v, after=%+v", before, after)
			}
		})
	}
}
