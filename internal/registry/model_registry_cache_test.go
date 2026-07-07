package registry

import "testing"

func TestGetAvailableModelsReturnsClonedSnapshots(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One"}})

	first := r.GetAvailableModels("openai")
	if len(first) != 1 {
		t.Fatalf("expected 1 model, got %d", len(first))
	}
	first[0]["id"] = "mutated"
	first[0]["display_name"] = "Mutated"

	second := r.GetAvailableModels("openai")
	if got := second[0]["id"]; got != "m1" {
		t.Fatalf("expected cached snapshot to stay isolated, got id %v", got)
	}
	if got := second[0]["display_name"]; got != "Model One" {
		t.Fatalf("expected cached snapshot to stay isolated, got display_name %v", got)
	}
}

func TestGetAvailableModelsClaudeIncludesAnthropicSchemaFields(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient(
		"client-1", "Claude", []*ModelInfo{
			{
				ID:                  "claude-sonnet-4-6",
				OwnedBy:             "anthropic",
				Type:                "claude",
				Created:             1771372800,
				ContextLength:       200000,
				MaxCompletionTokens: 64000,
			},
			{ID: "claude-no-limits", OwnedBy: "anthropic", Type: "claude"},
		},
	)

	models := r.GetAvailableModels("claude")
	byID := make(map[string]map[string]any, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		byID[id] = model
	}

	withLimits := byID["claude-sonnet-4-6"]
	if withLimits == nil {
		t.Fatalf("expected claude-sonnet-4-6 in available models, got %v", byID)
	}
	if got := withLimits["created_at"]; got != "2026-02-18T00:00:00Z" {
		t.Fatalf("created_at = %v, want RFC3339 timestamp", got)
	}
	if got := withLimits["max_input_tokens"]; got != 200000 {
		t.Fatalf("max_input_tokens = %v, want 200000", got)
	}
	if got := withLimits["max_tokens"]; got != 64000 {
		t.Fatalf("max_tokens = %v, want 64000", got)
	}

	withDefaults := byID["claude-no-limits"]
	if withDefaults == nil {
		t.Fatalf("expected claude-no-limits in available models, got %v", byID)
	}
	if got := withDefaults["display_name"]; got != "claude-no-limits" {
		t.Fatalf("display_name = %v, want model id fallback", got)
	}
	if got := withDefaults["max_input_tokens"]; got != defaultClaudeMaxInputTokens {
		t.Fatalf("max_input_tokens = %v, want %d", got, defaultClaudeMaxInputTokens)
	}
	if got := withDefaults["max_tokens"]; got != defaultClaudeMaxOutputTokens {
		t.Fatalf("max_tokens = %v, want %d", got, defaultClaudeMaxOutputTokens)
	}
}

func TestGetAvailableModelsInvalidatesCacheOnRegistryChanges(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One"}})

	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if got := models[0]["display_name"]; got != "Model One" {
		t.Fatalf("expected initial display_name Model One, got %v", got)
	}

	r.RegisterClient(
		"client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One Updated"}},
	)
	models = r.GetAvailableModels("openai")
	if got := models[0]["display_name"]; got != "Model One Updated" {
		t.Fatalf("expected updated display_name after cache invalidation, got %v", got)
	}

	r.SuspendClientModel("client-1", "m1", "manual")
	models = r.GetAvailableModels("openai")
	if len(models) != 0 {
		t.Fatalf("expected no available models after suspension, got %d", len(models))
	}

	r.ResumeClientModel("client-1", "m1")
	models = r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected model to reappear after resume, got %d", len(models))
	}
}
