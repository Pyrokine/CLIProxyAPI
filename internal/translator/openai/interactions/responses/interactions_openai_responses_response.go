package responses

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type interactionsToResponsesStreamState struct {
	ID                 string
	EnvironmentID      string
	Model              string
	FunctionCalls      map[int]*interactionsFunctionCallState
	ItemIDs            map[int]string
	ItemTypes          map[int]string
	ReasoningEncrypted map[int]string
	ReasoningSummaries map[int][]string
	TextOutputs        map[int]*strings.Builder
	Seq                int
	Completed          bool
	Done               bool
	ToolIdentity       *translatorcommon.ResponsesToolIdentity
	PatchTools         map[string]bool
	ReasoningDone      map[int]bool
	Err                error
}

func (st *interactionsToResponsesStreamState) ResponseTranslationError() error {
	return st.Err
}

type interactionsFunctionCallState struct {
	ID                      string
	CallID                  string
	Name                    string
	Arguments               strings.Builder
	InitialArgumentsEmitted bool
	ArgumentsDoneEmitted    bool
	ItemDoneEmitted         bool
	Custom                  bool
	Input                   string
	Snapshot                string
	SourceStopped           bool
	Added                   bool
	Pending                 [][]byte
	PendingError            error
}

type responsesToInteractionsStreamState struct {
	ID                     string
	EnvironmentID          string
	Model                  string
	ActiveThoughtSignature string
	Created                bool
	StatusUpdated          bool
	Completed              bool
	Done                   bool
	StepIndex              int
	ActiveStepIndex        int
	ActiveStepType         string
	ActiveStepOpen         bool
	SentText               map[string]bool
	UnkeyedTextDelta       bool
	FunctionCallIndexes    map[string]int
	FunctionArgsSent       map[string]bool
}

func convertInteractionsResponseToOpenAIResponses(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	_ = ctx
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &interactionsToResponsesStreamState{}
	}
	st := (*param).(*interactionsToResponsesStreamState)
	if st.Err != nil {
		return nil
	}
	if st.ToolIdentity == nil {
		st.ToolIdentity, st.PatchTools, st.Err = interactionsResponseToolIdentity(originalRequestRawJSON)
		if st.Err != nil {
			return nil
		}
	}
	if st.ReasoningDone == nil {
		st.ReasoningDone = make(map[int]bool)
	}
	if st.Model == "" {
		st.Model = modelName
	}
	if st.FunctionCalls == nil {
		st.FunctionCalls = make(map[int]*interactionsFunctionCallState)
	}
	if st.ItemIDs == nil {
		st.ItemIDs = make(map[int]string)
	}
	if st.ItemTypes == nil {
		st.ItemTypes = make(map[int]string)
	}
	if st.ReasoningEncrypted == nil {
		st.ReasoningEncrypted = make(map[int]string)
	}
	if st.ReasoningSummaries == nil {
		st.ReasoningSummaries = make(map[int][]string)
	}
	if st.TextOutputs == nil {
		st.TextOutputs = make(map[int]*strings.Builder)
	}
	events := convertInteractionsEventToResponses(
		modelName,
		originalRequestRawJSON,
		requestRawJSON,
		rawJSON,
		st,
	)
	if st.Err != nil {
		st.Completed = true
		failure, _ := sjson.SetBytes([]byte(`{"error":{"type":"server_error"}}`), "error.message", st.Err.Error())
		return [][]byte{responsesFailedEvent(modelName, gjson.ParseBytes(failure), st)}
	}
	return events
}

func convertInteractionsResponseToOpenAIResponsesNonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) []byte {
	_ = ctx
	identity, patchTools, err := interactionsResponseToolIdentity(originalRequestRawJSON)
	if err == nil && len(patchTools) > 0 && !gjson.ValidBytes(rawJSON) {
		err = fmt.Errorf("invalid Interactions apply_patch response JSON")
	}
	if err != nil {
		if param != nil {
			*param = &interactionsToResponsesStreamState{Err: err}
		}
		return nil
	}
	_ = requestRawJSON
	root := gjson.ParseBytes(rawJSON)
	out := []byte(`{"id":"","object":"response","status":"completed","model":"","output":[]}`)
	out, _ = sjson.SetBytes(out, "id", firstNonEmpty(root.Get("id").String(), root.Get("interaction.id").String()))
	model := responseModel(modelName, root)
	signatureModel := firstNonEmpty(root.Get("interaction.model").String(), root.Get("model").String(), model)
	out, _ = sjson.SetBytes(out, "model", model)
	if status := interactionTerminalStatus(root); status != "" {
		out, _ = sjson.SetBytes(out, "status", status)
	}
	out = copyInteractionIncompleteDetailsToResponse(out, root, "incomplete_details")
	steps := root.Get("steps")
	if !steps.Exists() {
		steps = root.Get("interaction.steps")
	}
	forAntigravity := isAntigravityModel(model)
	steps.ForEach(
		func(_, step gjson.Result) bool {
			if item, ok := interactionsStepToResponsesOutput(step, signatureModel, forAntigravity); ok {
				item = restoreInteractionsResponseTool(item, identity, "")
				out, _ = sjson.SetRawBytes(out, "output.-1", item)
			}
			return true
		},
	)
	if environmentID := interactionEnvironmentID(root); environmentID != "" {
		out, _ = sjson.SetBytes(out, "environment_id", environmentID)
	}
	out = setResponsesUsageFromInteractions(out, "usage", translatorcommon.InteractionsUsage(root))
	return out
}

func interactionsResponseToolIdentity(request []byte) (*translatorcommon.ResponsesToolIdentity, map[string]bool, error) {
	identity := translatorcommon.NewResponsesToolIdentity(request)
	var patchTools map[string]bool
	for _, declaration := range identity.Declarations() {
		if declaration.Kind == translatorcommon.ResponsesCustomTool && declaration.Name == "apply_patch" {
			if patchTools == nil {
				patchTools = make(map[string]bool)
			}
			patchTools[declaration.QualifiedName] = true
		}
	}
	if len(patchTools) == 0 {
		return identity, nil, nil
	}
	normalized, err := translatorcommon.NormalizeResponsesPatchTools(request)
	if err != nil {
		return nil, patchTools, err
	}
	return translatorcommon.NewResponsesToolIdentity(normalized), patchTools, nil
}

func interactionsSourceStepIndex(explicit, step gjson.Result, fallback int, st *interactionsToResponsesStreamState) int {
	if len(st.PatchTools) == 0 {
		return int(explicit.Int())
	}
	index, indexed := fallback, false
	for _, value := range []gjson.Result{explicit, step.Get("index")} {
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.Number || value.Int() < 0 || value.Float() != float64(value.Int()) || (indexed && index != int(value.Int())) {
			st.Err = fmt.Errorf("invalid or conflicting Interactions source index")
			return index
		}
		index, indexed = int(value.Int()), true
	}
	matched := -1
	itemID, callID := step.Get("id").String(), step.Get("call_id").String()
	for candidate, id := range st.ItemIDs {
		if itemID != "" && id == itemID {
			if (matched >= 0 && matched != candidate) || (indexed && index != candidate) {
				st.Err = fmt.Errorf("conflicting Interactions source item aliases")
				return index
			}
			matched = candidate
		}
	}
	for candidate, call := range st.FunctionCalls {
		if (itemID != "" && itemID == call.ID) || (callID != "" && callID == call.CallID) {
			if (matched >= 0 && matched != candidate) || (indexed && index != candidate) {
				st.Err = fmt.Errorf("conflicting Interactions source aliases")
				return index
			}
			matched = candidate
		}
	}
	if !indexed && matched >= 0 {
		return matched
	}
	if !indexed && (itemID != "" || callID != "") {
		if call := st.FunctionCalls[index]; call != nil &&
			(itemID == "" || call.ID == "" || itemID == call.ID) &&
			(callID == "" || call.CallID == "" || callID == call.CallID) {
			return index
		}
		for st.ItemTypes[index] != "" {
			index++
		}
	}
	return index
}

