package handlers

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestRequiresContext1MCapabilityValidation_RequiresKnownProvider(t *testing.T) {
	if !requiresContext1MCapabilityValidation([]string{"claude"}) {
		t.Fatal("expected claude provider to require [1m] capability validation")
	}
	if !requiresContext1MCapabilityValidation([]string{"openai"}) {
		t.Fatal("expected openai provider to require [1m] capability validation")
	}
	if requiresContext1MCapabilityValidation(nil) {
		t.Fatal("expected empty providers to skip [1m] capability validation")
	}
}

func TestSupportsContext1M_RejectsStaticOpenAIModelsBelowOneMillion(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol[1m]", "gpt-6-astra[1m]"} {
		if supportsContext1M(model, []string{"openai"}) {
			t.Fatalf("expected %s to reject native [1m] capability", model)
		}
	}
}

func TestSupportsContext1M_UsesDynamicMetadataForRegisteredModels(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		"test-context1m-claude", "claude", []*registry.ModelInfo{{ID: "claude-sonnet-4-5", ContextLength: 1_000_000}},
	)
	defer reg.UnregisterClient("test-context1m-claude")
	if !supportsContext1M("claude-sonnet-4-5[1m]", []string{"claude"}) {
		t.Fatal("expected dynamic claude model to support [1m]")
	}
}
