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
	"github.com/gin-gonic/gin"
)

func TestPostProviderEntryAppendsToCurrentList(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "existing-secret", Prefix: "existing"}},
	}
	h := newConfigListTestHandler(t, cfg)

	recorder := runConfigListRequest(
		h.PostGeminiKey,
		http.MethodPost,
		"/gemini-api-key",
		`{"api-key":"new-secret","priority":20,"prefix":"new"}`,
	)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("PostGeminiKey status = %d, want 201: %s", recorder.Code, recorder.Body.String())
	}
	updated := h.configSnapshot().GeminiKey
	if len(updated) != 2 {
		t.Fatalf("GeminiKey length = %d, want 2: %+v", len(updated), updated)
	}
	if updated[0].APIKey != "existing-secret" || updated[0].Prefix != "existing" {
		t.Fatalf("existing Gemini key changed: %+v", updated[0])
	}
	if updated[1].APIKey != "new-secret" || updated[1].Priority != 20 || updated[1].Prefix != "new" {
		t.Fatalf("new Gemini key = %+v", updated[1])
	}
}

func TestPostProviderEntryRejectsEmptyIdentity(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(
		h.PostGeminiKey,
		http.MethodPost,
		"/gemini-api-key",
		`{"api-key":"","prefix":"invalid"}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PostGeminiKey status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if len(h.configSnapshot().GeminiKey) != 0 {
		t.Fatalf("empty Gemini key was added: %+v", h.configSnapshot().GeminiKey)
	}
}

func TestPostProviderEntryRejectsDuplicate(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "existing-secret", Prefix: "existing"}},
	}
	h := newConfigListTestHandler(t, cfg)

	recorder := runConfigListRequest(
		h.PostGeminiKey,
		http.MethodPost,
		"/gemini-api-key",
		`{"api-key":"existing-secret","prefix":"existing"}`,
	)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("PostGeminiKey status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	updated := h.configSnapshot().GeminiKey
	if len(updated) != 1 || updated[0].Prefix != "existing" {
		t.Fatalf("Gemini keys changed after duplicate POST: %+v", updated)
	}
}

func TestPostProviderEntryUsesFullRouteIdentity(t *testing.T) {
	t.Run(
		"gemini", func(t *testing.T) {
			cfg := &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "shared-gemini-secret"}}}
			h := newConfigListTestHandler(t, cfg)
			body := `{"api-key":"shared-gemini-secret","base-url":"https://gateway.example/v1"}`

			recorder := runConfigListRequest(h.PostGeminiKey, http.MethodPost, "/gemini-api-key", body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf(
					"PostGeminiKey distinct route status = %d, want 201: %s", recorder.Code, recorder.Body.String(),
				)
			}
			if len(h.configSnapshot().GeminiKey) != 2 {
				t.Fatalf("GeminiKey length = %d, want 2", len(h.configSnapshot().GeminiKey))
			}

			recorder = runConfigListRequest(h.PostGeminiKey, http.MethodPost, "/gemini-api-key", body)
			if recorder.Code != http.StatusConflict {
				t.Fatalf(
					"PostGeminiKey duplicate route status = %d, want 409: %s", recorder.Code, recorder.Body.String(),
				)
			}
		},
	)

	t.Run(
		"gemini style route dimensions", func(t *testing.T) {
			resources := []struct {
				name string
				set  func(*config.Config, []config.GeminiKey)
				get  func(*config.Config) []config.GeminiKey
				post func(*Handler, *gin.Context)
				path string
			}{
				{
					name: "gemini",
					set:  func(cfg *config.Config, entries []config.GeminiKey) { cfg.GeminiKey = entries },
					get:  func(cfg *config.Config) []config.GeminiKey { return cfg.GeminiKey },
					post: (*Handler).PostGeminiKey,
					path: "/gemini-api-key",
				},
				{
					name: "interactions",
					set:  func(cfg *config.Config, entries []config.GeminiKey) { cfg.InteractionsKey = entries },
					get:  func(cfg *config.Config) []config.GeminiKey { return cfg.InteractionsKey },
					post: (*Handler).PostInteractionsKey,
					path: "/interactions-api-key",
				},
			}
			for _, resource := range resources {
				resource := resource
				for _, route := range []struct {
					name       string
					body       string
					wantStatus int
				}{
					{
						name:       "different proxy",
						body:       `{"api-key":"shared-secret","base-url":"https://gateway.example/v1","proxy-url":"https://proxy-b.example","prefix":"team-a","headers":{"X-A":"1","X-B":"2"}}`,
						wantStatus: http.StatusCreated,
					},
					{
						name:       "different prefix",
						body:       `{"api-key":"shared-secret","base-url":"https://gateway.example/v1","proxy-url":"https://proxy-a.example","prefix":"team-b","headers":{"X-A":"1","X-B":"2"}}`,
						wantStatus: http.StatusCreated,
					},
					{
						name:       "different header value",
						body:       `{"api-key":"shared-secret","base-url":"https://gateway.example/v1","proxy-url":"https://proxy-a.example","prefix":"team-a","headers":{"X-A":"1","X-B":"3"}}`,
						wantStatus: http.StatusCreated,
					},
					{
						name:       "same normalized route",
						body:       `{"api-key":" shared-secret ","base-url":" https://gateway.example/v1 ","proxy-url":" https://proxy-a.example ","prefix":" /team-a/ ","headers":{"X-B":"2","X-A":"1"}}`,
						wantStatus: http.StatusConflict,
					},
				} {
					route := route
					t.Run(resource.name+"/"+route.name, func(t *testing.T) {
						cfg := &config.Config{}
						resource.set(
							cfg,
							[]config.GeminiKey{{
								APIKey:   "shared-secret",
								BaseURL:  "https://gateway.example/v1",
								ProxyURL: "https://proxy-a.example",
								Prefix:   "team-a",
								Headers:  map[string]string{"X-A": "1", "X-B": "2"},
							}},
						)
						h := newConfigListTestHandler(t, cfg)
						recorder := runConfigListRequest(
							func(c *gin.Context) { resource.post(h, c) }, http.MethodPost, resource.path, route.body,
						)
						if recorder.Code != route.wantStatus {
							t.Fatalf("status = %d, want %d: %s", recorder.Code, route.wantStatus, recorder.Body.String())
						}
						wantCount := 2
						if route.wantStatus == http.StatusConflict {
							wantCount = 1
						}
						if got := len(resource.get(h.configSnapshot())); got != wantCount {
							t.Fatalf("credential count = %d, want %d", got, wantCount)
						}
					})
				}
			}
		},
	)

	t.Run(
		"claude", func(t *testing.T) {
			cfg := &config.Config{
				ClaudeKey: []config.ClaudeKey{
					{
						APIKey:  "shared-claude-secret",
						BaseURL: "https://api.anthropic.com",
						Prefix:  "first",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			body := `{"api-key":"shared-claude-secret","base-url":"https://api.anthropic.com","priority":10,"prefix":"second","proxy-url":"http://127.0.0.1:8080","models":[{"name":"claude-upstream","alias":"claude-alias"}]}`

			recorder := runConfigListRequest(h.PostClaudeKey, http.MethodPost, "/claude-api-key", body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf(
					"PostClaudeKey distinct route status = %d, want 201: %s", recorder.Code, recorder.Body.String(),
				)
			}
			if len(h.configSnapshot().ClaudeKey) != 2 {
				t.Fatalf("ClaudeKey length = %d, want 2", len(h.configSnapshot().ClaudeKey))
			}

			recorder = runConfigListRequest(h.PostClaudeKey, http.MethodPost, "/claude-api-key", body)
			if recorder.Code != http.StatusConflict {
				t.Fatalf(
					"PostClaudeKey duplicate route status = %d, want 409: %s", recorder.Code, recorder.Body.String(),
				)
			}
		},
	)

	t.Run(
		"codex", func(t *testing.T) {
			cfg := &config.Config{
				CodexKey: []config.CodexKey{
					{
						APIKey:  "shared-codex-secret",
						BaseURL: "https://api.openai.com",
						Prefix:  "first",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			body := `{"api-key":"shared-codex-secret","base-url":"https://api.openai.com","priority":10,"prefix":"second","websockets":true,"proxy-url":"http://127.0.0.1:8080","models":[{"name":"gpt-upstream","alias":"gpt-alias"}]}`

			recorder := runConfigListRequest(h.PostCodexKey, http.MethodPost, "/codex-api-key", body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("PostCodexKey distinct route status = %d, want 201: %s", recorder.Code, recorder.Body.String())
			}
			if len(h.configSnapshot().CodexKey) != 2 {
				t.Fatalf("CodexKey length = %d, want 2", len(h.configSnapshot().CodexKey))
			}

			recorder = runConfigListRequest(h.PostCodexKey, http.MethodPost, "/codex-api-key", body)
			if recorder.Code != http.StatusConflict {
				t.Fatalf(
					"PostCodexKey duplicate route status = %d, want 409: %s", recorder.Code, recorder.Body.String(),
				)
			}
		},
	)
}

func TestCodexConfigurationUpdateCapabilityMutations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		support bool
	}{{"enabled", true}, {"disabled", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConfigListTestHandler(t, &config.Config{})
			payload := func(support bool) string {
				body, err := json.Marshal(map[string]any{
					"api-key": "test-codex-key", "base-url": "https://codex.example/v1",
					"models": []map[string]any{{
						"name": "model", "alias": "alias", "support-configuration-update": support,
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				return string(body)
			}
			assertCapabilities := func(want ...bool) {
				t.Helper()
				recorder := runConfigListRequest(h.GetCodexKeys, http.MethodGet, "/codex-api-key", "")
				var response map[string][]config.CodexKey
				if recorder.Code != http.StatusOK {
					t.Fatalf("GET status = %d: %s", recorder.Code, recorder.Body.String())
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				entries := response["codex-api-key"]
				if len(entries) != len(want) {
					t.Fatalf("GET entries = %d, want %d", len(entries), len(want))
				}
				for i, support := range want {
					if len(entries[i].Models) != 1 || entries[i].Models[0].SupportConfigurationUpdate != support {
						t.Fatalf("GET entry %d models = %+v, want capability %t", i, entries[i].Models, support)
					}
				}
			}
			recorder := runConfigListRequest(h.PutCodexKeys, http.MethodPut, "/codex-api-key", "["+payload(tc.support)+"]")
			if recorder.Code != http.StatusOK {
				t.Fatalf("PUT status = %d: %s", recorder.Code, recorder.Body.String())
			}
			assertCapabilities(tc.support)
			recorder = runConfigListRequest(h.PatchCodexKey, http.MethodPatch, "/codex-api-key",
				`{"index":0,"value":`+payload(!tc.support)+`}`)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PATCH status = %d: %s", recorder.Code, recorder.Body.String())
			}
			assertCapabilities(!tc.support)
			recorder = runConfigListRequest(h.PostCodexKey, http.MethodPost, "/codex-api-key", payload(tc.support))
			if recorder.Code != http.StatusCreated {
				t.Fatalf("POST distinct capability status = %d, want 201: %s", recorder.Code, recorder.Body.String())
			}
			assertCapabilities(!tc.support, tc.support)
			recorder = runConfigListRequest(h.PostCodexKey, http.MethodPost, "/codex-api-key", payload(tc.support))
			if recorder.Code != http.StatusConflict {
				t.Fatalf("POST duplicate capability status = %d, want 409: %s", recorder.Code, recorder.Body.String())
			}
			assertCapabilities(!tc.support, tc.support)
		})
	}
}

func TestPostProviderEntryNormalizesPrefixBeforeDuplicateCheck(t *testing.T) {
	t.Run(
		"claude", func(t *testing.T) {
			cfg := &config.Config{
				ClaudeKey: []config.ClaudeKey{
					{
						APIKey:  "shared-claude-secret",
						BaseURL: "https://api.anthropic.com",
						Prefix:  "team",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			body := `{"api-key":" shared-claude-secret ","base-url":"https://api.anthropic.com","prefix":" /team/ "}`
			recorder := runConfigListRequest(h.PostClaudeKey, http.MethodPost, "/claude-api-key", body)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("PostClaudeKey status = %d, want 409: %s", recorder.Code, recorder.Body.String())
			}
		},
	)

	t.Run(
		"codex", func(t *testing.T) {
			cfg := &config.Config{
				CodexKey: []config.CodexKey{
					{
						APIKey:  "shared-codex-secret",
						BaseURL: "https://api.openai.com",
						Prefix:  "team",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			body := `{"api-key":"shared-codex-secret","base-url":"https://api.openai.com","prefix":"/team/"}`
			recorder := runConfigListRequest(h.PostCodexKey, http.MethodPost, "/codex-api-key", body)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("PostCodexKey status = %d, want 409: %s", recorder.Code, recorder.Body.String())
			}
		},
	)
}

func TestPatchProviderEntriesUpdatesAllEditableFields(t *testing.T) {
	t.Run(
		"gemini", func(t *testing.T) {
			cfg := &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "gemini-secret"}}}
			h := newConfigListTestHandler(t, cfg)
			recorder := runConfigListRequest(
				h.PatchGeminiKey,
				http.MethodPatch,
				"/gemini-api-key",
				`{"index":0,"value":{"priority":7,"prefix":"g","base-url":"https://generativelanguage.googleapis.com","proxy-url":"http://127.0.0.1:8080","models":[{"name":"gemini-upstream","alias":"gemini-alias","display-name":"Gemini Alias","max-context-length":262144}],"headers":{"X-Test":"gemini"},"excluded-models":["gemini-old"]}}`,
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PatchGeminiKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			updated := h.configSnapshot().GeminiKey[0]
			if updated.Priority != 7 || updated.Prefix != "g" || updated.BaseURL == "" || updated.ProxyURL == "" ||
				len(updated.Models) != 1 || updated.Models[0].Alias != "gemini-alias" ||
				updated.Models[0].DisplayName != "Gemini Alias" || updated.Models[0].MaxContextLength != 262144 ||
				updated.Headers["X-Test"] != "gemini" || len(updated.ExcludedModels) != 1 {
				t.Fatalf("updated Gemini key = %+v", updated)
			}
		},
	)

	t.Run(
		"claude", func(t *testing.T) {
			cfg := &config.Config{
				ClaudeKey: []config.ClaudeKey{
					{
						APIKey: "claude-secret", BaseURL: "https://api.anthropic.com",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			recorder := runConfigListRequest(
				h.PatchClaudeKey,
				http.MethodPatch,
				"/claude-api-key",
				`{"index":0,"value":{"models":[{"name":"claude-upstream","alias":"claude-alias","display-name":"Claude Alias","max-context-length":262144}]}}`,
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PatchClaudeKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			model := h.configSnapshot().ClaudeKey[0].Models[0]
			if model.DisplayName != "Claude Alias" || model.MaxContextLength != 262144 {
				t.Fatalf("updated Claude model = %+v", model)
			}
		},
	)

	t.Run(
		"codex", func(t *testing.T) {
			cfg := &config.Config{
				CodexKey: []config.CodexKey{
					{
						APIKey: "codex-secret", BaseURL: "https://api.openai.com",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			recorder := runConfigListRequest(
				h.PatchCodexKey,
				http.MethodPatch,
				"/codex-api-key",
				`{"index":0,"value":{"priority":8,"prefix":"c","base-url":"https://api.openai.com/v1","websockets":true,"proxy-url":"http://127.0.0.1:8081","models":[{"name":"gpt-upstream","alias":"gpt-alias","display-name":"Codex Alias","max-context-length":262144}],"headers":{"X-Test":"codex"},"excluded-models":["gpt-old"]}}`,
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PatchCodexKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			updated := h.configSnapshot().CodexKey[0]
			if updated.Priority != 8 || updated.Prefix != "c" || !updated.Websockets || updated.ProxyURL == "" ||
				len(updated.Models) != 1 || updated.Models[0].Alias != "gpt-alias" ||
				updated.Models[0].DisplayName != "Codex Alias" || updated.Models[0].MaxContextLength != 262144 ||
				updated.Headers["X-Test"] != "codex" || len(updated.ExcludedModels) != 1 {
				t.Fatalf("updated Codex key = %+v", updated)
			}
		},
	)

	t.Run(
		"vertex", func(t *testing.T) {
			cfg := &config.Config{
				VertexCompatAPIKey: []config.VertexCompatKey{
					{
						APIKey: "vertex-secret", BaseURL: "https://aiplatform.googleapis.com",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			recorder := runConfigListRequest(
				h.PatchVertexCompatKey,
				http.MethodPatch,
				"/vertex-api-key",
				`{"index":0,"value":{"priority":9,"prefix":"v","base-url":"https://aiplatform.googleapis.com/v1","proxy-url":"http://127.0.0.1:8082","models":[{"name":"vertex-upstream","alias":"vertex-alias","display-name":"Vertex Alias"}],"headers":{"X-Test":"vertex"},"excluded-models":["vertex-old"]}}`,
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PatchVertexCompatKey status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			updated := h.configSnapshot().VertexCompatAPIKey[0]
			if updated.Priority != 9 || updated.Prefix != "v" || updated.ProxyURL == "" ||
				len(updated.Models) != 1 || updated.Models[0].Alias != "vertex-alias" ||
				updated.Models[0].DisplayName != "Vertex Alias" ||
				updated.Headers["X-Test"] != "vertex" || len(updated.ExcludedModels) != 1 {
				t.Fatalf("updated Vertex key = %+v", updated)
			}
		},
	)

	t.Run(
		"openai", func(t *testing.T) {
			cfg := &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{
					{
						Name: "openai", BaseURL: "https://api.openai.com",
					},
				},
			}
			h := newConfigListTestHandler(t, cfg)
			recorder := runConfigListRequest(
				h.PatchOpenAICompat,
				http.MethodPatch,
				"/openai-compatibility",
				`{"index":0,"name":"openai","value":{"priority":10,"prefix":"o","base-url":"https://api.openai.com/v1","disabled":true,"api-key-entries":[{"api-key":"openai-secret"}],"models":[{"name":"openai-upstream","alias":"openai-alias","display-name":"OpenAI Alias","max-context-length":262144}],"headers":{"X-Test":"openai"},"support-prompt-cache-key":true,"disable-cooling":true}}`,
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PatchOpenAICompat status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			updated := h.configSnapshot().OpenAICompatibility[0]
			if updated.Priority != 10 || updated.Prefix != "o" || !updated.Disabled || !updated.SupportPromptCacheKey || updated.DisableCooling == nil || !*updated.DisableCooling ||
				len(updated.APIKeyEntries) != 1 || updated.APIKeyEntries[0].APIKey != "openai-secret" ||
				len(updated.Models) != 1 || updated.Models[0].Alias != "openai-alias" ||
				updated.Models[0].DisplayName != "OpenAI Alias" || updated.Models[0].MaxContextLength != 262144 ||
				updated.Headers["X-Test"] != "openai" {
				t.Fatalf("updated OpenAI provider = %+v", updated)
			}
		},
	)
}

func TestOpenAICompatibilityManagementRoundTripPreservesC05Fields(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	putBody := `[{"name":"compat","base-url":"https://compat.example/v1","support-prompt-cache-key":true,"disable-cooling":true,"models":[{"name":"gpt-5.5","alias":"configured-gpt","display-name":"Configured GPT","max-context-length":262144}]}]`
	putRecorder := runConfigListRequest(h.PutOpenAICompat, http.MethodPut, "/openai-compatibility", putBody)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PutOpenAICompat status = %d, want 200: %s", putRecorder.Code, putRecorder.Body.String())
	}

	getRecorder := runConfigListRequest(h.GetOpenAICompat, http.MethodGet, "/openai-compatibility", "")
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GetOpenAICompat status = %d, want 200: %s", getRecorder.Code, getRecorder.Body.String())
	}
	var response struct {
		Entries []struct {
			SupportPromptCacheKey bool                              `json:"support-prompt-cache-key"`
			DisableCooling        bool                              `json:"disable-cooling"`
			Models                []config.OpenAICompatibilityModel `json:"models"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GetOpenAICompat response: %v", err)
	}
	if len(response.Entries) != 1 || !response.Entries[0].SupportPromptCacheKey || !response.Entries[0].DisableCooling || len(response.Entries[0].Models) != 1 {
		t.Fatalf("management GET response = %+v", response.Entries)
	}
	model := response.Entries[0].Models[0]
	if model.DisplayName != "Configured GPT" || model.MaxContextLength != 262144 {
		t.Fatalf("management GET model = %+v", model)
	}
}

