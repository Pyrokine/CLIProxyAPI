package management

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestProviderRequestScopedErrorsPatchAndPersistence(t *testing.T) {
	tests := []struct {
		name  string
		cfg   *config.Config
		patch func(*Handler, *gin.Context)
		rules func(*config.Config) []config.RequestScopedErrorRule
	}{
		{
			name: "gemini", cfg: &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "g"}}},
			patch: (*Handler).PatchGeminiKey,
			rules: func(cfg *config.Config) []config.RequestScopedErrorRule { return cfg.GeminiKey[0].RequestScopedErrors },
		},
		{
			name: "interactions", cfg: &config.Config{InteractionsKey: []config.GeminiKey{{APIKey: "i"}}},
			patch: (*Handler).PatchInteractionsKey,
			rules: func(cfg *config.Config) []config.RequestScopedErrorRule {
				return cfg.InteractionsKey[0].RequestScopedErrors
			},
		},
		{
			name: "claude", cfg: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "c"}}},
			patch: (*Handler).PatchClaudeKey,
			rules: func(cfg *config.Config) []config.RequestScopedErrorRule { return cfg.ClaudeKey[0].RequestScopedErrors },
		},
		{
			name: "codex", cfg: &config.Config{CodexKey: []config.CodexKey{{APIKey: "x", BaseURL: "https://api.openai.com/v1"}}},
			patch: (*Handler).PatchCodexKey,
			rules: func(cfg *config.Config) []config.RequestScopedErrorRule { return cfg.CodexKey[0].RequestScopedErrors },
		},
		{
			name: "xai", cfg: &config.Config{XAIKey: []config.CodexKey{{APIKey: "a", BaseURL: "https://api.x.ai/v1"}}},
			patch: (*Handler).PatchXAIKey,
			rules: func(cfg *config.Config) []config.RequestScopedErrorRule { return cfg.XAIKey[0].RequestScopedErrors },
		},
		{
			name:  "openai compatibility",
			cfg:   &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "compat", BaseURL: "https://example.test"}}},
			patch: (*Handler).PatchOpenAICompat,
			rules: func(cfg *config.Config) []config.RequestScopedErrorRule {
				return cfg.OpenAICompatibility[0].RequestScopedErrors
			},
		},
	}

	const body = `{"index":0,"value":{"request-scoped-errors":[{"status":400,"match":["limit"],"match-regexr":["^limit$"],"action":"stop"}]}}`
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newConfigListTestHandler(t, test.cfg)
			response := runConfigListRequest(func(c *gin.Context) { test.patch(h, c) }, http.MethodPatch, "/provider", body)
			if response.Code != http.StatusOK {
				t.Fatalf("PATCH status = %d, body = %s", response.Code, response.Body.String())
			}

			persisted, err := os.ReadFile(h.configFilePath)
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := config.ParseConfigBytes(persisted)
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			for source, cfg := range map[string]*config.Config{"snapshot": h.configSnapshot(), "reloaded": reloaded} {
				rules := test.rules(cfg)
				if len(rules) != 1 || rules[0].Status != 400 || rules[0].Action != "stop" ||
					len(rules[0].Match) != 1 || rules[0].Match[0] != "limit" ||
					len(rules[0].MatchRegexr) != 1 || rules[0].MatchRegexr[0] != "^limit$" {
					t.Fatalf("%s rules = %#v", source, rules)
				}
			}

			clear := runConfigListRequest(func(c *gin.Context) { test.patch(h, c) }, http.MethodPatch, "/provider", `{"index":0,"value":{"request-scoped-errors":[]}}`)
			if clear.Code != http.StatusOK || len(test.rules(h.configSnapshot())) != 0 {
				t.Fatalf("clear PATCH status = %d, rules = %#v", clear.Code, test.rules(h.configSnapshot()))
			}
		})
	}
}

