package openai

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestXAIVideosPreviewModelIsSupported(t *testing.T) {
	model := "xai/grok-imagine-video-1.5-preview"

	if !isXAIVideosModel(model) {
		t.Fatalf("isXAIVideosModel(%q) = false", model)
	}
	if got := canonicalXAIVideosModel(model); got != xaiVideos15PreviewModel {
		t.Fatalf("canonicalXAIVideosModel(%q) = %q, want %q", model, got, xaiVideos15PreviewModel)
	}

	payload, meta, err := buildXAIVideosCreateRequest([]byte(`{"prompt":"make a clip","seconds":"5"}`), model)
	if err != nil {
		t.Fatalf("buildXAIVideosCreateRequest() error = %v", err)
	}
	if got := gjson.GetBytes(payload, "model").String(); got != xaiVideos15PreviewModel {
		t.Fatalf("payload model = %q, want %q; payload=%s", got, xaiVideos15PreviewModel, payload)
	}
	if meta.Model != xaiVideos15PreviewModel {
		t.Fatalf("meta.Model = %q, want %q", meta.Model, xaiVideos15PreviewModel)
	}
}
