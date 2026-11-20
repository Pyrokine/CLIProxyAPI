package common

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeCodexStreamOptions keeps reasoning_summary_delivery without normalizing other request fields.
func NormalizeCodexStreamOptions(rawJSON []byte) []byte {
	streamOptions := gjson.GetBytes(rawJSON, "stream_options")
	if !streamOptions.Exists() {
		return rawJSON
	}
	if delivery := streamOptions.Get("reasoning_summary_delivery"); delivery.Exists() {
		allowed, _ := sjson.SetRawBytes([]byte(`{}`), "reasoning_summary_delivery", []byte(delivery.Raw))
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "stream_options", allowed)
		return rawJSON
	}
	updated, err := sjson.DeleteBytes(rawJSON, "stream_options")
	if err == nil {
		return updated
	}
	return rawJSON
}

// IsGeminiThoughtPart reports whether a Gemini request part contains hidden
// model thought that must not be exposed as ordinary message content.
func IsGeminiThoughtPart(part gjson.Result) bool {
	return part.Get("thought").Bool()
}

// ContentHasGeminiFunctionResponse reports whether a Gemini content contains a
// function response in either supported field spelling.
func ContentHasGeminiFunctionResponse(content []byte) bool {
	hasFunctionResponse := false
	gjson.GetBytes(content, "parts").ForEach(
		func(_, part gjson.Result) bool {
			if part.Get("functionResponse").Exists() || part.Get("function_response").Exists() {
				hasFunctionResponse = true
				return false
			}
			return true
		},
	)
	return hasFunctionResponse
}

// GeminiToolCallID extracts a gateway-provided tool identity from a Gemini
// functionCall or functionResponse object.
func GeminiToolCallID(node gjson.Result) string {
	if id := strings.TrimSpace(node.Get("id").String()); id != "" {
		return id
	}
	if callID := strings.TrimSpace(node.Get("call_id").String()); callID != "" {
		return callID
	}
	return strings.TrimSpace(node.Get("callId").String())
}

// GeminiToolCallQueue matches Gemini function responses by explicit ID, then
// function name, then request order when the response has no name.
type GeminiToolCallQueue struct {
	ids       []string
	nameByID  map[string]string
	idsByName map[string][]string
}

func (queue *GeminiToolCallQueue) Add(name, id string) {
	if id == "" {
		return
	}
	if queue.nameByID == nil {
		queue.nameByID = make(map[string]string)
		queue.idsByName = make(map[string][]string)
	}
	queue.ids = append(queue.ids, id)
	queue.nameByID[id] = name
	queue.idsByName[name] = append(queue.idsByName[name], id)
}

func (queue *GeminiToolCallQueue) Match(name, explicitID string) string {
	if explicitID != "" {
		queue.remove(explicitID)
		return explicitID
	}
	if ids := queue.idsByName[name]; name != "" && len(ids) > 0 {
		id := ids[0]
		queue.remove(id)
		return id
	}
	if name != "" || len(queue.ids) == 0 {
		return ""
	}
	id := queue.ids[0]
	queue.remove(id)
	return id
}

// PendingIDs returns the unmatched call IDs in request order. The returned
// slice is detached so callers can preserve pairings across interrupted turns.
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (queue *GeminiToolCallQueue) PendingIDs() []string {
	return append([]string(nil), queue.ids...)
}

func (queue *GeminiToolCallQueue) remove(id string) {
	queue.ids = removeFirstString(queue.ids, id)
	name, ok := queue.nameByID[id]
	if !ok {
		return
	}
	queue.idsByName[name] = removeFirstString(queue.idsByName[name], id)
	delete(queue.nameByID, id)
}

func removeFirstString(values []string, target string) []string {
	for index, value := range values {
		if value == target {
			return append(values[:index], values[index+1:]...)
		}
	}
	return values
}

// DeduplicateToolResultsByRawID keeps the first result position for each raw
// call ID while replacing its value with the final result. Empty IDs and items
// that are not tool results are preserved.
func DeduplicateToolResultsByRawID(
	items []gjson.Result,
	rawID func(gjson.Result) (string, bool),
) []gjson.Result {
	if len(items) == 0 || rawID == nil {
		return items
	}

	lastByID := make(map[string]gjson.Result)
	for _, item := range items {
		if id, ok := rawID(item); ok && id != "" {
			lastByID[id] = item
		}
	}

	seen := make(map[string]struct{}, len(lastByID))
	deduplicated := make([]gjson.Result, 0, len(items))
	for _, item := range items {
		id, ok := rawID(item)
		if !ok || id == "" {
			deduplicated = append(deduplicated, item)
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		deduplicated = append(deduplicated, lastByID[id])
	}
	return deduplicated
}

// RequestModelName returns the model from the original request and falls back
// to the translated request when the original request has no usable model.
func RequestModelName(originalRequestRawJSON, requestRawJSON []byte) string {
	for _, rawJSON := range [][]byte{originalRequestRawJSON, requestRawJSON} {
		if modelName := requestModelName(rawJSON); modelName != "" {
			return modelName
		}
	}
	return ""
}

func requestModelName(rawJSON []byte) string {
	if len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
		return ""
	}

	root := gjson.ParseBytes(rawJSON)
	for _, path := range []string{"model", "request.model"} {
		model := root.Get(path)
		if model.Type == gjson.String && strings.TrimSpace(model.String()) != "" {
			return model.String()
		}
	}
	return ""
}
