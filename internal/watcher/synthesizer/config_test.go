package synthesizer

import (
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestNewConfigSynthesizer(t *testing.T) {
	synth := NewConfigSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestConfigSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_Synthesize_NilConfig(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config:      nil,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_GeminiKeys(t *testing.T) {
	tests := []struct {
		name       string
		geminiKeys []config.GeminiKey
		wantLen    int
		validate   func(*testing.T, []*coreauth.Auth)
	}{
		{
			name: "single gemini key",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a"},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Provider != "gemini" {
					t.Errorf("expected provider gemini, got %s", auths[0].Provider)
				}
				if auths[0].Prefix != "team-a" {
					t.Errorf("expected prefix team-a, got %s", auths[0].Prefix)
				}
				if auths[0].Label != "gemini-apikey" {
					t.Errorf("expected label gemini-apikey, got %s", auths[0].Label)
				}
				if auths[0].Attributes["api_key"] != "test-key-123" {
					t.Errorf("expected api_key test-key-123, got %s", auths[0].Attributes["api_key"])
				}
				if auths[0].Status != coreauth.StatusActive {
					t.Errorf("expected status active, got %s", auths[0].Status)
				}
			},
		},
		{
			name: "gemini key with base url and proxy",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:   "api-key",
					BaseURL:  "https://custom.api.com",
					ProxyURL: "http://proxy.local:8080",
					Prefix:   "custom",
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["base_url"] != "https://custom.api.com" {
					t.Errorf("expected base_url https://custom.api.com, got %s", auths[0].Attributes["base_url"])
				}
				if auths[0].ProxyURL != "http://proxy.local:8080" {
					t.Errorf("expected proxy_url http://proxy.local:8080, got %s", auths[0].ProxyURL)
				}
			},
		},
		{
			name: "gemini key with headers",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:  "api-key",
					Headers: map[string]string{"X-Custom": "value"},
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["header:X-Custom"] != "value" {
					t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
				}
			},
		},
		{
			name: "empty api key skipped",
			geminiKeys: []config.GeminiKey{
				{APIKey: ""},
				{APIKey: "  "},
				{APIKey: "valid-key"},
			},
			wantLen: 1,
		},
		{
			name: "multiple gemini keys",
			geminiKeys: []config.GeminiKey{
				{APIKey: "key-1", Prefix: "a"},
				{APIKey: "key-2", Prefix: "b"},
				{APIKey: "key-3", Prefix: "c"},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				synth := NewConfigSynthesizer()
				ctx := &SynthesisContext{
					Config: &config.Config{
						GeminiKey: tt.geminiKeys,
					},
					Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
					IDGenerator: NewStableIDGenerator(),
				}

				auths, err := synth.Synthesize(ctx)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(auths) != tt.wantLen {
					t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
				}

				if tt.validate != nil && len(auths) > 0 {
					tt.validate(t, auths)
				}
			},
		)
	}
}

