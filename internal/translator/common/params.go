// Package common provides shared helpers for cross-format request translation.
package common

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CopyModelAndGenParams copies common generation parameters (model, max_tokens, temperature, top_p)
// from the parsed root into the output JSON string. Both OpenAI and Claude formats use the same
// field names for these parameters.
func CopyModelAndGenParams(out string, root gjson.Result, modelName string) string {
	out, _ = sjson.Set(out, "model", modelName)

	if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = sjson.Set(out, "max_tokens", maxTokens.Int())
	}

	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = sjson.Set(out, "temperature", temp.Float())
	} else if topP := root.Get("top_p"); topP.Exists() {
		out, _ = sjson.Set(out, "top_p", topP.Float())
	}

	return out
}
