package auth

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestProviderCoolingDisabledForOpenAICompatibility(t *testing.T) {
	cfg := &internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name:           "compat",
				DisableCooling: true,
			},
		},
	}
	auth := &Auth{
		Provider: "compat",
		Attributes: map[string]string{
			"provider_key": "compat",
			"compat_name":  "compat",
		},
	}
	if !quotaCooldownDisabledForAuthWithConfig(auth, cfg) {
		t.Fatal("provider cooldown = enabled, want disabled")
	}
}
