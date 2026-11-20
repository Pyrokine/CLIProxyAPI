package cliproxy

import (
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestMediaModelsReachOnlyEligibleCredentialCatalogs(t *testing.T) {
	service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	for _, test := range []struct {
		provider  string
		kind      string
		models    []string
		available bool
	}{
		{"codex", "apikey", []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"}, true},
		{"codex", "oauth", []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"}, false},
		{"meta", "apikey", []string{"muse-image-1.0", "muse-voice-transcribe-1.0"}, true},
		{"xai", "apikey", []string{"grok-imagine-video-1.5-lite"}, true},
	} {
		t.Run(
			test.provider+"/"+test.kind, func(t *testing.T) {
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: test.provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"auth_kind": test.kind, "plan_type": "pro"},
				}
				service.registerModelsForAuth(auth)
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
				for _, model := range test.models {
					if got := GlobalModelRegistry().ClientSupportsModel(auth.ID, model); got != test.available {
						t.Fatalf("%s on %s/%s = %t", model, test.provider, test.kind, got)
					}
				}
			},
		)
	}
}
