package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type xaiPatchDispatcherCall struct {
	dispatcher xaiNamespaceDispatcher
	itemID     string
	callID     string
	index      int64
	namespace  string
	child      string
	arguments  []byte
	pending    [][]byte
	added      []byte
	released   bool
	finished   bool
	ordinary   bool
}

func (n *xaiPreparedResponseNormalizer) patchDispatcherRecord(root, item gjson.Result) (*xaiPatchDispatcherCall, error) {
	var call *xaiPatchDispatcherCall
	var keys []string
	for _, field := range []struct {
		value  gjson.Result
		prefix string
	}{
		{root.Get("item_id"), "item:"}, {item.Get("id"), "item:"},
		{root.Get("call_id"), "call:"}, {item.Get("call_id"), "call:"},
	} {
		if !field.value.Exists() || field.value.Type == gjson.String && field.value.String() == "" {
			continue
		}
		if field.value.Type != gjson.String || strings.TrimSpace(field.value.String()) == "" {
			return nil, fmt.Errorf("invalid dispatcher identity")
		}
		keys = append(keys, field.prefix+field.value.String())
	}
	index := root.Get("output_index")
	if index.Exists() {
		if index.Type != gjson.Number || index.Int() < 0 || index.Float() != float64(index.Int()) {
			return nil, fmt.Errorf("invalid dispatcher output index")
		}
		keys = append(keys, fmt.Sprintf("index:%d", index.Int()))
	}
	for _, key := range keys {
		if known := n.patchDispatcherCall[key]; known != nil {
			if call != nil && call != known {
				return nil, fmt.Errorf("conflicting dispatcher aliases")
			}
			call = known
		}
	}
	for _, key := range []string{"type", "name"} {
		if value := item.Get(key); value.Exists() && value.Type != gjson.String {
			return nil, fmt.Errorf("invalid dispatcher source tool")
		}
	}
	name := item.Get("name").String()
	dispatcher, declared := n.dispatcher.state.dispatchers[name]
	declared = declared && n.patchNamespaces[dispatcher.namespace]
	if call == nil {
		if item.Get("type").String() != "function_call" {
			return nil, nil
		}
		if len(keys) == 0 {
			if !declared && name != "" {
				return nil, nil
			}
			return nil, fmt.Errorf("missing dispatcher identity")
		}
		call = &xaiPatchDispatcherCall{index: -1, ordinary: name != "" && !declared}
	}
	if declared {
		if call.dispatcher.namespace != "" && call.dispatcher.namespace != dispatcher.namespace {
			return nil, fmt.Errorf("changed dispatcher namespace")
		}
		call.dispatcher = dispatcher
	}
	if call.dispatcher.namespace != "" && name != "" && !declared {
		if _, known := call.dispatcher.children[name]; !known || call.child != "" && call.child != name {
			return nil, fmt.Errorf("changed dispatcher child identity")
		}
		call.child = name
	}
	for _, field := range []struct {
		value  gjson.Result
		target *string
	}{
		{root.Get("item_id"), &call.itemID}, {item.Get("id"), &call.itemID},
		{root.Get("call_id"), &call.callID}, {item.Get("call_id"), &call.callID},
		{item.Get("namespace"), &call.namespace},
	} {
		if !field.value.Exists() || field.value.String() == "" {
			continue
		}
		if field.value.Type != gjson.String || *field.target != "" && *field.target != field.value.String() {
			return nil, fmt.Errorf("changed dispatcher source identity")
		}
		*field.target = field.value.String()
	}
	if index.Exists() {
		if call.index >= 0 && call.index != index.Int() {
			return nil, fmt.Errorf("changed dispatcher output index")
		}
		call.index = index.Int()
	}
	if call.dispatcher.namespace != "" && call.namespace != "" && call.namespace != call.dispatcher.namespace {
		return nil, fmt.Errorf("contradictory dispatcher source namespace")
	}
	for _, key := range keys {
		n.patchDispatcherCall[key] = call
	}
	return call, nil
}