func TestOpenAICompatibilityGETProjectsRequestScopedErrors(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "compat", BaseURL: "https://example.test",
		RequestScopedErrors: []config.RequestScopedErrorRule{{Status: 400, Match: []string{"limit"}, Action: "stop"}},
	}}})

	response := runConfigListRequest(h.GetOpenAICompat, http.MethodGet, "/openai-compatibility", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", response.Code, response.Body.String())
	}
	var document struct {
		Entries []struct {
			Rules []config.RequestScopedErrorRule `json:"request-scoped-errors"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Entries) != 1 || len(document.Entries[0].Rules) != 1 || document.Entries[0].Rules[0].Action != "stop" {
		t.Fatalf("GET projection = %#v", document.Entries)
	}
}

func TestOAuthRequestScopedErrorsManagementLifecycle(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})

	put := runConfigListRequest(
		h.PutOAuthRequestScopedErrors,
		http.MethodPut,
		"/oauth-request-scoped-errors",
		`{"items":{" Vertex ":[{"status":400,"match":[" limit ",""],"match-regexr":[" ^limit$ "],"action":" STOP "}],"ignored":[{"status":0,"match":["bad"],"action":"stop"}]}}`,
	)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", put.Code, put.Body.String())
	}
	vertex := h.configSnapshot().OAuthRequestScopedErrors["vertex"]
	if len(h.configSnapshot().OAuthRequestScopedErrors) != 1 || len(vertex) != 1 || vertex[0].Action != "stop" ||
		len(vertex[0].Match) != 1 || vertex[0].Match[0] != "limit" || vertex[0].MatchRegexr[0] != "^limit$" {
		t.Fatalf("PUT snapshot = %#v", h.configSnapshot().OAuthRequestScopedErrors)
	}

	get := runConfigListRequest(h.GetOAuthRequestScopedErrors, http.MethodGet, "/oauth-request-scoped-errors", "")
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"vertex"`) || !strings.Contains(get.Body.String(), `"stop"`) {
		t.Fatalf("GET status = %d, body = %s", get.Code, get.Body.String())
	}

	patch := runConfigListRequest(
		h.PatchOAuthRequestScopedErrors,
		http.MethodPatch,
		"/oauth-request-scoped-errors",
		`{"provider":" Claude ","rules":[{"status":429,"match":["rate-limit"],"action":"continue-and-cooldown"}]}`,
	)
	if patch.Code != http.StatusOK || len(h.configSnapshot().OAuthRequestScopedErrors["claude"]) != 1 {
		t.Fatalf("PATCH status = %d, snapshot = %#v", patch.Code, h.configSnapshot().OAuthRequestScopedErrors)
	}

	removeVertex := runConfigListRequest(
		h.PatchOAuthRequestScopedErrors,
		http.MethodPatch,
		"/oauth-request-scoped-errors",
		`{"channel":"vertex","rules":[{"status":0,"match":["invalid"],"action":"stop"}]}`,
	)
	if removeVertex.Code != http.StatusOK {
		t.Fatalf("empty PATCH status = %d, body = %s", removeVertex.Code, removeVertex.Body.String())
	}
	if _, present := h.configSnapshot().OAuthRequestScopedErrors["vertex"]; present {
		t.Fatalf("vertex channel remains after empty PATCH: %#v", h.configSnapshot().OAuthRequestScopedErrors)
	}

	missing := runConfigListRequest(
		h.DeleteOAuthRequestScopedErrors,
		http.MethodDelete,
		"/oauth-request-scoped-errors?channel=vertex",
		"",
	)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing status = %d, body = %s", missing.Code, missing.Body.String())
	}

	removeLast := runConfigListRequest(
		h.DeleteOAuthRequestScopedErrors,
		http.MethodDelete,
		"/oauth-request-scoped-errors?provider=CLAUDE",
		"",
	)
	if removeLast.Code != http.StatusOK || h.configSnapshot().OAuthRequestScopedErrors != nil {
		t.Fatalf("DELETE last status = %d, snapshot = %#v", removeLast.Code, h.configSnapshot().OAuthRequestScopedErrors)
	}
	persisted, err := os.ReadFile(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), "oauth-request-scoped-errors: {}") {
		t.Fatalf("persisted empty map =\n%s", persisted)
	}
}