func updateInteractionsFunctionCall(index int, step gjson.Result, st *interactionsToResponsesStreamState, initial bool) [][]byte {
	if kind := st.ItemTypes[index]; kind != "" && kind != "function_call" {
		st.Err = fmt.Errorf("conflicting Interactions function call source type")
		return nil
	}
	call := st.FunctionCalls[index]
	if call == nil {
		call = &interactionsFunctionCallState{}
		st.FunctionCalls[index] = call
	}
	if kind := step.Get("type"); kind.Exists() && (kind.Type != gjson.String || kind.String() != "function_call") {
		st.Err = fmt.Errorf("conflicting Interactions function call type")
		return nil
	}
	for _, field := range []struct {
		name   string
		target *string
	}{{"id", &call.ID}, {"call_id", &call.CallID}, {"name", &call.Name}} {
		value := step.Get(field.name)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.String || (value.String() != "" && strings.TrimSpace(value.String()) == "") {
			st.Err = fmt.Errorf("invalid Interactions function call %s", field.name)
			return nil
		}
		text := value.String()
		if field.name == "name" {
			if isAntigravityModel(st.Model) {
				text = translatorcommon.AntigravityUpstreamToolNameToClient(text)
			}
			if declaration, err := st.ToolIdentity.ResolveDeclaration(text, ""); err == nil {
				text = declaration.QualifiedName
			}
		}
		if text != "" {
			if *field.target != "" && *field.target != text {
				st.Err = fmt.Errorf("conflicting Interactions function call %s", field.name)
				return nil
			}
			*field.target = text
		}
	}
	if st.PatchTools[call.Name] && call.PendingError != nil {
		st.Err = call.PendingError
		return nil
	}
	args := step.Get("arguments")
	if initial && !call.Added && call.Snapshot == "" && call.Arguments.Len() == 0 && strings.TrimSpace(args.Raw) == "{}" {
		args = gjson.Result{}
	}
	if args.Exists() {
		call.Snapshot = jsonStringValue(args, "{}")
		if call.Snapshot == "" {
			err := fmt.Errorf("empty Interactions apply_patch source snapshot")
			if st.PatchTools[call.Name] {
				st.Err = err
				return nil
			}
			if call.Name == "" {
				call.PendingError = err
			}
		}
	}
	st.ItemTypes[index] = "function_call"
	st.ItemIDs[index] = call.ID
	added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","status":"in_progress","arguments":""}}`)
	added, _ = sjson.SetBytes(added, "sequence_number", nextResponsesSeq(st))
	added, _ = sjson.SetBytes(added, "output_index", index)
	added, _ = sjson.SetBytes(added, "item.id", call.ID)
	added, _ = sjson.SetBytes(added, "item.call_id", call.CallID)
	added, _ = sjson.SetBytes(added, "item.name", call.Name)
	if args.Exists() {
		added, _ = translatorcommon.SetStringWithoutHTMLEscape(added, "item.arguments", call.Snapshot)
	}
	if call.Name == "" {
		call.Pending = append(call.Pending, emitResponsesEvent("response.output_item.added", added))
		return nil
	}
	call.Custom = st.ToolIdentity.IsCustom(call.Name)
	if !st.PatchTools[call.Name] {
		snapshot := call.Snapshot
		call.Snapshot = ""
		if call.Added {
			return nil
		}
		call.ID = firstNonEmpty(call.ID, call.CallID, fmt.Sprintf("item_%d", index))
		call.CallID = firstNonEmpty(call.CallID, call.ID)
		st.ItemIDs[index] = call.ID
		added, _ = sjson.SetBytes(added, "item.id", call.ID)
		added, _ = sjson.SetBytes(added, "item.call_id", call.CallID)
		added, _ = sjson.SetBytes(added, "item.arguments", "")
		added = restoreInteractionsResponseTool(added, st.ToolIdentity, "item")
		call.Added = true
		call.Pending = nil
		call.PendingError = nil
		events := [][]byte{emitResponsesEvent("response.output_item.added", added)}
		if snapshot != "" {
			fragments := call.Arguments.String()
			call.Arguments.Reset()
			call.Arguments.WriteString(snapshot)
			call.Arguments.WriteString(fragments)
		}
		if call.Arguments.Len() > 0 {
			if event := responsesFunctionCallArgumentsDeltaToResponses(index, call.ID, call.Arguments.String(), st); len(event) > 0 {
				events = append(events, event)
			}
		}
		return events
	}
	call.Added = true
	events := [][]byte{emitResponsesEvent("response.output_item.added", added)}
	for _, pending := range call.Pending {
		payload := interactionsSSEPayload(pending)
		kind := gjson.GetBytes(payload, "type").String()
		if kind == "response.output_item.added" {
			payload, _ = sjson.SetBytes(payload, "item.id", call.ID)
			payload, _ = sjson.SetBytes(payload, "item.call_id", call.CallID)
			payload, _ = sjson.SetBytes(payload, "item.name", call.Name)
		} else {
			payload, _ = sjson.SetBytes(payload, "item_id", call.ID)
		}
		events = append(events, emitResponsesEvent(kind, payload))
	}
	call.Pending = nil
	return events
}

func restoreInteractionsResponseTool(item []byte, identity *translatorcommon.ResponsesToolIdentity, itemPath string) []byte {
	prefix := itemPath
	if prefix != "" {
		prefix += "."
	}
	if gjson.GetBytes(item, prefix+"type").String() != "function_call" {
		return item
	}
	name := gjson.GetBytes(item, prefix+"name").String()
	item = identity.SetToolCallIdentity(item, name, itemPath)
	if identity.IsCustom(name) {
		arguments := gjson.GetBytes(item, prefix+"arguments").String()
		item, _ = sjson.SetBytes(item, prefix+"type", "custom_tool_call")
		item, _ = translatorcommon.SetStringWithoutHTMLEscape(item, prefix+"input", translatorcommon.UnwrapResponsesCustomToolInput(arguments))
		item, _ = sjson.DeleteBytes(item, prefix+"arguments")
	}
	return item
}

func convertInteractionsEventToResponses(
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	st *interactionsToResponsesStreamState,
) [][]byte {
	payload := interactionsSSEPayload(rawJSON)
	if len(payload) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		if st.Done {
			return nil
		}
		st.Done = true
		return [][]byte{[]byte("data: [DONE]\n\n")}
	}
	if len(st.PatchTools) > 0 && !gjson.ValidBytes(payload) {
		st.Err = fmt.Errorf("invalid Interactions apply_patch event JSON")
		return nil
	}
	root := gjson.ParseBytes(payload)
	if !root.Exists() {
		return nil
	}
	eventType := root.Get("event_type").String()
	if st.Completed && eventType != "done" {
		return nil
	}
	switch eventType {
	case "interaction.created":
		return [][]byte{
			responsesCreatedEvent(
				modelName,
				originalRequestRawJSON,
				requestRawJSON,
				root,
				st,
			),
		}
	case "step.start":
		return interactionsStepStartToResponses(modelName, root, st)
	case "step.delta":
		return interactionsStepDeltaToResponses(root, st)
	case "step.stop":
		return interactionsStepStopToResponses(root, st)
	case "interaction.completed", "finish":
		var events [][]byte
		if len(st.PatchTools) > 0 {
			steps := root.Get("interaction.steps")
			if !steps.Exists() {
				steps = root.Get("steps")
			}
			for position, step := range steps.Array() {
				index := interactionsSourceStepIndex(gjson.Result{}, step, position, st)
				if st.Err != nil {
					return nil
				}
				if step.Get("type").String() == "function_call" || st.FunctionCalls[index] != nil {
					events = append(events, updateInteractionsFunctionCall(index, step, st, false)...)
				}
			}
			indexes := make([]int, 0, len(st.FunctionCalls))
			for index := range st.FunctionCalls {
				indexes = append(indexes, index)
			}
			slices.Sort(indexes)
			for _, index := range indexes {
				call := st.FunctionCalls[index]
				if call.Name == "" {
					st.Err = fmt.Errorf("unresolved Interactions apply_patch call identity")
					return nil
				}
				if !st.PatchTools[call.Name] {
					continue
				}
				call.ID = firstNonEmpty(call.ID, call.CallID, fmt.Sprintf("item_%d", index))
				call.CallID = firstNonEmpty(call.CallID, call.ID)
				st.ItemIDs[index] = call.ID
				events = append(events, updateInteractionsFunctionCall(index, gjson.Result{}, st, false)...)
				stop := gjson.Parse(fmt.Sprintf(`{"index":%d}`, index))
				events = append(events, interactionsStepStopToResponses(stop, st)...)
			}
			if st.Err != nil {
				return nil
			}
		}
		if event := responsesCompletedEvent(modelName, root, st); len(event) > 0 {
			events = append(events, event)
		}
		return events
	case "response.failed", "interaction.failed":
		if len(st.PatchTools) > 0 {
			st.Err = fmt.Errorf("upstream Interactions apply_patch execution failed")
		}
		st.Completed = true
		return [][]byte{responsesFailedEvent(modelName, root, st)}
	case "done":
		if st.Done {
			return nil
		}
		st.Done = true
		return [][]byte{[]byte("data: [DONE]\n\n")}
	}
	return nil
}

func interactionsStepToResponsesOutput(step gjson.Result, modelName string, forAntigravity bool) ([]byte, bool) {
	switch step.Get("type").String() {
	case "model_output":
		item := []byte(`{"type":"message","status":"completed","role":"assistant","content":[]}`)
		if id := firstNonEmpty(step.Get("id").String(), step.Get("step_id").String()); id != "" {
			item, _ = sjson.SetBytes(item, "id", id)
		}
		content := step.Get("content")
		if content.Type == gjson.String {
			part := []byte(`{"type":"output_text","text":""}`)
			part, _ = sjson.SetBytes(part, "text", content.String())
			item, _ = sjson.SetRawBytes(item, "content.-1", part)
		} else {
			content.ForEach(
				func(_, part gjson.Result) bool {
					if converted, ok := interactionsContentPartToResponses(part, "assistant"); ok {
						item, _ = sjson.SetRawBytes(item, "content.-1", converted)
					}
					return true
				},
			)
		}
		return item, true
	case "thought":
		item := []byte(`{"type":"reasoning","status":"completed","summary":[]}`)
		if signature := interactionsThoughtSignature(step, modelName); signature != "" {
			item, _ = sjson.SetBytes(item, "encrypted_content", signature)
		}
		for _, text := range interactionsContentTexts(step.Get("content")) {
			part := []byte(`{"type":"summary_text","text":""}`)
			part, _ = sjson.SetBytes(part, "text", text)
			item, _ = sjson.SetRawBytes(item, "summary.-1", part)
		}
		return item, true
	case "function_call":
		item := interactionsFunctionCallToResponses(step, forAntigravity)
		item, _ = sjson.SetBytes(item, "status", "completed")
		return item, true
	}
	return nil, false
}

