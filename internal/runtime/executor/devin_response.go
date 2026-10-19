package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxDevinToolCalls        = 128
	maxDevinLoggedFields     = 256
	maxDevinDecodedLogSource = 1 << 20
)

type devinToolAccumulator struct {
	index     int
	id        string
	name      string
	arguments strings.Builder
}

type devinResponseAccumulator struct {
	contentBuffer    helps.UTF8SplitBuffer
	thinkingBuffer   helps.UTF8SplitBuffer
	captureLogBody   bool
	logBodyLimit     int
	logBodyBytes     int
	logBodyTruncated bool
	content          strings.Builder
	preToolContent   strings.Builder
	postToolContent  strings.Builder
	thinking         strings.Builder
	signature        []byte
	signatureType    string
	usage            *helps.DevinUsage
	tools            map[int]*devinToolAccumulator
	toolsByID        map[string]*devinToolAccumulator
	toolOrder        []*devinToolAccumulator
	lastTool         *devinToolAccumulator
	unknownFields    map[int]struct{}
	frames           int
	stopReason       uint64
}

type devinUsageLog struct {
	PromptTokens     *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64 `json:"completion_tokens,omitempty"`
	CachedTokens     *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	StatusCode       uint64 `json:"status_code,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	ModelName        string `json:"model_name,omitempty"`
}

type devinResponseLog struct {
	Status        string                `json:"status"`
	Frames        int                   `json:"frames"`
	Truncated     bool                  `json:"truncated,omitempty"`
	Content       string                `json:"content,omitempty"`
	Thinking      string                `json:"thinking,omitempty"`
	Signature     string                `json:"signature,omitempty"`
	SignatureType string                `json:"signature_type,omitempty"`
	ToolCalls     []helps.DevinToolCall `json:"tool_calls,omitempty"`
	Usage         *devinUsageLog        `json:"usage,omitempty"`
	UnknownFields []int                 `json:"unknown_fields,omitempty"`
}

func consumeDevinFrames(
	body io.Reader,
	model string,
) ([]byte, []byte, string, error) {
	state := newDevinResponseAccumulator()
	for {
		flag, payload, err := helps.ReadConnectFrame(body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				protocolErr := devinProtocolError("Devin response ended before the Connect EOS trailer")
				return nil, state.logBytes("missing_eos"), "", protocolErr
			}
			protocolErr := devinProtocolError(fmt.Sprintf("read Devin Connect frame: %v", err))
			return nil, state.logBytes("read_error"), "", protocolErr
		}
		state.frames++
		if flag&helps.ConnectFlagEndStream != 0 {
			statusCode, trailerErr := helps.ParseDevinTrailerError(payload)
			if trailerErr != nil {
				if statusCode == 0 {
					statusCode = http.StatusBadGateway
				}
				return nil, state.logBytes("trailer_error"), "", newUpstreamStatusErrMessage(
					statusCode,
					trailerErr.Error(),
					nil,
				)
			}
			break
		}

		frame, parseErr := helps.ParseDevinFrame(payload)
		if parseErr != nil {
			protocolErr := devinProtocolError(fmt.Sprintf("parse Devin response frame: %v", parseErr))
			return nil, state.logBytes("parse_error"), "", protocolErr
		}
		if applyErr := state.applyFrame(frame); applyErr != nil {
			protocolErr := devinProtocolError(applyErr.Error())
			return nil, state.logBytes("protocol_error"), "", protocolErr
		}
	}

	if err := state.finishText(); err != nil {
		return nil, state.logBytes("utf8_error"), "", devinProtocolError(err.Error())
	}
	if err := state.validateTools(); err != nil {
		return nil, state.logBytes("tool_error"), "", devinProtocolError(err.Error())
	}
	interaction, err := state.interactionJSON(model)
	if err != nil {
		return nil, state.logBytes("encode_error"), "", devinProtocolError(err.Error())
	}
	responseModel := ""
	if state.usage != nil {
		responseModel = state.usage.ModelName
	}
	return interaction, state.logBytes("completed"), responseModel, nil
}

func newDevinResponseAccumulator() *devinResponseAccumulator {
	return &devinResponseAccumulator{
		captureLogBody: true,
		tools:          make(map[int]*devinToolAccumulator),
		toolsByID:      make(map[string]*devinToolAccumulator),
		unknownFields:  make(map[int]struct{}),
	}
}

func newDevinStreamingResponseAccumulator(captureLogBody bool) *devinResponseAccumulator {
	state := newDevinResponseAccumulator()
	state.captureLogBody = captureLogBody
	state.logBodyLimit = maxDevinDecodedLogSource
	return state
}

func (state *devinResponseAccumulator) recordUnknownFields(fields []int) {
	if state == nil || !state.captureLogBody {
		return
	}
	for _, field := range fields {
		if _, exists := state.unknownFields[field]; exists {
			continue
		}
		if len(state.unknownFields) >= maxDevinLoggedFields {
			state.logBodyTruncated = true
			continue
		}
		state.unknownFields[field] = struct{}{}
	}
}

func (state *devinResponseAccumulator) captureLogText(builder *strings.Builder, value string) {
	if state == nil || !state.captureLogBody || builder == nil || value == "" {
		return
	}
	if state.logBodyLimit <= 0 {
		builder.WriteString(value)
		return
	}
	remaining := state.logBodyLimit - state.logBodyBytes
	if remaining <= 0 {
		state.logBodyTruncated = true
		return
	}
	if len(value) > remaining {
		value = value[:remaining]
		state.logBodyTruncated = true
	}
	builder.WriteString(value)
	state.logBodyBytes += len(value)
}

func (state *devinResponseAccumulator) captureLogSignature(value []byte) {
	if state == nil || !state.captureLogBody || len(value) == 0 {
		return
	}
	if state.logBodyLimit <= 0 {
		state.signature = append(state.signature, value...)
		return
	}
	remaining := state.logBodyLimit - state.logBodyBytes
	if remaining <= 0 {
		state.logBodyTruncated = true
		return
	}
	if len(value) > remaining {
		value = value[:remaining]
		state.logBodyTruncated = true
	}
	state.signature = append(state.signature, value...)
	state.logBodyBytes += len(value)
}

func (state *devinResponseAccumulator) applyFrame(frame helps.DevinFrameResult) error {
	if state == nil {
		return errors.New("devin response state is nil")
	}
	if frame.StopReason != 0 {
		state.stopReason = frame.StopReason
	}
	mergeDevinUsage(&state.usage, frame.Usage)
	if len(frame.ResponseDimensionGroups) > 0 {
		fallback, err := helps.ParseDevinResponseDimensionGroups(frame.ResponseDimensionGroups...)
		if err != nil {
			return fmt.Errorf("parse Devin response dimension usage: %w", err)
		}
		if state.usage == nil {
			state.usage = &helps.DevinUsage{}
		}
		state.usage.FillMissingTokenMetrics(fallback)
	}
	state.recordUnknownFields(frame.UnknownFieldNumbers)
	if frame.ThinkingText != "" {
		state.thinking.WriteString(state.thinkingBuffer.Feed([]byte(frame.ThinkingText)))
	}
	if len(frame.DeltaSignature) > 0 {
		state.signature = append(state.signature, frame.DeltaSignature...)
	}
	if frame.DeltaSignatureType != "" {
		state.signatureType = frame.DeltaSignatureType
	}
	for _, delta := range frame.ToolCallDeltas {
		if err := state.applyToolDelta(delta); err != nil {
			return err
		}
	}
	if frame.ContentText != "" {
		text := state.contentBuffer.Feed([]byte(frame.ContentText))
		state.content.WriteString(text)
		if len(state.toolOrder) > 0 {
			state.postToolContent.WriteString(text)
		} else {
			state.preToolContent.WriteString(text)
		}
	}
	return validateDevinUsage(state.usage)
}

func mergeDevinUsage(dst **helps.DevinUsage, src *helps.DevinUsage) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = &helps.DevinUsage{}
	}
	target := *dst
	if src.PromptTokensPresent {
		target.PromptTokens = src.PromptTokens
		target.PromptTokensPresent = true
	}
	if src.CompletionTokensPresent {
		target.CompletionTokens = src.CompletionTokens
		target.CompletionTokensPresent = true
	}
	if src.CachedTokensPresent {
		target.CachedTokens = src.CachedTokens
		target.CachedTokensPresent = true
	}
	if src.CacheWriteTokensPresent {
		target.CacheWriteTokens = src.CacheWriteTokens
		target.CacheWriteTokensPresent = true
	}
	if src.StatusCode != 0 {
		target.StatusCode = src.StatusCode
	}
	if src.RequestID != "" {
		target.RequestID = src.RequestID
	}
	if src.ModelName != "" {
		target.ModelName = src.ModelName
	}
	if len(src.Headers) > 0 {
		if target.Headers == nil {
			target.Headers = make(map[string]string, len(src.Headers))
		}
		for key, value := range src.Headers {
			target.Headers[key] = value
		}
	}
}

func validateDevinUsage(value *helps.DevinUsage) error {
	if value == nil {
		return nil
	}
	if value.CacheWriteTokens < 0 {
		return errors.New("devin cache write token count is negative")
	}
	input, ok := usage.AddNonNegativeInt64(value.PromptTokens, value.CachedTokens)
	if !ok {
		return errors.New("devin input token count overflows int64")
	}
	if _, ok = usage.AddNonNegativeInt64(input, value.CompletionTokens); !ok {
		return errors.New("devin total token count overflows int64")
	}
	return nil
}

func (state *devinResponseAccumulator) applyToolDelta(delta helps.DevinToolCallDelta) error {
	if delta.Index < 0 || delta.Index >= maxDevinToolCalls {
		return fmt.Errorf("devin tool call index %d exceeds supported range [0,%d)", delta.Index, maxDevinToolCalls)
	}
	if delta.IsCustomToolCall || delta.InvalidJSONStr != "" || delta.InvalidJSONErr != "" {
		return fmt.Errorf("devin custom tool call %q is unsupported without a custom-tool bridge", delta.Name)
	}
	tool := state.tools[delta.Index]
	if delta.ID != "" {
		if existing := state.toolsByID[delta.ID]; existing != nil {
			tool = existing
		} else if tool != nil && tool.id != "" {
			tool = nil
		}
	}
	if tool == nil {
		if len(state.toolOrder) >= maxDevinToolCalls {
			return fmt.Errorf("devin tool call count exceeds supported limit %d", maxDevinToolCalls)
		}
		tool = &devinToolAccumulator{index: delta.Index}
		state.toolOrder = append(state.toolOrder, tool)
	}
	if delta.ID != "" {
		tool.id = delta.ID
		state.toolsByID[delta.ID] = tool
	}
	if delta.Name != "" {
		if tool.name != "" && tool.name != delta.Name {
			return fmt.Errorf("devin tool call %d changed name from %q to %q", delta.Index, tool.name, delta.Name)
		}
		tool.name = delta.Name
	}
	if delta.Arguments != "" {
		tool.arguments.WriteString(delta.Arguments)
	}
	state.tools[delta.Index] = tool
	state.lastTool = tool
	return nil
}

func (state *devinResponseAccumulator) finishText() error {
	if err := state.thinkingBuffer.Finish(); err != nil {
		return err
	}
	return state.contentBuffer.Finish()
}

func (state *devinResponseAccumulator) validateTools() error {
	for _, tool := range state.toolOrder {
		if strings.TrimSpace(tool.name) == "" {
			return fmt.Errorf("devin tool call %d is missing a name", tool.index)
		}
		arguments := strings.TrimSpace(tool.arguments.String())
		if arguments != "" && !json.Valid([]byte(arguments)) {
			return fmt.Errorf("devin tool call %d has invalid JSON arguments", tool.index)
		}
	}
	return nil
}

func (state *devinResponseAccumulator) toolCalls() []helps.DevinToolCall {
	calls := make([]helps.DevinToolCall, 0, len(state.toolOrder))
	for _, tool := range state.toolOrder {
		id := tool.id
		if id == "" {
			id = fmt.Sprintf("call_%d", tool.index)
		}
		calls = append(calls, helps.DevinToolCall{
			ID:        id,
			Name:      tool.name,
			Arguments: tool.arguments.String(),
		})
	}
	return calls
}

func (state *devinResponseAccumulator) interactionJSON(model string) ([]byte, error) {
	interactionID := "interaction_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	out := []byte(`{"id":"","object":"interaction","model":"","status":"completed","steps":[],"usage":{}}`)
	out, _ = sjson.SetBytes(out, "id", interactionID)
	out, _ = sjson.SetBytes(out, "model", model)
	out = setDevinTerminationJSON(out, "", state.stopReason)

	if state.thinking.Len() > 0 || len(state.signature) > 0 {
		step := []byte(`{"type":"thought","content":[]}`)
		if state.thinking.Len() > 0 {
			content := []byte(`{"type":"text","text":""}`)
			content, _ = sjson.SetBytes(content, "text", state.thinking.String())
			step, _ = sjson.SetRawBytes(step, "content.-1", content)
		}
		if len(state.signature) > 0 {
			step, _ = sjson.SetBytes(step, "signature", string(state.signature))
			step, _ = sjson.SetBytes(step, "thought_signature", string(state.signature))
		}
		if state.signatureType != "" {
			step, _ = sjson.SetBytes(step, "signature_type", state.signatureType)
		}
		out, _ = sjson.SetRawBytes(out, "steps.-1", step)
	}
	if state.preToolContent.Len() > 0 {
		step := []byte(`{"type":"model_output","content":[{"type":"text","text":""}]}`)
		step, _ = sjson.SetBytes(step, "content.0.text", state.preToolContent.String())
		out, _ = sjson.SetRawBytes(out, "steps.-1", step)
	}
	for _, call := range state.toolCalls() {
		id := helps.EncodeDevinToolCallID(call.ID)
		step := []byte(`{"type":"function_call","id":"","call_id":"","name":"","arguments":{}}`)
		step, _ = sjson.SetBytes(step, "id", id)
		step, _ = sjson.SetBytes(step, "call_id", id)
		step, _ = sjson.SetBytes(step, "name", call.Name)
		if strings.TrimSpace(call.Arguments) != "" {
			step, _ = sjson.SetRawBytes(step, "arguments", []byte(call.Arguments))
		}
		out, _ = sjson.SetRawBytes(out, "steps.-1", step)
	}
	if state.postToolContent.Len() > 0 {
		step := []byte(`{"type":"model_output","content":[{"type":"text","text":""}]}`)
		step, _ = sjson.SetBytes(step, "content.0.text", state.postToolContent.String())
		out, _ = sjson.SetRawBytes(out, "steps.-1", step)
	}
	out, err := setDevinUsageJSON(out, "usage", state.usage)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func setDevinTerminationJSON(out []byte, path string, stopReason uint64) []byte {
	var reason, detail string
	switch stopReason {
	case 1, 3:
		reason, detail = "length", "max_output_tokens"
	case 11:
		reason, detail = "content_filter", "content_filter"
	default:
		return out
	}
	if path != "" {
		path += "."
	}
	out, _ = sjson.SetBytes(out, path+"status", "incomplete")
	out, _ = sjson.SetBytes(out, path+"finish_reason", reason)
	out, _ = sjson.SetBytes(out, path+"incomplete_details.reason", detail)
	return out
}

func setDevinUsageJSON(out []byte, path string, value *helps.DevinUsage) ([]byte, error) {
	if value == nil {
		return out, nil
	}
	if err := validateDevinUsage(value); err != nil {
		return nil, err
	}
	if value.PromptTokensPresent || value.CachedTokensPresent {
		input, _ := usage.AddNonNegativeInt64(value.PromptTokens, value.CachedTokens)
		out, _ = sjson.SetBytes(out, path+".total_input_tokens", input)
		out, _ = sjson.SetBytes(out, path+".input_tokens", input)
	}
	if value.CompletionTokensPresent {
		out, _ = sjson.SetBytes(out, path+".total_output_tokens", value.CompletionTokens)
		out, _ = sjson.SetBytes(out, path+".output_tokens", value.CompletionTokens)
	}
	if value.CachedTokensPresent {
		out, _ = sjson.SetBytes(out, path+".total_cached_tokens", value.CachedTokens)
		out, _ = sjson.SetBytes(out, path+".cached_tokens", value.CachedTokens)
	}
	if value.CacheWriteTokensPresent {
		out, _ = sjson.SetBytes(out, path+".cache_write_tokens", value.CacheWriteTokens)
	}
	if value.PromptTokensPresent && value.CompletionTokensPresent && value.CachedTokensPresent {
		input, _ := usage.AddNonNegativeInt64(value.PromptTokens, value.CachedTokens)
		total, _ := usage.AddNonNegativeInt64(input, value.CompletionTokens)
		out, _ = sjson.SetBytes(out, path+".total_tokens", total)
	}
	return out, nil
}

func (state *devinResponseAccumulator) logBytes(status string) []byte {
	if state == nil {
		return nil
	}
	unknownFields := make([]int, 0, len(state.unknownFields))
	for field := range state.unknownFields {
		unknownFields = append(unknownFields, field)
	}
	sort.Ints(unknownFields)
	remaining := maxDevinDecodedLogSource
	content, contentTruncated := takeDevinLogValue(state.content.String(), &remaining)
	thinking, thinkingTruncated := takeDevinLogValue(state.thinking.String(), &remaining)
	signature, signatureTruncated := takeDevinLogValue(string(state.signature), &remaining)
	toolCalls, toolsTruncated := state.toolCallsForLog(&remaining)
	logValue := devinResponseLog{
		Status: status,
		Frames: state.frames,
		Truncated: state.logBodyTruncated || contentTruncated || thinkingTruncated ||
			signatureTruncated || toolsTruncated,
		Content:       content,
		Thinking:      thinking,
		Signature:     signature,
		SignatureType: state.signatureType,
		ToolCalls:     toolCalls,
		Usage:         devinUsageForLog(state.usage),
		UnknownFields: unknownFields,
	}
	encoded, err := json.MarshalIndent(logValue, "", "  ")
	if err != nil {
		return nil
	}
	return append([]byte("DEVIN CONNECT RESPONSE\n"), encoded...)
}

func (state *devinResponseAccumulator) toolCallsForLog(remaining *int) ([]helps.DevinToolCall, bool) {
	calls := make([]helps.DevinToolCall, 0, len(state.toolOrder))
	truncated := false
	for _, tool := range state.toolOrder {
		id := tool.id
		if id == "" {
			id = fmt.Sprintf("call_%d", tool.index)
		}
		loggedID, clippedID := takeDevinLogValue(id, remaining)
		loggedName, clippedName := takeDevinLogValue(tool.name, remaining)
		loggedArguments, clippedArguments := takeDevinLogValue(tool.arguments.String(), remaining)
		truncated = truncated || clippedID || clippedName || clippedArguments
		calls = append(calls, helps.DevinToolCall{
			ID:        loggedID,
			Name:      loggedName,
			Arguments: loggedArguments,
		})
	}
	return calls, truncated
}

func takeDevinLogValue(value string, remaining *int) (string, bool) {
	if value == "" {
		return "", false
	}
	if remaining == nil || *remaining <= 0 {
		return "", true
	}
	if len(value) <= *remaining {
		*remaining -= len(value)
		return value, false
	}
	clipped := value[:*remaining]
	*remaining = 0
	return clipped, true
}

func devinUsageForLog(value *helps.DevinUsage) *devinUsageLog {
	if value == nil {
		return nil
	}
	logged := &devinUsageLog{
		StatusCode: value.StatusCode,
		RequestID:  value.RequestID,
		ModelName:  value.ModelName,
	}
	if value.PromptTokensPresent {
		logged.PromptTokens = new(value.PromptTokens)
	}
	if value.CompletionTokensPresent {
		logged.CompletionTokens = new(value.CompletionTokens)
	}
	if value.CachedTokensPresent {
		logged.CachedTokens = new(value.CachedTokens)
	}
	if value.CacheWriteTokensPresent {
		logged.CacheWriteTokens = new(value.CacheWriteTokens)
	}
	return logged
}

func devinProtocolError(message string) error {
	return newUpstreamStatusErrMessage(http.StatusBadGateway, message, nil)
}

type devinPendingAction struct {
	text      string
	toolDelta *helps.DevinToolCallDelta
}

type devinStreamState struct {
	executor       *DevinExecutor
	ctx            context.Context
	req            cliproxyexecutor.Request
	opts           cliproxyexecutor.Options
	requestBody    []byte
	responseFormat sdktranslator.Format
	reporter       *usageReporter
	out            chan<- cliproxyexecutor.StreamChunk
	interactionID  string
	createdSent    bool
	translateParam any

	response             *devinResponseAccumulator
	toolStepIndices      map[*devinToolAccumulator]int
	toolStepStarted      map[*devinToolAccumulator]bool
	nextStepIndex        int
	activeTextType       string
	activeTextIndex      int
	thoughtStepIndex     int
	pendingActions       []devinPendingAction
	postToolContent      []string
	deferredThoughtStops []int
	thoughtSignatures    map[int]string
	upstreamDone         bool
	failed               bool
}

func (e *DevinExecutor) streamDevinFrames(
	ctx context.Context,
	body io.Reader,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	requestBody []byte,
	reporter *usageReporter,
	out chan<- cliproxyexecutor.StreamChunk,
) {
	state := &devinStreamState{
		executor:         e,
		ctx:              ctx,
		req:              req,
		opts:             opts,
		requestBody:      requestBody,
		responseFormat:   devinResponseFormat(opts),
		reporter:         reporter,
		out:              out,
		interactionID:    "interaction_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		response:         newDevinStreamingResponseAccumulator(requestLogCaptureEnabled(e.cfg)),
		toolStepIndices:  make(map[*devinToolAccumulator]int),
		toolStepStarted:  make(map[*devinToolAccumulator]bool),
		activeTextIndex:  -1,
		thoughtStepIndex: -1,
	}
	defer func() {
		if !state.upstreamDone && !state.failed && ctx.Err() != nil {
			reporter.publishFailure(ctx, markResponseStarted(ctx.Err()))
		}
		reporter.ensurePublished(ctx)
	}()

	for {
		flag, payload, err := helps.ReadConnectFrame(body)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				state.fail(devinProtocolError("Devin stream ended before the Connect EOS trailer"), "missing_eos")
				return
			}
			state.fail(
				devinProtocolError(fmt.Sprintf("read Devin Connect frame: %v", err)),
				"stream_read_error",
			)
			return
		}
		state.response.frames++
		if flag&helps.ConnectFlagEndStream != 0 {
			statusCode, trailerErr := helps.ParseDevinTrailerError(payload)
			if trailerErr != nil {
				if statusCode == 0 {
					statusCode = http.StatusBadGateway
				}
				state.fail(
					newUpstreamStatusErrMessage(statusCode, trailerErr.Error(), nil),
					"trailer_error",
				)
				return
			}
			state.upstreamDone = true
			break
		}

		frame, parseErr := helps.ParseDevinFrame(payload)
		if parseErr != nil {
			state.fail(
				devinProtocolError(fmt.Sprintf("parse Devin response frame: %v", parseErr)),
				"frame_parse_error",
			)
			return
		}
		if processErr := state.processFrame(frame); processErr != nil {
			if ctx.Err() != nil {
				return
			}
			state.fail(devinProtocolError(processErr.Error()), "protocol_error")
			return
		}
	}

	if err := state.response.finishText(); err != nil {
		state.fail(devinProtocolError(err.Error()), "utf8_error")
		return
	}
	if err := state.flushPendingActions(); err != nil {
		if ctx.Err() == nil {
			state.fail(devinProtocolError(err.Error()), "protocol_error")
		}
		return
	}
	if err := state.response.validateTools(); err != nil {
		state.fail(devinProtocolError(err.Error()), "tool_error")
		return
	}
	if !state.closeDeferredThoughts() || !state.stopText() || !state.flushTools() {
		return
	}
	for _, text := range state.postToolContent {
		if !state.emitText("model_output", text) {
			return
		}
	}
	state.postToolContent = nil
	if !state.stopText() {
		return
	}
	state.complete()
}

func (state *devinStreamState) processFrame(frame helps.DevinFrameResult) error {
	if frame.StopReason != 0 {
		state.response.stopReason = frame.StopReason
	}
	mergeDevinUsage(&state.response.usage, frame.Usage)
	if len(frame.ResponseDimensionGroups) > 0 {
		fallback, err := helps.ParseDevinResponseDimensionGroups(frame.ResponseDimensionGroups...)
		if err != nil {
			return fmt.Errorf("parse Devin response dimension usage: %w", err)
		}
		if state.response.usage == nil {
			state.response.usage = &helps.DevinUsage{}
		}
		state.response.usage.FillMissingTokenMetrics(fallback)
	}
	if err := validateDevinUsage(state.response.usage); err != nil {
		return err
	}
	if state.response.usage != nil {
		state.reporter.setResponseModel(state.response.usage.ModelName)
	}
	state.response.recordUnknownFields(frame.UnknownFieldNumbers)

	if frame.ThinkingText != "" {
		if err := state.flushPendingActions(); err != nil {
			return err
		}
		text := state.response.thinkingBuffer.Feed([]byte(frame.ThinkingText))
		state.response.captureLogText(&state.response.thinking, text)
		if text != "" && !state.emitText("thought", text) {
			return context.Canceled
		}
	}
	if len(frame.DeltaSignature) > 0 {
		state.response.captureLogSignature(frame.DeltaSignature)
		if !state.emitSignature(frame.DeltaSignature, frame.DeltaSignatureType) {
			return context.Canceled
		}
	}
	if frame.DeltaSignatureType != "" {
		state.response.signatureType = frame.DeltaSignatureType
	}
	for _, delta := range frame.ToolCallDeltas {
		if state.activeTextType == "thought" {
			copyDelta := delta
			state.pendingActions = append(state.pendingActions, devinPendingAction{toolDelta: &copyDelta})
			continue
		}
		if err := state.processToolDelta(delta); err != nil {
			return err
		}
	}
	if frame.ContentText != "" {
		text := state.response.contentBuffer.Feed([]byte(frame.ContentText))
		state.response.captureLogText(&state.response.content, text)
		if text != "" {
			streamContentEarly := state.responseFormat == sdktranslator.FormatOpenAI ||
				state.responseFormat == sdktranslator.FormatOpenAIResponse
			if state.activeTextType == "thought" && (!streamContentEarly || len(state.pendingActions) > 0) {
				state.pendingActions = append(state.pendingActions, devinPendingAction{text: text})
			} else if !state.emitText("model_output", text) {
				return context.Canceled
			}
		}
	}
	return nil
}

func (state *devinStreamState) flushPendingActions() error {
	if len(state.pendingActions) == 0 {
		return nil
	}
	if !state.stopText() {
		return context.Canceled
	}
	actions := state.pendingActions
	state.pendingActions = nil
	var firstErr error
	for _, action := range actions {
		if action.toolDelta != nil {
			if err := state.processToolDelta(*action.toolDelta); err != nil {
				if !state.failed || errors.Is(err, context.Canceled) {
					return err
				}
				if firstErr == nil {
					firstErr = err
				}
			}
		} else if !state.emitText("model_output", action.text) {
			return context.Canceled
		}
	}
	return firstErr
}

func (state *devinStreamState) processToolDelta(delta helps.DevinToolCallDelta) error {
	previous := state.response.tools[delta.Index]
	if delta.ID != "" {
		if known := state.response.toolsByID[delta.ID]; known != nil {
			previous = known
		} else if previous != nil && previous.id != "" {
			previous = nil
		}
	}
	metadataChanged := previous != nil && ((previous.id == "" && delta.ID != "") ||
		(previous.name == "" && delta.Name != ""))
	if err := state.response.applyToolDelta(delta); err != nil {
		return err
	}
	tool := state.response.lastTool
	if state.toolStepIndices == nil {
		state.toolStepIndices = make(map[*devinToolAccumulator]int)
	}
	stepIndex, exists := state.toolStepIndices[tool]
	if !exists {
		if !state.stopText() {
			return context.Canceled
		}
		stepIndex = state.nextStepIndex
		state.nextStepIndex++
		state.toolStepIndices[tool] = stepIndex
	}
	if state.responseFormat == sdktranslator.FormatInteractions {
		if !exists || metadataChanged {
			if !state.emitToolStart(stepIndex, tool) {
				return context.Canceled
			}
		}
		if delta.Arguments != "" && !state.emitToolArguments(stepIndex, delta.Arguments) {
			return context.Canceled
		}
		return nil
	}
	if !state.toolStepStarted[tool] {
		if tool.id == "" || strings.TrimSpace(tool.name) == "" {
			return nil
		}
		if !state.emitToolStart(stepIndex, tool) {
			return context.Canceled
		}
		if state.toolStepStarted == nil {
			state.toolStepStarted = make(map[*devinToolAccumulator]bool)
		}
		state.toolStepStarted[tool] = true
		if tool.arguments.Len() > 0 && !state.emitToolArguments(stepIndex, tool.arguments.String()) {
			return context.Canceled
		}
		return nil
	}
	if delta.Arguments != "" && !state.emitToolArguments(stepIndex, delta.Arguments) {
		return context.Canceled
	}
	return nil
}

func (state *devinStreamState) emitToolArguments(stepIndex int, arguments string) bool {
	event := []byte(`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":""}}`)
	event, _ = sjson.SetBytes(event, "index", stepIndex)
	event, _ = translatorcommon.SetStringWithoutHTMLEscape(event, "delta.arguments", arguments)
	return state.emit(event)
}