func TestConfigSynthesizer_InteractionsKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			InteractionsKey: []config.GeminiKey{
				{
					APIKey:         " interactions-key ",
					BaseURL:        " https://interactions.example.com ",
					ProxyURL:       " http://proxy.local:8080 ",
					Prefix:         "team-a",
					Priority:       7,
					Headers:        map[string]string{"X-Custom": "value"},
					ExcludedModels: []string{"gemini-2.5-flash"},
					Models: []config.GeminiModel{
						{Name: "gemini-2.5-pro", Alias: "g25p"},
					},
				},
				{APIKey: ""},
			},
		},
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "gemini-interactions" {
		t.Fatalf("provider = %q, want gemini-interactions", auth.Provider)
	}
	if auth.Label != "interactions-apikey" {
		t.Fatalf("label = %q, want interactions-apikey", auth.Label)
	}
	if auth.Prefix != "team-a" {
		t.Fatalf("prefix = %q, want team-a", auth.Prefix)
	}
	if auth.ProxyURL != "http://proxy.local:8080" {
		t.Fatalf("proxy url = %q, want http://proxy.local:8080", auth.ProxyURL)
	}
	if !strings.HasPrefix(auth.ID, "gemini-interactions:apikey:") {
		t.Fatalf("id = %q, want gemini-interactions:apikey prefix", auth.ID)
	}
	if got := auth.Attributes["source"]; !strings.HasPrefix(got, "config:interactions[") {
		t.Fatalf("source = %q, want config:interactions[...]", got)
	}
	if got := auth.Attributes["api_key"]; got != "interactions-key" {
		t.Fatalf("api_key = %q, want interactions-key", got)
	}
	if got := auth.Attributes["base_url"]; got != "https://interactions.example.com" {
		t.Fatalf("base_url = %q, want https://interactions.example.com", got)
	}
	if got := auth.Attributes["priority"]; got != "7" {
		t.Fatalf("priority = %q, want 7", got)
	}
	if got := auth.Attributes["header:X-Custom"]; got != "value" {
		t.Fatalf("header:X-Custom = %q, want value", got)
	}
	if got := auth.Attributes["auth_kind"]; got != "apikey" {
		t.Fatalf("auth_kind = %q, want apikey", got)
	}
	if got := auth.Attributes["excluded_models"]; got != "gemini-2.5-flash" {
		t.Fatalf("excluded_models = %q, want gemini-2.5-flash", got)
	}
	if _, ok := auth.Attributes["excluded_models_hash"]; !ok {
		t.Fatal("expected excluded_models_hash")
	}
	if _, ok := auth.Attributes["models_hash"]; !ok {
		t.Fatal("expected models_hash")
	}
}

func TestConfigSynthesizer_ClaudeKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:                  "sk-ant-api-xxx",
					Prefix:                  "main",
					BaseURL:                 "https://api.anthropic.com",
					RebuildMidSystemMessage: true,
					Models: []config.ClaudeModel{
						{Name: "claude-3-opus"},
						{Name: "claude-3-sonnet"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "claude" {
		t.Errorf("expected provider claude, got %s", auths[0].Provider)
	}
	if auths[0].Label != "claude-apikey" {
		t.Errorf("expected label claude-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "main" {
		t.Errorf("expected prefix main, got %s", auths[0].Prefix)
	}
	if auths[0].Attributes["api_key"] != "sk-ant-api-xxx" {
		t.Errorf("expected api_key sk-ant-api-xxx, got %s", auths[0].Attributes["api_key"])
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if got := auths[0].Attributes["rebuild_mid_system_message"]; got != "true" {
		t.Errorf("expected rebuild_mid_system_message=true, got %s", got)
	}
}

func TestConfigSynthesizer_ClaudeKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: ""},    // empty, should be skipped
				{APIKey: "   "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"X-Custom": "value"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:X-Custom"] != "value" {
		t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
	}
}

func TestConfigSynthesizer_CodexKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{
					APIKey:     "codex-key-123",
					Prefix:     "dev",
					BaseURL:    "https://api.openai.com",
					ProxyURL:   "http://proxy.local",
					Websockets: true,
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "codex" {
		t.Errorf("expected provider codex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "codex-apikey" {
		t.Errorf("expected label codex-apikey, got %s", auths[0].Label)
	}
	if auths[0].ProxyURL != "http://proxy.local" {
		t.Errorf("expected proxy_url http://proxy.local, got %s", auths[0].ProxyURL)
	}
	if auths[0].Attributes["websockets"] != "true" {
		t.Errorf("expected websockets=true, got %s", auths[0].Attributes["websockets"])
	}
}

func TestConfigSynthesizer_CodexKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: ""},   // empty, should be skipped
				{APIKey: "  "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"X-Custom": "value"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:X-Custom"] != "value" {
		t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
	}
}

func TestConfigSynthesizer_CodexKeys_RejectsRestrictedHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: "valid-key", Headers: map[string]string{"Authorization": "Bearer xyz"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	if _, err := synth.Synthesize(ctx); err == nil {
		t.Fatal("expected restricted header error")
	}
}