func responsesCreatedEvent(
	modelName string,
	originalRequestRawJSON, requestRawJSON []byte,
	root gjson.Result,
	st *interactionsToResponsesStreamState,
) []byte {
	payload := []byte(`{"type":"response.created","response":{"id":"","object":"response","status":"in_progress","model":"","output":[]}}`)
	payload, _ = sjson.SetBytes(payload, "sequence_number", nextResponsesSeq(st))
	st.ID = firstNonEmpty(root.Get("interaction.id").String(), root.Get("id").String(), st.ID)
	payload, _ = sjson.SetBytes(payload, "response.id", st.ID)
	if environmentID := interactionEnvironmentID(root); environmentID != "" {
		st.EnvironmentID = environmentID
		payload, _ = sjson.SetBytes(payload, "response.environment_id", environmentID)
	}
	requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
	if requestModelName == "" {
		requestModelName = modelName
	}
	st.Model = firstNonEmpty(root.Get("interaction.model").String(), root.Get("model").String(), st.Model, requestModelName)
	if requestModelName != "" {
		payload, _ = sjson.SetBytes(payload, "response.model", requestModelName)
	}
	return emitResponsesEvent("response.created", payload)
}

func interactionsStepStartToResponses(
	modelName string,
	root gjson.Result,
	st *interactionsToResponsesStreamState,
) [][]byte {
	step := root.Get("step")
	index := interactionsSourceStepIndex(root.Get("index"), step, 0, st)
	if st.Err != nil {
		return nil
	}
	stepType := step.Get("type").String()
	if len(st.PatchTools) > 0 && (stepType == "function_call" || st.FunctionCalls[index] != nil) {
		events := updateInteractionsFunctionCall(index, step, st, true)
		if call := st.FunctionCalls[index]; st.Err == nil && call.SourceStopped && call.Name != "" {
			events = append(events, interactionsStepStopToResponses(gjson.Parse(fmt.Sprintf(`{"index":%d}`, index)), st)...)
		}
		return events
	}
	name := step.Get("name").String()
	if isAntigravityModel(modelName) {
		name = translatorcommon.AntigravityUpstreamToolNameToClient(name)
	}
	if call := st.FunctionCalls[index]; stepType == "function_call" && call != nil {
		itemID, callID := step.Get("id").String(), step.Get("call_id").String()
		if itemID != "" && itemID != call.ID || callID != "" && callID != call.CallID || name != "" && name != call.Name {
			st.Err = fmt.Errorf("conflicting Interactions function call identity at index %d", index)
			st.Completed = true
			failure, _ := sjson.SetBytes([]byte(`{"error":{"type":"server_error"}}`), "error.message", st.Err.Error())
			return [][]byte{responsesFailedEvent(modelName, gjson.ParseBytes(failure), st)}
		}
		return nil
	}
	itemID := firstNonEmpty(step.Get("id").String(), step.Get("call_id").String(), fmt.Sprintf("item_%d", index))
	callID := firstNonEmpty(step.Get("call_id").String(), step.Get("id").String(), itemID)
	st.ItemIDs[index] = itemID
	st.ItemTypes[index] = stepType
	switch stepType {
	case "model_output":
		added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"","type":"message","status":"in_progress","role":"assistant","content":[]}}`)
		added, _ = sjson.SetBytes(added, "sequence_number", nextResponsesSeq(st))
		added, _ = sjson.SetBytes(added, "output_index", index)
		added, _ = sjson.SetBytes(added, "item.id", itemID)
		part := []byte(`{"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"","part":{"type":"output_text","text":""}}`)
		part, _ = sjson.SetBytes(part, "sequence_number", nextResponsesSeq(st))
		part, _ = sjson.SetBytes(part, "output_index", index)
		part, _ = sjson.SetBytes(part, "item_id", itemID)
		return [][]byte{
			emitResponsesEvent("response.output_item.added", added),
			emitResponsesEvent("response.content_part.added", part),
		}
	case "thought":
		if signature := interactionsThoughtSignature(step, firstNonEmpty(st.Model, modelName)); signature != "" {
			st.ReasoningEncrypted[index] = signature
		}
		added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","encrypted_content":"","summary":[]}}`)
		added, _ = sjson.SetBytes(added, "sequence_number", nextResponsesSeq(st))
		added, _ = sjson.SetBytes(added, "output_index", index)
		added, _ = sjson.SetBytes(added, "item.id", itemID)
		if signature := st.ReasoningEncrypted[index]; signature != "" {
			added, _ = sjson.SetBytes(added, "item.encrypted_content", signature)
		}
		return [][]byte{
			emitResponsesEvent("response.output_item.added", added),
			responsesReasoningSummaryEvent("response.reasoning_summary_part.added", index, "", st),
		}
	case "function_call":
		call := &interactionsFunctionCallState{
			ID:     itemID,
			CallID: callID,
			Name:   name,
			Custom: st.ToolIdentity.IsCustom(name),
		}
		if args := step.Get("arguments"); args.Exists() && strings.TrimSpace(args.Raw) != "{}" {
			call.Arguments.WriteString(jsonStringValue(args, "{}"))
		}
		st.FunctionCalls[index] = call
		added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"","type":"function_call","status":"in_progress","call_id":"","name":"","arguments":""}}`)
		added, _ = sjson.SetBytes(added, "sequence_number", nextResponsesSeq(st))
		added, _ = sjson.SetBytes(added, "output_index", index)
		added, _ = sjson.SetBytes(added, "item.id", itemID)
		added, _ = sjson.SetBytes(added, "item.call_id", callID)
		added, _ = sjson.SetBytes(added, "item.name", call.Name)
		added = restoreInteractionsResponseTool(added, st.ToolIdentity, "item")
		events := [][]byte{emitResponsesEvent("response.output_item.added", added)}
		if call.Arguments.Len() > 0 && !call.InitialArgumentsEmitted {
			events = append(
				events, responsesFunctionCallArgumentsDeltaToResponses(index, itemID, call.Arguments.String(), st),
			)
			call.InitialArgumentsEmitted = true
		}
		return events
	}
	return nil
}

func interactionsStepDeltaToResponses(root gjson.Result, st *interactionsToResponsesStreamState) [][]byte {
	index := interactionsSourceStepIndex(root.Get("index"), root.Get("step"), 0, st)
	if st.Err != nil {
		return nil
	}
	delta := root.Get("delta")
	var events [][]byte
	if len(st.PatchTools) > 0 {
		call := st.FunctionCalls[index]
		if call != nil && call.SourceStopped && delta.Get("type").String() == "arguments_delta" && delta.Get("arguments").String() != "" {
			err := fmt.Errorf("interactions apply_patch delta after source stop")
			if st.PatchTools[call.Name] || st.PatchTools[root.Get("step.name").String()] {
				st.Err = err
				return nil
			}
			if call.Name == "" {
				call.PendingError = err
				return nil
			}
		}
		if step := root.Get("step"); step.IsObject() && (step.Get("type").String() == "function_call" || call != nil) {
			events = updateInteractionsFunctionCall(index, step, st, false)
			if st.Err != nil {
				return nil
			}
		}
	}
	switch delta.Get("type").String() {
	case "thought_summary":
		text := firstNonEmpty(delta.Get("content.text").String(), delta.Get("text").String())
		if st.ReasoningDone[index] {
			return nil
		}
		recordResponsesReasoningSummary(st, index, text)
		return [][]byte{responsesReasoningSummaryEvent("response.reasoning_summary_text.delta", index, text, st)}
	case "thought_signature":
		if signature := interactionsThoughtSignature(delta, st.Model); signature != "" {
			st.ReasoningEncrypted[index] = signature
		}
		return nil
	case "arguments_delta":
		call := st.FunctionCalls[index]
		if call == nil && len(st.PatchTools) > 0 {
			call = &interactionsFunctionCallState{}
			st.FunctionCalls[index] = call
			st.ItemTypes[index] = "function_call"
		}
		if call == nil || call.ItemDoneEmitted {
			return events
		}
		if len(st.PatchTools) > 0 && delta.Get("invalid_json_str").Exists() {
			err := fmt.Errorf("legacy freeform arguments are invalid for Interactions apply_patch")
			if st.PatchTools[call.Name] {
				st.Err = err
				return nil
			}
			if call.Name == "" {
				call.PendingError = err
			}
		}
		arguments := delta.Get("arguments").String()
		call.Arguments.WriteString(arguments)
		if event := responsesFunctionCallArgumentsDeltaToResponses(index, call.ID, arguments, st); len(event) > 0 {
			if len(st.PatchTools) > 0 && call.Name == "" {
				call.Pending = append(call.Pending, event)
				return events
			}
			events = append(events, event)
		}
		return events
	default:
		payload := []byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"","delta":""}`)
		payload, _ = sjson.SetBytes(payload, "sequence_number", nextResponsesSeq(st))
		payload, _ = sjson.SetBytes(payload, "output_index", index)
		payload, _ = sjson.SetBytes(payload, "item_id", st.ItemIDs[index])
		text := delta.Get("text").String()
		recordResponsesTextOutput(st, index, text)
		payload, _ = sjson.SetBytes(payload, "delta", text)
		return [][]byte{emitResponsesEvent("response.output_text.delta", payload)}
	}
}

