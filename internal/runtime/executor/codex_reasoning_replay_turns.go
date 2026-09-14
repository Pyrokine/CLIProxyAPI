package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"slices"
	"strings"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexReasoningReplayTurn struct {
	marked               bool
	assistantFingerprint string
	requestFingerprint   string
	callIDs              []string
	items                [][]byte
}

func insertCodexReasoningReplayTurns(body []byte, replayItems [][]byte) ([]byte, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() || len(replayItems) == 0 {
		return body, false
	}
	inputItems := input.Array()
	turns := splitCodexReasoningReplayTurns(replayItems)
	insertions := make(map[int][][]byte)
	usedAnchorIndexes := make(map[int]bool)
	prefixFingerprints := newCodexReplayPrefixFingerprints(inputItems)
	fallbackAnchorEnd := len(inputItems) - 1
	inserted := false
	for _, turn := range slices.Backward(turns) {

		if len(turn.items) == 0 {
			continue
		}
		if !turn.marked {
			items := filterCodexReasoningReplayItemsForInput(body, turn.items)
			if len(items) == 0 {
				continue
			}
			index := codexReasoningReplayInsertIndex(inputItems, items)
			items = codexAlignReasoningReplayToolCallIDs(inputItems, items)
			insertions[index] = append(items, insertions[index]...)
			inserted = true
			continue
		}

		anchorIndex, matched := codexReasoningReplayTurnAnchorIndex(
			inputItems,
			turn,
			fallbackAnchorEnd,
			usedAnchorIndexes,
			prefixFingerprints,
		)
		if !matched {
			continue
		}
		usedAnchorIndexes[anchorIndex] = true
		if turn.requestFingerprint == "" {
			fallbackAnchorEnd = anchorIndex - 1
		}
		items := filterCodexReasoningReplayTurnItems(inputItems, turn.items)
		if len(items) == 0 {
			continue
		}
		items = codexAlignReasoningReplayToolCallIDs(inputItems, items)
		insertions[anchorIndex] = append(items, insertions[anchorIndex]...)
		inserted = true
	}
	if !inserted {
		return body, false
	}

	items := make([]string, 0, len(inputItems)+len(replayItems))
	for index, inputItem := range inputItems {
		for _, replayItem := range insertions[index] {
			items = append(items, string(replayItem))
		}
		items = append(items, inputItem.Raw)
	}
	for _, replayItem := range insertions[len(inputItems)] {
		items = append(items, string(replayItem))
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(items, ",")+"]"))
	if err != nil {
		return body, false
	}
	return updated, true
}

func splitCodexReasoningReplayTurns(items [][]byte) []codexReasoningReplayTurn {
	turns := make([]codexReasoningReplayTurn, 0)
	current := codexReasoningReplayTurn{}
	appendCurrent := func() {
		if len(current.items) > 0 {
			turns = append(turns, current)
		}
	}
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		if strings.TrimSpace(itemResult.Get("type").String()) == internalcache.CodexReasoningReplayTurnType {
			appendCurrent()
			current = codexReasoningReplayTurn{
				marked:               true,
				assistantFingerprint: strings.TrimSpace(itemResult.Get("assistant_fingerprint").String()),
				requestFingerprint:   strings.TrimSpace(itemResult.Get("request_fingerprint").String()),
			}
			if callIDs := itemResult.Get("call_ids"); callIDs.IsArray() {
				for _, callIDResult := range callIDs.Array() {
					if callID := strings.TrimSpace(callIDResult.String()); callID != "" {
						current.callIDs = append(current.callIDs, callID)
					}
				}
			}
			continue
		}
		current.items = append(current.items, item)
	}
	appendCurrent()
	return turns
}

func codexReasoningReplayTurnAnchorIndex(
	inputItems []gjson.Result,
	turn codexReasoningReplayTurn,
	fallbackEnd int,
	used map[int]bool,
	prefixFingerprints *codexReplayPrefixFingerprints,
) (int, bool) {
	searchEnd := fallbackEnd
	if turn.requestFingerprint != "" {
		searchEnd = len(inputItems) - 1
	}
	if searchEnd >= len(inputItems) {
		searchEnd = len(inputItems) - 1
	}
	matchesRequestPrefix := func(index int) bool {
		return turn.requestFingerprint == "" || prefixFingerprints.at(index) == turn.requestFingerprint
	}
	if len(turn.callIDs) > 0 {
		callIDs := make(map[string]bool)
		for _, callID := range turn.callIDs {
			for _, candidate := range codexReplayComparableCallIDs(callID) {
				callIDs[candidate] = true
			}
		}
		for index := searchEnd; index >= 0; index-- {
			if used[index] || !matchesRequestPrefix(index) {
				continue
			}
			itemType := strings.TrimSpace(inputItems[index].Get("type").String())
			if itemType != "function_call" && itemType != "custom_tool_call" &&
				itemType != "function_call_output" && itemType != "custom_tool_call_output" {
				continue
			}
			for _, candidate := range codexReplayComparableCallIDs(inputItems[index].Get("call_id").String()) {
				if callIDs[candidate] {
					return index, true
				}
			}
		}
	}
	if turn.assistantFingerprint != "" {
		for index := searchEnd; index >= 0; index-- {
			if used[index] || !matchesRequestPrefix(index) {
				continue
			}
			if codexReplayAssistantMessageFingerprint(inputItems[index]) == turn.assistantFingerprint {
				return index, true
			}
		}
	}
	if len(turn.callIDs) == 0 && turn.assistantFingerprint == "" {
		return codexReasoningReplayInsertIndex(inputItems, turn.items), true
	}
	return 0, false
}

