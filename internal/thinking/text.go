package thinking

import (
	"github.com/tidwall/gjson"
)

// GetThinkingText extracts the thinking text from a content part.
// It accepts a string in the thinking or text field, a thinking object with
// nested text, and Gemini content that marks text as a thought.
func GetThinkingText(part gjson.Result) string {
	// Try direct text field first (Gemini-style)
	if text := part.Get("text"); text.Exists() && text.Type == gjson.String {
		return text.String()
	}

	// Try thinking field
	thinkingField := part.Get("thinking")
	if !thinkingField.Exists() {
		return ""
	}

	// thinking is a string
	if thinkingField.Type == gjson.String {
		return thinkingField.String()
	}

	// thinking is an object with inner text/thinking
	if thinkingField.IsObject() {
		if inner := thinkingField.Get("text"); inner.Exists() && inner.Type == gjson.String {
			return inner.String()
		}
		if inner := thinkingField.Get("thinking"); inner.Exists() && inner.Type == gjson.String {
			return inner.String()
		}
	}

	return ""
}
