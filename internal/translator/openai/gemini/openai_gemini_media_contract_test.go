package gemini_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/gemini"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/gemini-cli"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiInlineMediaAcrossRegisteredOpenAIEntries(t *testing.T) {
	media := []struct {
		name     string
		mime     string
		kind     string
		format   string
		filename string
	}{
		{name: "image", mime: "image/png", kind: "image_url"},
		{name: "wav", mime: "audio/wav", kind: "input_audio", format: "wav"},
		{name: "wave_alias", mime: "audio/x-wav", kind: "input_audio", format: "wav"},
		{name: "mp3", mime: "audio/mpeg", kind: "input_audio", format: "mp3"},
		{name: "opus", mime: "audio/ogg", kind: "input_audio", format: "opus"},
		{name: "pcm", mime: "audio/l16", kind: "input_audio", format: "pcm16"},
		{name: "flac", mime: "audio/flac", kind: "input_audio", format: "flac"},
		{name: "video", mime: "video/mp4", kind: "video_url"},
		{name: "pdf", mime: "application/pdf", kind: "file", filename: "document.pdf"},
		{name: "text", mime: "text/plain", kind: "file", filename: "document.txt"},
		{name: "missing_mime", kind: "file", filename: "document.bin"},
	}
	const data = "aGVsbG8="
	for _, source := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
		for _, placement := range []string{"systemInstruction", "system_instruction", "user", "model"} {
			for _, inlineKey := range []string{"inlineData", "inline_data"} {
				for _, mimeKey := range []string{"mimeType", "mime_type"} {
					for _, stream := range []bool{false, true} {
						for _, item := range media {
							name := fmt.Sprintf("%s/%s/%s/%s/stream_%t/%s", source, placement, inlineKey, mimeKey, stream, item.name)
							t.Run(name, func(t *testing.T) {
								inline := map[string]any{"data": data}
								if item.mime != "" {
									inline[mimeKey] = item.mime
								}
								parts := []any{map[string]any{"text": "before"}, map[string]any{inlineKey: inline}, map[string]any{"text": "after"}}
								request := map[string]any{}
								role := placement
								if placement == "user" || placement == "model" {
									request["contents"] = []any{map[string]any{"role": placement, "parts": parts}}
									if placement == "model" {
										role = "assistant"
									}
								} else {
									request[placement] = map[string]any{"parts": parts}
									role = "system"
								}
								if source == sdktranslator.FormatGeminiCLI {
									request = map[string]any{"request": request}
								}
								input, err := json.Marshal(request)
								if err != nil {
									t.Fatal(err)
								}
								body, err := sdktranslator.TranslateRequestChecked(context.Background(), source, sdktranslator.FormatOpenAI, "gpt-test", input, stream)
								if err != nil {
									t.Fatal(err)
								}
								out := gjson.ParseBytes(body)
								if out.Get("model").String() != "gpt-test" || out.Get("stream").Bool() != stream || out.Get("messages.#").Int() != 1 || out.Get("messages.0.role").String() != role {
									t.Fatalf("unexpected request envelope: %s", body)
								}
								content := out.Get("messages.0.content").Array()
								if len(content) != 3 || content[0].Get("text").String() != "before" || content[2].Get("text").String() != "after" {
									t.Fatalf("media or adjacent text changed: %s", body)
								}
								part := content[1]
								if part.Get("type").String() != item.kind {
									t.Fatalf("media type = %q, want %q: %s", part.Get("type").String(), item.kind, body)
								}
								mimeType := item.mime
								if mimeType == "" {
									mimeType = "application/octet-stream"
								}
								dataURL := "data:" + mimeType + ";base64," + data
								switch item.kind {
								case "image_url", "video_url":
									if part.Get(item.kind+".url").String() != dataURL {
										t.Fatalf("media URL changed: %s", body)
									}
								case "input_audio":
									if part.Get("input_audio.data").String() != data || part.Get("input_audio.format").String() != item.format {
										t.Fatalf("audio data or format changed: %s", body)
									}
								case "file":
									if part.Get("file.file_data").String() != dataURL || part.Get("file.filename").String() != item.filename {
										t.Fatalf("file data or filename changed: %s", body)
									}
								}
							})
						}
					}
				}
			}
		}
	}
}
