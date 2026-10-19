package management

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestPatchCodexKeyCloakingOverride(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{
		Codex:    config.CodexConfig{DisableCodexCloaking: true},
		CodexKey: []config.CodexKey{{APIKey: "synthetic-config-key", BaseURL: "https://example.test"}},
	})
	for _, step := range []struct {
		name, value, expected string
		status                int
	}{
		{"override_false", "false", "false", http.StatusOK},
		{"override_true", "true", "true", http.StatusOK},
		{"reject_string", `"true"`, "true", http.StatusBadRequest},
		{"inherit", "null", "", http.StatusOK},
	} {
		t.Run(step.name, func(t *testing.T) {
			response := runConfigListRequest(h.PatchCodexKey, http.MethodPatch, "/codex-api-key",
				`{"index":0,"value":{"disable-codex-cloaking":`+step.value+`}}`)
			if response.Code != step.status {
				t.Fatalf("PATCH status = %d: %s", response.Code, response.Body.String())
			}
			data, err := os.ReadFile(h.configFilePath)
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := config.ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range []*config.Config{h.configSnapshot(), reloaded} {
				key := cfg.CodexKey[0]
				if key.APIKey != "synthetic-config-key" || !cfg.Codex.DisableCodexCloaking {
					t.Fatalf("Codex key or global setting changed: %+v", cfg)
				}
				if step.expected == "" && key.DisableCodexCloaking != nil ||
					step.expected == "true" && (key.DisableCodexCloaking == nil || !*key.DisableCodexCloaking) ||
					step.expected == "false" && (key.DisableCodexCloaking == nil || *key.DisableCodexCloaking) {
					t.Fatalf("Codex override = %v, want %q", key.DisableCodexCloaking, step.expected)
				}
				auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
					Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
				})
				if err != nil || len(auths) != 1 || auths[0].Attributes[coreauth.AttributeCodexDisableCloaking] != step.expected {
					t.Fatalf("auth cloaking attribute = %+v, error = %v", auths, err)
				}
			}
		})
	}
}

func TestOtherKeysRejectCodexCloakingOverride(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create func(*Handler, *gin.Context)
		key    string
	}{
		{"xai", (*Handler).PostXAIKey, "xai-api-key"},
		{"meta", (*Handler).PostMetaKey, "meta-api-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConfigListTestHandler(t, &config.Config{})
			response := runConfigListRequest(func(c *gin.Context) { tc.create(h, c) }, http.MethodPost, "/"+tc.key,
				`{"api-key":"synthetic-config-key","base-url":"https://example.test","disable-codex-cloaking":true}`)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "disable-codex-cloaking") {
				t.Fatalf("unsupported %s override: status=%d body=%s", tc.key, response.Code, response.Body.String())
			}
		})
	}
}

