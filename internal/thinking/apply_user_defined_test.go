package thinking_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	"github.com/tidwall/gjson"
)

func TestApplyThinking_UserDefinedClaudePreservesAdaptiveLevel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-user-defined-claude-" + t.Name()
	modelID := "custom-claude-4-6"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{ID: modelID, UserDefined: true}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	tests := []struct {
		name       string
		model      string
		body       []byte
		wantType   string
		wantBudget bool // whether thinking.budget_tokens should exist
	}{
		{
			name:       "claude adaptive effort body",
			model:      modelID,
			body:       []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`),
			wantType:   "adaptive",
			wantBudget: false,
		},
		{
			// Suffix level is converted to budget-based thinking for Claude.
			// normalizeUserDefinedConfig converts Level→Budget when toFormat is budget-based (claude).
			name:       "suffix level converts to budget",
			model:      modelID + "(high)",
			body:       []byte(`{}`),
			wantType:   "enabled",
			wantBudget: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := thinking.ApplyThinking(tt.body, tt.model, "openai", "claude", "claude")
			if err != nil {
				t.Fatalf("ApplyThinking() error = %v", err)
			}
			if got := gjson.GetBytes(out, "thinking.type").String(); got != tt.wantType {
				t.Fatalf("thinking.type = %q, want %q, body=%s", got, tt.wantType, string(out))
			}
			if tt.wantBudget {
				if !gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
					t.Fatalf("thinking.budget_tokens should exist, body=%s", string(out))
				}
				if got := gjson.GetBytes(out, "thinking.budget_tokens").Int(); got != 24576 {
					t.Fatalf("thinking.budget_tokens = %d, want 24576, body=%s", got, string(out))
				}
			} else {
				if gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
					t.Fatalf("thinking.budget_tokens should be removed, body=%s", string(out))
				}
			}
		})
	}
}
