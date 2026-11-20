package common

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAIMessageContentWithImages includes generated image parts when replaying an assistant message.
func OpenAIMessageContentWithImages(message gjson.Result) gjson.Result {
	content := message.Get("content")
	images := message.Get("images")
	if message.Get("role").String() != "assistant" || !images.IsArray() || len(images.Array()) == 0 {
		return content
	}
	var parts [][]byte
	if content.IsArray() {
		for _, part := range content.Array() {
			parts = append(parts, []byte(part.Raw))
		}
	} else if content.Type == gjson.String && content.String() != "" {
		part, _ := sjson.SetBytes([]byte(`{"type":"text","text":""}`), "text", content.String())
		parts = append(parts, part)
	}
	for _, image := range images.Array() {
		parts = append(parts, []byte(image.Raw))
	}
	return gjson.ParseBytes(JoinRawArray(parts))
}
