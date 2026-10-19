package chat_completions

import (
	"context"
	"fmt"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestRegisteredOpenAIToGeminiAudioAndPrefill(t *testing.T) {
	for _, tc := range []struct {
		format string
		mime   string
	}{
		{"", "audio/wav"},
		{"wav", "audio/wav"},
		{"mp3", "audio/mpeg"},
		{"ogg", "audio/ogg"},
		{"flac", "audio/flac"},
		{"aac", "audio/aac"},
		{"webm", "audio/webm"},
		{"pcm16", "audio/pcm"},
		{"g711_ulaw", "audio/basic"},
		{"g711_alaw", "audio/basic"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("audio/%s/%t", tc.format, stream), func(t *testing.T) {
				request := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"text","text":"before"},{"type":"input_audio","input_audio":{"format":%q,"data":"SUQzBA=="}},{"type":"text","text":"after"}]}]}`, tc.format))
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatGemini, "gemini-test", request, stream)
				if err != nil {
					t.Fatal(err)
				}
				parts := gjson.GetBytes(body, "contents.0.parts").Array()
				if len(parts) != 3 || parts[0].Get("text").String() != "before" || parts[2].Get("text").String() != "after" {
					t.Fatalf("audio or adjacent text lost: %s", body)
				}
				if parts[1].Get("inlineData.mime_type").String() != tc.mime || parts[1].Get("inlineData.data").String() != "SUQzBA==" {
					t.Fatalf("audio conversion mismatch: %s", body)
				}
			})
		}
	}
	t.Run("assistant prefill", func(t *testing.T) {
		request := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"previous answer"}]}`)
		body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatGemini, "gemini-test", request, false)
		if err != nil {
			t.Fatal(err)
		}
		contents := gjson.GetBytes(body, "contents").Array()
		if len(contents) != 1 || contents[0].Get("role").String() != "user" || contents[0].Get("parts.0.text").String() != "hello" {
			t.Fatalf("assistant prefill retained: %s", body)
		}
	})
}
