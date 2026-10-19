package diff

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestComputeOpenAICompatModelsHash_Deterministic(t *testing.T) {
	models := []config.OpenAICompatibilityModel{
		{Name: "gpt-4", Alias: "gpt4"},
		{Name: "gpt-3.5-turbo"},
	}
	hash1 := ComputeOpenAICompatModelsHash(models)
	hash2 := ComputeOpenAICompatModelsHash(models)
	if hash1 == "" {
		t.Fatal("hash should not be empty")
	}
	if hash1 != hash2 {
		t.Fatalf("hash should be deterministic, got %s vs %s", hash1, hash2)
	}
	changed := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "gpt-4"}, {Name: "gpt-4.1"}})
	if hash1 == changed {
		t.Fatal("hash should change when model list changes")
	}
}

func TestComputeOpenAICompatModelsHash_NormalizesAndDedups(t *testing.T) {
	a := []config.OpenAICompatibilityModel{
		{Name: "gpt-4", Alias: "gpt4"},
		{Name: " "},
		{Name: "GPT-4", Alias: "GPT4"},
		{Alias: "a1"},
	}
	b := []config.OpenAICompatibilityModel{
		{Alias: "A1"},
		{Name: "gpt-4", Alias: "gpt4"},
	}
	h1 := ComputeOpenAICompatModelsHash(a)
	h2 := ComputeOpenAICompatModelsHash(b)
	if h1 == "" || h2 == "" {
		t.Fatal("expected non-empty hashes for non-empty model sets")
	}
	if h1 != h2 {
		t.Fatalf("expected normalized hashes to match, got %s / %s", h1, h2)
	}
}

func TestComputeOpenAICompatModelsHash_IncludesImageFlag(t *testing.T) {
	textHash := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "gpt-image", Alias: "image"}})
	imageHash := ComputeOpenAICompatModelsHash(
		[]config.OpenAICompatibilityModel{
			{
				Name: "gpt-image", Alias: "image", Image: true,
			},
		},
	)
	if textHash == "" || imageHash == "" {
		t.Fatal("expected non-empty hashes")
	}
	if textHash == imageHash {
		t.Fatalf("hash should change when image flag changes, got %s", textHash)
	}
}

func TestComputeVertexCompatModelsHash_DifferentInputs(t *testing.T) {
	models := []config.VertexCompatModel{{Name: "gemini-pro", Alias: "pro"}}
	hash1 := ComputeVertexCompatModelsHash(models)
	hash2 := ComputeVertexCompatModelsHash([]config.VertexCompatModel{{Name: "gemini-1.5-pro", Alias: "pro"}})
	if hash1 == "" || hash2 == "" {
		t.Fatal("hashes should not be empty for non-empty models")
	}
	if hash1 == hash2 {
		t.Fatal("hash should differ when model content differs")
	}
}

func TestComputeVertexCompatModelsHash_IgnoresBlankAndOrder(t *testing.T) {
	a := []config.VertexCompatModel{
		{Name: "m1", Alias: "a1"},
		{Name: " "},
		{Name: "M1", Alias: "A1"},
	}
	b := []config.VertexCompatModel{
		{Name: "m1", Alias: "a1"},
	}
	if h1, h2 := ComputeVertexCompatModelsHash(a), ComputeVertexCompatModelsHash(b); h1 == "" || h1 != h2 {
		t.Fatalf("expected same hash ignoring blanks/dupes, got %q / %q", h1, h2)
	}
}

func TestComputeClaudeModelsHash_Empty(t *testing.T) {
	if got := ComputeClaudeModelsHash(nil); got != "" {
		t.Fatalf("expected empty hash for nil models, got %q", got)
	}
	if got := ComputeClaudeModelsHash([]config.ClaudeModel{}); got != "" {
		t.Fatalf("expected empty hash for empty slice, got %q", got)
	}
}

func TestComputeCodexModelsHash_ConfigurationUpdateCapability(t *testing.T) {
	base := config.CodexModel{Name: "model", Alias: "alias"}
	withoutSupport := ComputeCodexModelsHash([]config.CodexModel{base})
	if want := "49a3a5b69ae7f02d63f46ca952e614f07f381136d1aa68701543d2062ed65760"; withoutSupport != want {
		t.Fatalf("default capability changed the existing hash: got %q, want %q", withoutSupport, want)
	}
	base.SupportConfigurationUpdate = true
	withSupport := ComputeCodexModelsHash([]config.CodexModel{base})
	if withSupport == withoutSupport {
		t.Fatal("configuration update capability did not change the model hash")
	}
	base.SupportConfigurationUpdate = false
	if got := ComputeCodexModelsHash([]config.CodexModel{base}); got != withoutSupport {
		t.Fatalf("explicit false differs from the default: got %q, want %q", got, withoutSupport)
	}
}

