package diff

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestModelSummariesIncludeIsCompat(t *testing.T) {
	if summarizeClaudeModels([]config.ClaudeModel{{Name: "m"}}).hash ==
		summarizeClaudeModels([]config.ClaudeModel{{Name: "m", IsCompat: true}}).hash {
		t.Fatal("Claude model summary did not change when IsCompat changed")
	}
	if summarizeGeminiModels([]config.GeminiModel{{Name: "m"}}).hash ==
		summarizeGeminiModels([]config.GeminiModel{{Name: "m", IsCompat: true}}).hash {
		t.Fatal("Gemini model summary did not change when IsCompat changed")
	}
	if summarizeCodexModels([]config.CodexModel{{Name: "m"}}).hash ==
		summarizeCodexModels([]config.CodexModel{{Name: "m", IsCompat: true}}).hash {
		t.Fatal("Codex model summary did not change when IsCompat changed")
	}
}

func TestOpenAICompatHashIncludesMaxCompletionTokens(t *testing.T) {
	base := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "upstream-model"}})
	changed := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "upstream-model", UseMaxCompletionTokens: true}})
	if base == changed {
		t.Fatal("OpenAI compatibility model hash did not change when UseMaxCompletionTokens changed")
	}
}

func TestModelHashesIncludeIsCompat(t *testing.T) {
	if ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m"}}) ==
		ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "m", IsCompat: true}}) {
		t.Fatal("Claude model hash did not change when IsCompat changed")
	}
	if ComputeGeminiModelsHash([]config.GeminiModel{{Name: "m"}}) ==
		ComputeGeminiModelsHash([]config.GeminiModel{{Name: "m", IsCompat: true}}) {
		t.Fatal("Gemini model hash did not change when IsCompat changed")
	}
	if ComputeCodexModelsHash([]config.CodexModel{{Name: "m"}}) ==
		ComputeCodexModelsHash([]config.CodexModel{{Name: "m", IsCompat: true}}) {
		t.Fatal("Codex model hash did not change when IsCompat changed")
	}
	if ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "m"}}) ==
		ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "m", IsCompat: true}}) {
		t.Fatal("OpenAI compatibility model hash did not change when IsCompat changed")
	}
}
