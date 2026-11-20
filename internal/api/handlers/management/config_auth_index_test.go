package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestBuildConfigAuthIndexViewsDoesNotShiftAfterInteractionsKeys(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "gemini-key"}},
		InteractionsKey: []config.GeminiKey{
			{
				APIKey:  "interactions-key",
				BaseURL: "https://interactions.example.com",
			},
		},
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "claude-key",
				BaseURL: "https://claude.example.com",
			},
		},
		CodexKey: []config.CodexKey{
			{
				APIKey:  "codex-key",
				BaseURL: "https://codex.example.com",
			},
		},
		XAIKey: []config.XAIKey{
			{
				APIKey:  "xai-key",
				BaseURL: "https://api.x.ai/v1",
			},
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "interactions",
				BaseURL:       "https://compat-interactions.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "compat-interactions-key"}},
			},
			{
				Name:          "custom",
				BaseURL:       "https://custom.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "custom-key"}},
			},
		},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config:      cfg,
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
			t.Fatalf("register %s auth: %v", auth.Provider, err)
		}
	}

	expected := make(map[string]string)
	for _, auth := range manager.List() {
		auth.EnsureIndex()
		expected[auth.Provider] = auth.Index
	}
	h := &Handler{authManager: manager}
	views := h.buildConfigAuthIndexViews(cfg)
	if got := views.interactions[0]; got != expected["gemini-interactions"] {
		t.Fatalf("Interactions auth index = %q, want %q", got, expected["gemini-interactions"])
	}
	if got := views.gemini[0]; got != expected["gemini"] {
		t.Fatalf("gemini auth index = %q, want %q", got, expected["gemini"])
	}
	if got := views.claude[0]; got != expected["claude"] {
		t.Fatalf("claude auth index = %q, want %q", got, expected["claude"])
	}
	if got := views.codex[0]; got != expected["codex"] {
		t.Fatalf("codex auth index = %q, want %q", got, expected["codex"])
	}
	if got := views.xai[0]; got != expected["xai"] {
		t.Fatalf("xai auth index = %q, want %q", got, expected["xai"])
	}
	for groupIndex, compat := range cfg.OpenAICompatibility {
		var expectedAuth *coreauth.Auth
		for _, auth := range manager.List() {
			if auth.Attributes["config_index"] == strconv.Itoa(groupIndex) &&
				auth.Attributes["compat_name"] == compat.Name &&
				auth.Attributes["api_key"] == compat.APIKeyEntries[0].APIKey &&
				auth.Attributes["base_url"] == compat.BaseURL {
				expectedAuth = auth
				break
			}
		}
		if expectedAuth == nil {
			t.Fatalf("compatibility group %d (%s) has no matching live auth", groupIndex, compat.Name)
		}
		expectedAuth.EnsureIndex()
		index := views.openAIEntries[groupIndex][0]
		if index == "" || index != expectedAuth.Index {
			t.Fatalf(
				"%s compatibility auth index = %q, want %q for %s", compat.Name, index, expectedAuth.Index,
				expectedAuth.ID,
			)
		}
		resolved := h.authByIndex(index)
		if resolved == nil || resolved.ID != expectedAuth.ID || resolved.Provider != "openai-compatible-"+compat.Name {
			t.Fatalf(
				"%s compatibility index resolved to %#v, want auth %s with namespaced runtime provider",
				compat.Name, resolved, expectedAuth.ID,
			)
		}
	}
	if views.claude[0] == expected["gemini-interactions"] {
		t.Fatal("claude config received the Interactions auth index")
	}
}

func TestBuildConfigAuthIndexViewsKeepsOpenAIPositionsWhenLiveAuthIsMissing(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "multi-key",
				BaseURL: "https://multi.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "openai-key-a"},
					{},
				},
			},
			{
				Name:          "disabled",
				BaseURL:       "https://disabled.example.com",
				Disabled:      true,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "disabled-key"}},
			},
			{
				Name:    "fallback",
				BaseURL: "https://fallback.example.com",
			},
		},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config:      cfg,
			Now:         time.Unix(1_700_000_000, 0),
			IDGenerator: synthesizer.NewStableIDGenerator(),
		},
	)
	if err != nil {
		t.Fatalf("synthesize auths: %v", err)
	}
	if len(auths) != 3 {
		t.Fatalf("synthesized auth count = %d, want 3", len(auths))
	}

	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{auths[0], auths[2]} {
		if _, err = manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s auth: %v", auth.Provider, err)
		}
		auth.EnsureIndex()
	}

	views := (&Handler{authManager: manager}).buildConfigAuthIndexViews(cfg)
	if got, want := views.openAIEntries[0][0], auths[0].Index; got != want {
		t.Fatalf("first OpenAI entry auth index = %q, want %q", got, want)
	}
	if got := views.openAIEntries[0][1]; got != "" {
		t.Fatalf("missing live OpenAI entry auth index = %q, want empty", got)
	}
	if len(views.openAIEntries[1]) != 0 || views.openAIFallback[1] != "" {
		t.Fatalf(
			"disabled OpenAI provider received auth indexes: entries=%#v fallback=%q", views.openAIEntries[1],
			views.openAIFallback[1],
		)
	}
	if got, want := views.openAIFallback[2], auths[2].Index; got != want {
		t.Fatalf("fallback OpenAI auth index = %q, want %q", got, want)
	}
}

func TestGetConfigIncludesAuthIndexes(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "gemini-key"}},
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "claude-key",
				BaseURL: "https://claude.example.com",
			},
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "multi-key",
				BaseURL: "https://openai.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "openai-key-a"},
					{APIKey: "openai-key-b"},
				},
			},
			{
				Name:    "without-key",
				BaseURL: "https://fallback.example.com",
			},
		},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config:      cfg,
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
			t.Fatalf("register %s auth: %v", auth.Provider, err)
		}
	}

	handler := NewHandler(cfg, "", manager, withEnvSecret(""))
	expected := handler.buildConfigAuthIndexViews(cfg).response()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/config", nil)
	handler.GetConfig(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GetConfig status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response struct {
		AuthIndexes configAuthIndexResponse `json:"auth-indexes"`
	}
	if err = json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GetConfig response: %v", err)
	}
	if !reflect.DeepEqual(response.AuthIndexes, expected) {
		t.Fatalf(
			"auth-indexes = %#v, want %#v",
			response.AuthIndexes,
			expected,
		)
	}
}
