package synthesizer

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestC04SynthesizeXAIAPIKeyWithoutDuplicatingOAuth(t *testing.T) {
	weight := 3
	ctx := &SynthesisContext{
		Config: &config.Config{
			XAIKey: []config.XAIKey{
				{
					APIKey:         "xai-key",
					Priority:       7,
					Weight:         &weight,
					Prefix:         "grok",
					BaseURL:        "https://api.x.ai/v1",
					ProxyURL:       "http://proxy.local",
					Websockets:     true,
					DisableCooling: new(true),
					Headers:        map[string]string{"X-Custom": "value"},
					Models:         []config.XAIModel{{Name: "grok-4.5", Alias: "grok-latest"}},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := NewConfigSynthesizer().Synthesize(ctx)
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "xai" || auth.Label != "xai-apikey" || auth.Attributes["auth_kind"] != "apikey" {
		t.Fatalf("auth identity = %+v", auth)
	}
	if auth.Attributes["api_key"] != "xai-key" || auth.Attributes["base_url"] != "https://api.x.ai/v1" {
		t.Fatalf("auth credentials = %#v", auth.Attributes)
	}
	if auth.Attributes["websockets"] != "true" || auth.Attributes["header:X-Custom"] != "value" || auth.Attributes["models_hash"] == "" {
		t.Fatalf("auth execution attributes = %#v", auth.Attributes)
	}
	if auth.Attributes["weight"] != "3" || auth.ProxyURL != "http://proxy.local" {
		t.Fatalf("auth routing = %+v", auth)
	}
	if disabled, ok := auth.Metadata["disable_cooling"].(bool); !ok || !disabled {
		t.Fatalf("disable_cooling = %#v, want true", auth.Metadata["disable_cooling"])
	}
}
