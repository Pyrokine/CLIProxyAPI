package handlers

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestRequiresContext1MCapabilityValidation_ClaudeOnly(t *testing.T) {
	if !requiresContext1MCapabilityValidation([]string{"claude"}) {
		t.Fatal("expected claude provider to require [1m] capability validation")
	}
	if requiresContext1MCapabilityValidation([]string{"openai"}) {
		t.Fatal("expected openai provider to skip [1m] capability validation")
	}
}

func TestSupportsContext1M_UsesStaticMetadataForOpenAIModels(t *testing.T) {
	if !supportsContext1M("gpt-5.4[1m]", []string{"openai"}) {
		t.Fatal("expected gpt-5.4 to support [1m]")
	}
	if supportsContext1M("gpt-5.5[1m]", []string{"openai"}) {
		t.Fatal("expected gpt-5.5 to reject native [1m] capability")
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
