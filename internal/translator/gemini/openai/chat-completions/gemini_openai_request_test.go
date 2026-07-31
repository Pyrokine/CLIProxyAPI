package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToGeminiMapsMaxTokens(t *testing.T) {
	input := []byte(`{"model":"gemini-3-flash","max_tokens":321,"messages":[{"role":"user","content":"hi"}]}`)

	out := ConvertOpenAIRequestToGemini("gemini-3-flash", input, false)

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 321 {
		t.Fatalf("maxOutputTokens = %d, want 321; payload=%s", got, out)
	}
}

func TestConvertOpenAIRequestToGeminiMapsMaxCompletionTokens(t *testing.T) {
	input := []byte(`{"model":"gemini-3-flash","max_completion_tokens":654,"messages":[{"role":"user","content":"hi"}]}`)

	out := ConvertOpenAIRequestToGemini("gemini-3-flash", input, false)

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 654 {
		t.Fatalf("maxOutputTokens = %d, want 654; payload=%s", got, out)
	}
}

func TestConvertOpenAIRequestToGeminiPreservesVideoURL(t *testing.T) {
	input := []byte(`{
		"model":"gemini-3-flash",
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAAIGZ0eXBtcDQy"}},
					{"type":"text","text":"Describe the video"}
				]
			}
		]
	}`)

	out := ConvertOpenAIRequestToGemini("gemini-3-flash", input, false)

	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.mime_type").String(); got != "video/mp4" {
		t.Fatalf("video mime = %q, want video/mp4; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.data").String(); got != "AAAAIGZ0eXBtcDQy" {
		t.Fatalf("video data = %q, want base64 payload; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.1.text").String(); got != "Describe the video" {
		t.Fatalf("text part = %q, want prompt text; payload=%s", got, out)
	}
}
