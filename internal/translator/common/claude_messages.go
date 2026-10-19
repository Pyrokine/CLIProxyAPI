package common

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeMessageAccumulator groups consecutive Claude messages by role.
type ClaudeMessageAccumulator struct {
	messages     [][]byte
	role         string
	content      [][]byte
	toolUseParts [][]byte
}

// NewClaudeMessageAccumulator creates an accumulator sized for the expected messages.
func NewClaudeMessageAccumulator(capacity int) *ClaudeMessageAccumulator {
	return &ClaudeMessageAccumulator{
		messages: NewRawArrayItems(int64(capacity)),
	}
}

// Append adds one Claude-shaped message to the current role turn.
func (a *ClaudeMessageAccumulator) Append(message []byte) {
	if len(message) == 0 {
		return
	}
	root := gjson.ParseBytes(message)
	role := root.Get("role").String()
	if role != "user" && role != "assistant" {
		return
	}
	parts := claudeMessageContentParts(root.Get("content"))
	if len(parts) == 0 {
		return
	}
	if a.role != "" && a.role != role {
		a.Flush()
	}
	a.role = role
	for _, part := range parts {
		if role == "assistant" && gjson.GetBytes(part, "type").String() == "tool_use" {
			a.toolUseParts = append(a.toolUseParts, part)
			continue
		}
		a.content = append(a.content, part)
	}
}

// Flush closes the current role turn while keeping accumulated messages.
func (a *ClaudeMessageAccumulator) Flush() {
	if a.role == "" {
		return
	}
	parts := a.content
	if len(a.toolUseParts) > 0 {
		combined := make([][]byte, 0, len(a.content)+len(a.toolUseParts))
		combined = append(combined, a.content...)
		combined = append(combined, a.toolUseParts...)
		parts = combined
	}
	if len(parts) > 0 {
		message := []byte(`{"role":"","content":[]}`)
		message, _ = sjson.SetBytes(message, "role", a.role)
		message, _ = sjson.SetRawBytes(message, "content", JoinRawArray(parts))
		a.messages = append(a.messages, message)
	}
	a.role = ""
	a.content = nil
	a.toolUseParts = nil
}

// Messages flushes the final turn and returns all accumulated messages.
func (a *ClaudeMessageAccumulator) Messages() [][]byte {
	a.Flush()
	return a.messages
}

// AlignClaudeToolResults orders matched tool_result blocks by the preceding
// tool_use IDs. Partial matches are allowed, and only matched result slots are
// replaced, so text, image, and unmatched result blocks keep their positions.
func AlignClaudeToolResults(content gjson.Result, toolUseIDs []string) gjson.Result {
	return alignToolResults(content, toolUseIDs, "type", "tool_result", "tool_use_id")
}

// AlignGeminiFunctionResponses 按调用 ID 排列结果，保留文本、图片和未匹配结果的位置
func AlignGeminiFunctionResponses(parts gjson.Result, toolUseIDs []string) gjson.Result {
	return alignToolResults(parts, toolUseIDs, "functionResponse", "", "functionResponse.id")
}

func alignToolResults(content gjson.Result, toolUseIDs []string, typePath, typeName, idPath string) gjson.Result {
	if !content.IsArray() || len(toolUseIDs) == 0 {
		return content
	}

	orderByID := make(map[string]int, len(toolUseIDs))
	for index, toolUseID := range toolUseIDs {
		if toolUseID == "" {
			continue
		}
		if _, exists := orderByID[toolUseID]; !exists {
			orderByID[toolUseID] = index
		}
	}

	type matchedResult struct {
		part  gjson.Result
		order int
	}
	parts := content.Array()
	matchedSlots := make([]int, 0, len(parts))
	matchedResults := make([]matchedResult, 0, len(parts))
	for index, part := range parts {
		resultType := part.Get(typePath)
		if !resultType.Exists() || typeName != "" && resultType.String() != typeName {
			continue
		}
		order, exists := orderByID[part.Get(idPath).String()]
		if !exists {
			continue
		}
		matchedSlots = append(matchedSlots, index)
		matchedResults = append(matchedResults, matchedResult{part: part, order: order})
	}
	if len(matchedResults) < 2 {
		return content
	}

	sort.SliceStable(matchedResults, func(left, right int) bool {
		return matchedResults[left].order < matchedResults[right].order
	})

	ordered := make([][]byte, len(parts))
	for index, part := range parts {
		ordered[index] = []byte(part.Raw)
	}
	for index, slot := range matchedSlots {
		ordered[slot] = []byte(matchedResults[index].part.Raw)
	}
	return gjson.ParseBytes(JoinRawArray(ordered))
}

// ClaudeToolResultContent 提取工具结果中的内联图片，合并文本并保留其他 JSON 内容
func ClaudeToolResultContent(content gjson.Result) (gjson.Result, []gjson.Result) {
	if !content.IsArray() || len(content.Array()) == 0 {
		return content, nil
	}

	var texts []string
	var images []gjson.Result
	var remaining [][]byte
	allText := true
	for _, part := range content.Array() {
		source := part.Get("source")
		if part.Get("type").String() == "image" && source.Get("type").String() == "base64" &&
			source.Get("media_type").String() != "" && source.Get("data").String() != "" {
			images = append(images, source)
			continue
		}
		remaining = append(remaining, []byte(part.Raw))
		if part.Get("type").String() == "text" && part.Get("text").Type == gjson.String {
			texts = append(texts, part.Get("text").String())
		} else {
			allText = false
		}
	}
	if !allText {
		return gjson.ParseBytes(JoinRawArray(remaining)), images
	}
	result, _ := sjson.SetBytes([]byte(`{"text":""}`), "text", strings.Join(texts, "\n"))
	return gjson.ParseBytes(result), images
}

func claudeMessageContentParts(content gjson.Result) [][]byte {
	if !content.Exists() || content.Type == gjson.Null {
		return nil
	}
	if content.Type == gjson.String {
		if content.String() == "" {
			return nil
		}
		part := []byte(`{"type":"text","text":""}`)
		part, _ = sjson.SetBytes(part, "text", content.String())
		return [][]byte{part}
	}
	if !content.IsArray() {
		return nil
	}
	parts := make([][]byte, 0, len(content.Array()))
	content.ForEach(
		func(_, part gjson.Result) bool {
			if part.IsObject() {
				parts = append(parts, []byte(part.Raw))
			}
			return true
		},
	)
	return parts
}
