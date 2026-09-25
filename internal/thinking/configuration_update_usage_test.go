package thinking

import "testing"

func TestExtractTranslatedReasoningEffortUsesLastConfigurationUpdate(t *testing.T) {
	payload := []byte(`{"reasoning":{"effort":"xhigh"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"type":"message","role":"user","content":"hello"},{"type":"configuration_update","reasoning":{"effort":"medium"}},{"type":"configuration_update","tools":[]}]}`)
	if got := ExtractTranslatedReasoningEffort(payload, "codex"); got != "medium" {
		t.Fatalf("reasoning effort = %q, want medium", got)
	}
	if got := ExtractTranslatedReasoningEffort(payload, "openai-response"); got != "medium" {
		t.Fatalf("openai-response reasoning effort = %q, want medium", got)
	}
}

func TestExtractTranslatedReasoningEffortFallsBackToTopLevel(t *testing.T) {
	payload := []byte(`{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","tools":[]}]}`)
	if got := ExtractTranslatedReasoningEffort(payload, "codex"); got != "high" {
		t.Fatalf("reasoning effort = %q, want high", got)
	}
}
