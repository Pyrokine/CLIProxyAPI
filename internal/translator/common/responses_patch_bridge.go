package common

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const patchFunctionParameters = `{"type":"object","properties":{"input":{"type":"string","description":"The complete apply_patch patch text."}},"required":["input"],"additionalProperties":false}`

// NormalizeResponsesPatchTools 使用现有声明优先级，将 freeform patch 转为函数参数
// 原始声明保留在 ResponsesPatchBridge 中，普通同名函数不参与转换
func NormalizeResponsesPatchTools(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid Responses request JSON")
	}
	identity := NewResponsesToolIdentity(body)
	affected := make(map[string]bool)
	for _, declaration := range identity.declarations {
		if declaration.Name == "apply_patch" && declaration.Kind == ResponsesCustomTool {
			affected[declaration.QualifiedName] = true
		}
	}
	seen := make(map[string]bool)
	var rewrite func(gjson.Result, string) []byte
	rewrite = func(tools gjson.Result, namespace string) []byte {
		items := make([][]byte, 0, len(tools.Array()))
		for _, tool := range tools.Array() {
			raw := []byte(tool.Raw)
			if tool.Get("type").String() == "namespace" {
				for _, key := range []string{"tools", "children"} {
					if nested := tool.Get(key); nested.IsArray() {
						children := rewrite(nested, joinResponsesNamespace(namespace, tool.Get("name").String()))
						raw, _ = sjson.SetRawBytes(raw, key, children)
						break
					}
				}
			} else {
				declaration, err := identity.ResolveDeclaration(responsesDeclarationName(tool), namespace)
				if err == nil && affected[declaration.QualifiedName] {
					if declaration.Raw != tool.Raw || declaration.Namespace != namespace || seen[declaration.QualifiedName] {
						continue
					}
					seen[declaration.QualifiedName] = true
					if declaration.Kind == ResponsesCustomTool {
						description := strings.ReplaceAll(tool.Get("description").String(), "This is a FREEFORM tool, so do not wrap the patch in JSON.", "")
						description = strings.TrimSpace(description) + `
Call this function with a JSON object whose input field contains the complete patch text.
Use the Codex apply_patch format, not a conventional git unified diff.
Start with *** Begin Patch and end with *** End Patch.
Use *** Add File: path, *** Delete File: path, or *** Update File: path.
Every added-file content line starts with +.
For updates, use @@; context lines start with one space, removed lines with -, and added lines with +.
Use *** Move to: path for a rename and *** End of File when required by the patch grammar.
Example input:
*** Begin Patch
*** Update File: src/main.go
@@
-old
+new
*** End Patch`
						if grammar := tool.Get("format.definition").String(); grammar != "" {
							if strings.Contains(grammar, "*** Environment ID:") {
								description += "\n\nUse *** Environment ID: as specified by the patch grammar."
							}
							description += "\nOriginal patch grammar:\n" + grammar
						}
						raw, _ = sjson.SetBytes(raw, "type", "function")
						raw, _ = sjson.SetBytes(raw, "description", description)
						raw, _ = sjson.SetRawBytes(raw, "parameters", []byte(patchFunctionParameters))
						raw, _ = sjson.DeleteBytes(raw, "format")
					}
				}
			}
			items = append(items, raw)
		}
		return JoinRawArray(items)
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", rewrite(tools, ""))
	}
	var choice func(gjson.Result) []byte
	choice = func(tool gjson.Result) []byte {
		raw := []byte(tool.Raw)
		declaration, err := identity.ResolveDeclaration(tool.Get("name").String(), tool.Get("namespace").String())
		if err == nil && declaration.Kind == ResponsesCustomTool && declaration.Name == "apply_patch" && tool.Get("type").String() == "custom" {
			raw, _ = sjson.SetBytes(raw, "type", "function")
		}
		for i, nested := range tool.Get("tools").Array() {
			raw, _ = sjson.SetRawBytes(raw, fmt.Sprintf("tools.%d", i), choice(nested))
		}
		return raw
	}
	if toolChoice := gjson.GetBytes(body, "tool_choice"); toolChoice.IsObject() {
		body, _ = sjson.SetRawBytes(body, "tool_choice", choice(toolChoice))
	}
	patchHistory := func(item gjson.Result) bool {
		if item.Get("type").String() != "custom_tool_call" {
			return false
		}
		if strings.TrimSpace(item.Get("name").String()) == "apply_patch" {
			return true
		}
		declaration, err := identity.ResolveDeclaration(item.Get("name").String(), item.Get("namespace").String())
		return err == nil && declaration.Name == "apply_patch"
	}
	patchCalls := make(map[string]bool)
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if patchHistory(item) {
			callID := item.Get("call_id")
			if callID.Type != gjson.String || strings.TrimSpace(callID.String()) == "" || patchCalls[callID.String()] {
				return nil, fmt.Errorf("invalid apply_patch history call ID")
			}
			patchCalls[callID.String()] = true
		}
	}
	for i, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "custom_tool_call_output" && patchCalls[item.Get("call_id").String()] {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("input.%d.type", i), "function_call_output")
			continue
		}
		if item.Get("type").String() == "additional_tools" {
			body, _ = sjson.SetRawBytes(body, fmt.Sprintf("input.%d.tools", i), rewrite(item.Get("tools"), ""))
			continue
		}
		if !patchHistory(item) {
			continue
		}
		if item.Get("input").Type != gjson.String {
			return nil, fmt.Errorf("apply_patch history input must be a string")
		}
		var input ResponsesPatchInput
		if _, err := input.Finish(`{"input":` + item.Get("input").Raw + `}`); err != nil {
			return nil, fmt.Errorf("invalid apply_patch history input")
		}
		path := fmt.Sprintf("input.%d", i)
		body, _ = sjson.SetBytes(body, path+".type", "function_call")
		body, _ = sjson.SetBytes(body, path+".arguments", WrapResponsesCustomToolInput(item.Get("input")))
		body, _ = sjson.SetBytes(body, path+".name", QualifyResponsesNamespaceToolName(item.Get("namespace").String(), item.Get("name").String()))
		body, _ = sjson.DeleteBytes(body, path+".namespace")
		body, _ = sjson.DeleteBytes(body, path+".input")
	}
	return body, nil
}

