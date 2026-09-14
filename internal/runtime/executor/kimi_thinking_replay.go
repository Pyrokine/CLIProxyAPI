package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	helpers "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type kimiThinkingReplayScope struct {
	modelFamily   string
	sessionKey    string
	snapshot      internalcache.KimiThinkingReplaySnapshot
	cacheReady    bool
	replayApplied bool
}

func (s kimiThinkingReplayScope) valid() bool {
	return strings.TrimSpace(s.modelFamily) != "" && strings.TrimSpace(s.sessionKey) != ""
}

func kimiThinkingReplayModelFamily(model string) string {
	baseModel := thinking.ParseSuffix(strings.TrimSpace(model)).ModelName
	normalized := normalizeKimiUpstreamModel(baseModel)
	switch normalized {
	case "k3", "k3-256k":
		return "k3"
	default:
		return normalized
	}
}

func kimiThinkingReplayScopeFromRequest(
	ctx context.Context,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) kimiThinkingReplayScope {
	sessionKey := codexReasoningReplaySessionKey(
		ctx,
		sdktranslator.FormatClaude,
		req,
		opts,
		req.Payload,
	)
	sessionKey = xaiReasoningReplayIsolateSessionKey(ctx, sessionKey)
	return kimiThinkingReplayScope{
		modelFamily: kimiThinkingReplayModelFamily(req.Model),
		sessionKey:  sessionKey,
	}
}

func prepareKimiThinkingReplayRequest(
	ctx context.Context,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Request, kimiThinkingReplayScope, error) {
	scope := kimiThinkingReplayScopeFromRequest(ctx, req, opts)
	if !scope.valid() {
		return req, scope, nil
	}
	content, snapshot, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	scope.snapshot = snapshot
	scope.cacheReady = errGet == nil
	if errGet != nil {
		return req, scope, fmt.Errorf("kimi executor: thinking replay cache read failed: %w", errGet)
	}
	if !found {
		return req, scope, nil
	}
	updated, restored := restoreKimiThinkingReplayContent(bytes.Clone(req.Payload), content)
	if restored {
		req.Payload = updated
		scope.replayApplied = true
	}
	return req, scope, nil
}

func cacheKimiThinkingReplayResponse(
	ctx context.Context,
	scope kimiThinkingReplayScope,
	response []byte,
) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	content := gjson.GetBytes(response, "content")
	if !content.IsArray() {
		return
	}
	cacheKimiThinkingReplayContent(ctx, scope, []byte(content.Raw))
}

func cacheKimiThinkingReplayContent(
	ctx context.Context,
	scope kimiThinkingReplayScope,
	content []byte,
) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	if kimiThinkingReplayContentIsReplayable(content) {
		if _, errReplace := internalcache.ReplaceKimiThinkingReplayIfUnchanged(
			ctx,
			scope.modelFamily,
			scope.sessionKey,
			scope.snapshot,
			content,
		); errReplace != nil {
			log.WithContext(ctx).Warnf("kimi thinking replay cache replace failed: %v", errReplace)
		}
		return
	}
	clearKimiThinkingReplayContent(ctx, scope)
}

func shouldClearKimiThinkingReplayAfterError(err error) bool {
	if err == nil {
		return false
	}
	if upstreamHTTPError, ok := errors.AsType[claudeUpstreamHTTPError](err); ok {
		statusCode := upstreamHTTPError.StatusCode()
		return statusCode == 400 || statusCode == 422
	}
	if terminated, ok := errors.AsType[*cliproxyexecutor.RequestTerminatedError](err); ok {
		statusCode := terminated.StatusCode()
		return statusCode == 400 || statusCode == 422
	}
	var upstreamSSEError claudeUpstreamSSEError
	return errors.As(err, &upstreamSSEError) && upstreamSSEError.rejectsReplayState()
}

func clearKimiThinkingReplayContent(ctx context.Context, scope kimiThinkingReplayScope) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	if _, errDelete := internalcache.DeleteKimiThinkingReplayIfUnchanged(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
		scope.snapshot,
	); errDelete != nil {
		log.WithContext(ctx).Warnf("kimi thinking replay cache delete failed: %v", errDelete)
	}
}