func TestClaudeKeyWritesNormalizeCloakAndReload(t *testing.T) {
	for _, test := range []struct {
		method string
		write  func(*Handler, *gin.Context)
	}{
		{http.MethodPut, (*Handler).PutClaudeKeys},
		{http.MethodPost, (*Handler).PostClaudeKey},
		{http.MethodPatch, (*Handler).PatchClaudeKey},
	} {
		for _, cacheUserID := range []string{"false", "true", "null"} {
			t.Run(test.method+"/"+cacheUserID, func(t *testing.T) {
				cfg := &config.Config{}
				if test.method == http.MethodPatch {
					cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "synthetic-key", ExperimentalCCHSigning: true}}
				}
				h := newConfigListTestHandler(t, cfg)
				entry := `{"api-key":"synthetic-key","cloak":{"mode":"  never  ","strict-mode":false,"cache-user-id":` + cacheUserID + `,"sensitive-words":[" alpha ","","  "," beta "]}}`
				body := entry
				switch test.method {
				case http.MethodPut:
					body = "[" + entry + "]"
				case http.MethodPatch:
					body = `{"index":0,"value":` + entry + `}`
				}
				response := runConfigListRequest(func(c *gin.Context) { test.write(h, c) }, test.method, "/claude-api-key", body)
				wantStatus := http.StatusOK
				if test.method == http.MethodPost {
					wantStatus = http.StatusCreated
				}
				if response.Code != wantStatus {
					t.Fatalf("%s status = %d, want %d: %s", test.method, response.Code, wantStatus, response.Body.String())
				}
				reloaded, err := config.LoadConfigOptional(h.configFilePath, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, current := range []*config.Config{h.configSnapshot(), reloaded} {
					if len(current.ClaudeKey) != 1 {
						t.Fatalf("Claude key count = %d", len(current.ClaudeKey))
					}
					key := current.ClaudeKey[0]
					if key.APIKey != "synthetic-key" || (test.method == http.MethodPatch && !key.ExperimentalCCHSigning) {
						t.Fatalf("Claude key identity or hidden fields changed: %+v", key)
					}
					cloak := key.Cloak
					if cloak == nil || cloak.Mode != "never" || cloak.StrictMode || !reflect.DeepEqual(cloak.SensitiveWords, []string{"alpha", "beta"}) {
						t.Fatalf("cloak not normalized: %+v", cloak)
					}
					if cacheUserID == "null" {
						if cloak.CacheUserID != nil {
							t.Fatalf("null cache-user-id became %v", cloak.CacheUserID)
						}
					} else if cloak.CacheUserID == nil || *cloak.CacheUserID != (cacheUserID == "true") {
						t.Fatalf("cache-user-id = %v, want %s", cloak.CacheUserID, cacheUserID)
					}
				}
			})
		}
	}
}

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

func TestPutClaudeKeysPreservesModeForUnchangedIdentity(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "secret-key", BaseURL: "https://api.example.com", Prefix: "old", Cloak: &config.CloakConfig{Mode: "always"}},
	}}
	h := newConfigListTestHandler(t, cfg)

	for _, body := range []string{
		`[{"api-key":"secret-key","base-url":"https://api.example.com","prefix":"old"}]`,
		`[{"api-key":"secret-key","base-url":"https://api.example.com","prefix":"old","cloak":{"strict-mode":true}}]`,
	} {
		recorder := runConfigListRequest(h.PutClaudeKeys, http.MethodPut, "/claude-api-key", body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PutClaudeKeys status = %d, want 200: %s", recorder.Code, recorder.Body.String())
		}
		key := h.configSnapshot().ClaudeKey[0]
		if key.Cloak == nil || key.Cloak.Mode != "always" {
			t.Fatalf("Claude key cloak after PUT = %+v, want mode always", key.Cloak)
		}
		data, err := os.ReadFile(h.configFilePath)
		if err != nil || !strings.Contains(string(data), "mode: always") {
			t.Fatalf("saved config does not preserve cloak mode: %q, error = %v", data, err)
		}
	}
	if !h.configSnapshot().ClaudeKey[0].Cloak.StrictMode {
		t.Fatal("PUT lost the explicitly submitted strict-mode")
	}
}

