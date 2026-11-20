package registry

import (
	"reflect"
	"testing"
)

func TestKimiFixedCatalogMetadataAndFallback(t *testing.T) {
	// v8.0.23 models.json defines these exact compatibility IDs and capabilities.
	tests := []struct {
		id, displayName, description string
		created                      int64
		contextLength                int
		inputModalities              []string
	}{
		{
			"kimi-k2.8", "Kimi K2.8 Preview",
			"Kimi K2.8 Preview - Lightweight coding and agent model with near-K3 performance and 1M context window",
			1789115500, 1048576, []string{"text", "image", "video"},
		},
		{
			"kimi-k2.8-code", "Kimi K2.8 Code Preview",
			"Kimi K2.8 Code Preview - Lightweight coding and agent model with " +
				"near-K3 performance and 1M context " +
				"window",
			1789115500, 1048576, []string{"text", "image", "video"},
		},
		{
			"kimi-k3-256k", "Kimi K3 256K",
			"Kimi K3 256K - 256K context version of Kimi K3 delivering the same results within 256K context " +
				"at reduced quota consumption; supports image input only (no video)",
			1785110400, 262144, []string{"text", "image"},
		},
	}
	original := getModels()
	t.Cleanup(
		func() {
			modelsCatalogStore.mu.Lock()
			modelsCatalogStore.data = original
			modelsCatalogStore.mu.Unlock()
		},
	)
	for _, fallback := range []bool{false, true} {
		name := "catalog"
		if fallback {
			name = "fallback"
			modelsCatalogStore.mu.Lock()
			modelsCatalogStore.data = &staticModelsJSON{}
			modelsCatalogStore.mu.Unlock()
		}
		t.Run(
			name, func(t *testing.T) {
				for _, test := range tests {
					t.Run(
						test.id, func(t *testing.T) {
							want := &ModelInfo{
								ID: test.id, Object: "model", Created: test.created, OwnedBy: "moonshot", Type: "kimi",
								DisplayName: test.displayName, Description: test.description,
								ContextLength:       test.contextLength,
								MaxCompletionTokens: 65536, SupportedInputModalities: test.inputModalities,
								SupportedOutputModalities: []string{"text"},
								Thinking: &ThinkingSupport{
									ZeroAllowed: true, Levels: []string{"low", "high", "max"},
								},
							}
							for channel, models := range map[string][]*ModelInfo{
								"runtime":  GetKimiModels(),
								"embedded": GetEmbeddedStaticModelDefinitionsByChannel("kimi"),
							} {
								if got := findModelByID(models, test.id); !reflect.DeepEqual(got, want) {
									t.Errorf("%s metadata = %+v, want %+v", channel, got, want)
								}
							}
							for _, channel := range []string{"kimi", "kimi-ai", "kimi.com", "kimi.ai"} {
								if got := LookupStaticModelInfoByChannel(test.id, channel); !reflect.DeepEqual(
									got, want,
								) {
									t.Errorf("%s lookup = %+v, want %+v", channel, got, want)
								}
							}
						},
					)
				}
			},
		)
	}
}
