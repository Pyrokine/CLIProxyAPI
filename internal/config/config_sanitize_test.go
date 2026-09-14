package config

import "testing"

func TestSanitizeGeminiKeysUsesCollisionFreeIdentity(t *testing.T) {
	cfg := &Config{
		GeminiKey: []GeminiKey{
			{APIKey: "key", BaseURL: "https://a.example/|https://b.example"},
			{APIKey: "key|https://a.example/", BaseURL: "https://b.example"},
		},
	}

	cfg.SanitizeGeminiKeys()

	if len(cfg.GeminiKey) != 2 {
		t.Fatalf("GeminiKey length = %d, want 2: %+v", len(cfg.GeminiKey), cfg.GeminiKey)
	}
}

func TestSanitizeGeminiKeysUsesKeyAndBaseURLIdentity(t *testing.T) {
	cfg := &Config{
		GeminiKey: []GeminiKey{
			{APIKey: " shared-key ", BaseURL: ""},
			{APIKey: "shared-key", BaseURL: " https://gateway.example/v1 "},
			{APIKey: "shared-key", BaseURL: "https://gateway.example/v1"},
		},
	}

	cfg.SanitizeGeminiKeys()

	if len(cfg.GeminiKey) != 2 {
		t.Fatalf("GeminiKey length = %d, want 2: %+v", len(cfg.GeminiKey), cfg.GeminiKey)
	}
	if cfg.GeminiKey[0].BaseURL != "" {
		t.Fatalf("default Gemini BaseURL = %q, want empty", cfg.GeminiKey[0].BaseURL)
	}
	if cfg.GeminiKey[1].BaseURL != "https://gateway.example/v1" {
		t.Fatalf("custom Gemini BaseURL = %q", cfg.GeminiKey[1].BaseURL)
	}
}