func TestComputeCodexModelsHash_Empty(t *testing.T) {
	if got := ComputeCodexModelsHash(nil); got != "" {
		t.Fatalf("expected empty hash for nil models, got %q", got)
	}
	if got := ComputeCodexModelsHash([]config.CodexModel{}); got != "" {
		t.Fatalf("expected empty hash for empty slice, got %q", got)
	}
}

func TestC04ComputeXAIModelsHashIncludesForceMapping(t *testing.T) {
	withoutForce := ComputeCodexModelsHash([]config.XAIModel{{Name: "grok-4.5", Alias: "grok-latest"}})
	withForce := ComputeCodexModelsHash([]config.XAIModel{{Name: "grok-4.5", Alias: "grok-latest", ForceMapping: true}})
	if withoutForce == "" || withForce == "" || withoutForce == withForce {
		t.Fatalf("hashes = %q / %q, want distinct non-empty values", withoutForce, withForce)
	}
}

func TestComputeClaudeModelsHash_IgnoresBlankAndDedup(t *testing.T) {
	a := []config.ClaudeModel{
		{Name: "m1", Alias: "a1"},
		{Name: " "},
		{Name: "M1", Alias: "A1"},
	}
	b := []config.ClaudeModel{
		{Name: "m1", Alias: "a1"},
	}
	if h1, h2 := ComputeClaudeModelsHash(a), ComputeClaudeModelsHash(b); h1 == "" || h1 != h2 {
		t.Fatalf("expected same hash ignoring blanks/dupes, got %q / %q", h1, h2)
	}
}

func TestComputeCodexModelsHash_IgnoresBlankAndDedup(t *testing.T) {
	a := []config.CodexModel{
		{Name: "m1", Alias: "a1"},
		{Name: " "},
		{Name: "M1", Alias: "A1"},
	}
	b := []config.CodexModel{
		{Name: "m1", Alias: "a1"},
	}
	if h1, h2 := ComputeCodexModelsHash(a), ComputeCodexModelsHash(b); h1 == "" || h1 != h2 {
		t.Fatalf("expected same hash ignoring blanks/dupes, got %q / %q", h1, h2)
	}
}

func TestConfiguredModelHashesIncludeDisplayNameAndMaxContextLength(t *testing.T) {
	base := []config.CodexModel{{Name: "gpt-5.5", Alias: "configured-gpt"}}
	withDisplayName := []config.CodexModel{{Name: "gpt-5.5", Alias: "configured-gpt", DisplayName: "Configured GPT"}}
	withMaxContext := []config.CodexModel{{Name: "gpt-5.5", Alias: "configured-gpt", MaxContextLength: 262144}}

	baseHash := ComputeCodexModelsHash(base)
	if baseHash == ComputeCodexModelsHash(withDisplayName) {
		t.Fatal("Codex model hash did not change for display-name")
	}
	if baseHash == ComputeCodexModelsHash(withMaxContext) {
		t.Fatal("Codex model hash did not change for max-context-length")
	}

	compatBase := []config.OpenAICompatibilityModel{{Name: "gpt-5.5", Alias: "configured-gpt"}}
	compatChanged := []config.OpenAICompatibilityModel{
		{
			Name:             "gpt-5.5",
			Alias:            "configured-gpt",
			DisplayName:      "Configured GPT",
			MaxContextLength: 262144,
		},
	}
	if ComputeOpenAICompatModelsHash(compatBase) == ComputeOpenAICompatModelsHash(compatChanged) {
		t.Fatal("OpenAI compatibility model hash did not include metadata overrides")
	}
}