func (state *devinStreamState) emitToolStart(stepIndex int, tool *devinToolAccumulator) bool {
	if tool == nil {
		return false
	}
	event := []byte(`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"","call_id":"","name":"","arguments":{}}}`)
	event, _ = sjson.SetBytes(event, "index", stepIndex)
	id := helps.EncodeDevinToolCallID(tool.id)
	event, _ = sjson.SetBytes(event, "step.id", id)
	event, _ = sjson.SetBytes(event, "step.call_id", id)
	event, _ = sjson.SetBytes(event, "step.name", tool.name)
	return state.emit(event)
}

func (state *devinStreamState) emitText(stepType, text string) bool {
	if stepType == "model_output" && len(state.toolStepIndices) > 0 {
		state.postToolContent = append(state.postToolContent, text)
		return true
	}
	if state.activeTextType != stepType {
		if state.activeTextType == "thought" && stepType == "model_output" &&
			state.responseFormat == sdktranslator.FormatOpenAIResponse {
			state.deferredThoughtStops = append(state.deferredThoughtStops, state.activeTextIndex)
			state.activeTextType = ""
			state.activeTextIndex = -1
		} else if !state.stopText() {
			return false
		}
		index := state.nextStepIndex
		state.nextStepIndex++
		start := []byte(`{"event_type":"step.start","index":0,"step":{"type":""}}`)
		start, _ = sjson.SetBytes(start, "index", index)
		start, _ = sjson.SetBytes(start, "step.type", stepType)
		if !state.emit(start) {
			return false
		}
		state.activeTextType = stepType
		state.activeTextIndex = index
		if stepType == "thought" {
			state.thoughtStepIndex = index
		}
	}

	var delta []byte
	if stepType == "thought" {
		delta = []byte(`{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","text":"","content":{"type":"text","text":""}}}`)
		delta, _ = sjson.SetBytes(delta, "delta.text", text)
		delta, _ = sjson.SetBytes(delta, "delta.content.text", text)
	} else {
		delta = []byte(`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":""}}`)
		delta, _ = sjson.SetBytes(delta, "delta.text", text)
	}
	delta, _ = sjson.SetBytes(delta, "index", state.activeTextIndex)
	return state.emit(delta)
}

