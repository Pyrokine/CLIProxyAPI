package auth

import (
	"context"
	"fmt"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestProviderCoolingDisabledForOpenAICompatibility(t *testing.T) {
	cfg := &internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name:           "compat",
				DisableCooling: new(true),
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

func TestProviderCoolingOverrideReachesMarkResult(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, provider := range []*bool{nil, new(false), new(true)} {
			for _, credential := range []*bool{nil, new(false), new(true)} {
				t.Run(fmt.Sprintf("global_%t/provider_%v/credential_%v", global, provider, credential), func(t *testing.T) {
					metadata := map[string]any{}
					if credential != nil {
						metadata["disable_cooling"] = *credential
					}
					manager, current := newExecutionOutcomeTestManager(t, &Auth{
						ID: "compat-override", Provider: "compat", Status: StatusActive, Metadata: metadata,
						Attributes: map[string]string{"provider_key": "compat", "compat_name": "compat"},
					})
					manager.SetConfig(&internalconfig.Config{DisableCooling: global, OpenAICompatibility: []internalconfig.OpenAICompatibility{{Name: "compat", DisableCooling: provider}}})
					manager.MarkResult(context.Background(), executionResult(current, current.Provider, "model-a", executionOutcomeStatusError{status: 401}, cliproxyexecutor.DispatchPhaseMaybeSent))
					after := getExecutionOutcomeTestAuth(t, manager, current.ID)
					disabled := global
					if provider != nil {
						disabled = *provider
					}
					if credential != nil {
						disabled = *credential
					}
					if after.Unavailable == disabled || after.NextRetryAfter.IsZero() != disabled {
						t.Fatalf("disabled=%t unavailable=%t deadline=%v", disabled, after.Unavailable, after.NextRetryAfter)
					}
				})
			}
		}
	}
}