func TestConfiguredModelHashesIncludeThinking(t *testing.T) {
	thinking := &registry.ThinkingSupport{Levels: []string{"high"}}
	tests := []struct {
		name string
		base func() string
		with func() string
	}{
		{
			name: "openai compatibility",
			base: func() string {
				return ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "model", Alias: "alias"}})
			},
			with: func() string {
				return ComputeOpenAICompatModelsHash(
					[]config.OpenAICompatibilityModel{
						{
							Name: "model", Alias: "alias", Thinking: thinking,
						},
					},
				)
			},
		},
		{
			name: "vertex",
			base: func() string {
				return ComputeVertexCompatModelsHash([]config.VertexCompatModel{{Name: "model", Alias: "alias"}})
			},
			with: func() string {
				return ComputeVertexCompatModelsHash(
					[]config.VertexCompatModel{
						{
							Name: "model", Alias: "alias", Thinking: thinking,
						},
					},
				)
			},
		},
		{
			name: "claude",
			base: func() string {
				return ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "model", Alias: "alias"}})
			},
			with: func() string {
				return ComputeClaudeModelsHash(
					[]config.ClaudeModel{
						{
							Name: "model", Alias: "alias", Thinking: thinking,
						},
					},
				)
			},
		},
		{
			name: "codex",
			base: func() string {
				return ComputeCodexModelsHash([]config.CodexModel{{Name: "model", Alias: "alias"}})
			},
			with: func() string {
				return ComputeCodexModelsHash([]config.CodexModel{{Name: "model", Alias: "alias", Thinking: thinking}})
			},
		},
		{
			name: "gemini",
			base: func() string {
				return ComputeGeminiModelsHash([]config.GeminiModel{{Name: "model", Alias: "alias"}})
			},
			with: func() string {
				return ComputeGeminiModelsHash(
					[]config.GeminiModel{
						{
							Name: "model", Alias: "alias", Thinking: thinking,
						},
					},
				)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				baseHash := testCase.base()
				withHash := testCase.with()
				if baseHash == "" || withHash == "" || baseHash == withHash {
					t.Fatalf("hashes = %q / %q, want distinct non-empty values", baseHash, withHash)
				}
			},
		)
	}
}

func TestComputeExcludedModelsHash_Normalizes(t *testing.T) {
	hash1 := ComputeExcludedModelsHash([]string{" A ", "b", "a"})
	hash2 := ComputeExcludedModelsHash([]string{"a", " b", "A"})
	if hash1 == "" || hash2 == "" {
		t.Fatal("hash should not be empty for non-empty input")
	}
	if hash1 != hash2 {
		t.Fatalf("hash should be order/space insensitive for same multiset, got %s vs %s", hash1, hash2)
	}
	hash3 := ComputeExcludedModelsHash([]string{"c"})
	if hash1 == hash3 {
		t.Fatal("hash should differ for different normalized sets")
	}
}

func TestComputeOpenAICompatModelsHash_Empty(t *testing.T) {
	if got := ComputeOpenAICompatModelsHash(nil); got != "" {
		t.Fatalf("expected empty hash for nil input, got %q", got)
	}
	if got := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{}); got != "" {
		t.Fatalf("expected empty hash for empty slice, got %q", got)
	}
	if got := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: " "}, {Alias: ""}}); got != "" {
		t.Fatalf("expected empty hash for blank models, got %q", got)
	}
}

func TestComputeVertexCompatModelsHash_Empty(t *testing.T) {
	if got := ComputeVertexCompatModelsHash(nil); got != "" {
		t.Fatalf("expected empty hash for nil input, got %q", got)
	}
	if got := ComputeVertexCompatModelsHash([]config.VertexCompatModel{}); got != "" {
		t.Fatalf("expected empty hash for empty slice, got %q", got)
	}
	if got := ComputeVertexCompatModelsHash([]config.VertexCompatModel{{Name: " "}}); got != "" {
		t.Fatalf("expected empty hash for blank models, got %q", got)
	}
}

func TestComputeExcludedModelsHash_Empty(t *testing.T) {
	if got := ComputeExcludedModelsHash(nil); got != "" {
		t.Fatalf("expected empty hash for nil input, got %q", got)
	}
	if got := ComputeExcludedModelsHash([]string{}); got != "" {
		t.Fatalf("expected empty hash for empty slice, got %q", got)
	}
	if got := ComputeExcludedModelsHash([]string{"  ", ""}); got != "" {
		t.Fatalf("expected empty hash for whitespace-only entries, got %q", got)
	}
}

func TestComputeClaudeModelsHash_Deterministic(t *testing.T) {
	models := []config.ClaudeModel{{Name: "a", Alias: "A"}, {Name: "b"}}
	h1 := ComputeClaudeModelsHash(models)
	h2 := ComputeClaudeModelsHash(models)
	if h1 == "" || h1 != h2 {
		t.Fatalf("expected deterministic hash, got %s / %s", h1, h2)
	}
	if h3 := ComputeClaudeModelsHash([]config.ClaudeModel{{Name: "a"}}); h3 == h1 {
		t.Fatalf("expected different hash when models change, got %s", h3)
	}
}

func TestComputeCodexModelsHash_Deterministic(t *testing.T) {
	models := []config.CodexModel{{Name: "a", Alias: "A"}, {Name: "b"}}
	h1 := ComputeCodexModelsHash(models)
	h2 := ComputeCodexModelsHash(models)
	if h1 == "" || h1 != h2 {
		t.Fatalf("expected deterministic hash, got %s / %s", h1, h2)
	}
	if h3 := ComputeCodexModelsHash([]config.CodexModel{{Name: "a"}}); h3 == h1 {
		t.Fatalf("expected different hash when models change, got %s", h3)
	}
}
