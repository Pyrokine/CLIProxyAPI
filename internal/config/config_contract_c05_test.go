package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestC05ConfigurationContractRoundTrip(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`host: 127.0.0.1
port: 8317
gemini-api-key:
  - api-key: gemini-secret
    models:
      - name: gemini-upstream
        alias: gemini-alias
        display-name: Gemini Display
        max-context-length: 262144
        thinking:
          levels: [high, none, auto]
          zero-allowed: true
          dynamic-allowed: true
interactions-api-key:
  - api-key: interactions-secret
    models:
      - name: interactions-upstream
        alias: interactions-alias
        display-name: Interactions Display
        max-context-length: 262144
claude-api-key:
  - api-key: claude-secret
    base-url: https://api.anthropic.com
    models:
      - name: claude-upstream
        alias: claude-alias
        display-name: Claude Display
        max-context-length: 262144
codex-api-key:
  - api-key: codex-secret
    base-url: https://api.openai.com/v1
    models:
      - name: codex-upstream
        alias: codex-alias
        display-name: Codex Display
        max-context-length: 262144
vertex-api-key:
  - api-key: vertex-secret
    models:
      - name: vertex-upstream
        alias: vertex-alias
        display-name: Vertex Display
openai-compatibility:
  - name: compat
    base-url: https://compat.example/v1
    support-prompt-cache-key: true
    models:
      - name: compat-upstream
        alias: compat-alias
        display-name: Compatibility Display
        max-context-length: 262144
oauth-model-alias:
  codex:
    - name: gpt-5.5
      alias: oauth-gpt
      display-name: OAuth GPT
`)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if err = SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() after save error = %v", err)
	}

	models := []struct {
		name             string
		displayName      string
		maxContextLength int
	}{
		{
			name: "gemini", displayName: reloaded.GeminiKey[0].Models[0].DisplayName,
			maxContextLength: reloaded.GeminiKey[0].Models[0].MaxContextLength,
		},
		{
			name: "interactions", displayName: reloaded.InteractionsKey[0].Models[0].DisplayName,
			maxContextLength: reloaded.InteractionsKey[0].Models[0].MaxContextLength,
		},
		{
			name: "claude", displayName: reloaded.ClaudeKey[0].Models[0].DisplayName,
			maxContextLength: reloaded.ClaudeKey[0].Models[0].MaxContextLength,
		},
		{
			name: "codex", displayName: reloaded.CodexKey[0].Models[0].DisplayName,
			maxContextLength: reloaded.CodexKey[0].Models[0].MaxContextLength,
		},
		{name: "vertex", displayName: reloaded.VertexCompatAPIKey[0].Models[0].DisplayName},
		{
			name: "openai compatibility", displayName: reloaded.OpenAICompatibility[0].Models[0].DisplayName,
			maxContextLength: reloaded.OpenAICompatibility[0].Models[0].MaxContextLength,
		},
	}
	for _, model := range models {
		if model.displayName == "" {
			t.Errorf("%s display name is empty", model.name)
		}
		if model.name != "vertex" && model.maxContextLength != 262144 {
			t.Errorf("%s max context length = %d", model.name, model.maxContextLength)
		}
	}
	if !reloaded.OpenAICompatibility[0].SupportPromptCacheKey {
		t.Fatal("support-prompt-cache-key was not preserved")
	}
	thinking := reloaded.GeminiKey[0].Models[0].Thinking
	if thinking == nil || len(thinking.Levels) != 3 || thinking.Levels[0] != "high" ||
		!thinking.ZeroAllowed || !thinking.DynamicAllowed {
		t.Fatalf("thinking support round trip = %+v", thinking)
	}
	aliases := reloaded.OAuthModelAlias["codex"]
	if len(aliases) != 1 || aliases[0].DisplayName != "OAuth GPT" {
		t.Fatalf("OAuth aliases round trip = %+v", aliases)
	}
}

