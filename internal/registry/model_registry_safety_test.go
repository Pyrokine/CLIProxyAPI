package registry

import (
	"testing"
)

// The registry functions return direct references (not clones) for performance.
// These tests verify the returned data is correct and accessible.

func TestGetModelInfoReturnsCorrectData(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient(
		"client-1", "gemini", []*ModelInfo{
			{
				ID:          "m1",
				DisplayName: "Model One",
				Thinking:    &ThinkingSupport{Min: 1, Max: 2, Levels: []string{"low", "high"}},
			},
		},
	)

	info := r.getModelInfo("m1", "gemini")
	if info == nil {
		t.Fatal("expected model info")
	}
	if info.DisplayName != "Model One" {
		t.Fatalf("expected display name %q, got %q", "Model One", info.DisplayName)
	}
	if info.Thinking == nil || len(info.Thinking.Levels) != 2 {
		t.Fatalf("expected thinking levels, got %+v", info.Thinking)
	}
	if info.Thinking.Levels[0] != "low" || info.Thinking.Levels[1] != "high" {
		t.Fatalf("expected levels [low, high], got %v", info.Thinking.Levels)
	}
}

func TestGetModelsForClientReturnsCorrectData(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient(
		"client-1", "gemini", []*ModelInfo{
			{
				ID:          "m1",
				DisplayName: "Model One",
				Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
			},
		},
	)

	models := r.GetModelsForClient("client-1")
	if len(models) != 1 || models[0] == nil {
		t.Fatalf("expected one model, got %+v", models)
	}
	if models[0].DisplayName != "Model One" {
		t.Fatalf("expected display name %q, got %q", "Model One", models[0].DisplayName)
	}
	if models[0].Thinking == nil || len(models[0].Thinking.Levels) != 2 {
		t.Fatalf("expected thinking levels, got %+v", models[0].Thinking)
	}
}

func TestGetAvailableModelsByProviderReturnsCorrectData(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient(
		"client-1", "gemini", []*ModelInfo{
			{
				ID:          "m1",
				DisplayName: "Model One",
				Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
			},
		},
	)

	models := r.GetAvailableModelsByProvider("gemini")
	if len(models) != 1 || models[0] == nil {
		t.Fatalf("expected one model, got %+v", models)
	}
	if models[0].DisplayName != "Model One" {
		t.Fatalf("expected display name %q, got %q", "Model One", models[0].DisplayName)
	}
	if models[0].Thinking == nil || len(models[0].Thinking.Levels) != 2 {
		t.Fatalf("expected thinking levels, got %+v", models[0].Thinking)
	}
}

func TestResolveModelIDCaseInsensitive(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient(
		"client-1", "codex", []*ModelInfo{
			{ID: "TeamA/gpt-image-2"},
		},
	)

	if got := r.ResolveModelIDCaseInsensitive("teama/GPT-IMAGE-2"); got != "TeamA/gpt-image-2" {
		t.Fatalf("ResolveModelIDCaseInsensitive() = %q, want %q", got, "TeamA/gpt-image-2")
	}
	if got := r.ResolveModelIDCaseInsensitive("TeamA/gpt-image-2"); got != "TeamA/gpt-image-2" {
		t.Fatalf("exact ResolveModelIDCaseInsensitive() = %q, want exact ID", got)
	}
	if got := r.ResolveModelIDCaseInsensitive("TeamB/gpt-image-2"); got != "" {
		t.Fatalf("unknown ResolveModelIDCaseInsensitive() = %q, want empty", got)
	}
}

func TestResolveModelIDCaseInsensitiveRejectsAmbiguousIDs(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "codex", []*ModelInfo{{ID: "TeamA/gpt-image-2"}})
	r.RegisterClient("client-2", "codex", []*ModelInfo{{ID: "teama/GPT-IMAGE-2"}})

	if got := r.ResolveModelIDCaseInsensitive("TEAMA/gpt-image-2"); got != "" {
		t.Fatalf("ambiguous ResolveModelIDCaseInsensitive() = %q, want empty", got)
	}
}

func TestGetAvailableModelsReturnsSupportedParameters(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient(
		"client-1", "openai", []*ModelInfo{
			{
				ID:                  "m1",
				DisplayName:         "Model One",
				SupportedParameters: []string{"temperature", "top_p"},
			},
		},
	)

	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected one model, got %d", len(models))
	}
	params, ok := models[0]["supported_parameters"].([]string)
	if !ok || len(params) != 2 {
		t.Fatalf("expected supported_parameters slice, got %#v", models[0]["supported_parameters"])
	}
	if params[0] != "temperature" || params[1] != "top_p" {
		t.Fatalf("expected [temperature, top_p], got %v", params)
	}
}

func TestLookupModelInfoReturnsStaticDefinition(t *testing.T) {
	info := LookupModelInfo("glm-4.6")
	if info == nil || info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		t.Fatalf("expected static model with thinking levels, got %+v", info)
	}
	if info.ID != "glm-4.6" {
		t.Fatalf("expected model ID %q, got %q", "glm-4.6", info.ID)
	}
}

func TestLookupModelInfoIncludesClaudeSonnet5(t *testing.T) {
	model := LookupModelInfo("claude-sonnet-5")
	if model == nil {
		t.Fatal("expected Claude Sonnet 5 static model")
	}
	if model.Type != "claude" {
		t.Fatalf("Claude Sonnet 5 type = %q, want claude", model.Type)
	}
	if model.ContextLength != 1000000 {
		t.Fatalf("Claude Sonnet 5 context length = %d, want 1000000", model.ContextLength)
	}
	if model.MaxCompletionTokens != 128000 {
		t.Fatalf("Claude Sonnet 5 max completion tokens = %d, want 128000", model.MaxCompletionTokens)
	}
	if model.Thinking == nil || !model.Thinking.ZeroAllowed || !model.Thinking.DynamicAllowed || model.Thinking.Min != 0 || model.Thinking.Max != 0 {
		t.Fatalf("expected Claude Sonnet 5 dynamic level-only thinking with zero allowed, got %+v", model.Thinking)
	}
	expectedLevels := []string{"low", "medium", "high", "xhigh", "max"}
	if len(model.Thinking.Levels) != len(expectedLevels) {
		t.Fatalf("Claude Sonnet 5 thinking levels = %+v, want %+v", model.Thinking.Levels, expectedLevels)
	}
	for i, level := range expectedLevels {
		if model.Thinking.Levels[i] != level {
			t.Fatalf("Claude Sonnet 5 thinking levels = %+v, want %+v", model.Thinking.Levels, expectedLevels)
		}
	}
}
