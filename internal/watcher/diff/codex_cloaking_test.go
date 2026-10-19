package diff

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestBuildConfigChangeDetailsCodexCloaking(t *testing.T) {
	oldCfg := &config.Config{}
	newCfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}

	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	expectContains(t, changes, "codex.disable-codex-cloaking: false -> true")
}

func TestBuildConfigChangeDetailsCodexKeyCloaking(t *testing.T) {
	oldCfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "test-key", BaseURL: "https://example.test"}}}
	disabled := false
	newCfg := &config.Config{CodexKey: []config.CodexKey{{
		APIKey: "test-key", BaseURL: "https://example.test", DisableCodexCloaking: &disabled,
	}}}
	expectContains(t, BuildConfigChangeDetails(oldCfg, newCfg), "codex[0].disable-codex-cloaking: inherit -> false")
}