func (state *devinStreamState) emitSignature(signature []byte, signatureType string) bool {
	if len(signature) == 0 {
		return true
	}
	if state.thoughtStepIndex < 0 {
		if state.activeTextType != "thought" {
			if !state.stopText() {
				return false
			}
			index := state.nextStepIndex
			state.nextStepIndex++
			start := []byte(`{"event_type":"step.start","index":0,"step":{"type":"thought"}}`)
			start, _ = sjson.SetBytes(start, "index", index)
			if !state.emit(start) {
				return false
			}
			state.activeTextType = "thought"
			state.activeTextIndex = index
		}
		state.thoughtStepIndex = state.activeTextIndex
	}
	delta := []byte(`{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":""}}`)
	delta, _ = sjson.SetBytes(delta, "index", state.thoughtStepIndex)
	value := string(signature)
	if state.responseFormat == sdktranslator.FormatOpenAIResponse {
		if state.thoughtSignatures == nil {
			state.thoughtSignatures = make(map[int]string)
		}
		state.thoughtSignatures[state.thoughtStepIndex] += value
		value = state.thoughtSignatures[state.thoughtStepIndex]
	}
	delta, _ = sjson.SetBytes(delta, "delta.signature", value)
	if signatureType != "" {
		delta, _ = sjson.SetBytes(delta, "delta.signature_type", signatureType)
	}
	return state.emit(delta)
}

