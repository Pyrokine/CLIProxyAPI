package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestPatchProviderCredentialWeight(t *testing.T) {
	tests := []struct {
		name  string
		cfg   *config.Config
		patch func(*Handler, *gin.Context)
		get   func(*config.Config) *int
	}{
		{
			name:  "gemini",
			cfg:   &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "key"}}},
			patch: (*Handler).PatchGeminiKey,
			get:   func(cfg *config.Config) *int { return cfg.GeminiKey[0].Weight },
		},
		{
			name:  "claude",
			cfg:   &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key"}}},
			patch: (*Handler).PatchClaudeKey,
			get:   func(cfg *config.Config) *int { return cfg.ClaudeKey[0].Weight },
		},
		{
			name: "vertex",
			cfg: &config.Config{
				VertexCompatAPIKey: []config.VertexCompatKey{
					{
						APIKey: "key", BaseURL: "https://example.com",
					},
				},
			},
			patch: (*Handler).PatchVertexCompatKey,
			get:   func(cfg *config.Config) *int { return cfg.VertexCompatAPIKey[0].Weight },
		},
		{
			name: "codex",
			cfg: &config.Config{
				CodexKey: []config.CodexKey{
					{
						APIKey: "key", BaseURL: "https://example.com",
					},
				},
			},
			patch: (*Handler).PatchCodexKey,
			get:   func(cfg *config.Config) *int { return cfg.CodexKey[0].Weight },
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				h := newConfigListTestHandler(t, test.cfg)
				recorder := runConfigListRequest(
					func(c *gin.Context) { test.patch(h, c) },
					http.MethodPatch,
					"/provider",
					`{"index":0,"value":{"weight":7}}`,
				)
				if recorder.Code != http.StatusOK {
					t.Fatalf("PATCH status = %d, want 200: %s", recorder.Code, recorder.Body.String())
				}
				weight := test.get(h.configSnapshot())
				if weight == nil || *weight != 7 {
					t.Fatalf("weight = %v, want 7", weight)
				}
			},
		)
	}
}

func TestPatchProviderCredentialWeightZeroResetAndStrictValidation(t *testing.T) {
	initial := 5
	h := newConfigListTestHandler(
		t, &config.Config{
			GeminiKey: []config.GeminiKey{{APIKey: "key", Weight: &initial}},
		},
	)
	patch := func(raw string) *httptest.ResponseRecorder {
		t.Helper()
		return runConfigListRequest(
			h.PatchGeminiKey,
			http.MethodPatch,
			"/gemini-api-key",
			`{"index":0,"value":{"weight":`+raw+`}}`,
		)
	}

	for _, invalid := range []string{"1.5", "1000001", "9223372036854775808", `"7"`} {
		recorder := patch(invalid)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("weight %s status = %d, want 400: %s", invalid, recorder.Code, recorder.Body.String())
		}
		weight := h.configSnapshot().GeminiKey[0].Weight
		if weight == nil || *weight != initial {
			t.Fatalf("invalid weight %s changed config to %v", invalid, weight)
		}
	}

	if recorder := patch("0"); recorder.Code != http.StatusOK {
		t.Fatalf("zero status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	weight := h.configSnapshot().GeminiKey[0].Weight
	if weight == nil || *weight != 0 {
		t.Fatalf("zero weight = %v, want explicit zero", weight)
	}

	if recorder := patch("null"); recorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if weight = h.configSnapshot().GeminiKey[0].Weight; weight != nil {
		t.Fatalf("reset weight = %v, want nil", weight)
	}
}

func TestPatchAuthFileWeightNormalizesAndPersistsZero(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:         "auth-1",
		Provider:   "codex",
		FileName:   "codex.json",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{},
		Metadata:   map[string]any{},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		bytes.NewBufferString(`{"name":"auth-1","weight":-5}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	updated, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("updated auth not found")
	}
	if updated.Attributes[coreauth.AttributeWeight] != "0" {
		t.Fatalf("attribute weight = %q, want 0", updated.Attributes[coreauth.AttributeWeight])
	}
	if updated.Metadata[coreauth.AttributeWeight] != int64(0) {
		t.Fatalf("metadata weight = %#v, want int64(0)", updated.Metadata[coreauth.AttributeWeight])
	}
	store.mu.Lock()
	persisted := store.items["auth-1"]
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata[coreauth.AttributeWeight] != int64(0) {
		t.Fatalf("persisted weight = %#v, want int64(0)", persisted)
	}
}

func TestPatchAuthFileWeightClearsWithNull(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID:         "auth-1",
			Provider:   "codex",
			Attributes: map[string]string{coreauth.AttributeWeight: "7"},
			Metadata:   map[string]any{coreauth.AttributeWeight: int64(7)},
		},
	); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		bytes.NewBufferString(`{"auth-index":"auth-1","weight":null}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	updated, _ := manager.GetByID("auth-1")
	if _, ok := updated.Metadata[coreauth.AttributeWeight]; ok {
		t.Fatalf("metadata weight was not cleared: %#v", updated.Metadata)
	}
	if _, ok := updated.Attributes[coreauth.AttributeWeight]; ok {
		t.Fatalf("attribute weight was not cleared: %#v", updated.Attributes)
	}
}

func TestPatchAuthFileWeightRejectsAboveMaximum(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(
		context.Background(), &coreauth.Auth{
			ID: "auth-1", Provider: "codex", Metadata: map[string]any{},
		},
	); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		bytes.NewBufferString(`{"name":"auth-1","weight":1000001}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	updated, _ := manager.GetByID("auth-1")
	if _, ok := updated.Metadata[coreauth.AttributeWeight]; ok {
		t.Fatalf("invalid weight changed metadata: %#v", updated.Metadata)
	}
}
