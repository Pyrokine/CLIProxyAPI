package multiagentv2

import (
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestCodexSpawnAgentCacheInvalidatesWithRegistryGeneration(t *testing.T) {
	modelA := "cache-model-a"
	modelB := "cache-model-b"
	clientID := "spawn-cache-test"
	global := registry.GetGlobalRegistry()
	global.UnregisterClient(clientID)
	t.Cleanup(func() { global.UnregisterClient(clientID) })

	global.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: modelA}})
	first, firstMarkdown := codexSpawnAgentModelsAndMarkdownForRequest(context.Background(), nil, false)
	assertSpawnModelPresent(t, first, modelA)
	if firstMarkdown == "" {
		t.Fatal("first cached model markdown is empty")
	}
	cachedGeneration := codexSpawnAgentCacheGeneration

	global.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	second, secondMarkdown := codexSpawnAgentModelsAndMarkdownForRequest(context.Background(), nil, false)
	assertSpawnModelPresent(t, second, modelB)
	if secondMarkdown == firstMarkdown {
		t.Fatalf("model markdown did not change after adding %s", modelB)
	}
	if codexSpawnAgentCacheGeneration == cachedGeneration {
		t.Fatalf("cache generation remained %d after registry update", cachedGeneration)
	}
}

func assertSpawnModelPresent(t *testing.T, models []codexSpawnAgentModel, modelID string) {
	t.Helper()
	for _, model := range models {
		if model.id == modelID {
			return
		}
	}
	t.Fatalf("model %q not found in %s", modelID, fmt.Sprint(models))
}
