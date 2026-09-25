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

func TestSanitizeGeminiStyleKeysUsesFullRouteIdentity(t *testing.T) {
	entries := []GeminiKey{
		{
			APIKey:   " shared-key ",
			BaseURL:  " https://gateway.example/v1 ",
			ProxyURL: " https://proxy-a.example ",
			Prefix:   " /team-a/ ",
			Headers:  map[string]string{" X-B ": " 2 ", "X-A": " 1 "},
		},
		{
			APIKey:   "shared-key",
			BaseURL:  "https://gateway.example/v1",
			ProxyURL: "https://proxy-a.example",
			Prefix:   "team-a",
			Headers:  map[string]string{"X-A": "1", "X-B": "2"},
		},
		{
			APIKey:   "shared-key",
			BaseURL:  "https://gateway.example/v1",
			ProxyURL: "https://proxy-b.example",
			Prefix:   "team-a",
			Headers:  map[string]string{"X-A": "1", "X-B": "2"},
		},
		{
			APIKey:   "shared-key",
			BaseURL:  "https://gateway.example/v1",
			ProxyURL: "https://proxy-a.example",
			Prefix:   "team-b",
			Headers:  map[string]string{"X-A": "1", "X-B": "2"},
		},
		{
			APIKey:   "shared-key",
			BaseURL:  "https://gateway.example/v1",
			ProxyURL: "https://proxy-a.example",
			Prefix:   "team-a",
			Headers:  map[string]string{"X-A": "1", "X-B": "3"},
		},
	}
	tests := []struct {
		name     string
		set      func(*Config, []GeminiKey)
		sanitize func(*Config)
		get      func(*Config) []GeminiKey
	}{
		{
			name:     "gemini",
			set:      func(cfg *Config, values []GeminiKey) { cfg.GeminiKey = values },
			sanitize: func(cfg *Config) { cfg.SanitizeGeminiKeys() },
			get:      func(cfg *Config) []GeminiKey { return cfg.GeminiKey },
		},
		{
			name:     "interactions",
			set:      func(cfg *Config, values []GeminiKey) { cfg.InteractionsKey = values },
			sanitize: func(cfg *Config) { cfg.SanitizeInteractionsKeys() },
			get:      func(cfg *Config) []GeminiKey { return cfg.InteractionsKey },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{}
			test.set(cfg, append([]GeminiKey(nil), entries...))
			test.sanitize(cfg)
			got := test.get(cfg)
			if len(got) != 4 {
				t.Fatalf("credential count = %d, want 4: %+v", len(got), got)
			}
			if got[0].ProxyURL != "https://proxy-a.example" || got[0].Prefix != "team-a" ||
				FormatSortedHeaders(got[0].Headers) != "X-A\x001\x00X-B\x002\x00" {
				t.Fatalf("first normalized route = %+v", got[0])
			}
		})
	}
}

func TestFormatSortedHeadersIsDeterministic(t *testing.T) {
	first := FormatSortedHeaders(map[string]string{"X-B": "2", "X-A": "1"})
	second := FormatSortedHeaders(map[string]string{"X-A": "1", "X-B": "2"})
	if first != second {
		t.Fatalf("header serialization differs by insertion order: %q != %q", first, second)
	}
	if first == FormatSortedHeaders(map[string]string{"X-A": "1", "X-B": "3"}) {
		t.Fatal("different header values produced the same serialization")
	}
}