func (state *devinStreamState) closeDeferredThoughts() bool {
	for _, index := range state.deferredThoughtStops {
		stop := []byte(`{"event_type":"step.stop","index":0}`)
		stop, _ = sjson.SetBytes(stop, "index", index)
		if !state.emit(stop) {
			return false
		}
	}
	state.deferredThoughtStops = nil
	return true
}

func (state *devinStreamState) stopText() bool {
	if state.activeTextType == "" {
		return true
	}
	stop := []byte(`{"event_type":"step.stop","index":0}`)
	stop, _ = sjson.SetBytes(stop, "index", state.activeTextIndex)
	if !state.emit(stop) {
		return false
	}
	state.activeTextType = ""
	state.activeTextIndex = -1
	return true
}

func (state *devinStreamState) flushTools() bool {
	type toolStep struct {
		tool      *devinToolAccumulator
		stepIndex int
	}
	steps := make([]toolStep, 0, len(state.toolStepIndices))
	for tool, stepIndex := range state.toolStepIndices {
		steps = append(steps, toolStep{tool: tool, stepIndex: stepIndex})
	}
	sort.Slice(steps, func(left, right int) bool {
		return steps[left].stepIndex < steps[right].stepIndex
	})
	for _, step := range steps {
		arguments := strings.TrimSpace(step.tool.arguments.String())
		if state.failed && (strings.TrimSpace(step.tool.name) == "" || (arguments != "" && !json.Valid([]byte(arguments)))) {
			delete(state.toolStepIndices, step.tool)
			delete(state.toolStepStarted, step.tool)
			continue
		}
		if state.responseFormat != sdktranslator.FormatInteractions && !state.toolStepStarted[step.tool] {
			if state.failed {
				delete(state.toolStepIndices, step.tool)
				continue
			}
			if step.tool.id == "" {
				step.tool.id = fmt.Sprintf("call_%d", step.tool.index)
			}
			if !state.emitToolStart(step.stepIndex, step.tool) {
				return false
			}
			if step.tool.arguments.Len() > 0 && !state.emitToolArguments(step.stepIndex, step.tool.arguments.String()) {
				return false
			}
		}
		stop := []byte(`{"event_type":"step.stop","index":0}`)
		stop, _ = sjson.SetBytes(stop, "index", step.stepIndex)
		if !state.emit(stop) {
			return false
		}
		delete(state.toolStepIndices, step.tool)
		delete(state.toolStepStarted, step.tool)
	}
	return true
}

