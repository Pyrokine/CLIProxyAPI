package helps

import "testing"

func TestIsClaudeServerToolType(t *testing.T) {
	for _, toolType := range []string{
		"advisor_20260301",
		"agent_toolset_20260401",
		"bash_20260310",
		"code_execution_20250825",
		"computer_20250124",
		"memory_20250818",
		"text_editor_20250728",
		"tool_search_tool_bm25_20251119",
		"web_fetch_20260209",
		"web_search_20260209",
		"  ADVISOR_20260301  ",
	} {
		if !IsClaudeServerToolType(toolType) {
			t.Errorf("IsClaudeServerToolType(%q) = false, want true", toolType)
		}
	}

	for _, toolType := range []string{"", "custom", "advisor", "agent_toolset", "my_advisor_20260301"} {
		if IsClaudeServerToolType(toolType) {
			t.Errorf("IsClaudeServerToolType(%q) = true, want false", toolType)
		}
	}
}

func TestAugmentClaudeBuiltinToolRegistryIncludesTypedServerTools(t *testing.T) {
	body := []byte(`{"tools":[
		{"type":"advisor_20260301","name":"advisor"},
		{"type":"agent_toolset_20260401","name":"agent_toolset"},
		{"type":"web_search_20260209","name":"web_search"},
		{"type":"custom","name":"caller_tool"}
	]}`)

	registry := AugmentClaudeBuiltinToolRegistry(body, nil)
	for _, name := range []string{"advisor", "agent_toolset", "web_search", "code_execution", "text_editor", "computer"} {
		if !registry[name] {
			t.Errorf("registry[%q] = false, want true", name)
		}
	}
	if registry["caller_tool"] {
		t.Fatal("custom caller tool was added to the built-in registry")
	}
}
