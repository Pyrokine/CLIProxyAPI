package common

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// AlignOpenAIToolCallMessages places complete, unambiguous tool results after their issuing assistant message.
func AlignOpenAIToolCallMessages(messages [][]byte, extraAmbiguousIDs ...string) [][]byte {
	if len(messages) < 2 {
		return messages
	}

	type calls struct {
		index int
		ids   []string
		valid bool
	}
	var assistants []calls
	owners := make(map[string]int)
	results := make(map[string][]int)
	ambiguous := make(map[string]bool)
	for _, id := range extraAmbiguousIDs {
		if id = strings.TrimSpace(id); id != "" {
			ambiguous[id] = true
		}
	}
	for index, message := range messages {
		switch gjson.GetBytes(message, "role").String() {
		case "assistant":
			toolCalls := gjson.GetBytes(message, "tool_calls")
			if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
				continue
			}
			record := calls{index: index, valid: true}
			for _, toolCall := range toolCalls.Array() {
				id := toolCall.Get("id").String()
				if id == "" {
					record.valid = false
					continue
				}
				if _, exists := owners[id]; exists {
					ambiguous[id] = true
				}
				owners[id] = index
				record.ids = append(record.ids, id)
			}
			assistants = append(assistants, record)
		case "tool":
			id := gjson.GetBytes(message, "tool_call_id").String()
			if id != "" {
				results[id] = append(results[id], index)
				if len(results[id]) > 1 {
					ambiguous[id] = true
				}
			}
		}
	}

	insert := make(map[int][]int)
	moved := make(map[int]bool)
	for _, assistant := range assistants {
		if !assistant.valid {
			continue
		}
		var matched []int
		for _, id := range assistant.ids {
			if ambiguous[id] || len(results[id]) != 1 || results[id][0] <= assistant.index {
				matched = nil
				break
			}
			matched = append(matched, results[id][0])
		}
		if len(matched) != len(assistant.ids) || len(matched) == 0 {
			continue
		}
		sort.Ints(matched)
		adjacent := true
		for offset, index := range matched {
			if index != assistant.index+offset+1 {
				adjacent = false
				break
			}
		}
		if adjacent {
			continue
		}
		insert[assistant.index] = matched
		for _, index := range matched {
			moved[index] = true
		}
	}
	if len(moved) == 0 {
		return messages
	}

	aligned := make([][]byte, 0, len(messages))
	for index, message := range messages {
		if moved[index] {
			continue
		}
		aligned = append(aligned, message)
		for _, resultIndex := range insert[index] {
			aligned = append(aligned, messages[resultIndex])
		}
	}
	return aligned
}