type responsesPatchCall struct {
	declaration   ResponsesToolDeclaration
	kind          string
	namespace     string
	itemID        string
	callID        string
	index         int64
	input         ResponsesPatchInput
	arguments     strings.Builder
	snapshot      string
	pending       [][]byte
	completedItem []byte
	added         bool
	inputDone     bool
	itemDone      bool
}

func (call *responsesPatchCall) isPatch() bool {
	return call.declaration.Name == "apply_patch" && call.declaration.Kind == ResponsesCustomTool
}

func (call *responsesPatchCall) identityReady() bool {
	return call.itemID != "" && call.callID != "" && call.index >= 0
}

// ResponsesPatchBridge 由非原生执行器显式创建，使用原始声明还原函数调用
// 每个请求独立持有调用身份、参数解码和失败状态
type ResponsesPatchBridge struct {
	identity *ResponsesToolIdentity
	byItem   map[string]*responsesPatchCall
	byCall   map[string]*responsesPatchCall
	byIndex  map[int64]*responsesPatchCall
	sequence int64
	err      error
}

func NewResponsesPatchBridge(body []byte) *ResponsesPatchBridge {
	identity := NewResponsesToolIdentity(body)
	for _, declaration := range identity.Declarations() {
		if declaration.Name == "apply_patch" && declaration.Kind == ResponsesCustomTool {
			return &ResponsesPatchBridge{identity: identity, byItem: make(map[string]*responsesPatchCall), byCall: make(map[string]*responsesPatchCall), byIndex: make(map[int64]*responsesPatchCall)}
		}
	}
	return nil
}

