package util

import "testing"

func TestOpenAICompatibleProviderKeyPreservesNamespaceContract(t *testing.T) {
	for _, sample := range []struct{ name, expected string }{
		{"", "openai-compatibility"},
		{"  ", "openai-compatibility"},
		{"openai-compatibility", "openai-compatibility"},
		{" KIMI ", "openai-compatible-kimi"},
		{"kimi.ai", "openai-compatible-kimi.ai"},
		{"custom", "openai-compatible-custom"},
		{" OpenAI-Compatible-Custom ", "openai-compatible-custom"},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				if actual := OpenAICompatibleProviderKey(sample.name); actual != sample.expected {
					t.Fatalf("provider key=%q, expected=%q", actual, sample.expected)
				}
				if twice := OpenAICompatibleProviderKey(sample.expected); twice != sample.expected {
					t.Fatalf("namespace was applied twice: %q", twice)
				}
			},
		)
	}
}
