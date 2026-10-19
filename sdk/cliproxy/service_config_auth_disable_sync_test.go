package cliproxy

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
)

func TestConfigAPIKeyExcludedAllRemovesModelsAcrossReload(t *testing.T) {
	const model = "only-model"
	cfg := &config.Config{VertexCompatAPIKey: []config.VertexCompatKey{{
		APIKey: "test-key", BaseURL: "https://vertex.example.com",
		Models: []config.VertexCompatModel{{Name: "upstream-only", Alias: model}},
	}}}
	synthesize := func() string {
		t.Helper()
		auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
			Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
		})
		if err != nil || len(auths) != 1 {
			t.Fatalf("synthesize auth: %v, count=%d", err, len(auths))
		}
		(&Service{cfg: cfg}).registerModelsForAuthWithConfig(auths[0], cfg)
		return auths[0].ID
	}
	id := synthesize()
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(id) })
	if !GlobalModelRegistry().ClientSupportsModel(id, model) {
		t.Fatal("active credential model was not registered")
	}
	cfg.VertexCompatAPIKey[0].ExcludedModels = []string{"*"}
	if nextID := synthesize(); nextID != id {
		t.Fatalf("disabled credential ID = %s, want %s", nextID, id)
	}
	if GlobalModelRegistry().ClientSupportsModel(id, model) {
		t.Fatal("disabled credential model remained registered")
	}
	cfg.VertexCompatAPIKey[0].ExcludedModels = nil
	if nextID := synthesize(); nextID != id {
		t.Fatalf("restored credential ID = %s, want %s", nextID, id)
	}
	if !GlobalModelRegistry().ClientSupportsModel(id, model) {
		t.Fatal("restored credential model was not registered")
	}
}
