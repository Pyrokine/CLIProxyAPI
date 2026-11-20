package auth

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPublishedCoolingPolicyKeepsAuthOverridePrecedence(t *testing.T) {
	for _, sample := range []struct {
		name     string
		global   bool
		override any
		disabled bool
	}{
		{"default", false, nil, false},
		{"global", true, nil, true},
		{"auth-disable", false, true, true},
		{"auth-enable", true, false, false},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				auth := &Auth{Provider: "antigravity", Metadata: make(map[string]any)}
				if sample.override != nil {
					auth.Metadata["disable_cooling"] = sample.override
				}
				if got := QuotaCooldownDisabledForAuthWithConfig(
					auth,
					&internalconfig.Config{DisableCooling: sample.global},
				); got != sample.disabled {
					t.Fatalf("cooling disabled=%t, want %t", got, sample.disabled)
				}
			},
		)
	}
}