func filterCodexReasoningReplayTurnItems(inputItems []gjson.Result, items [][]byte) [][]byte {
	existingReasoning := make(map[string]bool)
	existingCalls := make(map[string]bool)
	existingOutputs := make(map[string]bool)
	for _, inputItem := range inputItems {
		itemType := strings.TrimSpace(inputItem.Get("type").String())
		switch itemType {
		case "reasoning":
			if encryptedContent := strings.TrimSpace(inputItem.Get("encrypted_content").String()); encryptedContent != "" {
				existingReasoning[encryptedContent] = true
			}
		case "function_call_output", "custom_tool_call_output":
			for _, candidate := range codexReplayComparableCallIDs(inputItem.Get("call_id").String()) {
				existingOutputs[candidate] = true
			}
		}
		for _, key := range codexReplayToolCallKeys(inputItem) {
			existingCalls[key] = true
		}
	}

	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "reasoning":
			if existingReasoning[strings.TrimSpace(itemResult.Get("encrypted_content").String())] {
				continue
			}
		case "function_call", "custom_tool_call":
			keys := codexReplayToolCallKeys(itemResult)
			if len(keys) == 0 || codexReplayAnyToolCallKeyExists(existingCalls, keys) {
				continue
			}
			hasMatchingOutput := false
			for _, candidate := range codexReplayComparableCallIDs(itemResult.Get("call_id").String()) {
				if existingOutputs[candidate] {
					hasMatchingOutput = true
					break
				}
			}
			if !hasMatchingOutput {
				continue
			}
			for _, key := range keys {
				existingCalls[key] = true
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func codexReplayAssistantMessageFingerprint(item gjson.Result) string {
	itemType := strings.TrimSpace(item.Get("type").String())
	if itemType != "" && itemType != "message" {
		return ""
	}
	if !strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "assistant") {
		return ""
	}
	content := item.Get("content")
	var builder strings.Builder
	if content.Type == gjson.String {
		builder.WriteString(content.String())
	} else if content.IsArray() {
		for _, part := range content.Array() {
			switch strings.TrimSpace(part.Get("type").String()) {
			case "input_text", "output_text":
				builder.WriteString(part.Get("text").String())
			case "refusal":
				builder.WriteString("\x00refusal\x00")
				builder.WriteString(part.Get("refusal").String())
			default:
				return ""
			}
		}
	} else {
		return ""
	}
	if builder.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func codexReplayInputPrefixFingerprint(inputItems []gjson.Result, end int) string {
	if end < 0 || end > len(inputItems) {
		return ""
	}
	hasher := sha256.New()
	for index := range end {
		_, _ = hasher.Write([]byte("\x00item\x00"))
		_, _ = hasher.Write([]byte(inputItems[index].Raw))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// codexReplayPrefixFingerprints answers prefix queries from one incremental hashing pass.
type codexReplayPrefixFingerprints struct {
	items  []gjson.Result
	hasher hash.Hash
	// sums[end] is the fingerprint of items[0:end], extended lazily.
	sums []string
}

func newCodexReplayPrefixFingerprints(items []gjson.Result) *codexReplayPrefixFingerprints {
	hasher := sha256.New()
	return &codexReplayPrefixFingerprints{
		items:  items,
		hasher: hasher,
		sums:   []string{hex.EncodeToString(hasher.Sum(nil))},
	}
}

func (f *codexReplayPrefixFingerprints) at(end int) string {
	if end < 0 || end > len(f.items) {
		return ""
	}
	for len(f.sums) <= end {
		next := len(f.sums) - 1
		_, _ = f.hasher.Write([]byte("\x00item\x00"))
		_, _ = io.WriteString(f.hasher, f.items[next].Raw)
		f.sums = append(f.sums, hex.EncodeToString(f.hasher.Sum(nil)))
	}
	return f.sums[end]
}
