package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

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
