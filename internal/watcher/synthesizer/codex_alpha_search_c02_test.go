package synthesizer

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSynthesizeCodexAlphaSearchCapability(t *testing.T) {
	synthesizer := NewConfigSynthesizer()
	auths, errSynthesize := synthesizer.Synthesize(
		&SynthesisContext{
			Config: &config.Config{
				CodexKey: []config.CodexKey{
					{
						APIKey:      "codex-key",
						BaseURL:     "https://codex.example.com/v1",
						AlphaSearch: true,
					},
				},
				XAIKey: []config.XAIKey{
					{
						APIKey:      "xai-key",
						BaseURL:     "https://api.x.ai/v1",
						AlphaSearch: true,
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
	var codexAuth, xaiAuth *coreauth.Auth
	for _, credential := range auths {
		switch credential.Provider {
		case "codex":
			codexAuth = credential
		case "xai":
			xaiAuth = credential
		}
	}
	if codexAuth == nil || codexAuth.Attributes[coreauth.AttributeCodexAlphaSearch] != "true" {
		t.Fatalf("Codex auth = %#v, want alpha-search capability", codexAuth)
	}
	if xaiAuth == nil {
		t.Fatal("xAI auth missing")
	}
	if _, exists := xaiAuth.Attributes[coreauth.AttributeCodexAlphaSearch]; exists {
		t.Fatalf("xAI auth unexpectedly contains %s", coreauth.AttributeCodexAlphaSearch)
	}
}
