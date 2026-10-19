package openai

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestXAIImagesOptionsReachRequests(t *testing.T) {
	for _, tt := range []struct {
		name        string
		body        []byte
		edit        bool
		wantRatio   string
		wantQuality string
	}{
		{"generation aspect ratio", []byte(`{"model":"grok-imagine-image","prompt":"draw","aspect_ratio":"9:20","quality":" high "}`), false, "9:20", "high"},
		{"generation size", []byte(`{"model":"grok-imagine-image","prompt":"draw","size":"20:9","quality":"low"}`), false, "20:9", "low"},
		{"edit", []byte(`{"model":"grok-imagine-image","prompt":"edit","size":"9:20","quality":"medium"}`), true, "9:20", "medium"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var request []byte
			if tt.edit {
				aspectRatio, resolution, quality, count := xaiImagesEditOptionsFromJSON(tt.body)
				request = buildXAIImagesEditRequest("grok-imagine-image", "edit", []string{"https://example.invalid/image.png"}, "url", aspectRatio, resolution, quality, count)
			} else {
				request = buildXAIImagesGenerationsRequest(tt.body, "grok-imagine-image", "url")
			}
			if got := gjson.GetBytes(request, "aspect_ratio").String(); got != tt.wantRatio {
				t.Errorf("aspect_ratio = %q, want %q; payload=%s", got, tt.wantRatio, request)
			}
			if got := gjson.GetBytes(request, "quality").String(); got != tt.wantQuality {
				t.Errorf("quality = %q, want %q; payload=%s", got, tt.wantQuality, request)
			}
		})
	}
}
