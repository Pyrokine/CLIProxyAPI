// Package gemini implements thinking configuration for Gemini models.
//
// Gemini models have two formats:
//   - Gemini 2.5: Uses thinkingBudget (numeric)
//   - Gemini 3.x: Uses thinkingLevel (string: minimal/low/medium/high)
//     or thinkingBudget=-1 for auto/dynamic mode
//
// Output format is determined by Config.Mode and ThinkingSupport.Levels:
//   - ModeAuto: Always uses thinkingBudget=-1 (both Gemini 2.5 and 3.x)
//   - len(Levels) > 0: Uses thinkingLevel (Gemini 3.x discrete levels)
//   - len(Levels) == 0: Uses thinkingBudget (Gemini 2.5)
package gemini

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
)

// Applier applies thinking configuration for Gemini models.
//
// Gemini-specific behavior:
//   - Gemini 2.5: thinkingBudget format, flash series supports ZeroAllowed
//   - Gemini 3.x: thinkingLevel format, cannot be disabled
//   - Use ThinkingSupport.Levels to decide output format
type Applier struct{}

// newApplier creates a new Gemini thinking applier.
func newApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("gemini", newApplier())
}

// Apply applies thinking configuration to Gemini request body.
//
// Expected output format (Gemini 2.5):
//
//	{
//	  "generationConfig": {
//	    "thinkingConfig": {
//	      "thinkingBudget": 8192,
//	      "includeThoughts": true
//	    }
//	  }
//	}
//
// Expected output format (Gemini 3.x):
//
//	{
//	  "generationConfig": {
//	    "thinkingConfig": {
//	      "thinkingLevel": "high",
//	      "includeThoughts": true
//	    }
//	  }
//	}
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

	// Choose format based on config.Mode and model capabilities:
	// - ModeLevel: use Level format (validation will reject unsupported levels)
	// - ModeNone: use Level format if model has Levels, else Budget format
	// - ModeBudget/ModeAuto: use Budget format
	switch config.Mode {
	case thinking.ModeLevel:
		return applyLevel(body, config)
	case thinking.ModeNone:
		// ModeNone: route based on model capability (has Levels or not)
		if len(modelInfo.Thinking.Levels) > 0 {
			return applyLevel(body, config)
		}
		return applyBudget(body, config)
	default:
		return applyBudget(body, config)
	}
}

func applyLevel(body []byte, config thinking.Config) ([]byte, error) {
	return thinking.ApplyGeminiLevelFormat(body, config, thinking.GeminiDirectPrefix)
}

func applyBudget(body []byte, config thinking.Config) ([]byte, error) {
	return thinking.ApplyGeminiBudgetFormat(body, config, config.Budget, thinking.GeminiDirectPrefix)
}
