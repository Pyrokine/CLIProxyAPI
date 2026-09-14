package diff

import (
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestBuildConfigChangeDetailsCodexAlphaSearch(t *testing.T) {
	oldConfig := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "key",
				BaseURL: "https://codex.example.com",
			},
		},
	}
	newConfig := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:      "key",
				BaseURL:     "https://codex.example.com",
				AlphaSearch: true,
			},
		},
	}

	changes := BuildConfigChangeDetails(oldConfig, newConfig)
	if !slices.Contains(changes, "codex[0].alpha-search: false -> true") {
		t.Fatalf("changes = %v", changes)
	}
}
