package interactions

import (
	"context"
	"fmt"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsCodexURIReferences(t *testing.T) {
	for _, field := range []string{"uri", "file_uri", "fileUri", "url"} {
		for _, mediaType := range []string{"image", "document"} {
			t.Run(mediaType+"/"+field, func(t *testing.T) {
				input := []byte(fmt.Sprintf(`{"input":[{"type":"user_input","content":[{"type":"%s","mime_type":"application/pdf","%s":"https://example.test/resource"}]}]}`, mediaType, field))
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatCodex, "codex-test", input, false)
				part := gjson.GetBytes(body, "input.0.content.0")
				valueField, partType := "file_url", "input_file"
				if mediaType == "image" {
					valueField, partType = "image_url", "input_image"
				}
				if err != nil || part.Get("type").String() != partType || part.Get(valueField).String() != "https://example.test/resource" {
					t.Fatalf("URI translation = (%s, %v)", body, err)
				}
			})
		}
	}
	t.Run("blank alias", func(t *testing.T) {
		input := []byte(`{"input":[{"type":"user_input","content":[{"type":"image","url":" ","file_uri":"","uri":"https://example.test/resource"}]}]}`)
		body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatCodex, "codex-test", input, false)
		if err != nil || gjson.GetBytes(body, "input.0.content.0.image_url").String() != "https://example.test/resource" {
			t.Fatalf("blank alias translation = (%s, %v)", body, err)
		}
	})
}

func TestInteractionsCodexInstructionRoles(t *testing.T) {
	for _, instruction := range []string{
		`{"type":"system","content":[{"type":"text","text":"note"}]}`,
		`{"type":"developer","content":[{"type":"text","text":"note"}]}`,
		`{"type":"system","steps":[{"type":"user_input","content":[{"type":"text","text":"note"}]}]}`,
		`{"type":"developer","role":"user","content":[{"type":"text","text":"note"}]}`,
	} {
		input := []byte(`{"input":[` + instruction + `]}`)
		body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, sdktranslator.FormatCodex, "codex-test", input, false)
		item := gjson.GetBytes(body, "input.0")
		if err != nil || item.Get("role").String() != "developer" || item.Get("content.0.text").String() != "note" {
			t.Fatalf("instruction translation = (%s, %v)", body, err)
		}
	}
}
