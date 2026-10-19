package claude

import (
	"context"
	"errors"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeToCodexImageSourcesReachRegisteredTranslator(t *testing.T) {
	cases := []struct {
		name   string
		source string
		field  string
		want   string
	}{
		{"base64", `{"type":"base64","media_type":"image/png","data":"aGVsbG8="}`, "image_url", "data:image/png;base64,aGVsbG8="},
		{"legacy base64", `{"mime_type":"image/jpeg","base64":"aGVsbG8="}`, "image_url", "data:image/jpeg;base64,aGVsbG8="},
		{"default MIME", `{"data":"aGVsbG8="}`, "image_url", "data:application/octet-stream;base64,aGVsbG8="},
		{"HTTP URL", `{"type":"url","url":"http://example.invalid/a.png"}`, "image_url", "http://example.invalid/a.png"},
		{"trimmed HTTPS URL", `{"type":"url","url":" https://example.invalid/a.png "}`, "image_url", "https://example.invalid/a.png"},
		{"file ID", `{"type":"file","file_id":"file-fixture"}`, "file_id", "file-fixture"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			image := `{"type":"image","source":` + tc.source + `}`
			for _, toolResult := range []bool{false, true} {
				content := image
				if toolResult {
					content = `{"type":"tool_result","tool_use_id":"call_fixture","content":[` + image + `]}`
				}
				payload := []byte(`{"messages":[{"role":"user","content":"history"},{"role":"assistant","content":"answer"},{"role":"user","content":[` + content + `]}]}`)
				envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatCodex, sdktranslator.RequestEnvelope{Model: "gpt-fixture", Body: payload})
				if envelope.Err != nil {
					t.Fatalf("toolResult=%v: %v", toolResult, envelope.Err)
				}
				path := "input.2.content.0"
				if toolResult {
					path = "input.2.output.0"
				}
				part := gjson.GetBytes(envelope.Body, path)
				if part.Get("type").String() != "input_image" || part.Get(tc.field).String() != tc.want {
					t.Fatalf("toolResult=%v: image = %s, body = %s", toolResult, part.Raw, envelope.Body)
				}
			}
		})
	}
}

func TestClaudeToCodexEmptiedImageTurnIsRejected(t *testing.T) {
	sources := map[string]string{
		"missing source": `null`,
		"unknown source": `{"type":"unknown"}`,
		"empty base64":   `{"type":"base64","data":""}`,
		"blank base64":   `{"type":"base64","data":" "}`,
		"numeric base64": `{"type":"base64","data":42}`,
		"empty URL":      `{"type":"url","url":""}`,
		"FTP URL":        `{"type":"url","url":"ftp://example.invalid/a.png"}`,
		"hostless URL":   `{"type":"url","url":"https:///a.png"}`,
		"numeric URL":    `{"type":"url","url":42}`,
		"missing file":   `{"type":"file"}`,
		"blank file":     `{"type":"file","file_id":" "}`,
		"numeric file":   `{"type":"file","file_id":42}`,
	}
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			payload := []byte(`{"messages":[{"role":"user","content":"history"},{"role":"assistant","content":"answer"},{"role":"system","content":"reminder"},{"role":"user","content":[{"type":"text","text":" "},{"type":"image","source":` + source + `}]}]}`)
			envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatCodex, sdktranslator.RequestEnvelope{Model: "gpt-fixture", Body: payload})
			var unsupported *translatorcommon.UnsupportedPartError
			if !errors.As(envelope.Err, &unsupported) || unsupported.Type != "image" || unsupported.StatusCode() != 400 || len(envelope.Body) != 0 {
				t.Fatalf("envelope error = %v, body = %s", envelope.Err, envelope.Body)
			}
			body, err := ConvertClaudeRequestToCodexWithCompatReturningError("compat-fixture", payload, false)
			if !errors.As(err, &unsupported) || !gjson.ValidBytes(body) {
				t.Fatalf("compat error = %v, body = %s", err, body)
			}
			for _, convert := range []func(string, []byte, bool) []byte{ConvertClaudeRequestToCodex, ConvertClaudeRequestToCodexWithCompat} {
				if body := convert("fixture", payload, false); len(body) != 0 {
					t.Fatalf("byte entrypoint returned a rejected request: %s", body)
				}
			}
		})
	}
}

func TestClaudeToCodexAttachmentDoesNotEraseAdjacentText(t *testing.T) {
	for _, partType := range []string{"image", "document", "container_upload"} {
		t.Run(partType, func(t *testing.T) {
			payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"keep"},{"type":"` + partType + `","source":{"type":"file"}}]}]}`)
			envelope := sdktranslator.TranslateRequestEnvelope(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatCodex, sdktranslator.RequestEnvelope{Model: "gpt-fixture", Body: payload})
			if envelope.Err != nil || gjson.GetBytes(envelope.Body, "input.0.content.0.text").String() != "keep" {
				t.Fatalf("error = %v, body = %s", envelope.Err, envelope.Body)
			}
		})
	}
}
