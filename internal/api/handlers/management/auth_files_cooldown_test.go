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
