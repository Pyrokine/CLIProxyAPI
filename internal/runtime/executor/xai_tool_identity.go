package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type xaiNamespaceToolRef struct {
	namespace string
	name      string
}

type xaiClientToolKey struct {
	namespace string
	name      string
	toolType  string
}

type xaiToolIdentityNormalizer struct {
	namespaceTools      map[string]xaiNamespaceToolRef
	clientDeclaredTools map[xaiClientToolKey]struct{}
}

func newXAIToolIdentityNormalizer(body []byte) *xaiToolIdentityNormalizer {
	normalizer := &xaiToolIdentityNormalizer{
		namespaceTools:      make(map[string]xaiNamespaceToolRef),
		clientDeclaredTools: make(map[xaiClientToolKey]struct{}),
	}
	if !gjson.ValidBytes(body) {
		return normalizer
	}

	ambiguousRoutes := make(map[string]struct{})
	normalizer.collectToolDeclarations(gjson.GetBytes(body, "tools"), "", ambiguousRoutes)
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() == "additional_tools" {
				normalizer.collectToolDeclarations(item.Get("tools"), "", ambiguousRoutes)
			}
		}
	}
	return normalizer
}

func (n *xaiToolIdentityNormalizer) collectToolDeclarations(
	tools gjson.Result,
	parentNamespace string,
	ambiguousRoutes map[string]struct{},
) {
	if n == nil || !tools.IsArray() {
		return
	}
	for _, tool := range tools.Array() {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if toolType == xaiNamespaceToolType {
			namespace := joinXAINamespace(parentNamespace, tool.Get("name").String())
			if namespace != "" {
				n.collectToolDeclarations(tool.Get("tools"), namespace, ambiguousRoutes)
			}
			continue
		}
		if toolType != xaiFunctionToolType && toolType != xaiCustomToolType {
			continue
		}
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			continue
		}
		effectiveType := toolType
		if effectiveType == xaiCustomToolType {
			effectiveType = xaiFunctionToolType
		}
		n.clientDeclaredTools[xaiClientToolKey{
			namespace: parentNamespace,
			name:      name,
			toolType:  effectiveType,
		}] = struct{}{}
		if parentNamespace == "" {
			delete(n.namespaceTools, name)
			ambiguousRoutes[name] = struct{}{}
			continue
		}
		qualifiedName := qualifyXAINamespaceToolName(parentNamespace, name)
		if qualifiedName == "" {
			continue
		}
		if _, ambiguous := ambiguousRoutes[qualifiedName]; ambiguous {
			continue
		}
		ref := xaiNamespaceToolRef{namespace: parentNamespace, name: name}
		if existing, exists := n.namespaceTools[qualifiedName]; exists && existing != ref {
			delete(n.namespaceTools, qualifiedName)
			ambiguousRoutes[qualifiedName] = struct{}{}
			continue
		}
		n.namespaceTools[qualifiedName] = ref
	}
}

func joinXAINamespace(parentNamespace, namespace string) string {
	parentNamespace = strings.TrimSpace(strings.TrimSuffix(parentNamespace, "__"))
	namespace = strings.TrimSpace(strings.TrimSuffix(namespace, "__"))
	if parentNamespace == "" {
		return namespace
	}
	if namespace == "" {
		return parentNamespace
	}
	prefix := parentNamespace + "__"
	if strings.HasPrefix(namespace, prefix) {
		return namespace
	}
	return prefix + namespace
}

func (n *xaiToolIdentityNormalizer) normalizeDeclarations(body []byte) ([]byte, error) {
	body = promoteXAIAdditionalTools(body)
	var err error
	body, err = normalizeXAITools(body)
	if err != nil {
		return nil, err
	}
	body = normalizeXAINamespaceToolChoice(body)
	body = normalizeXAIForcedWebSearchToolChoice(body)
	body = pruneXAIOrphanedToolChoice(body)
	body = normalizeXAIToolChoiceForTools(body)
	if xaiRequestHasNativeXSearch(body) {
		body = ensureXAINativeXSearchAllowedTools(body)
	}
	return body, nil
}

