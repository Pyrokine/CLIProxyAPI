package management

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const configAPIKeyDisablePattern = "*"

func setConfigAPIKeyExcludedAll(models []string, disabled bool) []string {
	filtered := make([]string, 0, len(models)+1)
	for _, model := range models {
		if strings.TrimSpace(model) != configAPIKeyDisablePattern {
			filtered = append(filtered, model)
		}
	}
	if disabled {
		filtered = append(filtered, configAPIKeyDisablePattern)
	}
	return config.NormalizeExcludedModels(filtered)
}

func toggleConfigAPIKeyExcludedAll(cfg *config.Config, target *coreauth.Auth, disabled bool) error {
	if cfg == nil || target == nil || !coreauth.IsConfigAPIKeyAuth(target) {
		return newConfigMutationError(http.StatusConflict, "config credential status is not editable")
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		return fmt.Errorf("synthesize config credentials: %w", err)
	}
	for _, auth := range auths {
		if auth.ID != target.ID || !coreauth.IsConfigAPIKeyAuth(auth) {
			continue
		}
		index, errIndex := strconv.Atoi(auth.Attributes[coreauth.AttributeConfigIndex])
		if errIndex != nil || index < 0 {
			break
		}
		var excluded *[]string
		switch {
		case strings.HasPrefix(auth.ID, "gemini:apikey:") && index < len(cfg.GeminiKey):
			excluded = &cfg.GeminiKey[index].ExcludedModels
		case strings.HasPrefix(auth.ID, "gemini-interactions:apikey:") && index < len(cfg.InteractionsKey):
			excluded = &cfg.InteractionsKey[index].ExcludedModels
		case strings.HasPrefix(auth.ID, "claude:apikey:") && index < len(cfg.ClaudeKey):
			excluded = &cfg.ClaudeKey[index].ExcludedModels
		case strings.HasPrefix(auth.ID, "codex:apikey:") && index < len(cfg.CodexKey):
			excluded = &cfg.CodexKey[index].ExcludedModels
		case strings.HasPrefix(auth.ID, "meta:apikey:") && index < len(cfg.MetaKey):
			excluded = &cfg.MetaKey[index].ExcludedModels
		case strings.HasPrefix(auth.ID, "xai:apikey:") && index < len(cfg.XAIKey):
			excluded = &cfg.XAIKey[index].ExcludedModels
		case strings.HasPrefix(auth.ID, "vertex:apikey:") && index < len(cfg.VertexCompatAPIKey):
			excluded = &cfg.VertexCompatAPIKey[index].ExcludedModels
		}
		if excluded != nil {
			*excluded = setConfigAPIKeyExcludedAll(*excluded, disabled)
			return nil
		}
		break
	}
	return newConfigMutationError(http.StatusConflict, "config credential has changed; refresh and retry")
}
