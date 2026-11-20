package responses

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func geminiResponsesImageItem(part gjson.Result, id string) []byte {
	inline := part.Get("inlineData")
	if !inline.Exists() {
		inline = part.Get("inline_data")
	}
	mime := inline.Get("mimeType").String()
	if mime == "" {
		mime = inline.Get("mime_type").String()
	}
	data := inline.Get("data").String()
	if !strings.HasPrefix(mime, "image/") || data == "" || part.Get("thought").Bool() {
		return nil
	}
	format := strings.TrimPrefix(mime, "image/")
	if format == "svg+xml" {
		format = "svg"
	}
	item := []byte(`{"type":"image_generation_call","id":"","status":"completed","result":"","output_format":""}`)
	item, _ = sjson.SetBytes(item, "id", id)
	item, _ = sjson.SetBytes(item, "result", data)
	item, _ = sjson.SetBytes(item, "output_format", format)
	if signature := firstNonEmptyMediaValue(
		part.Get("thoughtSignature").String(), part.Get("thought_signature").String(),
	); signature != "" {
		item, _ = sjson.SetBytes(item, "thought_signature", signature)
	}
	return item
}
