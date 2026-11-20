package config_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestPublishedCodexHeaderSanitizer(t *testing.T) {
	cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "fixture-credential"}}}
	cfg.CodexHeaderDefaults.UserAgent = "  fixture-agent \n"
	cfg.CodexHeaderDefaults.BetaFeatures = "\tfixture-beta  "
	cfg.SanitizeCodexHeaderDefaults()
	if cfg.CodexHeaderDefaults.UserAgent != "fixture-agent" || cfg.CodexHeaderDefaults.BetaFeatures != "fixture-beta" {
		t.Fatalf("header defaults were not normalized: %+v", cfg.CodexHeaderDefaults)
	}
	if len(cfg.CodexKey) != 1 || cfg.CodexKey[0].APIKey != "fixture-credential" {
		t.Fatal("header normalization changed Codex credential entries")
	}
	var empty *config.Config
	empty.SanitizeCodexHeaderDefaults()
}