func (state *devinStreamState) complete() {
	completed := []byte(`{"event_type":"interaction.completed","interaction":{"id":"","object":"interaction","status":"completed","model":"","usage":{}}}`)
	completed, _ = sjson.SetBytes(completed, "interaction.id", state.interactionID)
	completed, _ = sjson.SetBytes(completed, "interaction.model", state.req.Model)
	completed = setDevinTerminationJSON(completed, "interaction", state.response.stopReason)
	var err error
	completed, err = setDevinUsageJSON(completed, "interaction.usage", state.response.usage)
	if err != nil {
		state.fail(devinProtocolError(err.Error()), "usage_error")
		return
	}
	if !state.emit(completed) || !state.emitDone() {
		return
	}
	state.reporter.publish(state.ctx, devinUsageWithCacheWrites(parseInteractionsUsage(completed)))
	state.appendResponseLog("completed")
}

func (state *devinStreamState) fail(err error, code string) {
	if state.failed || err == nil {
		return
	}
	state.failed = true
	if flushErr := state.flushPendingActions(); flushErr != nil && !errors.Is(flushErr, context.Canceled) {
		err = devinProtocolError(flushErr.Error())
		code = "protocol_error"
	}
	_ = state.closeDeferredThoughts()
	_ = state.stopText()
	_ = state.flushTools()
	texts := state.postToolContent
	state.postToolContent = nil
	for _, text := range texts {
		if !state.emitText("model_output", text) {
			break
		}
	}
	_ = state.stopText()
	state.appendResponseLog(code)
	recordAPIResponseError(state.ctx, state.executor.cfg, err)
	state.reporter.publishFailureWithDetail(state.ctx, devinUsageDetail(state.response.usage), err)

	statusCode := http.StatusBadGateway
	if statusErrValue, ok := err.(interface{ StatusCode() int }); ok && statusErrValue.StatusCode() != 0 {
		statusCode = statusErrValue.StatusCode()
	}
	failed := []byte(`{"event_type":"response.failed","interaction_id":"","response":{"id":"","status":"failed","error":{"type":"api_error","code":"","message":""}},"error":{"type":"api_error","code":"","message":""}}`)
	failed, _ = sjson.SetBytes(failed, "interaction_id", state.interactionID)
	failed, _ = sjson.SetBytes(failed, "response.id", state.interactionID)
	failed, _ = sjson.SetBytes(failed, "response.error.code", code)
	failed, _ = sjson.SetBytes(failed, "response.error.message", err.Error())
	failed, _ = sjson.SetBytes(failed, "response.status_code", statusCode)
	failed, _ = sjson.SetBytes(failed, "error.code", code)
	failed, _ = sjson.SetBytes(failed, "error.message", err.Error())
	_ = state.emit(failed)
	_ = sendStreamChunk(state.ctx, state.out, cliproxyexecutor.StreamChunk{Err: err})
}