func kimiThinkingReplayContentIsReplayable(content []byte) bool {
	if len(content) == 0 || len(content) > internalcache.KimiThinkingReplayCacheMaxBytesPerEntry ||
		!gjson.ValidBytes(content) {
		return false
	}
	root := gjson.ParseBytes(content)
	if !root.IsArray() {
		return false
	}
	blocks := root.Array()
	if len(blocks) == 0 || len(blocks) > internalcache.KimiThinkingReplayCacheMaxBlocksPerEntry {
		return false
	}
	hasSignedThinking := false
	hasToolUse := false
	for _, part := range blocks {
		if !part.IsObject() || part.Get("type").Type != gjson.String ||
			strings.TrimSpace(part.Get("type").String()) == "" {
			return false
		}
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking":
			if strings.TrimSpace(part.Get("signature").String()) != "" {
				hasSignedThinking = true
			}
		case "tool_use":
			if strings.TrimSpace(part.Get("id").String()) != "" {
				hasToolUse = true
			}
		}
	}
	return hasSignedThinking && hasToolUse
}

func restoreKimiThinkingReplayContent(body, cachedContent []byte) ([]byte, bool) {
	cachedParts, cachedOK := kimiNonThinkingContentParts(gjson.ParseBytes(cachedContent))
	if !cachedOK {
		return body, false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, false
	}
	messageItems := messages.Array()
	for index, message := range slices.Backward(messageItems) {

		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		currentContent := message.Get("content")
		if kimiJSONEqual([]byte(currentContent.Raw), cachedContent) {
			return body, false
		}
		if kimiContentHasThinking(currentContent) {
			continue
		}
		currentParts, currentOK := kimiNonThinkingContentParts(currentContent)
		if !currentOK || !kimiCanonicalPartsEqual(currentParts, cachedParts) {
			continue
		}
		updated, errSet := sjson.SetRawBytes(
			body,
			fmt.Sprintf("messages.%d.content", index),
			cachedContent,
		)
		if errSet != nil {
			return body, false
		}
		return updated, true
	}
	return body, false
}

func kimiContentHasThinking(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking", "redacted_thinking":
			return true
		}
	}
	return false
}

func kimiNonThinkingContentParts(content gjson.Result) ([][]byte, bool) {
	if !content.IsArray() {
		return nil, false
	}
	parts := make([][]byte, 0, len(content.Array()))
	hasToolUse := false
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use":
			if strings.TrimSpace(part.Get("id").String()) == "" {
				return nil, false
			}
			hasToolUse = true
		}
		canonical, ok := kimiCanonicalJSON([]byte(part.Raw))
		if !ok {
			return nil, false
		}
		parts = append(parts, canonical)
	}
	return parts, hasToolUse
}

func kimiCanonicalPartsEqual(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func kimiJSONEqual(left, right []byte) bool {
	canonicalLeft, leftOK := kimiCanonicalJSON(left)
	canonicalRight, rightOK := kimiCanonicalJSON(right)
	return leftOK && rightOK && bytes.Equal(canonicalLeft, canonicalRight)
}

func kimiCanonicalJSON(raw []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, false
	}
	canonical, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, false
	}
	return canonical, true
}

type kimiThinkingReplayStreamBlock struct {
	raw                  []byte
	text                 strings.Builder
	thinking             strings.Builder
	signature            strings.Builder
	input                strings.Builder
	citations            [][]byte
	textInitialized      bool
	thinkingInitialized  bool
	signatureInitialized bool
	hasInputDelta        bool
	finished             bool
}

type kimiThinkingReplayStreamAccumulator struct {
	blocks               map[int]*kimiThinkingReplayStreamBlock
	upstreamErrorType    string
	observed             bool
	complete             bool
	abandoned            bool
	discardOldOnComplete bool
	bytesUsed            int
}

func newKimiThinkingReplayStreamAccumulator() *kimiThinkingReplayStreamAccumulator {
	return &kimiThinkingReplayStreamAccumulator{
		blocks: make(map[int]*kimiThinkingReplayStreamBlock),
	}
}

func (a *kimiThinkingReplayStreamAccumulator) observe(chunk []byte) {
	for line := range bytes.SplitSeq(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(payload) {
			a.abandon()
			continue
		}
		root := gjson.ParseBytes(payload)
		switch root.Get("type").String() {
		case "message_start":
			if a.observed || a.complete || len(a.blocks) != 0 {
				a.abandon()
				continue
			}
			a.observed = true
		case "content_block_start":
			if !a.observed || a.complete {
				a.abandon()
				continue
			}
			if !a.abandoned {
				a.observeBlockStart(root)
			}
		case "content_block_delta":
			if !a.observed || a.complete {
				a.abandon()
				continue
			}
			if !a.abandoned {
				a.observeBlockDelta(root)
			}
		case "content_block_stop":
			if !a.observed || a.complete {
				a.abandon()
				continue
			}
			if !a.abandoned {
				a.finishBlock(root)
			}
		case "message_delta":
			if !a.observed || a.complete {
				a.abandon()
			}
		case "message_stop":
			if !a.observed || a.complete {
				a.abandon()
				continue
			}
			a.complete = true
		case "error":
			a.upstreamErrorType = strings.TrimSpace(root.Get("error.type").String())
			a.abandon()
		}
	}
}

