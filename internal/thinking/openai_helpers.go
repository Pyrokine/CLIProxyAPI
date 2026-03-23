package thinking

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Reasoning effort field paths for OpenAI-family providers.
const (
	OpenAIEffortField = "reasoning_effort"
	CodexEffortField  = "reasoning.effort"
)

// ApplyReasoningEffort applies thinking configuration using the reasoning effort format.
// Used by openai and codex providers which share identical logic but differ in JSON field paths.
func ApplyReasoningEffort(body []byte, config Config, modelInfo *registry.ModelInfo, fieldPath string) ([]byte, error) {
	if IsUserDefinedModel(modelInfo) {
		return applyReasoningEffortCompatible(body, config, fieldPath)
	}
	if modelInfo.Thinking == nil {
		return body, nil
	}

	if config.Mode != ModeLevel && config.Mode != ModeNone {
		return body, nil
	}

	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	if config.Mode == ModeLevel {
		result, _ := sjson.SetBytes(body, fieldPath, string(config.Level))
		return result, nil
	}

	// ModeNone: resolve effort from model capabilities
	effort := ""
	support := modelInfo.Thinking
	if config.Budget == 0 {
		if support.ZeroAllowed || isLevelSupported(string(LevelNone), support.Levels) {
			effort = string(LevelNone)
		}
	}
	if effort == "" && config.Level != "" {
		effort = string(config.Level)
	}
	if effort == "" && len(support.Levels) > 0 {
		effort = support.Levels[0]
	}
	if effort == "" {
		return body, nil
	}

	result, _ := sjson.SetBytes(body, fieldPath, effort)
	return result, nil
}

// applyReasoningEffortCompatible handles user-defined models using the reasoning effort format.
func applyReasoningEffortCompatible(body []byte, config Config, fieldPath string) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	var effort string
	switch config.Mode {
	case ModeLevel:
		if config.Level == "" {
			return body, nil
		}
		effort = string(config.Level)
	case ModeNone:
		effort = string(LevelNone)
		if config.Level != "" {
			effort = string(config.Level)
		}
	case ModeAuto:
		effort = string(LevelAuto)
	case ModeBudget:
		level, ok := ConvertBudgetToLevel(config.Budget)
		if !ok {
			return body, nil
		}
		effort = level
	default:
		return body, nil
	}

	result, _ := sjson.SetBytes(body, fieldPath, effort)
	return result, nil
}
