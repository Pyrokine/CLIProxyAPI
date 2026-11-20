package config

import "testing"

func TestSanitizeGeminiKeys_AllowsEmptyAPIKeyWithBaseURL(t *testing.T) {
	cfg := &Config{
		GeminiKey: []GeminiKey{
			{APIKey: ""},
			{APIKey: "  "},
			{
				APIKey: "", BaseURL: "https://custom-gemini.example.com",
				Headers: map[string]string{"Header-A": "1"},
			},
			{
				APIKey: "", BaseURL: "https://custom-gemini.example.com",
				Headers: map[string]string{"Header-B": "2"},
			},
			{APIKey: "key-1", BaseURL: "https://custom-gemini.example.com"},
		},
		InteractionsKey: []GeminiKey{
			{APIKey: ""},
			{APIKey: "  "},
			{APIKey: "", BaseURL: "https://custom-interactions.example.com"},
		},
	}
	cfg.SanitizeGeminiKeys()
	cfg.SanitizeInteractionsKeys()
	if len(cfg.GeminiKey) != 3 {
		t.Fatalf("expected 3 GeminiKey entries, got %d", len(cfg.GeminiKey))
	}
	if cfg.GeminiKey[0].BaseURL != "https://custom-gemini.example.com" {
		t.Fatalf("expected BaseURL https://custom-gemini.example.com, got %s", cfg.GeminiKey[0].BaseURL)
	}
	if cfg.GeminiKey[0].Headers["Header-A"] != "1" || cfg.GeminiKey[1].Headers["Header-B"] != "2" {
		t.Fatalf("distinct header identities were merged: %#v", cfg.GeminiKey)
	}
	if len(cfg.InteractionsKey) != 1 {
		t.Fatalf("expected 1 InteractionsKey entry, got %d", len(cfg.InteractionsKey))
	}
	if cfg.InteractionsKey[0].BaseURL != "https://custom-interactions.example.com" {
		t.Fatalf("expected BaseURL https://custom-interactions.example.com, got %s", cfg.InteractionsKey[0].BaseURL)
	}
}