func responsesFunctionCallArgumentsDeltaToResponses(
	index int,
	itemID, arguments string,
	st *interactionsToResponsesStreamState,
) []byte {
	kind := "response.function_call_arguments.delta"
	if call := st.FunctionCalls[index]; call != nil && call.Custom {
		input, ok := translatorcommon.ParseResponsesCustomToolInput(call.Arguments.String())
		if !ok || input == call.Input {
			return nil
		}
		if !strings.HasPrefix(input, call.Input) {
			st.Err = fmt.Errorf("interactions custom tool input changed at index %d", index)
			return nil
		}
		arguments = strings.TrimPrefix(input, call.Input)
		call.Input = input
		kind = "response.custom_tool_call_input.delta"
	}
	payload := []byte(`{"type":"","output_index":0,"item_id":"","delta":""}`)
	payload, _ = sjson.SetBytes(payload, "type", kind)
	payload, _ = sjson.SetBytes(payload, "sequence_number", nextResponsesSeq(st))
	payload, _ = sjson.SetBytes(payload, "output_index", index)
	payload, _ = sjson.SetBytes(payload, "item_id", itemID)
	payload, _ = translatorcommon.SetStringWithoutHTMLEscape(payload, "delta", arguments)
	return emitResponsesEvent(kind, payload)
}

func responsesFunctionCallArgumentsDoneToResponses(
	index int,
	itemID, arguments string,
	st *interactionsToResponsesStreamState,
) []byte {
	kind, field := "response.function_call_arguments.done", "arguments"
	if call := st.FunctionCalls[index]; call != nil && call.Custom {
		kind, field = "response.custom_tool_call_input.done", "input"
		arguments = translatorcommon.UnwrapResponsesCustomToolInput(arguments)
	}
	payload := []byte(`{"type":"","output_index":0,"item_id":""}`)
	payload, _ = sjson.SetBytes(payload, "type", kind)
	payload, _ = sjson.SetBytes(payload, "sequence_number", nextResponsesSeq(st))
	payload, _ = sjson.SetBytes(payload, "output_index", index)
	payload, _ = sjson.SetBytes(payload, "item_id", itemID)
	payload, _ = translatorcommon.SetStringWithoutHTMLEscape(payload, field, arguments)
	return emitResponsesEvent(kind, payload)
}

func responsesReasoningSummaryEvent(kind string, index int, text string, st *interactionsToResponsesStreamState) []byte {
	payload := []byte(`{"type":"","output_index":0,"item_id":"","summary_index":0}`)
	payload, _ = sjson.SetBytes(payload, "type", kind)
	payload, _ = sjson.SetBytes(payload, "sequence_number", nextResponsesSeq(st))
	payload, _ = sjson.SetBytes(payload, "output_index", index)
	payload, _ = sjson.SetBytes(payload, "item_id", st.ItemIDs[index])
	switch kind {
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		payload, _ = sjson.SetRawBytes(payload, "part", []byte(`{"type":"summary_text","text":""}`))
		payload, _ = sjson.SetBytes(payload, "part.text", text)
	case "response.reasoning_summary_text.delta":
		payload, _ = sjson.SetBytes(payload, "delta", text)
	case "response.reasoning_summary_text.done":
		payload, _ = sjson.SetBytes(payload, "text", text)
	}
	return emitResponsesEvent(kind, payload)
}

func interactionsStepStopToResponses(root gjson.Result, st *interactionsToResponsesStreamState) [][]byte {
	index := interactionsSourceStepIndex(root.Get("index"), root.Get("step"), 0, st)
	if st.Err != nil {
		return nil
	}
	var updates [][]byte
	if len(st.PatchTools) > 0 {
		if step := root.Get("step"); step.IsObject() && (step.Get("type").String() == "function_call" || st.FunctionCalls[index] != nil) {
			updates = updateInteractionsFunctionCall(index, step, st, false)
			if st.Err != nil {
				return nil
			}
		}
		if st.FunctionCalls[index] == nil && st.ItemTypes[index] == "" {
			st.FunctionCalls[index] = &interactionsFunctionCallState{}
			st.ItemTypes[index] = "function_call"
		}
		if call := st.FunctionCalls[index]; call != nil {
			call.SourceStopped = true
			if call.Name == "" {
				return updates
			}
		}
	}
	itemID := st.ItemIDs[index]
	switch st.ItemTypes[index] {
	case "model_output":
		text := ""
		if builder := st.TextOutputs[index]; builder != nil {
			text = builder.String()
		}
		textDone := []byte(`{"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"","text":"","logprobs":[]}`)
		textDone, _ = sjson.SetBytes(textDone, "sequence_number", nextResponsesSeq(st))
		textDone, _ = sjson.SetBytes(textDone, "output_index", index)
		textDone, _ = sjson.SetBytes(textDone, "item_id", itemID)
		textDone, _ = sjson.SetBytes(textDone, "text", text)
		part := []byte(`{"type":"response.content_part.done","output_index":0,"content_index":0,"item_id":"","part":{"type":"output_text","text":""}}`)
		part, _ = sjson.SetBytes(part, "sequence_number", nextResponsesSeq(st))
		part, _ = sjson.SetBytes(part, "output_index", index)
		part, _ = sjson.SetBytes(part, "item_id", itemID)
		part, _ = sjson.SetBytes(part, "part.text", text)
		done := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"","type":"message","status":"completed","role":"assistant","content":[]}}`)
		done, _ = sjson.SetBytes(done, "sequence_number", nextResponsesSeq(st))
		done, _ = sjson.SetBytes(done, "output_index", index)
		done, _ = sjson.SetBytes(done, "item.id", itemID)
		outputText := []byte(`{"type":"output_text","text":""}`)
		outputText, _ = sjson.SetBytes(outputText, "text", text)
		done, _ = sjson.SetRawBytes(done, "item.content.-1", outputText)
		return [][]byte{
			emitResponsesEvent("response.output_text.done", textDone),
			emitResponsesEvent("response.content_part.done", part),
			emitResponsesEvent("response.output_item.done", done),
		}
	case "thought":
		if st.ReasoningDone[index] {
			return nil
		}
		st.ReasoningDone[index] = true
		text := strings.Join(st.ReasoningSummaries[index], "")
		textDone := responsesReasoningSummaryEvent("response.reasoning_summary_text.done", index, text, st)
		partDone := responsesReasoningSummaryEvent("response.reasoning_summary_part.done", index, text, st)
		done := []byte(`{"type":"response.output_item.done","output_index":0,"item":{}}`)
		done, _ = sjson.SetBytes(done, "sequence_number", nextResponsesSeq(st))
		done, _ = sjson.SetBytes(done, "output_index", index)
		done, _ = sjson.SetRawBytes(done, "item", responsesReasoningItem(index, st))
		return [][]byte{textDone, partDone, emitResponsesEvent("response.output_item.done", done)}
	case "function_call":
		call := st.FunctionCalls[index]
		if call == nil {
			call = &interactionsFunctionCallState{ID: itemID, CallID: itemID}
			st.FunctionCalls[index] = call
		}
		if call.ItemDoneEmitted {
			return updates
		}
		arguments := responsesFunctionCallArguments(call)
		events := updates
		if !call.ArgumentsDoneEmitted {
			events = append(events, responsesFunctionCallArgumentsDoneToResponses(index, itemID, arguments, st))
			call.ArgumentsDoneEmitted = true
		}
		done := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"","type":"function_call","status":"completed","call_id":"","name":"","arguments":""}}`)
		done, _ = sjson.SetBytes(done, "sequence_number", nextResponsesSeq(st))
		done, _ = sjson.SetBytes(done, "output_index", index)
		done, _ = sjson.SetBytes(done, "item.id", itemID)
		done, _ = sjson.SetBytes(done, "item.call_id", call.CallID)
		done, _ = sjson.SetBytes(done, "item.name", call.Name)
		done, _ = translatorcommon.SetStringWithoutHTMLEscape(done, "item.arguments", arguments)
		done = restoreInteractionsResponseTool(done, st.ToolIdentity, "item")
		call.ItemDoneEmitted = true
		return append(events, emitResponsesEvent("response.output_item.done", done))
	default:
		done := []byte(`{"type":"response.output_item.done","output_index":0,"item":{}}`)
		done, _ = sjson.SetBytes(done, "sequence_number", nextResponsesSeq(st))
		done, _ = sjson.SetBytes(done, "output_index", index)
		done, _ = sjson.SetRawBytes(done, "item", responsesReasoningItem(index, st))
		return [][]byte{emitResponsesEvent("response.output_item.done", done)}
	}
}