func (b *ResponsesPatchBridge) fail() error {
	if b.err == nil {
		b.err = fmt.Errorf("invalid apply_patch tool arguments received from upstream")
		b.byItem = nil
		b.byCall = nil
		b.byIndex = nil
	}
	return b.err
}

func (b *ResponsesPatchBridge) lookup(event, item gjson.Result) (*responsesPatchCall, error) {
	var call *responsesPatchCall
	match := func(candidate *responsesPatchCall) bool {
		if candidate == nil {
			return true
		}
		if call != nil && call != candidate {
			return false
		}
		call = candidate
		return true
	}
	for _, value := range []gjson.Result{event.Get("item_id"), item.Get("id"), event.Get("call_id"), item.Get("call_id")} {
		if value.Exists() && (value.Type != gjson.String || (value.String() != "" && strings.TrimSpace(value.String()) == "")) {
			return nil, b.fail()
		}
	}
	for _, id := range []string{event.Get("item_id").String(), item.Get("id").String()} {
		if id != "" && !match(b.byItem[id]) {
			return nil, b.fail()
		}
	}
	for _, id := range []string{event.Get("call_id").String(), item.Get("call_id").String()} {
		if id != "" && !match(b.byCall[id]) {
			return nil, b.fail()
		}
	}
	if index := event.Get("output_index"); index.Exists() {
		if index.Type != gjson.Number || index.Int() < 0 || index.Float() != float64(index.Int()) || !match(b.byIndex[index.Int()]) {
			return nil, b.fail()
		}
	}
	return call, nil
}

func (b *ResponsesPatchBridge) call(event, item gjson.Result) (*responsesPatchCall, error) {
	call, err := b.lookup(event, item)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"type", "name", "namespace"} {
		if value := item.Get(key); value.Exists() && value.Type != gjson.String {
			return nil, b.fail()
		}
	}
	name, namespace := item.Get("name").String(), item.Get("namespace").String()
	var declaration ResponsesToolDeclaration
	if name != "" {
		declaration, err = b.identity.ResolveDeclaration(name, namespace)
		if err != nil {
			if call != nil {
				return nil, b.fail()
			}
			return nil, nil
		}
	} else if call != nil {
		declaration = call.declaration
	}
	if call == nil {
		call = &responsesPatchCall{index: -1}
	}
	if declaration.QualifiedName != "" {
		if call.declaration.QualifiedName != "" && declaration.QualifiedName != call.declaration.QualifiedName {
			return nil, b.fail()
		}
		call.declaration = declaration
	}
	if kind := item.Get("type").String(); kind != "" {
		if call.kind != "" && call.kind != kind {
			return nil, b.fail()
		}
		call.kind = kind
	}
	if namespace != "" {
		if call.namespace != "" && call.namespace != namespace {
			return nil, b.fail()
		}
		call.namespace = namespace
	}
	if call.isPatch() && (call.kind != "function_call" || (call.namespace != "" && call.namespace != call.declaration.Namespace)) {
		return nil, b.fail()
	}
	if index := event.Get("output_index"); index.Exists() {
		if call.index >= 0 && index.Int() != call.index {
			return nil, b.fail()
		}
		call.index = index.Int()
	}
	for _, id := range []string{event.Get("item_id").String(), item.Get("id").String()} {
		if id == "" {
			continue
		}
		if call.itemID != "" && call.itemID != id {
			return nil, b.fail()
		}
		call.itemID = id
	}
	for _, id := range []string{event.Get("call_id").String(), item.Get("call_id").String()} {
		if id == "" {
			continue
		}
		if call.callID != "" && call.callID != id {
			return nil, b.fail()
		}
		call.callID = id
	}
	if call.itemID != "" {
		b.byItem[call.itemID] = call
	}
	if call.callID != "" {
		b.byCall[call.callID] = call
	}
	if call.index >= 0 {
		b.byIndex[call.index] = call
	}
	if call.index < 0 && call.itemID == "" && call.callID == "" {
		return nil, b.fail()
	}
	return call, nil
}

