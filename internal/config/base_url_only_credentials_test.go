package config

import "testing"

func TestSanitizeKeepsBaseURLOnlyGeminiAndXAIEntries(t *testing.T) {
	cfg := &Config{
		GeminiKey: []GeminiKey{{BaseURL: " https://gemini.example "}, {}},
		XAIKey:    []XAIKey{{BaseURL: " https://xai.example "}, {}},
	}

	cfg.SanitizeGeminiKeys()
	cfg.SanitizeXAIKeys()

	if len(cfg.GeminiKey) != 1 || cfg.GeminiKey[0].BaseURL != "https://gemini.example" {
		t.Fatalf("GeminiKey = %+v", cfg.GeminiKey)
	}
	if len(cfg.XAIKey) != 1 || cfg.XAIKey[0].BaseURL != "https://xai.example" {
		t.Fatalf("XAIKey = %+v", cfg.XAIKey)
	}
}
