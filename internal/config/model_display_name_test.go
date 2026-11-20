package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelDisplayNameConfigDecoding(t *testing.T) {
	const yamlConfig = `codex-api-key:
  - models:
      - name: codex-upstream
        alias: codex-alias
        display-name: Codex Name
xai-api-key:
  - models:
      - name: xai-upstream
        alias: xai-alias
        display-name: xAI Name
claude-api-key:
  - models:
      - name: claude-upstream
        alias: claude-alias
        display-name: Claude Name
gemini-api-key:
  - models:
      - name: gemini-upstream
        alias: gemini-alias
        display-name: Gemini Name
vertex-api-key:
  - models:
      - name: vertex-upstream
        alias: vertex-alias
        display-name: Vertex Name
openai-compatibility:
  - models:
      - name: compat-upstream
        alias: compat-alias
        display-name: Compatibility Name
`
	const jsonConfig = `{"codex-api-key":[{"models":[` +
		`{"name":"codex-upstream","alias":"codex-alias","display-name":"Codex Name"}]}],` +
		`"xai-api-key":[{"models":[{"name":"xai-upstream","alias":"xai-alias","display-name":"xAI Name"}]}],` +
		(`"claude-api-key":[{"models":[{"name":"claude-upstream","alias":` +
			`"claude-alias","display-name":"Claude Name"}]}],`) +
		(`"gemini-api-key":[{"models":[{"name":"gemini-upstream","alias":` +
			`"gemini-alias","display-name":"Gemini Name"}]}],`) +
		(`"vertex-api-key":[{"models":[{"name":"vertex-upstream","alias":` +
			`"vertex-alias","display-name":"Vertex Name"}]}],`) +
		`"openai-compatibility":[{"models":[` +
		`{"name":"compat-upstream","alias":"compat-alias","display-name":"Compatibility Name"}]}]}`
	for _, test := range []struct {
		name   string
		decode func(*Config) error
	}{
		{name: "YAML", decode: func(cfg *Config) error { return yaml.Unmarshal([]byte(yamlConfig), cfg) }},
		{name: "JSON", decode: func(cfg *Config) error { return json.Unmarshal([]byte(jsonConfig), cfg) }},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				var cfg Config
				if err := test.decode(&cfg); err != nil {
					t.Fatalf("decode config: %v", err)
				}
				for _, model := range []struct {
					got, want string
				}{
					{cfg.CodexKey[0].Models[0].DisplayName, "Codex Name"},
					{cfg.XAIKey[0].Models[0].DisplayName, "xAI Name"},
					{cfg.ClaudeKey[0].Models[0].DisplayName, "Claude Name"},
					{cfg.GeminiKey[0].Models[0].DisplayName, "Gemini Name"},
					{cfg.VertexCompatAPIKey[0].Models[0].DisplayName, "Vertex Name"},
					{cfg.OpenAICompatibility[0].Models[0].DisplayName, "Compatibility Name"},
				} {
					if model.got != model.want {
						t.Errorf("display-name = %q, want %q", model.got, model.want)
					}
				}
			},
		)
	}
}
