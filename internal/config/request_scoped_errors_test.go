package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigRequestScopedErrorRules(t *testing.T) {
	const source = `
gemini-api-key:
  - api-key: gemini-key
    request-scoped-errors:
      - status: 400
        match: [gemini-limit]
        action: stop
interactions-api-key:
  - api-key: interactions-key
    request-scoped-errors:
      - status: 400
        match: [interactions-limit]
        action: continue
claude-api-key:
  - api-key: claude-key
    request-scoped-errors:
      - status: 400
        match-regexr: ["^claude-limit$"]
        action: stop-and-cooldown
codex-api-key:
  - api-key: codex-key
    base-url: https://api.openai.com/v1
    request-scoped-errors:
      - status: 429
        match: [codex-limit]
        action: continue-and-cooldown
xai-api-key:
  - api-key: xai-key
    base-url: https://api.x.ai/v1
    request-scoped-errors:
      - status: 500
        match: [xai-limit]
        action: stop
openai-compatibility:
  - name: compat
    base-url: https://compat.example/v1
    request-scoped-errors:
      - status: 422
        match: [compat-limit]
        action: continue
oauth-request-scoped-errors:
  Vertex:
    - status: 400
      match: [" vertex-limit "]
      match-regexr: [" ^vertex-limit$ "]
      action: " STOP "
`

	cfg, err := ParseConfigBytes([]byte(source))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	checks := []struct {
		name   string
		rules  []RequestScopedErrorRule
		status int
		action string
	}{
		{name: "gemini", rules: cfg.GeminiKey[0].RequestScopedErrors, status: 400, action: "stop"},
		{name: "interactions", rules: cfg.InteractionsKey[0].RequestScopedErrors, status: 400, action: "continue"},
		{name: "claude", rules: cfg.ClaudeKey[0].RequestScopedErrors, status: 400, action: "stop-and-cooldown"},
		{name: "codex", rules: cfg.CodexKey[0].RequestScopedErrors, status: 429, action: "continue-and-cooldown"},
		{name: "xai", rules: cfg.XAIKey[0].RequestScopedErrors, status: 500, action: "stop"},
		{name: "openai compatibility", rules: cfg.OpenAICompatibility[0].RequestScopedErrors, status: 422, action: "continue"},
	}
	for _, check := range checks {
		if len(check.rules) != 1 || check.rules[0].Status != check.status || check.rules[0].Action != check.action {
			t.Fatalf("%s rules = %#v", check.name, check.rules)
		}
	}

	vertex := cfg.OAuthRequestScopedErrors["vertex"]
	if len(cfg.OAuthRequestScopedErrors) != 1 || len(vertex) != 1 {
		t.Fatalf("OAuth request-scoped errors = %#v", cfg.OAuthRequestScopedErrors)
	}
	if vertex[0].Status != 400 || vertex[0].Action != "stop" ||
		len(vertex[0].Match) != 1 || vertex[0].Match[0] != "vertex-limit" ||
		len(vertex[0].MatchRegexr) != 1 || vertex[0].MatchRegexr[0] != "^vertex-limit$" {
		t.Fatalf("sanitized vertex rule = %#v", vertex[0])
	}
}

func TestParseConfigOAuthRequestScopedErrorChannels(t *testing.T) {
	channels := []string{"vertex", "aistudio", "antigravity", "claude", "codex", "kimi", "xai", "meta"}
	var source strings.Builder
	source.WriteString("oauth-request-scoped-errors:\n")
	for _, channel := range channels {
		source.WriteString("  " + channel + ":\n    - status: 400\n      match: [context_length_exceeded]\n      action: stop\n")
	}
	cfg, err := ParseConfigBytes([]byte(source.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OAuthRequestScopedErrors) != len(channels) {
		t.Fatalf("OAuth channels = %d, want %d", len(cfg.OAuthRequestScopedErrors), len(channels))
	}
	for _, channel := range channels {
		rules := cfg.OAuthRequestScopedErrors[channel]
		if len(rules) != 1 || rules[0].Status != 400 || rules[0].Action != "stop" ||
			len(rules[0].Match) != 1 || rules[0].Match[0] != "context_length_exceeded" {
			t.Fatalf("%s rules = %+v", channel, rules)
		}
	}
}

func TestSanitizeOAuthRequestScopedErrorsKeepsRuntimeInvalidValues(t *testing.T) {
	cfg := &Config{OAuthRequestScopedErrors: map[string][]RequestScopedErrorRule{
		" Vertex ": {
			{
				Status:      400,
				Match:       []string{" token ", ""},
				MatchRegexr: []string{" [invalid ", "  "},
				Action:      " NOT-ACTION ",
			},
			{Status: 0, Match: []string{"ignored"}, Action: "stop"},
			{Status: 400, Match: []string{"  "}, Action: "stop"},
		},
		" ":      {{Status: 400, Match: []string{"ignored"}, Action: "stop"}},
		"claude": nil,
	}}

	cfg.SanitizeOAuthRequestScopedErrors()

	rules := cfg.OAuthRequestScopedErrors["vertex"]
	if len(cfg.OAuthRequestScopedErrors) != 1 || len(rules) != 1 {
		t.Fatalf("sanitized map = %#v", cfg.OAuthRequestScopedErrors)
	}
	if rules[0].Action != "not-action" || len(rules[0].Match) != 1 || rules[0].Match[0] != "token" ||
		len(rules[0].MatchRegexr) != 1 || rules[0].MatchRegexr[0] != "[invalid" {
		t.Fatalf("sanitized rule = %#v", rules[0])
	}
}

func TestSaveConfigPreserveCommentsPrunesOAuthRequestScopedErrors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte(`# keep this comment
host: 127.0.0.1
oauth-request-scoped-errors:
  vertex:
    - status: 400
      match: [stale-vertex]
      action: stop
  claude:
    - status: 429
      match: [stale-claude]
      action: continue
`)
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Host: "127.0.0.1", OAuthRequestScopedErrors: map[string][]RequestScopedErrorRule{
		"vertex": {{Status: 400, Match: []string{"current-vertex"}, Action: "stop-and-cooldown"}},
	}}
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	if !strings.Contains(text, "# keep this comment") || !strings.Contains(text, "current-vertex") ||
		strings.Contains(text, "stale-vertex") || strings.Contains(text, "stale-claude") ||
		strings.Contains(text, "  claude:") {
		t.Fatalf("saved config =\n%s", text)
	}

	cfg.OAuthRequestScopedErrors = nil
	if err = SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() deleting last channel error = %v", err)
	}
	written, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text = string(written)
	if !strings.Contains(text, "oauth-request-scoped-errors: {}") || strings.Contains(text, "current-vertex") {
		t.Fatalf("saved empty OAuth rule map =\n%s", text)
	}
	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if len(reloaded.OAuthRequestScopedErrors) != 0 {
		t.Fatalf("reloaded OAuth request-scoped errors = %#v", reloaded.OAuthRequestScopedErrors)
	}
}
