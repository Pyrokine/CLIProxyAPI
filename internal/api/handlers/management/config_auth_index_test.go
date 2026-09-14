package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	views := (&Handler{authManager: manager}).buildConfigAuthIndexViews(cfg)
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
	if got := views.openAIEntries[0][0]; got != expected["interactions"] {
		t.Fatalf("Interactions-named compatibility auth index = %q, want %q", got, expected["interactions"])
	}
	if got := views.openAIEntries[1][0]; got != expected["custom"] {
		t.Fatalf("custom compatibility auth index = %q, want %q", got, expected["custom"])
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