func (call *xaiPatchDispatcherCall) snapshot(text string) error {
	name, arguments, valid := unwrapXAINamespaceDispatcherArguments(text, call.dispatcher)
	if !valid || call.child != "" && call.child != name {
		return fmt.Errorf("invalid dispatcher child snapshot")
	}
	if name == "apply_patch" {
		var input translatorcommon.ResponsesPatchInput
		if _, err := input.Finish(string(arguments)); err != nil {
			return err
		}
	}
	if call.arguments != nil {
		var previous, current any
		if json.Unmarshal(call.arguments, &previous) != nil || json.Unmarshal(arguments, &current) != nil || !reflect.DeepEqual(previous, current) {
			return fmt.Errorf("conflicting dispatcher argument snapshots")
		}
	}
	call.child, call.arguments = name, bytes.Clone(arguments)
	return nil
}

func (call *xaiPatchDispatcherCall) restore(event []byte) ([]byte, error) {
	root := gjson.ParseBytes(event)
	item := root.Get("item")
	if item.Exists() {
		name := item.Get("name").String()
		if item.Get("type").String() != "function_call" || name != "" && name != call.dispatcher.namespace && name != call.child {
			return nil, fmt.Errorf("changed dispatcher source tool")
		}
		if namespace := item.Get("namespace"); namespace.Exists() && (namespace.Type != gjson.String || namespace.String() != "" && namespace.String() != call.dispatcher.namespace) {
			return nil, fmt.Errorf("changed dispatcher source namespace")
		}
		event, _ = sjson.SetBytes(event, "item.name", call.child)
		event, _ = sjson.SetBytes(event, "item.namespace", call.dispatcher.namespace)
		event, _ = sjson.SetBytes(event, "item.id", call.itemID)
		event, _ = sjson.SetBytes(event, "item.call_id", call.callID)
		arguments := ""
		if root.Get("type").String() != "response.output_item.added" {
			arguments = string(call.arguments)
		}
		event, _ = sjson.SetBytes(event, "item.arguments", arguments)
	} else {
		event, _ = sjson.SetBytes(event, "item_id", call.itemID)
		event, _ = sjson.SetBytes(event, "call_id", call.callID)
		event, _ = sjson.SetBytes(event, "arguments", string(call.arguments))
		if root.Get("name").Exists() {
			event, _ = sjson.SetBytes(event, "name", call.child)
		}
		if root.Get("namespace").Exists() {
			event, _ = sjson.SetBytes(event, "namespace", call.dispatcher.namespace)
		}
	}
	event, _ = sjson.SetBytes(event, "output_index", call.index)
	return event, nil
}

func (call *xaiPatchDispatcherCall) release() ([][]byte, error) {
	var fragments strings.Builder
	for _, event := range call.pending {
		root := gjson.ParseBytes(event)
		if root.Get("type").String() == "response.function_call_arguments.delta" {
			if root.Get("delta").Type != gjson.String {
				return nil, fmt.Errorf("invalid dispatcher argument delta")
			}
			fragments.WriteString(root.Get("delta").String())
			continue
		}
		arguments := root.Get("arguments")
		if item := root.Get("item"); item.Exists() {
			arguments = item.Get("arguments")
		}
		if !arguments.Exists() || arguments.Type == gjson.String && arguments.String() == "" && root.Get("type").String() == "response.output_item.added" {
			continue
		}
		if arguments.Type != gjson.String {
			return nil, fmt.Errorf("invalid dispatcher argument snapshot")
		}
		text := arguments.String()
		name := root.Get("name").String()
		if item := root.Get("item"); item.Exists() {
			name = item.Get("name").String()
		}
		if name == "" && call.child != "" && !gjson.Get(text, "name").Exists() {
			name = call.child
		}
		if name != "" && name != call.dispatcher.namespace {
			text = fmt.Sprintf(`{"name":%q,"arguments":%s}`, name, text)
		}
		if err := call.snapshot(text); err != nil {
			return nil, err
		}
	}
	if text := fragments.String(); gjson.Valid(text) {
		if err := call.snapshot(text); err != nil {
			return nil, err
		}
	}
	if call.child == "" || call.arguments == nil || call.itemID == "" || call.callID == "" || call.index < 0 {
		return nil, nil
	}
	var events [][]byte
	if !call.released && len(call.added) != 0 {
		added, err := call.restore(call.added)
		if err != nil {
			return nil, err
		}
		events = append(events, added)
	}
	for _, event := range call.pending {
		kind := gjson.GetBytes(event, "type").String()
		if kind == "response.function_call_arguments.delta" || kind == "response.output_item.added" {
			continue
		}
		converted, err := call.restore(event)
		if err != nil {
			return nil, err
		}
		events = append(events, converted)
		if kind == "response.output_item.done" {
			call.finished = true
		}
	}
	call.pending = nil
	call.released = true
	return events, nil
}