func (n *xaiToolIdentityNormalizer) normalizeInputHistory(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}

	inputItems := input.Array()
	items := make([]json.RawMessage, 0, len(inputItems))
	changed := false
	for _, item := range inputItems {
		itemType := strings.TrimSpace(item.Get("type").String())
		var normalized []byte
		switch itemType {
		case "custom_tool_call":
			callID := strings.TrimSpace(item.Get("call_id").String())
			name := strings.TrimSpace(item.Get("name").String())
			if callID == "" || name == "" {
				changed = true
				continue
			}
			normalized = []byte(`{"type":"function_call"}`)
			normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
			normalized, _ = sjson.SetBytes(normalized, "name", name)
			normalized, _ = sjson.SetBytes(normalized, "arguments", xaiCustomToolCallArguments(item.Get("input")))
		case "custom_tool_call_output":
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID == "" {
				changed = true
				continue
			}
			normalized = []byte(`{"type":"function_call_output"}`)
			normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
			normalized, _ = sjson.SetBytes(normalized, "output", xaiCustomToolCallOutput(item.Get("output")))
		case "function_call":
			namespace := strings.TrimSpace(item.Get("namespace").String())
			name := strings.TrimSpace(item.Get("name").String())
			qualifiedName := qualifyXAINamespaceToolName(namespace, name)
			if namespace == "" || qualifiedName == "" {
				items = append(items, json.RawMessage(item.Raw))
				continue
			}
			normalized = []byte(item.Raw)
			normalized, _ = sjson.SetBytes(normalized, "name", qualifiedName)
			normalized, _ = sjson.DeleteBytes(normalized, "namespace")
		default:
			items = append(items, json.RawMessage(item.Raw))
			continue
		}
		items = append(items, json.RawMessage(normalized))
		changed = true
	}
	if !changed {
		return body
	}
	rawInput, err := json.Marshal(items)
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "input", rawInput)
	if err != nil {
		return body
	}
	return updated
}

func xaiCustomToolCallArguments(input gjson.Result) string {
	if !input.Exists() {
		return "{}"
	}
	if input.Type == gjson.String {
		text := input.String()
		trimmed := strings.TrimSpace(text)
		if gjson.Valid(trimmed) {
			parsed := gjson.Parse(trimmed)
			if parsed.IsObject() {
				return parsed.Raw
			}
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return "{}"
		}
		return `{"input":` + string(encoded) + `}`
	}
	if input.IsObject() {
		return input.Raw
	}
	if input.Raw != "" {
		return `{"input":` + input.Raw + `}`
	}
	return "{}"
}

func xaiCustomToolCallOutput(output gjson.Result) string {
	if !output.Exists() {
		return ""
	}
	if output.Type == gjson.String {
		return output.String()
	}
	return output.Raw
}

func normalizeXAIToolDeclarations(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return body, nil
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil
	}
	normalized, changed, err := normalizeXAIToolArray(tools, "")
	if err != nil {
		return nil, err
	}
	if !changed {
		return body, nil
	}
	updated, err := sjson.SetRawBytes(body, "tools", normalized)
	if err != nil {
		return nil, fmt.Errorf("xai: normalize tools: %w", err)
	}
	return updated, nil
}

func normalizeXAIToolArray(tools gjson.Result, parentNamespace string) ([]byte, bool, error) {
	items := make([]json.RawMessage, 0, len(tools.Array()))
	changed := false
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == xaiNamespaceToolType {
			changed = true
			namespace := joinXAINamespace(parentNamespace, tool.Get("name").String())
			nested := tool.Get("tools")
			if namespace == "" || !nested.IsArray() {
				continue
			}
			nestedRaw, _, err := normalizeXAIToolArray(nested, namespace)
			if err != nil {
				return nil, false, err
			}
			for _, nestedTool := range gjson.ParseBytes(nestedRaw).Array() {
				items = append(items, json.RawMessage(nestedTool.Raw))
			}
			continue
		}
		raw, toolChanged, err := normalizeXAITool(tool, parentNamespace)
		if err != nil {
			return nil, false, err
		}
		changed = changed || toolChanged
		if len(raw) > 0 {
			items = append(items, json.RawMessage(raw))
		}
	}
	if !changed {
		return nil, false, nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, false, fmt.Errorf("xai: marshal normalized tools: %w", err)
	}
	return raw, true, nil
}

