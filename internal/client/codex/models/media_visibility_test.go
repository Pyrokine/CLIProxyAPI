package models

import "testing"

func TestLatestMediaModelsStayOutOfCodexTextSelection(t *testing.T) {
	ids := []string{
		"gpt-image-2.6-flare", "gpt-image-2.6-sunburst", "openai/gpt-image-2.6-flare",
		"grok-imagine-video-1.5-lite", "xai/grok-imagine-video-1.5-lite",
		"muse-image-1.0", "meta/muse-image-1.0", "muse-voice-transcribe-1.0",
	}
	input := []map[string]any{{"id": "gpt-6.1-sol"}}
	for _, id := range ids {
		input = append(input, map[string]any{"id": id})
	}
	models := codexClientModelsBySlug(t, input)
	for _, id := range ids {
		entry := models[id]
		if entry == nil || !isCodexClientNonTextModel(id) || entry["visibility"] != "hide" {
			t.Fatalf("media model %s remained in text selection: %+v", id, entry)
		}
	}
	if models["gpt-6.1-sol"]["visibility"] != "list" {
		t.Fatal("text model visibility changed")
	}
}
