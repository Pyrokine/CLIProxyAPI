package auth

import (
	"encoding/json"
	"strconv"
	"strings"
)

// AttributeFilePriority identifies a priority inherited from a credential file.
const AttributeFilePriority = "file_priority"

// ApplyAuthPriorityMetadata copies a valid file priority into an auth record.
func ApplyAuthPriorityMetadata(auth *Auth, metadata map[string]any) {
	if auth == nil {
		return
	}
	delete(auth.Attributes, AttributeFilePriority)
	raw, ok := metadata["priority"]
	if !ok {
		return
	}
	var priority string
	switch value := raw.(type) {
	case float64:
		priority = strconv.Itoa(int(value))
	case json.Number:
		number, err := value.Float64()
		if err != nil {
			return
		}
		priority = strconv.Itoa(int(number))
	case string:
		priority = strings.TrimSpace(value)
		if _, err := strconv.Atoi(priority); err != nil {
			return
		}
	default:
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["priority"] = raw
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["priority"] = priority
	auth.Attributes[AttributeFilePriority] = "true"
}
