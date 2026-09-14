package auth

import (
	"context"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestC04XAIAPIKeyAliasAndForceMapping(t *testing.T) {
	cfg := &internalconfig.Config{
		XAIKey: []internalconfig.XAIKey{
			{
				APIKey:  "xai-key",
				BaseURL: "https://api.x.ai/v1",
				Models: []internalconfig.XAIModel{
					{
						Name:         "grok-4.5",
						Alias:        "grok-latest",
						ForceMapping: true,
					},
				},
			},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &Auth{
		ID:       "xai-config-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":   "xai-key",
			"base_url":  "https://api.x.ai/v1",
			"auth_kind": "apikey",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	result := manager.resolveAPIKeyModelAliasWithResult(auth, "grok-latest")
	if result.UpstreamModel != "grok-4.5" || !result.ForceMapping || result.OriginalAlias != "grok-latest" {
		t.Fatalf("alias result = %+v", result)
	}

	oauth := &Auth{Provider: "xai", Attributes: map[string]string{"auth_kind": "oauth"}}
	oauthResult := manager.resolveAPIKeyModelAliasWithResult(oauth, "grok-latest")
	if oauthResult.UpstreamModel != "" || oauthResult.ForceMapping {
		t.Fatalf("OAuth auth used API-key alias path: %+v", oauthResult)
	}
}