// dispatcher 原始事件保留到子工具与真实身份明确后，完整快照不能覆盖此前的冲突证据
func (n *xaiPreparedResponseNormalizer) normalizePatchDispatcher(event []byte) [][]byte {
	if n.patch == nil || n.dispatcher == nil || n.dispatcher.state == nil {
		return [][]byte{event}
	}
	fail := func() [][]byte {
		n.failed = true
		n.patchDispatcherCall = nil
		return [][]byte{invalidXAIToolCallEvent("xAI returned invalid apply_patch namespace arguments")}
	}
	root := gjson.ParseBytes(event)
	kind := root.Get("type").String()
	if kind == "response.failed" || kind == "error" {
		n.patchDispatcherCall = nil
		return [][]byte{event}
	}
	if kind == "response.completed" || kind == "response.done" || kind == "response.incomplete" {
		var events [][]byte
		for index, item := range root.Get("response.output").Array() {
			indexed := []byte(`{}`)
			if n.patchDispatcherCall["item:"+item.Get("id").String()] == nil && n.patchDispatcherCall["call:"+item.Get("call_id").String()] == nil {
				indexed, _ = sjson.SetBytes(indexed, "output_index", index)
			}
			call, err := n.patchDispatcherRecord(gjson.ParseBytes(indexed), item)
			if err != nil {
				return fail()
			}
			if call == nil || call.ordinary || call.dispatcher.namespace == "" {
				continue
			}
			if call.index < 0 {
				call.index = int64(index)
			}
			synthetic := bytes.Clone(event)
			synthetic, _ = sjson.DeleteBytes(synthetic, "response")
			synthetic, _ = sjson.SetBytes(synthetic, "type", "response.output_item.done")
			responseID := root.Get("response_id").String()
			if responseID == "" {
				responseID = root.Get("response.id").String()
			}
			if responseID != "" {
				synthetic, _ = sjson.SetBytes(synthetic, "response_id", responseID)
			}
			synthetic, _ = sjson.SetRawBytes(synthetic, "item", []byte(item.Raw))
			call.pending = append(call.pending, synthetic)
			out, err := call.release()
			if err != nil || !call.finished {
				return fail()
			}
			events = append(events, out...)
			converted, err := call.restore(synthetic)
			if err != nil {
				return fail()
			}
			event, _ = sjson.SetRawBytes(event, fmt.Sprintf("response.output.%d", index), []byte(gjson.GetBytes(converted, "item").Raw))
		}
		for _, call := range n.patchDispatcherCall {
			if !call.ordinary && (!call.finished || call.dispatcher.namespace == "") {
				return fail()
			}
		}
		return append(events, event)
	}
	item := root.Get("item")
	if kind == "response.function_call_arguments.delta" || kind == "response.function_call_arguments.done" {
		raw := []byte(`{"type":"function_call"}`)
		for _, key := range []string{"name", "namespace"} {
			if value := root.Get(key); value.Exists() {
				raw, _ = sjson.SetRawBytes(raw, key, []byte(value.Raw))
			}
		}
		item = gjson.ParseBytes(raw)
	}
	if !item.Exists() || item.Get("type").String() != "function_call" {
		return [][]byte{event}
	}
	call, err := n.patchDispatcherRecord(root, item)
	if err != nil {
		return fail()
	}
	if call == nil || call.ordinary {
		return [][]byte{event}
	}
	if call.dispatcher.namespace == "" && item.Get("name").String() != "" {
		call.ordinary = true
		pending := call.pending
		call.pending = nil
		return append(pending, event)
	}
	if call.released && kind == "response.function_call_arguments.delta" {
		if delta := root.Get("delta"); delta.Type == gjson.String && delta.String() == "" {
			return nil
		}
		if call.child == "apply_patch" {
			return fail()
		}
		return [][]byte{event}
	}
	call.pending = append(call.pending, bytes.Clone(event))
	if kind == "response.output_item.added" && call.added == nil {
		call.added = bytes.Clone(event)
	}
	if call.dispatcher.namespace == "" {
		return nil
	}
	if kind == "response.function_call_arguments.delta" {
		if root.Get("delta").Type != gjson.String {
			return fail()
		}
		return nil
	}
	events, err := call.release()
	if err != nil {
		return fail()
	}
	return events
}
