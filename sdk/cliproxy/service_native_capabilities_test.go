package cliproxy

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestConfigModelBuildersCopyStaticNativeCapabilities(t *testing.T) {
	tests := []struct {
		name   string
		models []*ModelInfo
	}{
		{
			name: "provider config alias",
			models: buildConfigModels(
				[]internalconfig.CodexModel{{Name: "gpt-5.6-sol", Alias: "team/sol"}},
				"openai",
				"openai",
			),
		},
		{
			name: "openai compatibility alias",
			models: buildOpenAICompatibilityConfigModels(&internalconfig.OpenAICompatibility{
				Name:   "gateway",
				Models: []internalconfig.OpenAICompatibilityModel{{Name: "gpt-5.6-sol", Alias: "gateway-sol"}},
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.models) != 1 {
				t.Fatalf("models = %d, want 1", len(test.models))
			}
			capabilities := test.models[0].NativeCapabilities
			if capabilities == nil || capabilities.WebSearch == nil || !*capabilities.WebSearch {
				t.Fatalf("native capabilities = %+v, want web_search=true", capabilities)
			}
		})
	}
}