func TestConfigSynthesizer_OpenAICompat(t *testing.T) {
	tests := []struct {
		name    string
		compat  []config.OpenAICompatibility
		wantLen int
	}{
		{
			name: "with APIKeyEntries",
			compat: []config.OpenAICompatibility{
				{
					Name:    "CustomProvider",
					BaseURL: "https://custom.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-1"},
						{APIKey: "key-2"},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "empty APIKeyEntries included (legacy)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "EmptyKeys",
					BaseURL: "https://empty.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: ""},
						{APIKey: "   "},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "without APIKeyEntries (fallback)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "NoKeyProvider",
					BaseURL: "https://no-key.api.com",
				},
			},
			wantLen: 1,
		},
		{
			name: "empty name defaults",
			compat: []config.OpenAICompatibility{
				{
					Name:    "",
					BaseURL: "https://default.api.com",
				},
			},
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				synth := NewConfigSynthesizer()
				ctx := &SynthesisContext{
					Config: &config.Config{
						OpenAICompatibility: tt.compat,
					},
					Now:         time.Now(),
					IDGenerator: NewStableIDGenerator(),
				}

				auths, err := synth.Synthesize(ctx)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(auths) != tt.wantLen {
					t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
				}
			},
		)
	}
}

func TestConfigSynthesizer_VertexCompat(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key-123",
					BaseURL: "https://vertex.googleapis.com",
					Prefix:  "vertex-prod",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "vertex" {
		t.Errorf("expected provider vertex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "vertex-apikey" {
		t.Errorf("expected label vertex-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "vertex-prod" {
		t.Errorf("expected prefix vertex-prod, got %s", auths[0].Prefix)
	}
}

func TestConfigSynthesizer_VertexCompat_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "", BaseURL: "https://vertex.api"},   // empty key creates auth without api_key attr
				{APIKey: "  ", BaseURL: "https://vertex.api"}, // whitespace key creates auth without api_key attr
				{APIKey: "valid-key", BaseURL: "https://vertex.api", Headers: map[string]string{"X-Vertex": "test"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Vertex compat doesn't skip empty keys - it creates auths without api_key attribute
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths, got %d", len(auths))
	}
	// First two should not have api_key attribute
	if _, ok := auths[0].Attributes["api_key"]; ok {
		t.Error("expected first auth to not have api_key attribute")
	}
	if _, ok := auths[1].Attributes["api_key"]; ok {
		t.Error("expected second auth to not have api_key attribute")
	}
	// Third should have headers
	if auths[2].Attributes["header:X-Vertex"] != "test" {
		t.Errorf("expected header:X-Vertex=test, got %s", auths[2].Attributes["header:X-Vertex"])
	}
}

func TestConfigSynthesizer_OpenAICompat_WithModelsHash(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "TestProvider",
					BaseURL: "https://test.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-a"},
						{Name: "model-b"},
					},
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-with-models"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if auths[0].Attributes["api_key"] != "key-with-models" {
		t.Errorf("expected api_key key-with-models, got %s", auths[0].Attributes["api_key"])
	}
}

func TestConfigSynthesizer_OpenAICompat_FallbackWithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "NoKeyWithModels",
					BaseURL: "https://nokey.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-x"},
					},
					Headers: map[string]string{"X-API": "header-value"},
					// No APIKeyEntries - should use fallback path
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in fallback path")
	}
	if auths[0].Attributes["header:X-API"] != "header-value" {
		t.Errorf("expected header:X-API=header-value, got %s", auths[0].Attributes["header:X-API"])
	}
}

func TestConfigSynthesizer_VertexCompat_WithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key",
					BaseURL: "https://vertex.api",
					Models: []config.VertexCompatModel{
						{Name: "gemini-pro", Alias: "pro"},
						{Name: "gemini-ultra", Alias: "ultra"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in vertex auth with models")
	}
}

