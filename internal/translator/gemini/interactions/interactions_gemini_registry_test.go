package interactions

import (
	"bytes"
	"context"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsIdentityRegisteredPipeline(t *testing.T) {
	format := sdktranslator.FormatInteractions
	if !sdktranslator.HasRequestTransformer(format, format) || !sdktranslator.HasResponseTransformer(format, format) ||
		!sdktranslator.HasStreamResponseTransformer(format, format) {
		t.Fatal("Interactions identity pipeline is not registered")
	}
	request := []byte(`{"model":"original","input":"hi","future":{"preserve":true}}`)
	for _, stream := range []bool{false, true} {
		translated, err := sdktranslator.TranslateRequestChecked(context.Background(), format, format, "target", request, stream)
		if err != nil || !bytes.Equal(translated, request) {
			t.Fatalf("identity request changed: %s, %v", translated, err)
		}
	}
	response := []byte(`{"event_type":"interaction.completed","interaction":{"future":true}}`)
	var state any
	frames := sdktranslator.TranslateStream(context.Background(), format, format, "target", request, request, response, &state)
	if len(frames) != 1 || !bytes.Equal(frames[0], response) {
		t.Fatalf("identity stream changed: %q", frames)
	}
	if result := sdktranslator.TranslateNonStream(context.Background(), format, format, "target", request, request, response, &state); !bytes.Equal(result, response) {
		t.Fatalf("identity nonstream changed: %s", result)
	}
}

func TestInteractionsRegisteredFailureToGemini(t *testing.T) {
	if !sdktranslator.HasRequestTransformer(sdktranslator.FormatGemini, sdktranslator.FormatInteractions) ||
		!sdktranslator.HasStreamResponseTransformer(sdktranslator.FormatGemini, sdktranslator.FormatInteractions) {
		t.Fatal("Gemini client / Interactions provider pipeline is not registered")
	}
	for _, event := range []string{
		`{"event_type":"response.failed","error":{"code":"resource_exhausted","message":"capacity"}}`,
		`{"event_type":"interaction.failed","interaction":{"error":{"code":"429","message":"capacity"}}}`,
		`{"event_type":"response.failed","response":{"error":{"code":"429","message":"capacity"}}}`,
	} {
		var state any
		frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatGemini,
			"gemini-fixture", nil, nil, []byte(event), &state)
		if len(frames) != 1 || gjson.GetBytes(frames[0], "error.code").Int() != 429 ||
			gjson.GetBytes(frames[0], "error.message").String() != "capacity" {
			t.Fatalf("registered failed output = %q", frames)
		}
		for _, terminal := range []string{`{"event_type":"interaction.completed"}`, `{"event_type":"finish"}`} {
			frames := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatGemini,
				"gemini-fixture", nil, nil, []byte(terminal), &state)
			if len(frames) != 0 {
				t.Fatalf("failure followed by false completion: %q", frames)
			}
		}
	}
}