func responsesCompletedEvent(modelName string, root gjson.Result, st *interactionsToResponsesStreamState) []byte {
	if st.Completed {
		return nil
	}
	status := interactionTerminalStatus(root)
	if status == "" {
		status = "completed"
	}
	eventType := "response.completed"
	if status == "incomplete" {
		eventType = "response.incomplete"
	}
	payload := []byte(`{"type":"","response":{"id":"","object":"response","status":"","model":"","output":[],"usage":{}}}`)
	payload, _ = sjson.SetBytes(payload, "type", eventType)
	payload, _ = sjson.SetBytes(payload, "sequence_number", nextResponsesSeq(st))
	interaction := root.Get("interaction")
	payload, _ = sjson.SetBytes(
		payload, "response.id", firstNonEmpty(interaction.Get("id").String(), root.Get("id").String()),
	)
	payload, _ = sjson.SetBytes(payload, "response.status", status)
	st.Model = firstNonEmpty(interaction.Get("model").String(), root.Get("model").String(), st.Model, modelName)
	payload, _ = sjson.SetBytes(payload, "response.model", firstNonEmpty(interaction.Get("model").String(), modelName))
	payload = copyInteractionIncompleteDetailsToResponse(payload, root, "response.incomplete_details")
	if environmentID := firstNonEmpty(interactionEnvironmentID(root), st.EnvironmentID); environmentID != "" {
		st.EnvironmentID = environmentID
		payload, _ = sjson.SetBytes(payload, "response.environment_id", environmentID)
	}
	payload = setResponsesCompletedOutput(payload, st)
	payload = setResponsesUsageFromInteractions(payload, "response.usage", translatorcommon.InteractionsUsage(root))
	st.Completed = true
	return emitResponsesEvent(eventType, payload)
}

func responsesFailedEvent(modelName string, root gjson.Result, st *interactionsToResponsesStreamState) []byte {
	payload := []byte(`{"type":"response.failed","response":{"id":"","object":"response","status":"failed","model":"","output":[],"error":{"type":"server_error","message":""}}}`)
	payload, _ = sjson.SetBytes(payload, "sequence_number", nextResponsesSeq(st))
	st.ID = firstNonEmpty(root.Get("interaction.id").String(), root.Get("response.id").String(), root.Get("interaction_id").String(), root.Get("id").String(), st.ID)
	payload, _ = sjson.SetBytes(payload, "response.id", st.ID)
	st.Model = firstNonEmpty(root.Get("interaction.model").String(), root.Get("response.model").String(), st.Model, modelName)
	payload, _ = sjson.SetBytes(payload, "response.model", st.Model)
	if environmentID := firstNonEmpty(interactionEnvironmentID(root), st.EnvironmentID); environmentID != "" {
		payload, _ = sjson.SetBytes(payload, "response.environment_id", environmentID)
	}
	errNode := root.Get("error")
	if !errNode.Exists() {
		errNode = root.Get("interaction.error")
	}
	if !errNode.Exists() {
		errNode = root.Get("response.error")
	}
	message := errNode.Get("message").String()
	if message == "" {
		message = "upstream execution failed"
	}
	payload, _ = sjson.SetBytes(payload, "response.error.message", message)
	if errType := errNode.Get("type").String(); errType != "" {
		payload, _ = sjson.SetBytes(payload, "response.error.type", errType)
	}
	if code := errNode.Get("code").String(); code != "" {
		payload, _ = sjson.SetBytes(payload, "response.error.code", code)
	}
	return emitResponsesEvent("response.failed", payload)
}

func interactionsThoughtSignature(step gjson.Result, modelName string) string {
	for _, path := range []string{
		"encrypted_content",
		"signature",
		"thought_signature",
		"thoughtSignature",
		"extra_content.google.thought_signature",
	} {
		if signature := compatibleResponsesInteractionSignature(modelName, step.Get(path).String()); signature != "" {
			return signature
		}
	}
	content := step.Get("content")
	if !content.IsArray() {
		return ""
	}
	var signature string
	content.ForEach(
		func(_, part gjson.Result) bool {
			for _, path := range []string{
				"signature",
				"thought_signature",
				"thoughtSignature",
				"extra_content.google.thought_signature",
			} {
				signature = compatibleResponsesInteractionSignature(modelName, part.Get(path).String())
				if signature != "" {
					return false
				}
			}
			return true
		},
	)
	return signature
}

func compatibleResponsesInteractionSignature(modelName, rawSignature string) string {
	rawSignature = strings.TrimSpace(rawSignature)
	if rawSignature == "" {
		return ""
	}
	provider := sigcompat.ProviderFromModelName(modelName)
	switch provider {
	case sigcompat.ProviderClaude, sigcompat.ProviderGemini, sigcompat.ProviderGPT:
	default:
		provider = sigcompat.DetectProvider(rawSignature)
	}
	blockKind := sigcompat.BlockKindGPTReasoning
	switch provider {
	case sigcompat.ProviderClaude:
		blockKind = sigcompat.BlockKindClaudeThinking
	case sigcompat.ProviderGemini:
		blockKind = sigcompat.BlockKindGeminiModelPart
	}
	normalized, compatible := sigcompat.CompatibleSignatureForProvider(provider, rawSignature, blockKind)
	if !compatible {
		return ""
	}
	return normalized
}

func recordResponsesReasoningSummary(st *interactionsToResponsesStreamState, index int, text string) {
	if text == "" {
		return
	}
	st.ReasoningSummaries[index] = append(st.ReasoningSummaries[index], text)
}

func recordResponsesTextOutput(st *interactionsToResponsesStreamState, index int, text string) {
	if text == "" {
		return
	}
	if st.TextOutputs[index] == nil {
		st.TextOutputs[index] = &strings.Builder{}
	}
	st.TextOutputs[index].WriteString(text)
}

func setResponsesCompletedOutput(payload []byte, st *interactionsToResponsesStreamState) []byte {
	indexes := make([]int, 0, len(st.ItemTypes))
	for index := range st.ItemTypes {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	for _, index := range indexes {
		item, ok := responsesCompletedOutputItem(index, st.ItemTypes[index], st)
		if ok {
			payload, _ = sjson.SetRawBytes(payload, "response.output.-1", item)
		}
	}
	return payload
}

func responsesFunctionCallArguments(call *interactionsFunctionCallState) string {
	if call != nil && call.Snapshot != "" {
		return call.Snapshot
	}
	if call == nil || call.Arguments.Len() == 0 {
		return "{}"
	}
	return call.Arguments.String()
}

func responsesCompletedOutputItem(index int, itemType string, st *interactionsToResponsesStreamState) ([]byte, bool) {
	switch itemType {
	case "model_output":
		item := []byte(`{"id":"","type":"message","status":"completed","role":"assistant","content":[]}`)
		item, _ = sjson.SetBytes(item, "id", st.ItemIDs[index])
		if builder := st.TextOutputs[index]; builder != nil && builder.String() != "" {
			part := []byte(`{"type":"output_text","text":""}`)
			part, _ = sjson.SetBytes(part, "text", builder.String())
			item, _ = sjson.SetRawBytes(item, "content.-1", part)
		}
		return item, true
	case "thought":
		return responsesReasoningItem(index, st), true
	case "function_call":
		item := []byte(`{"id":"","type":"function_call","status":"completed","call_id":"","name":"","arguments":"{}"}`)
		itemID := st.ItemIDs[index]
		item, _ = sjson.SetBytes(item, "id", itemID)
		if call := st.FunctionCalls[index]; call != nil {
			item, _ = sjson.SetBytes(item, "call_id", call.CallID)
			item, _ = sjson.SetBytes(item, "name", call.Name)
			item, _ = translatorcommon.SetStringWithoutHTMLEscape(item, "arguments", responsesFunctionCallArguments(call))
		}
		return restoreInteractionsResponseTool(item, st.ToolIdentity, ""), true
	}
	return nil, false
}

func responsesReasoningItem(index int, st *interactionsToResponsesStreamState) []byte {
	item := []byte(`{"id":"","type":"reasoning","status":"completed","encrypted_content":"","summary":[]}`)
	item, _ = sjson.SetBytes(item, "id", st.ItemIDs[index])
	if signature := st.ReasoningEncrypted[index]; signature != "" {
		item, _ = sjson.SetBytes(item, "encrypted_content", signature)
	}
	part := []byte(`{"type":"summary_text","text":""}`)
	part, _ = sjson.SetBytes(part, "text", strings.Join(st.ReasoningSummaries[index], ""))
	item, _ = sjson.SetRawBytes(item, "summary.-1", part)
	return item
}

func setResponsesUsageFromInteractions(out []byte, path string, usage gjson.Result) []byte {
	input, _ := firstUsageInt(usage, "input_tokens", "total_input_tokens")
	output, _ := firstUsageInt(usage, "output_tokens", "total_output_tokens")
	input, output = max(0, input), max(0, output)
	total, hasTotal := firstUsageInt(usage, "total_tokens")
	if !hasTotal {
		total = input + output
		if total < 0 {
			total = math.MaxInt64
		}
	}
	out, _ = sjson.SetBytes(out, path+".input_tokens", input)
	out, _ = sjson.SetBytes(out, path+".output_tokens", output)
	out, _ = sjson.SetBytes(out, path+".total_tokens", max(0, total))
	if v, ok := firstUsageInt(usage, "cached_tokens", "total_cached_tokens"); ok {
		out, _ = sjson.SetBytes(out, path+".input_tokens_details.cached_tokens", v)
	}
	if v, ok := firstUsageInt(usage, "reasoning_tokens", "total_thought_tokens"); ok {
		out, _ = sjson.SetBytes(out, path+".output_tokens_details.reasoning_tokens", v)
	}
	return out
}

func interactionIncompleteReason(root gjson.Result) string {
	reason := firstNonEmpty(
		root.Get("interaction.incomplete_details.reason").String(), root.Get("incomplete_details.reason").String(),
		root.Get("interaction.finish_reason").String(), root.Get("finish_reason").String(),
	)
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	}
	return ""
}

func interactionTerminalStatus(root gjson.Result) string {
	if interactionIncompleteReason(root) != "" {
		return "incomplete"
	}
	return firstNonEmpty(root.Get("interaction.status").String(), root.Get("status").String())
}

