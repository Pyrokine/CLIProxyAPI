package synthesizer

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestOpenAICompatibilityDisableCoolingMetadata(t *testing.T) {
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(
		&SynthesisContext{
			Config: &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{
					{
						Name:           "compat",
						DisableCooling: true,
						APIKeyEntries: []config.OpenAICompatibilityAPIKey{
							{
								APIKey: "test-key",
							},
						},
					},
				},
			},
			Now:         time.Now(),
			IDGenerator: NewStableIDGenerator(),
		},
	)
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("Synthesize() auth count = %d, want 1", len(auths))
	}
	if disabled, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !disabled {
		t.Fatalf("disable_cooling metadata = %#v, want true", auths[0].Metadata["disable_cooling"])
	}
}