func (b *ResponsesPatchBridge) restoreItem(item []byte, call *responsesPatchCall) []byte {
	item, _ = sjson.SetBytes(item, "type", "custom_tool_call")
	item, _ = sjson.SetBytes(item, "id", call.itemID)
	item, _ = sjson.SetBytes(item, "call_id", call.callID)
	item, _ = sjson.SetBytes(item, "input", call.input.Input())
	item, _ = sjson.DeleteBytes(item, "arguments")
	return b.identity.SetToolCallIdentity(item, call.declaration.QualifiedName, "")
}

func (b *ResponsesPatchBridge) itemEvent(kind string, call *responsesPatchCall, item, prototype []byte) []byte {
	raw := b.inputEvent(kind, call, "", prototype)
	for _, key := range []string{"item_id", "call_id", "delta"} {
		raw, _ = sjson.DeleteBytes(raw, key)
	}
	raw, _ = sjson.SetRawBytes(raw, "item", item)
	return raw
}

func (b *ResponsesPatchBridge) addedEvent(call *responsesPatchCall, item, prototype []byte) []byte {
	if call.added {
		return nil
	}
	call.added = true
	item = b.restoreItem(item, call)
	item, _ = sjson.SetBytes(item, "input", "")
	return b.itemEvent("response.output_item.added", call, item, prototype)
}

func (b *ResponsesPatchBridge) inputEvent(kind string, call *responsesPatchCall, text string, prototype []byte) []byte {
	raw := bytes.Clone(prototype)
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	responseID := gjson.GetBytes(raw, "response_id").String()
	if responseID == "" {
		responseID = gjson.GetBytes(raw, "response.id").String()
	}
	for _, key := range []string{"item", "response", "arguments", "delta", "input"} {
		raw, _ = sjson.DeleteBytes(raw, key)
	}
	if responseID != "" {
		raw, _ = sjson.SetBytes(raw, "response_id", responseID)
	}
	raw, _ = sjson.SetBytes(raw, "type", kind)
	raw, _ = sjson.SetBytes(raw, "item_id", call.itemID)
	raw, _ = sjson.SetBytes(raw, "call_id", call.callID)
	raw, _ = sjson.SetBytes(raw, "output_index", call.index)
	field := "delta"
	if kind == "response.custom_tool_call_input.done" {
		field = "input"
	}
	raw, _ = sjson.SetBytes(raw, field, text)
	return raw
}

func (b *ResponsesPatchBridge) snapshot(call *responsesPatchCall, arguments gjson.Result, final bool) error {
	if !arguments.Exists() {
		return nil
	}
	if arguments.Type != gjson.String {
		return b.fail()
	}
	if arguments.String() == "" && !final {
		return nil
	}
	var input ResponsesPatchInput
	if _, err := input.Finish(arguments.String()); err != nil || !strings.HasPrefix(input.Input(), call.input.Input()) || (call.input.phase == patchInputComplete && input.Input() != call.input.Input()) {
		return b.fail()
	}
	if call.snapshot != "" {
		var previous ResponsesPatchInput
		if _, err := previous.Finish(call.snapshot); err != nil || previous.Input() != input.Input() {
			return b.fail()
		}
	}
	call.snapshot = arguments.String()
	return nil
}