func copyInteractionIncompleteDetailsToResponse(out []byte, root gjson.Result, path string) []byte {
	details := root.Get("interaction.incomplete_details")
	if !details.Exists() {
		details = root.Get("incomplete_details")
	}
	if details.Exists() {
		out, _ = sjson.SetRawBytes(out, path, []byte(details.Raw))
	}
	if reason := interactionIncompleteReason(root); reason != "" && gjson.GetBytes(out, path+".reason").String() == "" {
		out, _ = sjson.SetBytes(out, path+".reason", reason)
	}
	return out
}

func interactionEnvironmentID(root gjson.Result) string {
	return firstNonEmpty(
		root.Get("interaction.environment_id").String(),
		root.Get("interaction.environment.id").String(),
		root.Get("environment_id").String(),
		root.Get("environment.id").String(),
	)
}

func responseEnvironmentID(root gjson.Result) string {
	return firstNonEmpty(
		root.Get("response.environment_id").String(),
		root.Get("response.environment.id").String(),
		root.Get("environment_id").String(),
		root.Get("environment.id").String(),
	)
}

func convertOpenAIResponsesResponseToInteractions(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	_ = ctx
	_ = originalRequestRawJSON
	_ = requestRawJSON
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &responsesToInteractionsStreamState{}
	}
	st := (*param).(*responsesToInteractionsStreamState)
	if st.Model == "" {
		st.Model = modelName
	}
	if st.FunctionCallIndexes == nil {
		st.FunctionCallIndexes = make(map[string]int)
	}
	if st.FunctionArgsSent == nil {
		st.FunctionArgsSent = make(map[string]bool)
	}
	return convertOpenAIResponsesEventToInteractions(modelName, rawJSON, st)
}

func convertOpenAIResponsesResponseToInteractionsNonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	_ *any,
) []byte {
	_ = ctx
	_ = originalRequestRawJSON
	_ = requestRawJSON
	root := gjson.ParseBytes(rawJSON)
	out := []byte(`{"id":"","object":"interaction","status":"completed","model":"","steps":[]}`)
	if status := root.Get("status").String(); status != "" {
		out, _ = sjson.SetBytes(out, "status", status)
	}
	out, _ = sjson.SetBytes(out, "id", root.Get("id").String())
	model := responseModel(modelName, root)
	signatureModel := firstNonEmpty(root.Get("model").String(), root.Get("response.model").String(), model)
	out, _ = sjson.SetBytes(out, "model", model)
	if incompleteDetails := root.Get("incomplete_details"); incompleteDetails.Exists() {
		out, _ = sjson.SetRawBytes(out, "incomplete_details", []byte(incompleteDetails.Raw))
	}
	forAntigravity := isAntigravityModel(model)
	root.Get("output").ForEach(
		func(_, item gjson.Result) bool {
			if step, ok := openAIResponsesOutputItemToInteractionsStep(item, signatureModel, forAntigravity); ok {
				out, _ = sjson.SetRawBytes(out, "steps.-1", step)
			}
			return true
		},
	)
	if environmentID := responseEnvironmentID(root); environmentID != "" {
		out, _ = sjson.SetBytes(out, "environment_id", environmentID)
	}
	out = setInteractionsUsageFromResponses(out, "usage", root.Get("usage"))
	return out
}

func convertOpenAIResponsesEventToInteractions(
	modelName string,
	rawJSON []byte,
	st *responsesToInteractionsStreamState,
) [][]byte {
	payload := interactionsSSEPayload(rawJSON)
	if len(payload) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return appendInteractionsDoneDirect(nil, st)
	}
	root := gjson.ParseBytes(payload)
	if !root.Exists() {
		return nil
	}
	switch root.Get("type").String() {
	case "response.created":
		return appendInteractionsCreatedDirect(nil, st, modelName, root.Get("response"))
	case "response.output_text.delta":
		out := ensureInteractionsStepDirect(nil, st, modelName, "model_output", gjson.Result{})
		out = appendInteractionsTextDeltaDirect(out, st, root.Get("delta").String(), false)
		st.markTextSent(textKeysFromResponsesEvent(root))
		return out
	case "response.reasoning_summary_text.delta":
		out := ensureInteractionsThoughtStepDirect(nil, st, modelName, gjson.Result{})
		return appendInteractionsTextDeltaDirect(out, st, root.Get("delta").String(), true)
	case "response.output_item.added":
		return openAIResponsesOutputItemAddedToInteractions(modelName, root, st)
	case "response.function_call_arguments.delta":
		out := ensureInteractionsFunctionCallStep(nil, st, modelName, root)
		out = appendInteractionsArgumentsDeltaDirect(out, st, root.Get("delta").String())
		st.markFunctionArgsSent(functionArgsKeysFromResponsesEvent(root))
		return out
	case "response.output_item.done":
		return openAIResponsesOutputItemDoneToInteractions(modelName, root, st)
	case "response.completed", "response.incomplete":
		return openAIResponsesCompletedToInteractions(modelName, root.Get("response"), st)
	}
	return nil
}

func openAIResponsesOutputItemToInteractionsStep(
	item gjson.Result,
	modelName string,
	forAntigravity bool,
) ([]byte, bool) {
	switch item.Get("type").String() {
	case "message":
		step := []byte(`{"type":"model_output","content":[]}`)
		item.Get("content").ForEach(
			func(_, part gjson.Result) bool {
				if converted, ok := responsesContentPartToInteractions(part); ok {
					step, _ = sjson.SetRawBytes(step, "content.-1", converted)
				}
				return true
			},
		)
		return step, true
	case "function_call":
		return responsesFunctionCallToInteractions(item, forAntigravity), true
	case "reasoning":
		step := []byte(`{"type":"thought","content":[]}`)
		hasContent := false
		item.Get("summary").ForEach(
			func(_, summary gjson.Result) bool {
				if text := summary.Get("text").String(); text != "" {
					part := []byte(`{"type":"text","text":""}`)
					part, _ = sjson.SetBytes(part, "text", text)
					step, _ = sjson.SetRawBytes(step, "content.-1", part)
					hasContent = true
				}
				return true
			},
		)
		if signature := compatibleResponsesInteractionSignature(
			modelName,
			item.Get("encrypted_content").String(),
		); signature != "" {
			step, _ = sjson.SetBytes(step, "signature", signature)
			hasContent = true
		}
		return step, hasContent
	}
	return nil, false
}

func openAIResponsesOutputItemAddedToInteractions(
	modelName string,
	root gjson.Result,
	st *responsesToInteractionsStreamState,
) [][]byte {
	item := root.Get("item")
	switch item.Get("type").String() {
	case "function_call":
		out := ensureInteractionsCreatedDirect(nil, st, modelName)
		out = appendInteractionsStepStopDirect(out, st)
		step := []byte(`{"type":"function_call","name":"","arguments":{}}`)
		name := item.Get("name").String()
		if isAntigravityModel(modelName) {
			name = translatorcommon.AntigravityToolNameToUpstream(name)
		}
		step, _ = sjson.SetBytes(step, "name", name)
		itemID := firstNonEmpty(item.Get("id").String(), item.Get("call_id").String())
		callID := firstNonEmpty(item.Get("call_id").String(), item.Get("id").String())
		if itemID != "" {
			step, _ = sjson.SetBytes(step, "id", itemID)
		}
		if callID != "" {
			step, _ = sjson.SetBytes(step, "call_id", callID)
			st.FunctionCallIndexes[callID] = st.StepIndex
		}
		out = appendInteractionsStepStartDirect(out, st, "function_call", gjson.ParseBytes(step))
		return out
	case "message":
		return ensureInteractionsStepDirect(nil, st, modelName, "model_output", gjson.Result{})
	case "reasoning":
		if compatibleResponsesInteractionSignature(
			firstNonEmpty(st.Model, modelName),
			item.Get("encrypted_content").String(),
		) == "" {
			return nil
		}
		return ensureInteractionsThoughtStepDirect(nil, st, modelName, item)
	}
	return nil
}

func openAIResponsesOutputItemDoneToInteractions(
	modelName string,
	root gjson.Result,
	st *responsesToInteractionsStreamState,
) [][]byte {
	item := root.Get("item")
	switch item.Get("type").String() {
	case "function_call":
		out := ensureInteractionsFunctionCallStep(nil, st, modelName, root)
		if args := item.Get("arguments"); args.Exists() && args.String() != "" && !st.hasSentFunctionArgs(functionArgsKeysFromResponsesEvent(root)) {
			out = appendInteractionsArgumentsDeltaDirect(out, st, jsonStringValue(args, "{}"))
		}
		return appendInteractionsStepStopDirect(out, st)
	case "reasoning":
		hasSummary := false
		item.Get("summary").ForEach(
			func(_, summary gjson.Result) bool {
				if summary.Get("text").String() != "" {
					hasSummary = true
					return false
				}
				return true
			},
		)
		signature := compatibleResponsesInteractionSignature(
			firstNonEmpty(st.Model, modelName),
			item.Get("encrypted_content").String(),
		)
		hasActiveThought := st.ActiveStepOpen && st.ActiveStepType == "thought"
		if !hasActiveThought && !hasSummary && signature == "" {
			return nil
		}
		out := ensureInteractionsThoughtStepDirect(nil, st, modelName, item)
		item.Get("summary").ForEach(
			func(_, summary gjson.Result) bool {
				if text := summary.Get("text").String(); text != "" {
					out = appendInteractionsTextDeltaDirect(out, st, text, true)
				}
				return true
			},
		)
		return appendInteractionsStepStopDirect(out, st)
	case "message":
		return appendResponsesMessageFallbackToInteractions(nil, modelName, item, root, st, true)
	}
	return nil
}

