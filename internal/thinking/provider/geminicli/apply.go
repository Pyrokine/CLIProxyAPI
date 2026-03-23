// Package geminicli implements thinking configuration for Gemini CLI API format.
//
// Gemini CLI uses request.generationConfig.thinkingConfig.* path instead of
// generationConfig.thinkingConfig.* used by standard Gemini API.
package geminicli

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
)

// Applier applies thinking configuration for Gemini CLI API format.
type Applier struct{}

var _ thinking.ProviderApplier = (*Applier)(nil)

// newApplier creates a new Gemini CLI thinking applier.
func newApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("gemini-cli", newApplier())
}

// Apply applies thinking configuration to Gemini CLI request body.
func (a *Applier) Apply(body []byte, config thinking.Config, modelInfo *registry.ModelInfo) ([]byte, error) {
	if thinking.IsUserDefinedModel(modelInfo) {
		return thinking.ApplyGeminiCompatible(body, config, applyLevel, applyBudget)
	}
	if modelInfo.Thinking == nil {
		return body, nil
	}

	var ok bool
	if body, ok = thinking.ValidateGeminiPreconditions(body, config); !ok {
		return body, nil
	}

	// ModeAuto: Always use Budget format with thinkingBudget=-1
	if config.Mode == thinking.ModeAuto || config.Mode == thinking.ModeBudget {
		return applyBudget(body, config)
	}

	// For non-auto modes, choose format based on model capabilities
	support := modelInfo.Thinking
	if len(support.Levels) > 0 {
		return applyLevel(body, config)
	}
	return applyBudget(body, config)
}

func applyLevel(body []byte, config thinking.Config) ([]byte, error) {
	return thinking.ApplyGeminiLevelFormat(body, config, thinking.GeminiCLIPrefix)
}

func applyBudget(body []byte, config thinking.Config) ([]byte, error) {
	return thinking.ApplyGeminiBudgetFormat(body, config, config.Budget, thinking.GeminiCLIPrefix)
}