func TestPatchProviderUsesAuthIndexAfterListReorder(t *testing.T) {
	tests := []struct {
		name          string
		original      *config.Config
		reorder       func(*config.Config)
		authIndex     func(configAuthIndexViews) string
		patch         func(*Handler, string) int
		updatedPrefix func(*config.Config) (string, string)
	}{
		{
			name: "gemini",
			original: &config.Config{
				GeminiKey: []config.GeminiKey{
					{
						APIKey: "shared-gemini-key", BaseURL: "https://gemini.example", Prefix: "first",
						ProxyURL: "https://proxy-a.example",
					},
					{
						APIKey: "shared-gemini-key", BaseURL: "https://gemini.example", Prefix: "second",
						ProxyURL: "https://proxy-b.example",
					},
				},
			},
			reorder:   func(cfg *config.Config) { cfg.GeminiKey[0], cfg.GeminiKey[1] = cfg.GeminiKey[1], cfg.GeminiKey[0] },
			authIndex: func(views configAuthIndexViews) string { return views.gemini[1] },
			patch: func(h *Handler, body string) int {
				return runConfigListRequest(h.PatchGeminiKey, http.MethodPatch, "/gemini-api-key", body).Code
			},
			updatedPrefix: func(cfg *config.Config) (
				string,
				string,
			) {
				return cfg.GeminiKey[0].Prefix, cfg.GeminiKey[1].Prefix
			},
		},
		{
			name: "codex",
			original: &config.Config{
				CodexKey: []config.CodexKey{
					{APIKey: "codex-first", BaseURL: "https://api.openai.com", Prefix: "first"},
					{APIKey: "codex-second", BaseURL: "https://api.openai.com/v1", Prefix: "second"},
				},
			},
			reorder:   func(cfg *config.Config) { cfg.CodexKey[0], cfg.CodexKey[1] = cfg.CodexKey[1], cfg.CodexKey[0] },
			authIndex: func(views configAuthIndexViews) string { return views.codex[1] },
			patch: func(h *Handler, body string) int {
				return runConfigListRequest(h.PatchCodexKey, http.MethodPatch, "/codex-api-key", body).Code
			},
			updatedPrefix: func(cfg *config.Config) (
				string,
				string,
			) {
				return cfg.CodexKey[0].Prefix, cfg.CodexKey[1].Prefix
			},
		},
		{
			name: "vertex",
			original: &config.Config{
				VertexCompatAPIKey: []config.VertexCompatKey{
					{APIKey: "vertex-first", BaseURL: "https://aiplatform.googleapis.com", Prefix: "first"},
					{APIKey: "vertex-second", BaseURL: "https://aiplatform.googleapis.com/v1", Prefix: "second"},
				},
			},
			reorder: func(cfg *config.Config) {
				cfg.VertexCompatAPIKey[0], cfg.VertexCompatAPIKey[1] = cfg.VertexCompatAPIKey[1], cfg.VertexCompatAPIKey[0]
			},
			authIndex: func(views configAuthIndexViews) string { return views.vertex[1] },
			patch: func(h *Handler, body string) int {
				return runConfigListRequest(h.PatchVertexCompatKey, http.MethodPatch, "/vertex-api-key", body).Code
			},
			updatedPrefix: func(cfg *config.Config) (string, string) {
				return cfg.VertexCompatAPIKey[0].Prefix, cfg.VertexCompatAPIKey[1].Prefix
			},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
					&synthesizer.SynthesisContext{
						Config:      tt.original,
						Now:         time.Unix(1_700_000_000, 0),
						IDGenerator: synthesizer.NewStableIDGenerator(),
					},
				)
				if err != nil {
					t.Fatalf("synthesize auths: %v", err)
				}
				for _, auth := range auths {
					if _, err = manager.Register(context.Background(), auth); err != nil {
						t.Fatalf("register auth: %v", err)
					}
				}
				views := (&Handler{authManager: manager}).buildConfigAuthIndexViews(tt.original)
				stableIndex := tt.authIndex(views)
				if stableIndex == "" {
					t.Fatal("stable auth index is empty")
				}

				current := *tt.original
				tt.reorder(&current)
				h := newConfigListTestHandler(t, &current)
				h.authManager = manager
				body, err := json.Marshal(
					map[string]any{
						"index":      1,
						"auth-index": stableIndex,
						"value":      map[string]any{"prefix": "updated-second"},
					},
				)
				if err != nil {
					t.Fatalf("marshal patch body: %v", err)
				}
				if status := tt.patch(h, string(body)); status != http.StatusOK {
					t.Fatalf("patch status = %d, want 200", status)
				}
				first, second := tt.updatedPrefix(h.configSnapshot())
				if first != "updated-second" || second != "first" {
					t.Fatalf("prefixes = %q, %q, want updated-second, first", first, second)
				}
			},
		)
	}
}