func (state *devinStreamState) appendResponseLog(status string) {
	if state == nil || state.executor == nil || !requestLogCaptureEnabled(state.executor.cfg) {
		return
	}
	appendAPIResponseChunk(state.ctx, state.executor.cfg, state.response.logBytes(status))
}

func devinUsageDetail(value *helps.DevinUsage) usage.Detail {
	if value == nil || validateDevinUsage(value) != nil {
		return usage.Detail{}
	}
	input, _ := usage.AddNonNegativeInt64(value.PromptTokens, value.CachedTokens)
	total, _ := usage.AddNonNegativeInt64(input, value.CompletionTokens)
	detail := usage.Detail{
		InputTokens:         input,
		OutputTokens:        value.CompletionTokens,
		CachedTokens:        value.CachedTokens,
		CacheReadTokens:     value.CachedTokens,
		CacheCreationTokens: value.CacheWriteTokens,
		TotalTokens:         total,
	}
	detail.TokenBreakdown = usage.NewIndependentTokenBreakdown(
		value.PromptTokens,
		value.CachedTokens,
		0,
		value.CompletionTokens,
		0,
		total,
	)
	return devinUsageWithCacheWrites(detail)
}

func devinUsageWithCacheWrites(detail usage.Detail) usage.Detail {
	if detail.CacheCreationTokens > 0 {
		// Devin excludes cache writes from its reported input and total token counts.
		detail.TokenBreakdown = usage.NewUnclassifiedTokenBreakdown(detail.TotalTokens)
		detail.TokenBreakdown.Quality = usage.TokenAccountingQualityUnclassified
	}
	return detail
}