func TestPutClaudeKeysDoesNotInheritModeFromDifferentOrAmbiguousIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		original []config.ClaudeKey
		body     string
	}{
		{
			name:     "changed prefix",
			original: []config.ClaudeKey{{APIKey: "key", Prefix: "old", Cloak: &config.CloakConfig{Mode: "always"}}},
			body:     `[{"api-key":"key","prefix":"new"}]`,
		},
		{
			name: "ambiguous identity",
			original: []config.ClaudeKey{
				{APIKey: "key", Cloak: &config.CloakConfig{Mode: "always"}},
				{APIKey: "key", Cloak: &config.CloakConfig{Mode: "never"}},
			},
			body: `[{"api-key":"key"}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newConfigListTestHandler(t, &config.Config{ClaudeKey: test.original})
			recorder := runConfigListRequest(h.PutClaudeKeys, http.MethodPut, "/claude-api-key", test.body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PutClaudeKeys status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			if got := h.configSnapshot().ClaudeKey[0].Cloak; got != nil && got.Mode != "" {
				t.Fatalf("inherited unrelated cloak mode: %+v", got)
			}
		})
	}
}

func TestPatchClaudeKeyClearsInheritedCloakForChangedIdentity(t *testing.T) {
	for _, tt := range []struct {
		name  string
		patch string
		mode  string
	}{
		{name: "api key", patch: `"api-key":"replacement-secret"`},
		{name: "base URL", patch: `"base-url":"https://new.example.com"`},
		{name: "prefix", patch: `"prefix":"new"`},
		{name: "proxy URL", patch: `"proxy-url":"http://new-proxy.example.com"`},
		{name: "unchanged identity", patch: `"priority":2`, mode: "always"},
		{name: "partial cloak for new identity", patch: `"api-key":"replacement-secret","cloak":{"strict-mode":false}`},
		{name: "explicit mode", patch: `"api-key":"replacement-secret","cloak":{"mode":"never"}`, mode: "never"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{{
				APIKey: "original-secret", BaseURL: "https://old.example.com", Prefix: "old",
				ProxyURL: "http://proxy.example.com", Cloak: &config.CloakConfig{Mode: "always", StrictMode: true},
			}}})
			response := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key",
				`{"index":0,"value":{`+tt.patch+`}}`)
			if response.Code != http.StatusOK {
				t.Fatalf("PATCH status = %d: %s", response.Code, response.Body.String())
			}
			key := h.configSnapshot().ClaudeKey[0]
			if key.Cloak == nil || key.Cloak.Mode != tt.mode {
				t.Fatalf("cloak after PATCH = %+v, want mode %q", key.Cloak, tt.mode)
			}
		})
	}
}

func TestPatchClaudeKeyMaskedIdentityPreservesCloak(t *testing.T) {
	const key = "original-secret"
	h := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{{
		APIKey: key, Cloak: &config.CloakConfig{Mode: "always"},
	}}})
	response := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key",
		`{"index":0,"value":{"api-key":"`+util.HideAPIKey(key)+`","priority":2}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d: %s", response.Code, response.Body.String())
	}
	updated := h.configSnapshot().ClaudeKey[0]
	if updated.APIKey != key || updated.Cloak == nil || updated.Cloak.Mode != "always" {
		t.Fatalf("masked identity changed key or cloak: %+v", updated)
	}
}

func TestPatchClaudeKeyCacheUserIDSupportsFalseAndNull(t *testing.T) {
	cacheUserID := true
	h := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "key", Cloak: &config.CloakConfig{Mode: "always", CacheUserID: &cacheUserID}},
	}})
	for _, test := range []struct {
		value string
		want  *bool
	}{
		{value: "false", want: new(false)},
		{value: "null", want: nil},
	} {
		recorder := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key",
			`{"index":0,"value":{"cloak":{"cache-user-id":`+test.value+`}}}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PatchClaudeKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
		}
		key := h.configSnapshot().ClaudeKey[0]
		if key.Cloak == nil || (key.Cloak.CacheUserID == nil) != (test.want == nil) ||
			(test.want != nil && *key.Cloak.CacheUserID != *test.want) || key.Cloak.Mode != "always" {
			t.Fatalf("Claude key cloak = %+v, want cache-user-id %v and mode always", key.Cloak, test.want)
		}
	}
}

func TestPatchClaudeKeyCacheUserIDRejectsInvalidValue(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "key", Cloak: &config.CloakConfig{Mode: "always"}},
	}})
	recorder := runConfigListRequest(h.PatchClaudeKey, http.MethodPatch, "/claude-api-key",
		`{"index":0,"value":{"cloak":{"cache-user-id":"invalid"}}}`)
	if recorder.Code != http.StatusBadRequest || h.configSnapshot().ClaudeKey[0].Cloak.CacheUserID != nil {
		t.Fatalf("invalid cloak cache-user-id accepted: status = %d, body = %s", recorder.Code, recorder.Body.String())
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