func kimiThinkingReplayStreamIndex(root gjson.Result) (int, bool) {
	result := root.Get("index")
	if result.Type != gjson.Number {
		return 0, false
	}
	index, errParse := strconv.Atoi(result.Raw)
	return index, errParse == nil && index >= 0
}

func (a *kimiThinkingReplayStreamAccumulator) observeBlockStart(root gjson.Result) {
	index, ok := kimiThinkingReplayStreamIndex(root)
	if !ok {
		a.abandon()
		return
	}
	block := root.Get("content_block")
	if !block.IsObject() || block.Get("type").Type != gjson.String ||
		strings.TrimSpace(block.Get("type").String()) == "" {
		a.abandon()
		return
	}
	if _, exists := a.blocks[index]; exists {
		a.abandon()
		return
	}
	if len(a.blocks) >= internalcache.KimiThinkingReplayCacheMaxBlocksPerEntry {
		a.abandonForLimit()
		return
	}
	raw := []byte(block.Raw)
	if !a.reserveBytes(len(raw)) {
		return
	}
	a.blocks[index] = &kimiThinkingReplayStreamBlock{raw: append([]byte(nil), raw...)}
}

func (a *kimiThinkingReplayStreamAccumulator) observeBlockDelta(root gjson.Result) {
	index, ok := kimiThinkingReplayStreamIndex(root)
	if !ok {
		a.abandon()
		return
	}
	block, ok := a.blocks[index]
	if !ok || block.finished {
		a.abandon()
		return
	}
	delta := root.Get("delta")
	switch delta.Get("type").String() {
	case "text_delta":
		a.appendBlockText(
			block,
			&block.text,
			&block.textInitialized,
			"text",
			delta.Get("text").String(),
		)
	case "thinking_delta":
		a.appendBlockText(
			block,
			&block.thinking,
			&block.thinkingInitialized,
			"thinking",
			delta.Get("thinking").String(),
		)
	case "signature_delta":
		a.appendBlockText(
			block,
			&block.signature,
			&block.signatureInitialized,
			"signature",
			delta.Get("signature").String(),
		)
	case "input_json_delta":
		suffix := delta.Get("partial_json").String()
		if a.reserveBytes(len(suffix)) {
			block.input.WriteString(suffix)
			block.hasInputDelta = true
		}
	case "citations_delta":
		a.appendBlockCitation(block, delta.Get("citation"))
	default:
		a.abandon()
	}
}

func (a *kimiThinkingReplayStreamAccumulator) appendBlockCitation(
	block *kimiThinkingReplayStreamBlock,
	citation gjson.Result,
) {
	if strings.TrimSpace(gjson.GetBytes(block.raw, "type").String()) != "text" || !citation.IsObject() {
		a.abandon()
		return
	}
	raw := []byte(citation.Raw)
	if a.reserveBytes(len(raw)) {
		block.citations = append(block.citations, append([]byte(nil), raw...))
	}
}

func (a *kimiThinkingReplayStreamAccumulator) appendBlockText(
	block *kimiThinkingReplayStreamBlock,
	builder *strings.Builder,
	initialized *bool,
	path,
	suffix string,
) {
	if !*initialized {
		initial := gjson.GetBytes(block.raw, path).String()
		if !a.reserveBytes(len(initial)) {
			return
		}
		builder.WriteString(initial)
		*initialized = true
	}
	if a.reserveBytes(len(suffix)) {
		builder.WriteString(suffix)
	}
}

func (a *kimiThinkingReplayStreamAccumulator) finishBlock(root gjson.Result) {
	index, ok := kimiThinkingReplayStreamIndex(root)
	if !ok {
		a.abandon()
		return
	}
	block, ok := a.blocks[index]
	if !ok || block.finished {
		a.abandon()
		return
	}
	if block.hasInputDelta && !gjson.Valid(block.input.String()) {
		a.abandon()
		return
	}
	block.finished = true
}

func (a *kimiThinkingReplayStreamAccumulator) reserveBytes(count int) bool {
	if count < 0 || count > internalcache.KimiThinkingReplayCacheMaxBytesPerEntry-a.bytesUsed {
		a.abandonForLimit()
		return false
	}
	a.bytesUsed += count
	return true
}