func TestConfigSynthesizer_IDStability(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "stable-key", Prefix: "test"},
		},
	}

	// Generate IDs twice with fresh generators
	synth1 := NewConfigSynthesizer()
	ctx1 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths1, _ := synth1.Synthesize(ctx1)

	synth2 := NewConfigSynthesizer()
	ctx2 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths2, _ := synth2.Synthesize(ctx2)

	if auths1[0].ID != auths2[0].ID {
		t.Errorf("same config should produce same ID: got %q and %q", auths1[0].ID, auths2[0].ID)
	}
}

func TestConfigSynthesizer_RouteIdentityStableAcrossReorderAndDelete(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		buildConfig func([]string) *config.Config
	}{
		{
			name:     "claude",
			provider: "claude",
			buildConfig: func(proxyURLs []string) *config.Config {
				keys := make([]config.ClaudeKey, 0, len(proxyURLs))
				for _, proxyURL := range proxyURLs {
					keys = append(keys, config.ClaudeKey{
						APIKey:   "shared-claude-key",
						BaseURL:  "https://claude.example.com",
						ProxyURL: proxyURL,
					})
				}
				return &config.Config{ClaudeKey: keys}
			},
		},
		{
			name:     "codex",
			provider: "codex",
			buildConfig: func(proxyURLs []string) *config.Config {
				keys := make([]config.CodexKey, 0, len(proxyURLs))
				for _, proxyURL := range proxyURLs {
					keys = append(keys, config.CodexKey{
						APIKey:   "shared-codex-key",
						BaseURL:  "https://codex.example.com",
						ProxyURL: proxyURL,
					})
				}
				return &config.Config{CodexKey: keys}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idsByProxy := func(proxyURLs []string) map[string]string {
				t.Helper()
				auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
					Config:      tt.buildConfig(proxyURLs),
					Now:         time.Unix(1_700_000_000, 0),
					IDGenerator: NewStableIDGenerator(),
				})
				if err != nil {
					t.Fatalf("synthesize %s auths: %v", tt.provider, err)
				}
				ids := make(map[string]string, len(auths))
				for _, auth := range auths {
					if auth.Provider == tt.provider {
						ids[auth.ProxyURL] = auth.ID
					}
				}
				if len(ids) != len(proxyURLs) {
					t.Fatalf("got %d %s auths, want %d", len(ids), tt.provider, len(proxyURLs))
				}
				return ids
			}

			proxyA := "https://proxy-a.example.com"
			proxyB := "https://proxy-b.example.com"
			original := idsByProxy([]string{proxyA, proxyB})
			reordered := idsByProxy([]string{proxyB, proxyA})
			remaining := idsByProxy([]string{proxyB})

			if original[proxyA] == original[proxyB] {
				t.Fatalf("different routes received the same auth ID %q", original[proxyA])
			}
			for _, proxyURL := range []string{proxyA, proxyB} {
				if reordered[proxyURL] != original[proxyURL] {
					t.Errorf(
						"auth ID changed after reorder for %s: got %q, want %q",
						proxyURL,
						reordered[proxyURL],
						original[proxyURL],
					)
				}
			}
			if remaining[proxyB] != original[proxyB] {
				t.Errorf(
					"auth ID changed after deleting the other route: got %q, want %q",
					remaining[proxyB],
					original[proxyB],
				)
			}
		})
	}
}