func (b *ResponsesPatchBridge) finish(call *responsesPatchCall, arguments gjson.Result, prototype []byte) ([][]byte, error) {
	if err := b.snapshot(call, arguments, true); err != nil {
		return nil, err
	}
	text := call.arguments.String()
	if call.snapshot != "" {
		text = call.snapshot
	}
	tail, err := call.input.Finish(text)
	if err != nil {
		return nil, b.fail()
	}
	call.snapshot = text
	var events [][]byte
	if tail != "" {
		events = append(events, b.inputEvent("response.custom_tool_call_input.delta", call, tail, prototype))
	}
	if !call.inputDone {
		events = append(events, b.inputEvent("response.custom_tool_call_input.done", call, call.input.Input(), prototype))
		call.inputDone = true
	}
	return events, nil
}

func (b *ResponsesPatchBridge) releasePending(call *responsesPatchCall) ([][]byte, error) {
	pending := call.pending
	call.pending = nil
	var events [][]byte
	for _, event := range pending {
		out, err := b.transform(event)
		if err != nil {
			return nil, err
		}
		events = append(events, out...)
	}
	return events, nil
}

func (b *ResponsesPatchBridge) Transform(event []byte) ([][]byte, error) {
	if b == nil {
		return [][]byte{event}, nil
	}
	events, err := b.transform(event)
	return b.number(events), err
}