func (a *kimiThinkingReplayStreamAccumulator) abandonForLimit() {
	a.discardOldOnComplete = true
	a.abandon()
}

func (a *kimiThinkingReplayStreamAccumulator) abandon() {
	a.abandoned = true
	a.blocks = nil
	a.bytesUsed = 0
}

func (a *kimiThinkingReplayStreamAccumulator) rejectsReplayState() bool {
	return claudeUpstreamSSEError{errorType: a.upstreamErrorType}.rejectsReplayState()
}

func (a *kimiThinkingReplayStreamAccumulator) content() ([]byte, bool) {
	if !a.observed || !a.complete || a.abandoned {
		return nil, false
	}
	indexes := make([]int, 0, len(a.blocks))
	for index := range a.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	parts := make([][]byte, 0, len(indexes))
	for _, index := range indexes {
		if index != len(parts) {
			a.abandon()
			return nil, false
		}
		block := a.blocks[index]
		if !block.finished {
			a.abandon()
			return nil, false
		}
		raw := append([]byte(nil), block.raw...)
		var errSet error
		if block.textInitialized {
			raw, errSet = sjson.SetBytes(raw, "text", block.text.String())
		}
		if errSet == nil && block.thinkingInitialized {
			raw, errSet = sjson.SetBytes(raw, "thinking", block.thinking.String())
		}
		if errSet == nil && block.signatureInitialized {
			raw, errSet = sjson.SetBytes(raw, "signature", block.signature.String())
		}
		if errSet == nil && block.hasInputDelta {
			raw, errSet = sjson.SetRawBytes(raw, "input", []byte(block.input.String()))
		}
		if errSet == nil && len(block.citations) > 0 {
			raw, errSet = appendKimiThinkingReplayCitations(raw, block.citations)
		}
		if errSet != nil {
			a.abandon()
			return nil, false
		}
		parts = append(parts, raw)
	}
	content := helpers.JoinRawJSONArray(parts)
	if len(content) > internalcache.KimiThinkingReplayCacheMaxBytesPerEntry {
		a.abandonForLimit()
		return nil, false
	}
	return content, true
}

func appendKimiThinkingReplayCitations(raw []byte, citations [][]byte) ([]byte, error) {
	parts := make([][]byte, 0, len(citations))
	existing := gjson.GetBytes(raw, "citations")
	if existing.Exists() {
		if !existing.IsArray() {
			return nil, fmt.Errorf("invalid citations content")
		}
		for _, citation := range existing.Array() {
			if !citation.IsObject() {
				return nil, fmt.Errorf("invalid citation content")
			}
			parts = append(parts, []byte(citation.Raw))
		}
	}
	for _, citation := range citations {
		if !gjson.ParseBytes(citation).IsObject() {
			return nil, fmt.Errorf("invalid citation delta")
		}
		parts = append(parts, append([]byte(nil), citation...))
	}
	return sjson.SetRawBytes(raw, "citations", helpers.JoinRawJSONArray(parts))
}

type kimiThinkingReplayStreamObserver struct {
	ctx         context.Context
	scope       kimiThinkingReplayScope
	accumulator *kimiThinkingReplayStreamAccumulator
}

func newKimiThinkingReplayStreamObserver(
	ctx context.Context,
	scope kimiThinkingReplayScope,
) *kimiThinkingReplayStreamObserver {
	return &kimiThinkingReplayStreamObserver{
		ctx:         ctx,
		scope:       scope,
		accumulator: newKimiThinkingReplayStreamAccumulator(),
	}
}

func (o *kimiThinkingReplayStreamObserver) ObserveClaudeSSELine(line []byte) {
	if o == nil || o.accumulator == nil {
		return
	}
	o.accumulator.observe(line)
}

func (o *kimiThinkingReplayStreamObserver) FinishClaudeStream(outcome claudeStreamOutcome) {
	if o == nil || o.accumulator == nil {
		return
	}
	// Incomplete, validation-failed, unreadable, and canceled streams preserve the existing replay state.
	// noinspection GoSwitchMissingCasesForIotaConsts
	switch outcome {
	case claudeStreamOutcomeComplete:
		if content, completed := o.accumulator.content(); completed {
			cacheKimiThinkingReplayContent(o.ctx, o.scope, content)
		} else if o.accumulator.discardOldOnComplete {
			clearKimiThinkingReplayContent(o.ctx, o.scope)
		}
	case claudeStreamOutcomeUpstreamError:
		if o.scope.replayApplied && o.accumulator.rejectsReplayState() {
			clearKimiThinkingReplayContent(o.ctx, o.scope)
		}
	}
}