func TestConfigSynthesizer_RoutingMetadataStableAcrossReorderAndDelete(t *testing.T) {
	tests := []struct {
		name            string
		provider        string
		identityAttr    string
		configureClaude func(*config.ClaudeKey, string)
		configureCodex  func(*config.CodexKey, string)
	}{
		{
			name:         "claude priority",
			provider:     "claude",
			identityAttr: "priority",
			configureClaude: func(key *config.ClaudeKey, variant string) {
				if variant == "a" {
					key.Priority = 1
				} else {
					key.Priority = 2
				}
			},
		},
		{
			name:         "claude models",
			provider:     "claude",
			identityAttr: "models_hash",
			configureClaude: func(key *config.ClaudeKey, variant string) {
				key.Models = []config.ClaudeModel{{Name: "claude-" + variant, Alias: "alias-" + variant}}
			},
		},
		{
			name:         "claude excluded models",
			provider:     "claude",
			identityAttr: "excluded_models_hash",
			configureClaude: func(key *config.ClaudeKey, variant string) {
				key.ExcludedModels = []string{"claude-excluded-" + variant}
			},
		},
		{
			name:         "codex priority",
			provider:     "codex",
			identityAttr: "priority",
			configureCodex: func(key *config.CodexKey, variant string) {
				if variant == "a" {
					key.Priority = 1
				} else {
					key.Priority = 2
				}
			},
		},
		{
			name:         "codex models",
			provider:     "codex",
			identityAttr: "models_hash",
			configureCodex: func(key *config.CodexKey, variant string) {
				key.Models = []config.CodexModel{{Name: "codex-" + variant, Alias: "alias-" + variant}}
			},
		},
		{
			name:         "codex excluded models",
			provider:     "codex",
			identityAttr: "excluded_models_hash",
			configureCodex: func(key *config.CodexKey, variant string) {
				key.ExcludedModels = []string{"codex-excluded-" + variant}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buildConfig := func(variants []string) *config.Config {
				cfg := &config.Config{}
				for _, variant := range variants {
					if tt.configureClaude != nil {
						key := config.ClaudeKey{
							APIKey:  "shared-claude-key",
							BaseURL: "https://claude.example.com",
						}
						tt.configureClaude(&key, variant)
						cfg.ClaudeKey = append(cfg.ClaudeKey, key)
						continue
					}
					key := config.CodexKey{
						APIKey:  "shared-codex-key",
						BaseURL: "https://codex.example.com",
					}
					tt.configureCodex(&key, variant)
					cfg.CodexKey = append(cfg.CodexKey, key)
				}
				return cfg
			}
			idsByMetadata := func(variants []string) map[string]string {
				t.Helper()
				auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
					Config:      buildConfig(variants),
					Now:         time.Unix(1_700_000_000, 0),
					IDGenerator: NewStableIDGenerator(),
				})
				if err != nil {
					t.Fatalf("synthesize %s auths: %v", tt.provider, err)
				}
				ids := make(map[string]string, len(auths))
				for _, auth := range auths {
					if auth.Provider != tt.provider {
						continue
					}
					identity := auth.Attributes[tt.identityAttr]
					if identity == "" {
						t.Fatalf("%s auth missing %s", tt.provider, tt.identityAttr)
					}
					ids[identity] = auth.ID
				}
				if len(ids) != len(variants) {
					t.Fatalf("got %d %s auth identities, want %d", len(ids), tt.provider, len(variants))
				}
				return ids
			}

			original := idsByMetadata([]string{"a", "b"})
			reordered := idsByMetadata([]string{"b", "a"})
			remaining := idsByMetadata([]string{"b"})
			for identity, originalID := range original {
				if reordered[identity] != originalID {
					t.Errorf(
						"auth ID changed after reorder for %s: got %q, want %q",
						identity,
						reordered[identity],
						originalID,
					)
				}
			}
			for identity, remainingID := range remaining {
				if original[identity] != remainingID {
					t.Errorf(
						"auth ID changed after deleting the other route for %s: got %q, want %q",
						identity,
						remainingID,
						original[identity],
					)
				}
			}
		})
	}
}

func TestConfigSynthesizer_AllProviders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "gemini-key"},
			},
			InteractionsKey: []config.GeminiKey{
				{APIKey: "interactions-key"},
			},
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-key"},
			},
			CodexKey: []config.CodexKey{
				{APIKey: "codex-key"},
			},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "compat", BaseURL: "https://compat.api"},
			},
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "vertex-key", BaseURL: "https://vertex.api"},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 6 {
		t.Fatalf("expected 6 auths, got %d", len(auths))
	}

	providers := make(map[string]bool)
	for _, a := range auths {
		providers[a.Provider] = true
	}

	expected := []string{"gemini", "gemini-interactions", "claude", "codex", "compat", "vertex"}
	for _, p := range expected {
		if !providers[p] {
			t.Errorf("expected provider %s not found", p)
		}
	}
}
