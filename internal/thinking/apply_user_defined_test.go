package thinking_test

import (
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/antigravity"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/gemini"
	"github.com/tidwall/gjson"
)

func TestApplyThinking_UserDefinedClaudePreservesAdaptiveLevel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-user-defined-claude-" + t.Name()
	modelID := "custom-claude-4-6"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{ID: modelID, UserDefined: true}})
	t.Cleanup(
		func() {
			reg.UnregisterClient(clientID)
		},
	)

	tests := []struct {
		name       string
		model      string
		body       []byte
		wantType   string
		wantEffort string
		wantBudget bool // whether thinking.budget_tokens should exist
	}{
		{
			name:       "claude adaptive effort body",
			model:      modelID,
			body:       []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`),
			wantType:   "adaptive",
			wantEffort: "high",
			wantBudget: false,
		},
		{
			name:       "suffix level preserves adaptive target",
			model:      modelID + "(high)",
			body:       []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`),
			wantType:   "adaptive",
			wantEffort: "high",
			wantBudget: false,
		},
		{
			// 未声明 adaptive 的自定义模型使用手动预算
			name:       "suffix level converts to budget",
			model:      modelID + "(high)",
			body:       []byte(`{}`),
			wantType:   "enabled",
			wantBudget: true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				out, err := thinking.ApplyThinking(tt.body, tt.model, "openai", "claude", "claude")
				if err != nil {
					t.Fatalf("ApplyThinking() error = %v", err)
				}
				if got := gjson.GetBytes(out, "thinking.type").String(); got != tt.wantType {
					t.Fatalf("thinking.type = %q, want %q, body=%s", got, tt.wantType, string(out))
				}
				if tt.wantEffort != "" &&
					gjson.GetBytes(out, "output_config.effort").String() != tt.wantEffort {
					t.Fatalf("adaptive effort differs from %q: %s", tt.wantEffort, out)
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
			},
		)
	}
}

func TestApplyThinkingUserDefinedBudgetMappingAcrossSources(t *testing.T) {
	for _, from := range []string{"claude", "gemini", "openai"} {
		for _, to := range []string{"gemini", "antigravity"} {
			for _, registered := range []bool{false, true} {
				for _, suffix := range []bool{false, true} {
					t.Run(
						fmt.Sprintf("%s-to-%s/registered=%t/suffix=%t", from, to, registered, suffix),
						func(t *testing.T) {
							model, level := "custom-budget-target", "high"
							var info *registry.ModelInfo
							if registered {
								info = &registry.ModelInfo{ID: model, UserDefined: true}
							}
							if suffix {
								model += "(high)"
								level = "low"
							}
							body := []byte(fmt.Sprintf(
								`{"other":true,"generationConfig":{"thinkingConfig":{"thinkingLevel":%q}}}`,
								level,
							))
							path := "generationConfig.thinkingConfig"
							if to == "antigravity" {
								body = []byte(fmt.Sprintf(
									`{"other":true,"request":{"generationConfig":{"thinkingConfig":`+
										`{"thinkingLevel":`+
										`%q}}}}`,
									level,
								))
								path = "request." + path
							}
							out, err := thinking.ApplyThinkingWithModelInfoAndSummary(
								body, nil, model, from, to, to, info, thinking.SummaryConfig{},
							)
							if err != nil || gjson.GetBytes(out, path+".thinkingBudget").Int() != 24576 ||
								gjson.GetBytes(out, path+".thinkingLevel").Exists() || !gjson.GetBytes(
								out, "other",
							).Bool() {
								t.Fatalf(
									"target budget or suffix precedence differs: output=%s error=%v",
									out,
									err,
								)
							}
						},
					)
				}
			}
		}
	}
}
