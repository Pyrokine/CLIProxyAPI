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