func (b *ResponsesPatchBridge) transform(event []byte) ([][]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	if !gjson.ValidBytes(event) {
		return nil, b.fail()
	}
	root := gjson.ParseBytes(event)
	var events [][]byte
	switch root.Get("type").String() {
	case "response.output_item.added", "response.output_item.done":
		item := root.Get("item")
		call, err := b.call(root, item)
		if err != nil {
			return nil, err
		}
		if call == nil || (call.kind != "function_call" && !call.isPatch()) {
			break
		}
		if call.declaration.QualifiedName == "" {
			call.pending = append(call.pending, bytes.Clone(event))
			return nil, nil
		}
		if !call.isPatch() {
			pending := call.pending
			call.pending = nil
			return append(pending, event), nil
		}
		done := root.Get("type").String() == "response.output_item.done"
		if err := b.snapshot(call, item.Get("arguments"), done); err != nil {
			return nil, err
		}
		if !call.identityReady() {
			call.pending = append(call.pending, bytes.Clone(event))
			return nil, nil
		}
		events, err = b.releasePending(call)
		if err != nil {
			return nil, err
		}
		if !call.added {
			if done {
				events = append(events, b.addedEvent(call, []byte(item.Raw), event))
			} else {
				call.added = true
				event, _ = sjson.SetRawBytes(event, "item", b.restoreItem([]byte(item.Raw), call))
				events = append(events, event)
			}
		}
		if !done {
			return events, nil
		}
		completed, err := b.finish(call, item.Get("arguments"), event)
		if err != nil {
			return nil, err
		}
		events = append(events, completed...)
		call.completedItem = b.restoreItem([]byte(item.Raw), call)
		if !call.itemDone {
			event, _ = sjson.SetRawBytes(event, "item", call.completedItem)
			events = append(events, event)
			call.itemDone = true
		}
		return events, nil
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		call, err := b.lookup(root, gjson.Result{})
		if err != nil {
			return nil, err
		}
		item := []byte(`{"type":"function_call"}`)
		if call != nil {
			item, _ = sjson.SetBytes(item, "name", call.declaration.QualifiedName)
		}
		for _, key := range []string{"name", "namespace"} {
			if value := root.Get(key); value.Exists() {
				item, _ = sjson.SetRawBytes(item, key, []byte(value.Raw))
			}
		}
		call, err = b.call(root, gjson.ParseBytes(item))
		if err != nil {
			return nil, err
		}
		if call == nil {
			return nil, b.fail()
		}
		if call.declaration.QualifiedName == "" {
			call.pending = append(call.pending, bytes.Clone(event))
			return nil, nil
		}
		if !call.isPatch() {
			pending := call.pending
			call.pending = nil
			return append(pending, event), nil
		}
		if root.Get("type").String() == "response.function_call_arguments.done" {
			if err := b.snapshot(call, root.Get("arguments"), true); err != nil {
				return nil, err
			}
		} else if root.Get("delta").Type != gjson.String {
			return nil, b.fail()
		}
		if !call.identityReady() {
			call.pending = append(call.pending, bytes.Clone(event))
			return nil, nil
		}
		events, err = b.releasePending(call)
		if err != nil {
			return nil, err
		}
		if added := b.addedEvent(call, item, event); added != nil {
			events = append(events, added)
		}
		if root.Get("type").String() == "response.function_call_arguments.done" {
			completed, err := b.finish(call, root.Get("arguments"), event)
			return append(events, completed...), err
		}
		if call.inputDone {
			return nil, b.fail()
		}
		fragment := root.Get("delta").String()
		call.arguments.WriteString(fragment)
		delta, err := call.input.Push(fragment)
		if err != nil {
			return nil, b.fail()
		}
		if delta != "" {
			events = append(events, b.inputEvent("response.custom_tool_call_input.delta", call, delta, event))
		}
		return events, nil
	case "response.completed", "response.done", "response.incomplete":
		var output [][]byte
		seen := make(map[*responsesPatchCall]bool)
		for _, call := range b.byIndex {
			seen[call] = false
		}
		for _, call := range b.byItem {
			seen[call] = false
		}
		for _, call := range b.byCall {
			seen[call] = false
		}
		for index, item := range root.Get("response.output").Array() {
			known, err := b.lookup(gjson.Result{}, item)
			if err != nil {
				return nil, err
			}
			if known != nil && known.kind != "" && item.Get("type").String() != known.kind {
				return nil, b.fail()
			}
			if item.Get("type").String() != "function_call" && item.Get("type").String() != "custom_tool_call" {
				output = append(output, []byte(item.Raw))
				continue
			}
			outputIndex := int64(index)
			if known != nil && known.index >= 0 {
				outputIndex = known.index
			}
			indexed, _ := sjson.SetBytes([]byte(`{}`), "output_index", outputIndex)
			call, err := b.call(gjson.ParseBytes(indexed), item)
			if err != nil {
				return nil, err
			}
			if call == nil || !call.isPatch() {
				output = append(output, []byte(item.Raw))
				continue
			}
			if seen[call] || !call.identityReady() {
				return nil, b.fail()
			}
			seen[call] = true
			if err := b.snapshot(call, item.Get("arguments"), true); err != nil {
				return nil, err
			}
			pending, err := b.releasePending(call)
			if err != nil {
				return nil, err
			}
			events = append(events, pending...)
			if added := b.addedEvent(call, []byte(item.Raw), event); added != nil {
				events = append(events, added)
			}
			completed, err := b.finish(call, item.Get("arguments"), event)
			if err != nil {
				return nil, err
			}
			events = append(events, completed...)
			call.completedItem = b.restoreItem([]byte(item.Raw), call)
			if !call.itemDone {
				events = append(events, b.itemEvent("response.output_item.done", call, call.completedItem, event))
				call.itemDone = true
			}
			output = append(output, call.completedItem)
		}
		var missing []*responsesPatchCall
		for call, included := range seen {
			if call.kind == "function_call" && call.declaration.QualifiedName == "" {
				return nil, b.fail()
			}
			if !call.isPatch() {
				continue
			}
			if !call.itemDone || !call.identityReady() {
				return nil, b.fail()
			}
			if !included {
				missing = append(missing, call)
			}
		}
		sort.Slice(missing, func(i, j int) bool { return missing[i].index < missing[j].index })
		for _, call := range missing {
			output = append(output, call.completedItem)
		}
		event, _ = sjson.SetRawBytes(event, "response.output", JoinRawArray(output))
	}
	events = append(events, event)
	return events, nil
}

func (b *ResponsesPatchBridge) number(events [][]byte) [][]byte {
	for i, event := range events {
		b.sequence++
		events[i], _ = sjson.SetBytes(event, "sequence_number", b.sequence)
	}
	return events
}