func promoteXAIAdditionalTools(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	remaining := make([]json.RawMessage, 0, len(input.Array()))
	promoted := make([]json.RawMessage, 0)
	for _, item := range input.Array() {
		if item.Get("type").String() != "additional_tools" {
			remaining = append(remaining, json.RawMessage(item.Raw))
			continue
		}
		for _, tool := range item.Get("tools").Array() {
			promoted = append(promoted, json.RawMessage(tool.Raw))
		}
	}
	if len(remaining) == len(input.Array()) {
		return body
	}
	rawInput, err := json.Marshal(remaining)
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "input", rawInput)
	if err != nil {
		return body
	}
	if len(promoted) == 0 {
		return updated
	}
	tools := make([]json.RawMessage, 0, len(gjson.GetBytes(updated, "tools").Array())+len(promoted))
	for _, tool := range gjson.GetBytes(updated, "tools").Array() {
		tools = append(tools, json.RawMessage(tool.Raw))
	}
	tools = append(tools, promoted...)
	rawTools, err := json.Marshal(tools)
	if err != nil {
		return body
	}
	updated, err = sjson.SetRawBytes(updated, "tools", rawTools)
	if err != nil {
		return body
	}
	return updated
}

func normalizeXAINamespaceToolChoice(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	original := body
	normalizeAtPath := func(path string) bool {
		choice := gjson.GetBytes(body, path)
		if !choice.IsObject() || choice.Get("type").String() != xaiFunctionToolType {
			return true
		}
		namespace := strings.TrimSpace(choice.Get("namespace").String())
		name := strings.TrimSpace(choice.Get("name").String())
		qualifiedName := qualifyXAINamespaceToolName(namespace, name)
		if namespace == "" || qualifiedName == "" {
			return true
		}
		updated, err := sjson.SetBytes(body, path+".name", qualifiedName)
		if err != nil {
			return false
		}
		updated, err = sjson.DeleteBytes(updated, path+".namespace")
		if err != nil {
			return false
		}
		body = updated
		return true
	}
	if !normalizeAtPath("tool_choice") {
		return original
	}
	allowed := gjson.GetBytes(body, "tool_choice.tools")
	if allowed.IsArray() {
		for index := range allowed.Array() {
			if !normalizeAtPath(fmt.Sprintf("tool_choice.tools.%d", index)) {
				return original
			}
		}
	}
	return body
}

func normalizeXAIForcedWebSearchToolChoice(body []byte) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() || strings.TrimSpace(choice.Get("type").String()) != xaiWebSearchToolType {
		return body
	}
	allowed := []byte(`{"type":"allowed_tools","mode":"required","tools":[]}`)
	allowed, err := sjson.SetRawBytes(allowed, "tools.-1", []byte(choice.Raw))
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "tool_choice", allowed)
	if err != nil {
		return body
	}
	return updated
}

type xaiToolChoiceKey struct {
	toolType string
	name     string
}

func pruneXAIOrphanedToolChoice(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.Exists() || choice.Type == gjson.String || !choice.IsObject() {
		return body
	}
	available := make(map[xaiToolChoiceKey]struct{})
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if toolType == "" {
			continue
		}
		key := xaiToolChoiceKey{toolType: toolType}
		if toolType == xaiFunctionToolType || toolType == xaiCustomToolType {
			key.name = strings.TrimSpace(tool.Get("name").String())
			if key.name == "" {
				continue
			}
		}
		available[key] = struct{}{}
	}
	matches := func(tool gjson.Result) bool {
		toolType := strings.TrimSpace(tool.Get("type").String())
		key := xaiToolChoiceKey{toolType: toolType}
		if toolType == xaiFunctionToolType || toolType == xaiCustomToolType {
			key.name = strings.TrimSpace(tool.Get("name").String())
		}
		_, ok := available[key]
		return ok
	}
	if choice.Get("type").String() != "allowed_tools" {
		if matches(choice) {
			return body
		}
		body, _ = sjson.DeleteBytes(body, "tool_choice")
		return body
	}
	allowed := choice.Get("tools")
	if !allowed.IsArray() {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
		return body
	}
	kept := make([]json.RawMessage, 0, len(allowed.Array()))
	for _, tool := range allowed.Array() {
		if matches(tool) {
			kept = append(kept, json.RawMessage(tool.Raw))
		}
	}
	if len(kept) == len(allowed.Array()) {
		return body
	}
	if len(kept) == 0 {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
		return body
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	body, _ = sjson.SetRawBytes(body, "tool_choice.tools", raw)
	return body
}