func openAIResponsesCompletedToInteractions(
	modelName string,
	response gjson.Result,
	st *responsesToInteractionsStreamState,
) [][]byte {
	var out [][]byte
	response.Get("output").ForEach(
		func(outputIndex, item gjson.Result) bool {
			if item.Get("type").String() == "message" {
				out = appendResponsesMessageFallbackToInteractions(
					out, modelName, item, responseOutputIndexRoot(item, outputIndex), st, false,
				)
			}
			return true
		},
	)
	out = appendInteractionsStepStopDirect(out, st)
	out = appendInteractionsCompletedDirect(out, st, modelName, response)
	return appendInteractionsDoneDirect(out, st)
}

func appendResponsesMessageFallbackToInteractions(
	out [][]byte,
	modelName string,
	item, root gjson.Result,
	st *responsesToInteractionsStreamState,
	stop bool,
) [][]byte {
	itemID := item.Get("id").String()
	outputIndex := int(root.Get("output_index").Int())
	hasOutputIndex := root.Get("output_index").Exists()
	item.Get("content").ForEach(
		func(contentIndex, part gjson.Result) bool {
			if part.Get("type").String() != "output_text" && part.Get("type").String() != "text" {
				return true
			}
			hasContentIndex := contentIndex.Exists()
			keys := openAIResponsesTextKeys(
				itemID, outputIndex, hasOutputIndex, int(contentIndex.Int()), hasContentIndex,
			)
			unkeyedKeys := openAIResponsesUnkeyedTextKeys(itemID, outputIndex, hasOutputIndex)
			if st.hasSentText(keys, hasContentIndex) || st.hasSentUnkeyedText(unkeyedKeys) {
				return true
			}
			text := part.Get("text").String()
			if text == "" {
				return true
			}
			out = ensureInteractionsStepDirect(out, st, modelName, "model_output", gjson.Result{})
			out = appendInteractionsTextDeltaDirect(out, st, text, false)
			st.markTextSent(keys)
			return true
		},
	)
	if stop {
		return appendInteractionsStepStopDirect(out, st)
	}
	return out
}

func responseOutputIndexRoot(item, outputIndex gjson.Result) gjson.Result {
	raw := []byte(`{"output_index":0}`)
	raw, _ = sjson.SetBytes(raw, "output_index", outputIndex.Int())
	if id := item.Get("id").String(); id != "" {
		raw, _ = sjson.SetBytes(raw, "item_id", id)
	}
	return gjson.ParseBytes(raw)
}

func appendInteractionsCreatedDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	modelName string,
	response gjson.Result,
	markStatus ...bool,
) [][]byte {
	if st.Created {
		return out
	}
	st.ID = firstNonEmpty(response.Get("id").String(), st.ID, fmt.Sprintf("interaction_%d", time.Now().UnixNano()))
	created := []byte(`{"interaction":{"id":"","status":"in_progress","object":"interaction","model":""},"event_type":"interaction.created"}`)
	created, _ = sjson.SetBytes(created, "interaction.id", st.ID)
	st.Model = firstNonEmpty(response.Get("model").String(), response.Get("response.model").String(), modelName, st.Model)
	created, _ = sjson.SetBytes(created, "interaction.model", responseModel(modelName, response))
	if environmentID := responseEnvironmentID(response); environmentID != "" {
		st.EnvironmentID = environmentID
		created, _ = sjson.SetBytes(created, "interaction.environment_id", environmentID)
	}
	out = append(out, emitInteractionsEvent("interaction.created", created))
	st.Created = true
	if len(markStatus) == 0 || markStatus[0] {
		out = appendInteractionsStatusUpdateDirect(out, st)
	}
	return out
}

func appendInteractionsStatusUpdateDirect(out [][]byte, st *responsesToInteractionsStreamState) [][]byte {
	if st.StatusUpdated {
		return out
	}
	statusUpdate := []byte(`{"interaction_id":"","status":"in_progress","event_type":"interaction.status_update"}`)
	statusUpdate, _ = sjson.SetBytes(statusUpdate, "interaction_id", st.ID)
	out = append(out, emitInteractionsEvent("interaction.status_update", statusUpdate))
	st.StatusUpdated = true
	return out
}

func ensureInteractionsStepDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	modelName, stepType string,
	step gjson.Result,
) [][]byte {
	out = ensureInteractionsCreatedDirect(out, st, modelName)
	if st.ActiveStepOpen && st.ActiveStepType == stepType {
		return out
	}
	out = appendInteractionsStepStopDirect(out, st)
	return appendInteractionsStepStartDirect(out, st, stepType, step)
}

func ensureInteractionsThoughtStepDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	modelName string,
	item gjson.Result,
) [][]byte {
	signature := compatibleResponsesInteractionSignature(
		firstNonEmpty(st.Model, modelName),
		item.Get("encrypted_content").String(),
	)
	step := []byte(`{"type":"thought"}`)
	if signature != "" {
		step, _ = sjson.SetBytes(step, "signature", signature)
	}
	thoughtAlreadyOpen := st.ActiveStepOpen && st.ActiveStepType == "thought"
	out = ensureInteractionsStepDirect(out, st, modelName, "thought", gjson.ParseBytes(step))
	if thoughtAlreadyOpen && signature != "" && signature != st.ActiveThoughtSignature {
		out = appendInteractionsThoughtSignatureDeltaDirect(out, st, signature)
	}
	return out
}

func ensureInteractionsCreatedDirect(out [][]byte, st *responsesToInteractionsStreamState, modelName string) [][]byte {
	return appendInteractionsCreatedDirect(out, st, modelName, gjson.Result{})
}

func appendInteractionsStepStartDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	stepType string,
	step gjson.Result,
) [][]byte {
	index := st.StepIndex
	st.StepIndex++
	st.ActiveStepIndex = index
	st.ActiveStepType = stepType
	st.ActiveStepOpen = true
	payload := []byte(`{"index":0,"step":{"type":""},"event_type":"step.start"}`)
	payload, _ = sjson.SetBytes(payload, "index", index)
	payload, _ = sjson.SetBytes(payload, "step.type", stepType)
	if stepType == "thought" {
		if signature := step.Get("signature").String(); signature != "" {
			payload, _ = sjson.SetBytes(payload, "step.signature", signature)
			st.ActiveThoughtSignature = signature
		}
	}
	if stepType == "function_call" {
		if itemID := firstNonEmpty(step.Get("id").String(), step.Get("call_id").String()); itemID != "" {
			payload, _ = sjson.SetBytes(payload, "step.id", itemID)
		}
		if callID := firstNonEmpty(step.Get("call_id").String(), step.Get("id").String()); callID != "" {
			payload, _ = sjson.SetBytes(payload, "step.call_id", callID)
		}
		payload, _ = sjson.SetBytes(payload, "step.name", step.Get("name").String())
		payload, _ = sjson.SetRawBytes(payload, "step.arguments", []byte(`{}`))
	}
	return append(out, emitInteractionsEvent("step.start", payload))
}

func appendInteractionsTextDeltaDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	text string,
	thought bool,
) [][]byte {
	if thought {
		payload := []byte(`{"index":0,"delta":{"content":{"text":"","type":"text"},"type":"thought_summary"},"event_type":"step.delta"}`)
		payload, _ = sjson.SetBytes(payload, "index", st.ActiveStepIndex)
		payload, _ = sjson.SetBytes(payload, "delta.content.text", text)
		return append(out, emitInteractionsEvent("step.delta", payload))
	}
	payload := []byte(`{"index":0,"delta":{"text":"","type":"text"},"event_type":"step.delta"}`)
	payload, _ = sjson.SetBytes(payload, "index", st.ActiveStepIndex)
	payload, _ = sjson.SetBytes(payload, "delta.text", text)
	return append(out, emitInteractionsEvent("step.delta", payload))
}

func appendInteractionsThoughtSignatureDeltaDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	signature string,
) [][]byte {
	payload := []byte(`{"index":0,"delta":{"signature":"","type":"thought_signature"},"event_type":"step.delta"}`)
	payload, _ = sjson.SetBytes(payload, "index", st.ActiveStepIndex)
	payload, _ = sjson.SetBytes(payload, "delta.signature", signature)
	st.ActiveThoughtSignature = signature
	return append(out, emitInteractionsEvent("step.delta", payload))
}

func appendInteractionsArgumentsDeltaDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	arguments string,
) [][]byte {
	payload := []byte(`{"index":0,"delta":{"arguments":"","type":"arguments_delta"},"event_type":"step.delta"}`)
	payload, _ = sjson.SetBytes(payload, "index", st.ActiveStepIndex)
	payload, _ = translatorcommon.SetStringWithoutHTMLEscape(payload, "delta.arguments", arguments)
	return append(out, emitInteractionsEvent("step.delta", payload))
}

func appendInteractionsStepStopDirect(out [][]byte, st *responsesToInteractionsStreamState) [][]byte {
	if !st.ActiveStepOpen {
		return out
	}
	payload := []byte(`{"index":0,"event_type":"step.stop"}`)
	payload, _ = sjson.SetBytes(payload, "index", st.ActiveStepIndex)
	out = append(out, emitInteractionsEvent("step.stop", payload))
	st.ActiveStepOpen = false
	st.ActiveStepType = ""
	st.ActiveThoughtSignature = ""
	return out
}

