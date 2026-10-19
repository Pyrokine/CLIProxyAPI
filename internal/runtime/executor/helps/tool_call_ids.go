package helps

import (
	"encoding/base64"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const devinToolIDPrefix = "cpa_tid_v1_"

func portableToolCallID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// EncodeDevinToolCallID carries a native ID through providers requiring portable tool IDs.
func EncodeDevinToolCallID(id string) string {
	if id == "" || (portableToolCallID(id) && !strings.HasPrefix(id, devinToolIDPrefix)) {
		return id
	}
	return devinToolIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// DecodeDevinToolCallID reverses one canonical encoding layer.
func DecodeDevinToolCallID(id string) string {
	if !strings.HasPrefix(id, devinToolIDPrefix) {
		return id
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, devinToolIDPrefix))
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) {
		return id
	}
	original := string(decoded)
	if EncodeDevinToolCallID(original) != id {
		return id
	}
	return original
}

// RestoreDevinToolCallIDs uses native IDs for assistant calls and tool results on the Devin wire.
func RestoreDevinToolCallIDs(prompts []DevinPrompt) {
	for i := range prompts {
		for j := range prompts[i].ToolCalls {
			prompts[i].ToolCalls[j].ID = DecodeDevinToolCallID(prompts[i].ToolCalls[j].ID)
		}
		prompts[i].ToolCallID = DecodeDevinToolCallID(prompts[i].ToolCallID)
	}
}

// NormalizeClaudeToolCallIDs repairs invalid client tool IDs without changing legal IDs.
func NormalizeClaudeToolCallIDs(body []byte) []byte {
	type field struct {
		start int
		end   int
		id    string
	}
	var invalid []field
	used := make(map[string]bool)
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		for _, block := range message.Get("content").Array() {
			name := ""
			switch block.Get("type").String() {
			case "tool_use":
				name = "id"
			case "tool_result":
				name = "tool_use_id"
			default:
				continue
			}
			value := block.Get(name)
			if value.Type != gjson.String || value.String() == "" {
				continue
			}
			id := value.String()
			if portableToolCallID(id) {
				used[id] = true
			} else {
				invalid = append(invalid, field{start: value.Index, end: value.Index + len(value.Raw), id: id})
			}
		}
	}
	if len(invalid) == 0 {
		return body
	}
	mapped := make(map[string]string)
	out := make([]byte, 0, len(body))
	offset := 0
	for _, item := range invalid {
		id := mapped[item.id]
		if id == "" {
			id = EncodeDevinToolCallID(item.id)
			for used[id] {
				id = "_" + id
			}
			mapped[item.id] = id
			used[id] = true
		}
		out = append(out, body[offset:item.start]...)
		out = strconv.AppendQuote(out, id)
		offset = item.end
	}
	return append(out, body[offset:]...)
}