func (state *devinStreamState) emit(payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	if !state.createdSent {
		eventType := gjson.GetBytes(payload, "event_type").String()
		if eventType == "response.failed" || eventType == "interaction.failed" {
			appendAPIResponseChunk(state.ctx, state.executor.cfg, append(bytes.Clone(payload), '\n'))
			return true
		}
		state.createdSent = true
		if eventType != "interaction.created" {
			created := []byte(`{"event_type":"interaction.created","interaction":{"id":"","object":"interaction","status":"in_progress","model":""}}`)
			created, _ = sjson.SetBytes(created, "interaction.id", state.interactionID)
			created, _ = sjson.SetBytes(created, "interaction.model", state.req.Model)
			if !state.emit(created) {
				return false
			}
		}
	}
	appendAPIResponseChunk(state.ctx, state.executor.cfg, append(bytes.Clone(payload), '\n'))
	if state.responseFormat == sdktranslator.FormatInteractions {
		return sendStreamChunk(
			state.ctx,
			state.out,
			cliproxyexecutor.StreamChunk{Payload: encodeSSEDataFrame(payload)},
		)
	}
	chunks := sdktranslator.TranslateStream(
		state.ctx,
		sdktranslator.FormatInteractions,
		state.responseFormat,
		state.req.Model,
		state.opts.OriginalRequest,
		state.requestBody,
		payload,
		&state.translateParam,
	)
	sent := sendStreamPayloads(state.ctx, state.out, chunks)
	if errTranslate := sdktranslator.ResponseTranslationError(&state.translateParam); errTranslate != nil {
		state.fail(errTranslate, "invalid_tool_arguments")
		return false
	}
	return sent
}

func (state *devinStreamState) emitDone() bool {
	if state.responseFormat == sdktranslator.FormatInteractions {
		return sendStreamChunk(
			state.ctx,
			state.out,
			cliproxyexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")},
		)
	}
	chunks := sdktranslator.TranslateStream(
		state.ctx,
		sdktranslator.FormatInteractions,
		state.responseFormat,
		state.req.Model,
		state.opts.OriginalRequest,
		state.requestBody,
		[]byte("[DONE]"),
		&state.translateParam,
	)
	if errTranslate := sdktranslator.ResponseTranslationError(&state.translateParam); errTranslate != nil {
		state.fail(errTranslate, "invalid_tool_arguments")
		return false
	}
	return sendStreamPayloads(state.ctx, state.out, chunks)
}