func xaiRequestHasNativeXSearch(body []byte) bool {
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if strings.TrimSpace(tool.Get("type").String()) == xaiXSearchToolType {
			return true
		}
	}
	return false
}

func ensureXAINativeXSearchTool(body []byte) []byte {
	if !gjson.ValidBytes(body) || xaiRequestHasNativeXSearch(body) {
		return ensureXAINativeXSearchAllowedTools(body)
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"x_search"}]`))
	} else {
		body, _ = sjson.SetRawBytes(body, "tools.-1", []byte(`{"type":"x_search"}`))
	}
	return ensureXAINativeXSearchAllowedTools(body)
}

func ensureXAINativeXSearchAllowedTools(body []byte) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() || choice.Get("type").String() != "allowed_tools" {
		return body
	}
	allowed := choice.Get("tools")
	if !allowed.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tool_choice.tools", []byte(`[{"type":"x_search"}]`))
		return body
	}
	for _, tool := range allowed.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == xaiXSearchToolType {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tool_choice.tools.-1", []byte(`{"type":"x_search"}`))
	return body
}

func (n *xaiToolIdentityNormalizer) newResponseNormalizer(filterInternalXSearch bool) *xaiToolIdentityResponseNormalizer {
	var namespaceTools map[string]xaiNamespaceToolRef
	var clientDeclaredTools map[xaiClientToolKey]struct{}
	if n != nil {
		namespaceTools = n.namespaceTools
		clientDeclaredTools = n.clientDeclaredTools
	}
	return &xaiToolIdentityResponseNormalizer{
		namespaceTools:        namespaceTools,
		clientDeclaredTools:   clientDeclaredTools,
		filterInternalXSearch: filterInternalXSearch,
		droppedOutputIndexes:  make(map[int64]struct{}),
		droppedItemIDs:        make(map[string]struct{}),
	}
}

type xaiToolIdentityResponseNormalizer struct {
	namespaceTools        map[string]xaiNamespaceToolRef
	clientDeclaredTools   map[xaiClientToolKey]struct{}
	filterInternalXSearch bool
	droppedOutputIndexes  map[int64]struct{}
	droppedItemIDs        map[string]struct{}
	terminal              bool
}

func (n *xaiToolIdentityResponseNormalizer) normalize(event []byte) []byte {
	if n == nil || len(event) == 0 || !gjson.ValidBytes(event) {
		return event
	}
	if n.terminal {
		return nil
	}
	event = restoreXAINamespaceToolCalls(event, n.namespaceTools)
	if n.filterInternalXSearch {
		if item := gjson.GetBytes(event, "item"); n.isInternalXSearchCall(item) {
			n.recordDroppedItem(event, item)
			return nil
		}
		event = n.filterCompletedOutput(event)
		if n.referencesDroppedItem(event) {
			return nil
		}
		event = n.compactOutputIndex(event)
	}
	if err := n.validateResponseToolCalls(event); err != nil {
		n.terminal = true
		return invalidXAIToolCallEvent(err.Error())
	}
	return event
}

func (n *xaiToolIdentityResponseNormalizer) validateResponseToolCalls(event []byte) error {
	if item := gjson.GetBytes(event, "item"); item.Exists() {
		if err := n.validateResponseToolCall(item, "item.name"); err != nil {
			return err
		}
	}
	output := gjson.GetBytes(event, "response.output")
	if !output.IsArray() {
		return nil
	}
	for index, item := range output.Array() {
		if err := n.validateResponseToolCall(item, fmt.Sprintf("response.output[%d].name", index)); err != nil {
			return err
		}
	}
	return nil
}

func (n *xaiToolIdentityResponseNormalizer) validateResponseToolCall(item gjson.Result, path string) error {
	itemType := strings.TrimSpace(item.Get("type").String())
	if itemType != xaiFunctionToolType && itemType != "function_call" && itemType != "custom_tool_call" {
		return nil
	}
	name := item.Get("name").String()
	if !validateProviderToolName(name) {
		return fmt.Errorf(
			"xAI returned an invalid tool name at %s; expected 1-%d ASCII characters matching ^[a-zA-Z0-9_-]+$",
			path,
			providerToolNameMaxBytes,
		)
	}
	namespace := strings.TrimSpace(item.Get("namespace").String())
	if _, declared := n.clientDeclaredTools[xaiClientToolKey{
		namespace: namespace,
		name:      name,
		toolType:  xaiFunctionToolType,
	}]; !declared {
		return fmt.Errorf("xAI returned an undeclared tool call at %s", path)
	}
	return nil
}

func (n *xaiToolIdentityResponseNormalizer) isInternalXSearchCall(item gjson.Result) bool {
	itemType := strings.TrimSpace(item.Get("type").String())
	declaredType := ""
	switch itemType {
	case "function_call":
		declaredType = xaiFunctionToolType
	case "custom_tool_call":
		declaredType = xaiCustomToolType
	default:
		return false
	}
	name := strings.TrimSpace(item.Get("name").String())
	switch name {
	case "x_user_search", "x_semantic_search", "x_keyword_search", "x_thread_fetch":
	default:
		return false
	}
	if strings.TrimSpace(item.Get("namespace").String()) != "" {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(item.Get("call_id").String()), "xs_call") {
		return true
	}
	_, declared := n.clientDeclaredTools[xaiClientToolKey{name: name, toolType: declaredType}]
	return !declared
}

func (n *xaiToolIdentityResponseNormalizer) recordDroppedItem(event []byte, item gjson.Result) {
	if index := gjson.GetBytes(event, "output_index"); index.Exists() {
		n.droppedOutputIndexes[index.Int()] = struct{}{}
	}
	for _, path := range []string{"id", "call_id"} {
		if id := strings.TrimSpace(item.Get(path).String()); id != "" {
			n.droppedItemIDs[id] = struct{}{}
		}
	}
}

func (n *xaiToolIdentityResponseNormalizer) referencesDroppedItem(event []byte) bool {
	if index := gjson.GetBytes(event, "output_index"); index.Exists() {
		if _, dropped := n.droppedOutputIndexes[index.Int()]; dropped {
			return true
		}
	}
	for _, path := range []string{"item_id", "call_id"} {
		id := strings.TrimSpace(gjson.GetBytes(event, path).String())
		if _, dropped := n.droppedItemIDs[id]; id != "" && dropped {
			return true
		}
	}
	return false
}

func (n *xaiToolIdentityResponseNormalizer) compactOutputIndex(event []byte) []byte {
	index := gjson.GetBytes(event, "output_index")
	if !index.Exists() {
		return event
	}
	original := index.Int()
	removedBefore := int64(0)
	for dropped := range n.droppedOutputIndexes {
		if dropped < original {
			removedBefore++
		}
	}
	if removedBefore == 0 {
		return event
	}
	updated, err := sjson.SetBytes(event, "output_index", original-removedBefore)
	if err != nil {
		return event
	}
	return updated
}

func (n *xaiToolIdentityResponseNormalizer) filterCompletedOutput(event []byte) []byte {
	output := gjson.GetBytes(event, "response.output")
	if !output.IsArray() {
		return event
	}
	items := make([]json.RawMessage, 0, len(output.Array()))
	changed := false
	for _, item := range output.Array() {
		if n.isInternalXSearchCall(item) {
			changed = true
			continue
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	if !changed {
		return event
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return event
	}
	updated, err := sjson.SetRawBytes(event, "response.output", raw)
	if err != nil {
		return event
	}
	return updated
}

func restoreXAINamespaceToolCalls(data []byte, refs map[string]xaiNamespaceToolRef) []byte {
	if len(refs) == 0 || len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	data = restoreXAINamespaceToolCallAtPath(data, "item", refs)
	output := gjson.GetBytes(data, "response.output")
	if output.IsArray() {
		for index := range output.Array() {
			data = restoreXAINamespaceToolCallAtPath(data, fmt.Sprintf("response.output.%d", index), refs)
		}
	}
	return data
}

func restoreXAINamespaceToolCallAtPath(data []byte, path string, refs map[string]xaiNamespaceToolRef) []byte {
	if gjson.GetBytes(data, path+".type").String() != "function_call" {
		return data
	}
	qualifiedName := strings.TrimSpace(gjson.GetBytes(data, path+".name").String())
	ref, ok := refs[qualifiedName]
	if !ok {
		return data
	}
	updated, err := sjson.SetBytes(data, path+".name", ref.name)
	if err != nil {
		return data
	}
	updated, err = sjson.SetBytes(updated, path+".namespace", ref.namespace)
	if err != nil {
		return data
	}
	return updated
}
