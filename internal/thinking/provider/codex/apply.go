// Package codex implements thinking configuration for Codex (OpenAI Responses API) models.
//
// Codex models use the reasoning.effort format with discrete levels
// (low/medium/high). This is similar to OpenAI but uses nested field
// "reasoning.effort" instead of "reasoning_effort".
// See: _bmad-output/planning-artifacts/architecture.md#Epic-8
package codex

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
)

// Applier implements thinking.ProviderApplier for Codex models.
//
// Codex-specific behavior:
//   - Output format: reasoning.effort (string: low/medium/high/xhigh)
//   - Level-only mode: no numeric budget support
//   - Some models support ZeroAllowed (gpt-5.1, gpt-5.2)
type Applier struct{}

var _ thinking.ProviderApplier = (*Applier)(nil)

// newApplier creates a new Codex thinking applier.
func newApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("codex", newApplier())
}

// Apply applies thinking configuration to Codex request body.
//
// Expected output format:
//
//	{
//	  "reasoning": {
//	    "effort": "high"
//	  }
//	}
func (a *Applier) Apply(body []byte, config thinking.Config, modelInfo *registry.ModelInfo) ([]byte, error) {
	return thinking.ApplyReasoningEffort(body, config, modelInfo, thinking.CodexEffortField)
}
