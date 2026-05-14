package cliproxy

import (
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestBuildCodexConfigModels_PreservesStaticMetadata(t *testing.T) {
	models := buildCodexConfigModels(
		&internalconfig.CodexKey{
			Models: []internalconfig.CodexModel{
				{
					Name: "gpt-5.5", Alias: "g55",
				},
			},
		},
	)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "g55" {
		t.Fatalf("expected alias g55, got %q", models[0].ID)
	}
	if models[0].ContextLength != 400000 {
		t.Fatalf("expected context_length 400000, got %d", models[0].ContextLength)
	}
	if models[0].MaxCompletionTokens == 0 {
		t.Fatal("expected max_completion_tokens to be preserved")
	}
	if models[0].Thinking == nil || len(models[0].Thinking.Levels) == 0 {
		t.Fatalf("expected thinking metadata, got %+v", models[0].Thinking)
	}
}

func TestBuildConfigModels_PreservesSupportedParameters(t *testing.T) {
	models := buildConfigModels([]internalconfig.CodexModel{{Name: "gpt-5.5", Alias: "g55"}}, "openai", "openai")
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	upstream := registry.LookupStaticModelInfo("gpt-5.5")
	if upstream == nil {
		t.Fatal("expected static metadata for gpt-5.5")
	}
	if len(models[0].SupportedParameters) != len(upstream.SupportedParameters) {
		t.Fatalf(
			"expected supported parameters to match upstream, got %v want %v", models[0].SupportedParameters,
			upstream.SupportedParameters,
		)
	}
}
