package registry

import (
	"slices"
	"testing"
)

func TestCurrentModelRefreshKeepsRetiredIDsLookupOnly(t *testing.T) {
	for _, test := range []struct{ channel, id string }{
		{"claude", "claude-opus-4-20250514"},
		{"claude", "claude-3-5-haiku-20241022"},
		{"vertex", "gemini-2.5-pro"},
		{"vertex", "imagen-4.0-generate-001"},
		{"gemini", "gemini-3.1-flash-lite-preview"},
		{"qwen", "vision-model"},
		{"iflow", "iflow-rome-30ba3b"},
		{"codex", "gpt-5.5"},
	} {
		if findModelByID(GetEmbeddedStaticModelDefinitionsByChannel(test.channel), test.id) != nil {
			t.Errorf("retired %s/%s remains in defaults", test.channel, test.id)
		}
		if model := LookupStaticModelInfoByChannel(test.id, test.channel); model == nil || model.ID != test.id {
			t.Errorf("historical %s/%s lookup = %+v", test.channel, test.id, model)
		}
	}
	if models := GetIFlowModels(); len(models) != 0 {
		t.Fatalf("retired iFlow service models = %+v", models)
	}
	qwen := GetQwenModels()
	if len(qwen) != 1 || qwen[0].ID != "coder-model" || qwen[0].DisplayName != "Qwen 3.7 Max" ||
		!slices.Contains(qwen[0].SupportedInputModalities, "image") {
		t.Fatalf("current Qwen OAuth catalog = %+v", qwen)
	}
}

func TestCurrentClaudeAndKimiModelCapabilities(t *testing.T) {
	for _, id := range []string{"claude-fable-5-2", "claude-haiku-5-5"} {
		model := LookupStaticModelInfoByChannel(id, "claude")
		if model == nil || model.ContextLength != 1000000 || model.MaxCompletionTokens != 128000 ||
			model.Thinking == nil || model.Thinking.Min != 0 || model.Thinking.Max != 0 ||
			!model.Thinking.DynamicAllowed || model.Thinking.ZeroAllowed != (id == "claude-haiku-5-5") {
			t.Fatalf("current Claude %s = %+v", id, model)
		}
	}
	for _, id := range []string{"kimi-k3.1-pro", "kimi-k3.1-pro-highspeed"} {
		model := LookupStaticModelInfoByChannel(id, "kimi")
		if model == nil || model.ContextLength != 1048576 || model.MaxCompletionTokens != 0 ||
			model.Thinking == nil || model.Thinking.ZeroAllowed ||
			!slices.Equal(model.Thinking.Levels, []string{"low", "high", "max"}) {
			t.Fatalf("current Kimi Coding %s = %+v", id, model)
		}
	}
	pro := LookupStaticModelInfo("deepseek-v4-pro")
	if pro == nil || !slices.Equal(pro.SupportedInputModalities, []string{"text"}) {
		t.Fatalf("DeepSeek V4 Pro input = %+v", pro)
	}
}

func TestCurrentGeminiGenerateContentUsesLevelThinking(t *testing.T) {
	for _, channel := range []string{"gemini", "aistudio", "vertex"} {
		for _, id := range []string{
			"gemini-4-carbon", "gemini-4-argon", "gemini-4-hydrogen", "gemini-nano-banana-2.1",
		} {
			model := LookupStaticModelInfoByChannel(id, channel)
			if model == nil || model.Thinking == nil || model.Thinking.Min != 0 || model.Thinking.Max != 0 ||
				model.Thinking.DynamicAllowed || model.Thinking.ZeroAllowed || len(model.Thinking.Levels) == 0 {
				t.Fatalf("%s/%s thinking = %+v", channel, id, model)
			}
			if id == "gemini-4-carbon" || id == "gemini-4-argon" {
				if model.MaxCompletionTokens != 32768 {
					t.Errorf("%s/%s REST output limit = %d", channel, id, model.MaxCompletionTokens)
				}
			}
		}
	}
}

func TestCurrentCodexPaidModelsAndFreeMediaBoundary(t *testing.T) {
	for _, getter := range []func() []*ModelInfo{GetCodexTeamModels, GetCodexPlusModels, GetCodexProModels} {
		for _, id := range []string{"gpt-6.2-astra", "gpt-6.2-sol", "gpt-6.1-terra"} {
			model := findModelByID(getter(), id)
			if model == nil || !model.SupportConfigurationUpdate || model.Thinking == nil ||
				slices.Contains(model.Thinking.Levels, "none") {
				t.Fatalf("paid Codex %s = %+v", id, model)
			}
		}
	}
	for _, model := range GetCodexFreeModels() {
		if IsCodexImageModel(model.ID) || model.ID == "gpt-6.2-astra" || model.ID == "gpt-6.2-sol" || model.ID == "gpt-6.1-terra" {
			t.Errorf("paid capability exposed in Free catalog: %s", model.ID)
		}
	}
}