func TestC05ConfigurationContractSanitizerPreservesFields(t *testing.T) {
	cfg := &Config{
		GeminiKey: []GeminiKey{
			{
				APIKey: " gemini-secret ",
				Models: []GeminiModel{{DisplayName: "Gemini Display", MaxContextLength: 262144}},
			},
		},
		InteractionsKey: []GeminiKey{
			{
				APIKey: " interactions-secret ",
				Models: []GeminiModel{{DisplayName: "Interactions Display", MaxContextLength: 262144}},
			},
		},
		ClaudeKey: []ClaudeKey{
			{
				APIKey: "claude-secret",
				Models: []ClaudeModel{{DisplayName: "Claude Display", MaxContextLength: 262144}},
			},
		},
		CodexKey: []CodexKey{
			{
				APIKey:  "codex-secret",
				BaseURL: " https://api.openai.com/v1 ",
				Models:  []CodexModel{{DisplayName: "Codex Display", MaxContextLength: 262144}},
			},
		},
		VertexCompatAPIKey: []VertexCompatKey{
			{
				APIKey: "vertex-secret",
				Models: []VertexCompatModel{
					{
						Name: "vertex-upstream", Alias: "vertex-alias", DisplayName: "Vertex Display",
					},
				},
			},
		},
		OpenAICompatibility: []OpenAICompatibility{
			{
				Name:                  " compat ",
				BaseURL:               " https://compat.example/v1 ",
				SupportPromptCacheKey: true,
				Models: []OpenAICompatibilityModel{
					{
						DisplayName:      "Compatibility Display",
						MaxContextLength: 262144,
					},
				},
			},
		},
		OAuthModelAlias: map[string][]OAuthModelAlias{
			" CoDeX ": {{Name: " gpt-5.5 ", Alias: " oauth-gpt ", DisplayName: " OAuth GPT "}},
		},
	}

	cfg.SanitizeGeminiKeys()
	cfg.SanitizeInteractionsKeys()
	cfg.SanitizeClaudeKeys()
	cfg.SanitizeCodexKeys()
	cfg.SanitizeVertexCompatKeys()
	cfg.SanitizeOpenAICompatibility()
	cfg.SanitizeOAuthModelAlias()

	models := []struct {
		name             string
		displayName      string
		maxContextLength int
	}{
		{
			name: "gemini", displayName: cfg.GeminiKey[0].Models[0].DisplayName,
			maxContextLength: cfg.GeminiKey[0].Models[0].MaxContextLength,
		},
		{
			name: "interactions", displayName: cfg.InteractionsKey[0].Models[0].DisplayName,
			maxContextLength: cfg.InteractionsKey[0].Models[0].MaxContextLength,
		},
		{
			name: "claude", displayName: cfg.ClaudeKey[0].Models[0].DisplayName,
			maxContextLength: cfg.ClaudeKey[0].Models[0].MaxContextLength,
		},
		{
			name: "codex", displayName: cfg.CodexKey[0].Models[0].DisplayName,
			maxContextLength: cfg.CodexKey[0].Models[0].MaxContextLength,
		},
		{name: "vertex", displayName: cfg.VertexCompatAPIKey[0].Models[0].DisplayName},
		{
			name: "openai compatibility", displayName: cfg.OpenAICompatibility[0].Models[0].DisplayName,
			maxContextLength: cfg.OpenAICompatibility[0].Models[0].MaxContextLength,
		},
	}
	for _, model := range models {
		if model.displayName == "" {
			t.Errorf("sanitized %s display name is empty", model.name)
		}
		if model.name != "vertex" && model.maxContextLength != 262144 {
			t.Errorf("sanitized %s max context length = %d", model.name, model.maxContextLength)
		}
	}
	if !cfg.OpenAICompatibility[0].SupportPromptCacheKey {
		t.Fatal("sanitizer dropped support-prompt-cache-key")
	}
	alias := cfg.OAuthModelAlias["codex"][0]
	if alias.DisplayName != "OAuth GPT" {
		t.Fatalf("OAuth display name = %q, want OAuth GPT", alias.DisplayName)
	}
}
