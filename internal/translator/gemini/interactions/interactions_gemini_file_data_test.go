package interactions

import (
	"context"
	"errors"
	"fmt"
	"testing"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestConvertInteractionsRequestToGeminiNormalizesOpenAIFileDataURL(t *testing.T) {
	input := []byte(`{"model":"gemini-3.5-flash","input":[{"type":"user_input","content":[{"type":"file","file":{"filename":"test.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]}`)

	out := convertInteractionsRequestToGemini("gemini-3.5-flash", input, false)
	inlineData := gjson.GetBytes(out, "contents.0.parts.0.inlineData")
	if got := inlineData.Get("mimeType").String(); got != "application/pdf" {
		t.Fatalf("inlineData.mimeType = %q, want application/pdf. Output: %s", got, out)
	}
	if got := inlineData.Get("data").String(); got != "JVBERi0xLjQK" {
		t.Fatalf("inlineData.data = %q, want raw base64 payload. Output: %s", got, out)
	}
}

func TestGeminiToInteractionsFileDataUserTurn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			translate := func(input string) ([]byte, error) {
				return sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatInteractions, "test-model", []byte(input), stream)
			}
			for _, tt := range []struct {
				name, data, wantType, wantURI, wantMIME string
			}{
				{"camel PDF", `{"fileData":{"mimeType":"application/pdf","fileUri":"gs://bucket/doc.pdf"}}`, "document", "gs://bucket/doc.pdf", "application/pdf"},
				{"snake video", `{"file_data":{"mime_type":"video/mp4","file_uri":"https://example.test/video.mp4"}}`, "video", "https://example.test/video.mp4", "video/mp4"},
				{"no MIME", `{"fileData":{"fileUri":"https://example.test/unknown"}}`, "document", "https://example.test/unknown", ""},
			} {
				t.Run(tt.name, func(t *testing.T) {
					body, err := translate(`{"contents":[{"role":"user","parts":[` + tt.data + `]}]}`)
					part := gjson.GetBytes(body, "input.0.content.0")
					if err != nil || part.Get("type").String() != tt.wantType || part.Get("uri").String() != tt.wantURI ||
						part.Get("mime_type").String() != tt.wantMIME {
						t.Fatalf("fileData = (%s, %v), want %s/%s/%s", body, err, tt.wantType, tt.wantURI, tt.wantMIME)
					}
				})
			}
			body, err := translate(`{"contents":[{"role":"user","parts":[{"text":"earlier"}]},{"role":"model","parts":[{"text":"response"}]},{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf"}}]},{"role":"model","parts":[{"text":"response"}]},{"role":"user","parts":[{"text":"later"}]}]}`)
			var unsupported *translatorcommon.UnsupportedPartError
			if !errors.As(err, &unsupported) || unsupported.Type != "fileData" || unsupported.StatusCode() != 400 ||
				!unsupported.IsRequestScoped() || len(body) != 0 {
				t.Fatalf("invalid fileData user turn = (%s, %v), want request-scoped 400", body, err)
			}
			legacy := convertGeminiRequestToInteractions("test-model", []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf"}}]}]}`), stream)
			if len(legacy) != 0 {
				t.Fatalf("legacy converter retained a rejected body: %q", legacy)
			}
			body, err = translate(`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf"}},{"text":"keep"}]}]}`)
			if err != nil || gjson.GetBytes(body, "input.0.content.0.text").String() != "keep" {
				t.Fatalf("mixed fileData and text = (%s, %v), want retained text", body, err)
			}
			body, err = translate(`{"contents":[{"role":"model","parts":[{"fileData":{"mimeType":"application/pdf"}}]},{"role":"user","parts":[{"text":"keep"}]}]}`)
			if err != nil || gjson.GetBytes(body, "input.0.content.0.text").String() != "keep" {
				t.Fatalf("model fileData incorrectly rejected: body=%s err=%v", body, err)
			}
		})
	}
}