func appendInteractionsCompletedDirect(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	modelName string,
	response gjson.Result,
) [][]byte {
	if st.Completed {
		return out
	}
	now := time.Now().UTC().Format(time.RFC3339)
	payload := []byte(`{"interaction":{"id":"","status":"completed","usage":{},"created":"","updated":"","service_tier":"standard","object":"interaction","model":""},"event_type":"interaction.completed"}`)
	payload, _ = sjson.SetBytes(payload, "interaction.id", st.ID)
	payload, _ = sjson.SetBytes(payload, "interaction.created", now)
	payload, _ = sjson.SetBytes(payload, "interaction.updated", now)
	st.Model = firstNonEmpty(response.Get("model").String(), response.Get("response.model").String(), st.Model, modelName)
	payload, _ = sjson.SetBytes(payload, "interaction.model", responseModel(modelName, response))
	if status := response.Get("status").String(); status != "" {
		payload, _ = sjson.SetBytes(payload, "interaction.status", status)
	}
	if details := response.Get("incomplete_details"); details.Exists() {
		payload, _ = sjson.SetRawBytes(payload, "interaction.incomplete_details", []byte(details.Raw))
	}
	if environmentID := firstNonEmpty(responseEnvironmentID(response), st.EnvironmentID); environmentID != "" {
		st.EnvironmentID = environmentID
		payload, _ = sjson.SetBytes(payload, "interaction.environment_id", environmentID)
	}
	payload = setInteractionsUsageFromResponses(payload, "interaction.usage", response.Get("usage"))
	out = append(out, emitInteractionsEvent("interaction.completed", payload))
	st.Completed = true
	return out
}

func appendInteractionsDoneDirect(out [][]byte, st *responsesToInteractionsStreamState) [][]byte {
	if st.Done {
		return out
	}
	out = append(out, emitInteractionsEvent("done", []byte("[DONE]")))
	st.Done = true
	return out
}

func ensureInteractionsFunctionCallStep(
	out [][]byte,
	st *responsesToInteractionsStreamState,
	modelName string,
	root gjson.Result,
) [][]byte {
	if st.ActiveStepOpen && st.ActiveStepType == "function_call" {
		return out
	}
	item := root.Get("item")
	if !item.Exists() {
		item = root
	}
	step := []byte(`{"type":"function_call","name":"","arguments":{}}`)
	name := item.Get("name").String()
	if isAntigravityModel(modelName) {
		name = translatorcommon.AntigravityToolNameToUpstream(name)
	}
	step, _ = sjson.SetBytes(step, "name", name)
	itemID := firstNonEmpty(
		item.Get("id").String(), root.Get("item_id").String(), item.Get("call_id").String(),
		root.Get("call_id").String(),
	)
	callID := firstNonEmpty(
		item.Get("call_id").String(), root.Get("call_id").String(), item.Get("id").String(),
		root.Get("item_id").String(),
	)
	if itemID != "" {
		step, _ = sjson.SetBytes(step, "id", itemID)
	}
	if callID != "" {
		step, _ = sjson.SetBytes(step, "call_id", callID)
	}
	out = ensureInteractionsCreatedDirect(out, st, modelName)
	out = appendInteractionsStepStopDirect(out, st)
	return appendInteractionsStepStartDirect(out, st, "function_call", gjson.ParseBytes(step))
}

func setInteractionsUsageFromResponses(out []byte, path string, usage gjson.Result) []byte {
	if !usage.Exists() {
		return out
	}
	if v := usage.Get("input_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, path+".input_tokens", v.Int())
		out, _ = sjson.SetBytes(out, path+".total_input_tokens", v.Int())
	}
	if v := usage.Get("output_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, path+".output_tokens", v.Int())
		out, _ = sjson.SetBytes(out, path+".total_output_tokens", v.Int())
	}
	if v := usage.Get("total_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, path+".total_tokens", v.Int())
	}
	if v := usage.Get("input_tokens_details.cached_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, path+".cached_tokens", v.Int())
		out, _ = sjson.SetBytes(out, path+".total_cached_tokens", v.Int())
	}
	if v := usage.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, path+".reasoning_tokens", v.Int())
		out, _ = sjson.SetBytes(out, path+".total_thought_tokens", v.Int())
	}
	return out
}

func interactionsSSEPayload(rawJSON []byte) []byte {
	trimmed := bytes.TrimSpace(rawJSON)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return trimmed
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return bytes.TrimSpace(trimmed[len("data:"):])
	}
	var dataLines [][]byte
	for line := range bytes.SplitSeq(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			dataLines = append(dataLines, bytes.TrimSpace(line[len("data:"):]))
		}
	}
	if len(dataLines) > 0 {
		return bytes.Join(dataLines, []byte("\n"))
	}
	return trimmed
}

func responseModel(modelName string, root gjson.Result) string {
	return firstNonEmpty(
		modelName, root.Get("model").String(), root.Get("response.model").String(),
		root.Get("interaction.model").String(),
	)
}

func firstUsageInt(root gjson.Result, paths ...string) (int64, bool) {
	for _, path := range paths {
		if v := root.Get(path); v.Exists() {
			return v.Int(), true
		}
	}
	return 0, false
}

func nextResponsesSeq(st *interactionsToResponsesStreamState) int {
	st.Seq++
	return st.Seq
}

func emitResponsesEvent(event string, payload []byte) []byte {
	return translatorcommon.SSEEventData(event, payload)
}

func emitInteractionsEvent(event string, payload []byte) []byte {
	return translatorcommon.SSEEventData(event, payload)
}

func textKeysFromResponsesEvent(root gjson.Result) []string {
	itemID := root.Get("item_id").String()
	outputIndex := int(root.Get("output_index").Int())
	hasOutputIndex := root.Get("output_index").Exists()
	contentIndex := int(root.Get("content_index").Int())
	hasContentIndex := root.Get("content_index").Exists()
	if !hasContentIndex {
		return openAIResponsesUnkeyedTextKeys(itemID, outputIndex, hasOutputIndex)
	}
	return openAIResponsesTextKeys(itemID, outputIndex, hasOutputIndex, contentIndex, hasContentIndex)
}

func functionArgsKeysFromResponsesEvent(root gjson.Result) []string {
	item := root.Get("item")
	outputIndex := int(root.Get("output_index").Int())
	hasOutputIndex := root.Get("output_index").Exists()
	keys := make([]string, 0, 5)
	for _, id := range []string{
		root.Get("item_id").String(),
		root.Get("call_id").String(),
		item.Get("call_id").String(),
		item.Get("id").String(),
	} {
		if id == "" {
			continue
		}
		key := fmt.Sprintf("item:%s", id)
		if !stringSliceContains(keys, key) {
			keys = append(keys, key)
		}
	}
	if hasOutputIndex {
		keys = append(keys, fmt.Sprintf("output:%d", outputIndex))
	}
	return keys
}

func stringSliceContains(values []string, target string) bool {
	return slices.Contains(values, target)
}

func openAIResponsesTextKeys(
	itemID string,
	outputIndex int,
	hasOutputIndex bool,
	contentIndex int,
	hasContentIndex bool,
) []string {
	if !hasContentIndex {
		return nil
	}
	keys := make([]string, 0, 3)
	if itemID != "" {
		keys = append(keys, fmt.Sprintf("item:%s:content:%d", itemID, contentIndex))
	}
	if hasOutputIndex {
		keys = append(keys, fmt.Sprintf("output:%d:content:%d", outputIndex, contentIndex))
	}
	keys = append(keys, fmt.Sprintf("content:%d", contentIndex))
	return keys
}

func openAIResponsesUnkeyedTextKeys(itemID string, outputIndex int, hasOutputIndex bool) []string {
	keys := make([]string, 0, 2)
	if itemID != "" {
		keys = append(keys, fmt.Sprintf("item:%s", itemID))
	}
	if hasOutputIndex {
		keys = append(keys, fmt.Sprintf("output:%d", outputIndex))
	}
	return keys
}

func (st *responsesToInteractionsStreamState) markTextSent(keys []string) {
	if len(keys) == 0 {
		st.UnkeyedTextDelta = true
		return
	}
	if st.SentText == nil {
		st.SentText = map[string]bool{}
	}
	for _, key := range keys {
		st.SentText[key] = true
	}
}

func (st *responsesToInteractionsStreamState) hasSentText(keys []string, hasContentIndex bool) bool {
	if !hasContentIndex && st.UnkeyedTextDelta {
		return true
	}
	for _, key := range keys {
		if st.SentText[key] {
			return true
		}
	}
	return false
}

func (st *responsesToInteractionsStreamState) hasSentUnkeyedText(keys []string) bool {
	if len(keys) == 0 {
		return st.UnkeyedTextDelta
	}
	for _, key := range keys {
		if st.SentText[key] {
			return true
		}
	}
	return false
}

func (st *responsesToInteractionsStreamState) markFunctionArgsSent(keys []string) {
	for _, key := range keys {
		st.FunctionArgsSent[key] = true
	}
}

func (st *responsesToInteractionsStreamState) hasSentFunctionArgs(keys []string) bool {
	for _, key := range keys {
		if st.FunctionArgsSent[key] {
			return true
		}
	}
	return false
}
