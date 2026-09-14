package management

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestPatchClaudeKeyPreservesSecretAndHiddenFields(t *testing.T) {
	cacheUserID := true
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:                  "secret-key",
				Priority:                100,
				Prefix:                  "old",
				BaseURL:                 "https://old.example.com",
				ProxyURL:                "http://proxy.example.com",
				Models:                  []config.ClaudeModel{{Name: "old-model", Alias: "old-alias"}},
				Headers:                 map[string]string{"X-Test": "old"},
				ExcludedModels:          []string{"old-*"},
				RebuildMidSystemMessage: true,
				Cloak: &config.CloakConfig{
					Mode:           "always",
					StrictMode:     true,
					SensitiveWords: []string{"old"},
					CacheUserID:    &cacheUserID,
				},
				ExperimentalCCHSigning: true,
			},
		},
	}
	h := newConfigListTestHandler(t, cfg)

	recorder := runConfigListRequest(
		h.PatchClaudeKey,
		http.MethodPatch,
		"/claude-api-key",
		`{"index":0,"value":{"priority":0,"prefix":"","base-url":"","proxy-url":"","models":[],"headers":{},"excluded-models":[],"cloak":{"mode":"never","strict-mode":false,"sensitive-words":[]}}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PatchClaudeKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	updated := h.configSnapshot().ClaudeKey[0]
	if updated.APIKey != "secret-key" {
		t.Fatalf("APIKey = %q, want preserved secret", updated.APIKey)
	}
	if updated.Priority != 0 || updated.Prefix != "" || updated.BaseURL != "" || updated.ProxyURL != "" {
		t.Fatalf("cleared scalar fields were retained: %+v", updated)
	}
	if len(updated.Models) != 0 || len(updated.Headers) != 0 || len(updated.ExcludedModels) != 0 {
		t.Fatalf("cleared collection fields were retained: %+v", updated)
	}
	if !updated.RebuildMidSystemMessage || !updated.ExperimentalCCHSigning {
		t.Fatalf("hidden fields changed: %+v", updated)
	}
	if updated.Cloak == nil || updated.Cloak.Mode != "never" || updated.Cloak.StrictMode || len(updated.Cloak.SensitiveWords) != 0 {
		t.Fatalf("Cloak = %+v, want updated visible fields", updated.Cloak)
	}
	if updated.Cloak.CacheUserID == nil || !*updated.Cloak.CacheUserID {
		t.Fatalf("Cloak.CacheUserID = %v, want preserved true", updated.Cloak.CacheUserID)
	}
}

func TestPatchClaudeKeyUsesAuthIndexAfterListReorder(t *testing.T) {
	original := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "first-secret", Prefix: "first"},
			{APIKey: "second-secret", Prefix: "second"},
		},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config:      original,
			Now:         time.Unix(1_700_000_000, 0),
			IDGenerator: synthesizer.NewStableIDGenerator(),
		},
	)
	if err != nil {
		t.Fatalf("synthesize auths: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err = manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth: %v", err)
		}
	}
	staleViews := (&Handler{authManager: manager}).buildConfigAuthIndexViews(original)
	if staleViews.claude[1] == "" {
		t.Fatal("second Claude auth index is empty")
	}

	current := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			original.ClaudeKey[1],
			original.ClaudeKey[0],
		},
	}
	h := newConfigListTestHandler(t, current)
	h.authManager = manager
	body, err := json.Marshal(
		map[string]any{
			"index":      1,
			"auth-index": staleViews.claude[1],
			"value":      map[string]any{"prefix": "updated-second"},
		},
	)
	if err != nil {
		t.Fatalf("marshal patch body: %v", err)
	}

	recorder := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key", string(body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("PatchClaudeKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	updated := h.configSnapshot().ClaudeKey
	if updated[0].Prefix != "updated-second" || updated[1].Prefix != "first" {
		t.Fatalf("Claude keys = %+v, want auth-index target updated after reorder", updated)
	}
}

func TestPatchClaudeKeyDoesNotFallbackWhenAuthIndexIsUnknown(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{APIKey: "first-secret", Prefix: "first"}},
	}
	h := newConfigListTestHandler(t, cfg)
	body := `{"index":0,"auth-index":"unknown","value":{"prefix":"changed"}}`

	recorder := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key", body)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("PatchClaudeKey status = %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
	updated := h.configSnapshot().ClaudeKey
	if updated[0].Prefix != "first" {
		t.Fatalf("Claude key changed after unknown auth-index: %+v", updated[0])
	}
}

func TestPatchClaudeKeyRejectsInvalidAuthIndex(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{APIKey: "first-secret", Prefix: "first"}},
	}
	h := newConfigListTestHandler(t, cfg)
	body := `{"index":0,"auth-index":123,"value":{"prefix":"changed"}}`

	recorder := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key", body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PatchClaudeKey status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	updated := h.configSnapshot().ClaudeKey
	if updated[0].Prefix != "first" {
		t.Fatalf("Claude key changed after invalid auth-index: %+v", updated[0])
	}
}

func TestPatchClaudeKeyCanClearCloakWithoutReplacingSecret(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey: "secret-key",
				Cloak:  &config.CloakConfig{Mode: "always"},
			},
		},
	}
	h := newConfigListTestHandler(t, cfg)

	recorder := runConfigListRequest(
		h.PatchClaudeKey,
		http.MethodPatch,
		"/claude-api-key",
		`{"index":0,"value":{"cloak":null}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PatchClaudeKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	updated := h.configSnapshot().ClaudeKey[0]
	if updated.APIKey != "secret-key" || updated.Cloak != nil {
		t.Fatalf("updated Claude key = %+v, want preserved secret and nil cloak", updated)
	}
}
