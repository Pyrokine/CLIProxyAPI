package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	codexClientVersion         = "0.144.0"
	codexUserAgent             = "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)"
	codexOriginator            = "codex-tui"
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadata = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"
	maxCodexResponseBytes      = 50 << 20
)

var dataTag = []byte("data:")

const codexIncompleteStreamMessage = "stream error: stream disconnected before completion: stream closed before response.completed"

type codexIncompleteStreamError struct {
	statusErr
}

func newCodexIncompleteStreamError() codexIncompleteStreamError {
	return codexIncompleteStreamError{
		code: http.StatusRequestTimeout,
		msg:  codexIncompleteStreamMessage,
	}
}

func (codexIncompleteStreamError) IsRequestScoped() bool {
	return true
}

func (codexIncompleteStreamError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeUnknown,
		RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
		EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
	}
}

func readCodexResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("codex executor: invalid response size limit %d", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return data, markResponseStarted(err)
	}
	if int64(len(data)) > maxBytes {
		return nil, markResponseStarted(
			statusErr{
				code: http.StatusBadGateway,
				msg: fmt.Sprintf(
					"codex executor: upstream response exceeds maximum allowed size of %d bytes", maxBytes,
				),
			},
		)
	}
	return data, nil
}

func collectCodexOutputItemDone(eventData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) {
	itemResult := gjson.GetBytes(eventData, "item")
	if !itemResult.Exists() || itemResult.Type != gjson.JSON {
		return
	}
	outputIndexResult := gjson.GetBytes(eventData, "output_index")
	if outputIndexResult.Exists() {
		outputItemsByIndex[outputIndexResult.Int()] = []byte(itemResult.Raw)
		return
	}
	*outputItemsFallback = append(*outputItemsFallback, []byte(itemResult.Raw))
}

func hydrateCodexCompletedOutputItemIDs(
	eventData []byte,
	outputItems []gjson.Result,
	outputItemsByIndex map[int64][]byte,
) []byte {
	patchedData := eventData
	for outputIndex, outputItem := range outputItems {
		itemID := outputItem.Get("id")
		if itemID.Exists() && itemID.Type != gjson.Null &&
			(itemID.Type != gjson.String || strings.TrimSpace(itemID.String()) != "") {
			continue
		}
		completedItem, ok := outputItemsByIndex[int64(outputIndex)]
		if !ok {
			continue
		}
		completedID := gjson.GetBytes(completedItem, "id")
		if completedID.Type != gjson.String || strings.TrimSpace(completedID.String()) == "" {
			continue
		}
		updatedData, err := sjson.SetRawBytes(
			patchedData, fmt.Sprintf("response.output.%d.id", outputIndex), []byte(completedID.Raw),
		)
		if err == nil {
			patchedData = updatedData
		}
	}
	return patchedData
}

func patchCodexCompletedOutput(
	eventData []byte,
	outputItemsByIndex map[int64][]byte,
	outputItemsFallback [][]byte,
) []byte {
	outputResult := gjson.GetBytes(eventData, "response.output")
	if outputResult.Exists() && outputResult.IsArray() && len(outputResult.Array()) > 0 {
		return hydrateCodexCompletedOutputItemIDs(eventData, outputResult.Array(), outputItemsByIndex)
	}
	shouldPatchOutput := (!outputResult.Exists() || !outputResult.IsArray() || len(outputResult.Array()) == 0) && (len(outputItemsByIndex) > 0 || len(outputItemsFallback) > 0)
	if !shouldPatchOutput {
		return eventData
	}

	indexes := make([]int64, 0, len(outputItemsByIndex))
	for idx := range outputItemsByIndex {
		indexes = append(indexes, idx)
	}
	slices.Sort(
		indexes)

	items := make([][]byte, 0, len(outputItemsByIndex)+len(outputItemsFallback))
	for _, idx := range indexes {
		items = append(items, outputItemsByIndex[idx])
	}
	items = append(items, outputItemsFallback...)
	array := make([]string, 0, len(items))
	for _, item := range items {
		if len(bytes.TrimSpace(item)) > 0 {
			array = append(array, string(item))
		}
	}
	if len(array) == 0 {
		return eventData
	}
	updated, err := sjson.SetRawBytes(eventData, "response.output", []byte("["+strings.Join(array, ",")+"]"))
	if err != nil {
		return eventData
	}
	return updated
}

type codexReasoningReplayScope struct {
	modelName          string
	sessionKey         string
	requestFingerprint string
}

func (s codexReasoningReplayScope) valid() bool {
	return strings.TrimSpace(s.modelName) != "" && strings.TrimSpace(s.sessionKey) != ""
}

func applyCodexReasoningReplayCacheRequired(
	ctx context.Context,
	from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
) ([]byte, codexReasoningReplayScope, error) {
	scope := codexReasoningReplayScopeFromRequest(ctx, from, req, opts, body)
	if !scope.valid() {
		return body, scope, nil
	}
	items, ok, errReplay := internalcache.GetCodexReasoningReplayItemsRequired(
		ctx,
		scope.modelName,
		scope.sessionKey,
	)
	if errReplay != nil || !ok {
		return body, scope, errReplay
	}
	updated, ok := insertCodexReasoningReplayTurns(body, items)
	if !ok {
		return body, scope, nil
	}
	return updated, scope, nil
}

func codexReasoningReplayScopeFromRequest(
	ctx context.Context,
	from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
) codexReasoningReplayScope {
	if !sourceFormatEqual(from, sdktranslator.FormatClaude) {
		return codexReasoningReplayScope{}
	}
	modelName := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if modelName == "" {
		modelName = thinking.ParseSuffix(req.Model).ModelName
	}
	inputItems := gjson.GetBytes(body, "input").Array()
	return codexReasoningReplayScope{
		modelName:          modelName,
		sessionKey:         codexReasoningReplaySessionKey(ctx, from, req, opts, body),
		requestFingerprint: codexReplayInputPrefixFingerprint(inputItems, len(inputItems)),
	}
}

func sourceFormatEqual(from, want sdktranslator.Format) bool {
	return strings.EqualFold(strings.TrimSpace(from.String()), want.String())
}

func codexClaudeCodeReplaySessionKey(
	ctx context.Context,
	payload []byte,
	headers http.Header,
) string {
	sessionKey, _ := helps.ClaudeCodeExecutionScope(ctx, payload, headers)
	return sessionKey
}

func codexReasoningReplaySessionKey(
	ctx context.Context,
	from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		if sessionKey := codexClaudeCodeReplaySessionKey(ctx, req.Payload, opts.Headers); sessionKey != "" {
			return sessionKey
		}
	}
	if value := metadataString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return "execution:" + value
	}
	if value := metadataString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return "execution:" + value
	}
	if value := codexReasoningReplaySessionKeyFromPayload(body); value != "" {
		return value
	}
	if value := codexReasoningReplaySessionKeyFromPayload(req.Payload); value != "" {
		return value
	}
	if value := codexReasoningReplaySessionKeyFromHeaders(opts.Headers); value != "" {
		return value
	}
	if ginCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		if value := codexReasoningReplaySessionKeyFromHeaders(ginCtx.Request.Header); value != "" {
			return value
		}
	}
	if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		if apiKey := strings.TrimSpace(apiKeyFromContext(ctx)); apiKey != "" {
			return "prompt-cache:" + uuid.NewSHA1(
				uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey),
			).String()
		}
	}
	return ""
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return cliproxysession.NormalizeExplicitID(v)
	case []byte:
		return cliproxysession.NormalizeExplicitID(string(v))
	default:
		return ""
	}
}

func codexReasoningReplaySessionKeyFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	if promptCacheKey := cliproxysession.NormalizeExplicitID(
		gjson.GetBytes(
			payload, "prompt_cache_key",
		).String(),
	); promptCacheKey != "" {
		return "prompt-cache:" + promptCacheKey
	}
	if windowID := cliproxysession.NormalizeExplicitID(
		gjson.GetBytes(
			payload, "client_metadata.x-codex-window-id",
		).String(),
	); windowID != "" {
		return "window:" + windowID
	}
	if turnMetadata := strings.TrimSpace(
		gjson.GetBytes(
			payload, "client_metadata.x-codex-turn-metadata",
		).String(),
	); turnMetadata != "" {
		return codexReasoningReplaySessionKeyFromTurnMetadata(turnMetadata)
	}
	return ""
}

func codexReasoningReplaySessionKeyFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if turnMetadata := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); turnMetadata != "" {
		if key := codexReasoningReplaySessionKeyFromTurnMetadata(turnMetadata); key != "" {
			return key
		}
	}
	if windowID := cliproxysession.ExplicitHeaderValue(headers, "X-Codex-Window-Id"); windowID != "" {
		return "window:" + windowID
	}
	for _, headerName := range []string{"Session_id", "session_id", "Session-Id"} {
		if value := cliproxysession.ExplicitHeaderValue(headers, headerName); value != "" {
			return "session-id:" + value
		}
	}
	if conversationID := cliproxysession.ExplicitHeaderValue(headers, "Conversation_id"); conversationID != "" {
		return "conversation_id:" + conversationID
	}
	return ""
}

func headerValueCaseInsensitive(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func setHeaderCasePreserved(headers http.Header, name string, value string) {
	if headers == nil {
		return
	}
	for key := range headers {
		if strings.EqualFold(key, name) && key != name {
			delete(headers, key)
		}
	}
	headers[name] = []string{value}
}

func codexReasoningReplaySessionKeyFromTurnMetadata(turnMetadata string) string {
	if promptCacheKey := cliproxysession.NormalizeExplicitID(
		gjson.Get(
			turnMetadata, "prompt_cache_key",
		).String(),
	); promptCacheKey != "" {
		return "prompt-cache:" + promptCacheKey
	}
	if windowID := cliproxysession.NormalizeExplicitID(gjson.Get(turnMetadata, "window_id").String()); windowID != "" {
		return "window:" + windowID
	}
	return ""
}

func codexInputHasValidReasoningEncryptedContent(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		encryptedContent := item.Get("encrypted_content")
		if encryptedContent.Type != gjson.String {
			continue
		}
		if internalcache.IsValidGPTReasoningEncryptedContent(encryptedContent.String()) {
			return true
		}
	}
	return false
}

func filterCodexReasoningReplayItemsForInput(body []byte, items [][]byte) [][]byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil
	}

	hasInputReasoning := codexInputHasValidReasoningEncryptedContent(body)
	existingCalls := make(map[string]bool)
	existingOutputs := make(map[string]bool)
	for _, inputItem := range input.Array() {
		itemType := strings.TrimSpace(inputItem.Get("type").String())
		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			callID := strings.TrimSpace(inputItem.Get("call_id").String())
			if callID != "" {
				for _, candidate := range codexReplayComparableCallIDs(callID) {
					existingOutputs[candidate] = true
				}
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
			if hasInputReasoning {
				continue
			}
		case "function_call", "custom_tool_call":
			keys := codexReplayToolCallKeys(itemResult)
			if len(keys) == 0 || codexReplayAnyToolCallKeyExists(existingCalls, keys) {
				continue
			}
			hasMatchingOutput := false
			callID := strings.TrimSpace(itemResult.Get("call_id").String())
			if callID != "" {
				for _, candidate := range codexReplayComparableCallIDs(callID) {
					if existingOutputs[candidate] {
						hasMatchingOutput = true
						break
					}
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

func insertCodexReasoningReplayItems(body []byte, replayItems [][]byte) ([]byte, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() || len(replayItems) == 0 {
		return body, false
	}
	inputItems := input.Array()
	insertIndex := codexReasoningReplayInsertIndex(inputItems, replayItems)
	replayItems = codexAlignReasoningReplayToolCallIDs(inputItems, replayItems)
	items := make([]string, 0, len(inputItems)+len(replayItems))
	for i, inputItem := range inputItems {
		if i == insertIndex {
			for _, replayItem := range replayItems {
				items = append(items, string(replayItem))
			}
		}
		items = append(items, inputItem.Raw)
	}
	if insertIndex == len(inputItems) {
		for _, replayItem := range replayItems {
			items = append(items, string(replayItem))
		}
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(items, ",")+"]"))
	if err != nil {
		return body, false
	}
	return updated, true
}

func codexReasoningReplayInsertIndex(inputItems []gjson.Result, replayItems [][]byte) int {
	replayCallIDs := make(map[string]bool)
	for _, replayItem := range replayItems {
		itemResult := gjson.ParseBytes(replayItem)
		itemType := strings.TrimSpace(itemResult.Get("type").String())
		if itemType != "function_call" && itemType != "custom_tool_call" {
			continue
		}
		for _, callID := range codexReplayComparableCallIDs(itemResult.Get("call_id").String()) {
			replayCallIDs[callID] = true
		}
	}
	if len(replayCallIDs) > 0 {
		for index, inputItem := range inputItems {
			itemType := strings.TrimSpace(inputItem.Get("type").String())
			if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
				continue
			}
			callID := strings.TrimSpace(inputItem.Get("call_id").String())
			if callID == "" || replayCallIDs[callID] {
				return index
			}
		}
	}
	for index, inputItem := range slices.Backward(inputItems) {

		if role, ok := codexReplayMessageRole(inputItem); ok && role == "assistant" {
			return index
		}
	}
	for index, inputItem := range inputItems {
		if shouldInsertCodexReasoningReplayBefore(inputItem) {
			return index
		}
	}
	return len(inputItems)
}

func codexAlignReasoningReplayToolCallIDs(inputItems []gjson.Result, replayItems [][]byte) [][]byte {
	outputCallIDs := codexReplayOutputCallIDs(inputItems)
	if len(outputCallIDs) == 0 {
		return replayItems
	}

	aligned := make([][]byte, 0, len(replayItems))
	for _, replayItem := range replayItems {
		itemResult := gjson.ParseBytes(replayItem)
		itemType := strings.TrimSpace(itemResult.Get("type").String())
		if itemType != "function_call" && itemType != "custom_tool_call" {
			aligned = append(aligned, replayItem)
			continue
		}

		callID := strings.TrimSpace(itemResult.Get("call_id").String())
		outputCallID := ""
		for _, candidate := range codexReplayComparableCallIDs(callID) {
			if value := outputCallIDs[candidate]; value != "" {
				outputCallID = value
				break
			}
		}
		if outputCallID == "" || outputCallID == callID {
			aligned = append(aligned, replayItem)
			continue
		}

		updated, err := sjson.SetBytes(replayItem, "call_id", outputCallID)
		if err != nil {
			aligned = append(aligned, replayItem)
			continue
		}
		aligned = append(aligned, updated)
	}
	return aligned
}

func codexReplayOutputCallIDs(inputItems []gjson.Result) map[string]string {
	outputCallIDs := make(map[string]string)
	for _, inputItem := range inputItems {
		itemType := strings.TrimSpace(inputItem.Get("type").String())
		if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
			continue
		}
		callID := strings.TrimSpace(inputItem.Get("call_id").String())
		if callID == "" {
			continue
		}
		for _, candidate := range codexReplayComparableCallIDs(callID) {
			outputCallIDs[candidate] = callID
		}
	}
	return outputCallIDs
}

func shouldInsertCodexReasoningReplayBefore(item gjson.Result) bool {
	role, ok := codexReplayMessageRole(item)
	if !ok {
		return true
	}
	switch role {
	case "developer", "system":
		return false
	default:
		return true
	}
}

func codexReplayMessageRole(item gjson.Result) (string, bool) {
	itemType := strings.TrimSpace(item.Get("type").String())
	role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
	if role == "" || (itemType != "" && itemType != "message") {
		return "", false
	}
	return role, true
}

func codexReplayToolCallKeys(item gjson.Result) []string {
	itemType := strings.TrimSpace(item.Get("type").String())
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return nil
	}
	callIDs := codexReplayComparableCallIDs(item.Get("call_id").String())
	if len(callIDs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(callIDs))
	for _, callID := range callIDs {
		keys = append(keys, itemType+":"+callID)
	}
	return keys
}

func codexReplayAnyToolCallKeyExists(existing map[string]bool, keys []string) bool {
	for _, key := range keys {
		if existing[key] {
			return true
		}
	}
	return false
}

func codexReplayComparableCallIDs(callID string) []string {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return nil
	}

	claudeVisibleCallID := shortenCodexReplayCallIDIfNeeded(util.SanitizeClaudeToolID(callID))
	if claudeVisibleCallID == "" || claudeVisibleCallID == callID {
		return []string{callID}
	}
	return []string{callID, claudeVisibleCallID}
}

func shortenCodexReplayCallIDIfNeeded(id string) string {
	const limit = 64
	if len(id) <= limit {
		return id
	}

	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLen := limit - len(suffix)
	if prefixLen <= 0 {
		return suffix[len(suffix)-limit:]
	}
	return id[:prefixLen] + suffix
}

func cacheCodexReasoningReplayFromCompleted(
	ctx context.Context,
	scope codexReasoningReplayScope,
	completedData []byte,
) {
	if !scope.valid() {
		return
	}
	output := gjson.GetBytes(completedData, "response.output")
	if !output.IsArray() {
		return
	}
	replayItems := make([][]byte, 0, len(output.Array()))
	callIDs := make([]string, 0)
	assistantFingerprint := ""
	for _, item := range output.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "reasoning":
			replayItems = append(replayItems, []byte(item.Raw))
		case "function_call", "custom_tool_call":
			replayItems = append(replayItems, []byte(item.Raw))
			if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
				callIDs = append(callIDs, callID)
			}
		case "message":
			if fingerprint := codexReplayAssistantMessageFingerprint(item); fingerprint != "" {
				assistantFingerprint = fingerprint
			}
		}
	}
	if len(replayItems) == 0 {
		return
	}

	hasher := sha256.New()
	_, _ = hasher.Write([]byte(scope.requestFingerprint))
	_, _ = hasher.Write([]byte("\x00assistant\x00" + assistantFingerprint))
	for _, callID := range callIDs {
		_, _ = hasher.Write([]byte("\x00call\x00" + callID))
	}
	for _, item := range replayItems {
		_, _ = hasher.Write([]byte("\x00item\x00"))
		_, _ = hasher.Write(item)
	}
	marker := []byte(`{"type":"` + internalcache.CodexReasoningReplayTurnType + `"}`)
	marker, _ = sjson.SetBytes(marker, "id", hex.EncodeToString(hasher.Sum(nil)))
	if assistantFingerprint != "" {
		marker, _ = sjson.SetBytes(marker, "assistant_fingerprint", assistantFingerprint)
	}
	if scope.requestFingerprint != "" {
		marker, _ = sjson.SetBytes(marker, "request_fingerprint", scope.requestFingerprint)
	}
	for _, callID := range callIDs {
		marker, _ = sjson.SetBytes(marker, "call_ids.-1", callID)
	}
	items := make([][]byte, 0, len(replayItems)+1)
	items = append(items, marker)
	items = append(items, replayItems...)
	if ctx == nil {
		ctx = context.Background()
	}
	if !internalcache.AppendCodexReasoningReplayItemsBestEffort(
		ctx,
		scope.modelName,
		scope.sessionKey,
		items,
	) {
		log.Warn("codex reasoning replay cache append failed")
	}
}

func clearCodexReasoningReplayOnInvalidSignature(
	ctx context.Context,
	scope codexReasoningReplayScope,
	statusCode int,
	body []byte,
) error {
	if !scope.valid() {
		return nil
	}
	code, _, ok := codexStatusErrorClassification(statusCode, body)
	if ok && code == "thinking_signature_invalid" {
		return internalcache.DeleteCodexReasoningReplayItemRequired(
			ctx,
			scope.modelName,
			scope.sessionKey,
		)
	}
	return nil
}

func codexStatusErrorClassification(statusCode int, body []byte) (string, string, bool) {
	if statusCode < 400 || len(body) == 0 {
		return "", "", false
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	typeName := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()))
	combined := strings.TrimSpace(strings.Join([]string{code, message, typeName, strings.ToLower(string(body))}, " "))
	switch {
	case strings.Contains(combined, "invalid signature in thinking block") || strings.Contains(
		combined, "invalid_encrypted_content",
	):
		return "thinking_signature_invalid", "invalid_request_error", true
	case code != "":
		return code, typeName, true
	case typeName != "":
		return typeName, typeName, true
	default:
		return "", "", false
	}
}

// codexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type codexExecutor struct {
	cfg *config.Config
}

func newCodexExecutor(cfg *config.Config) *codexExecutor { return &codexExecutor{cfg: cfg} }

func (e *codexExecutor) Identifier() string { return "codex" }

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *codexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *codexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (
	*http.Response,
	error,
) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *codexExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImage(ctx, auth, req, opts)
	}
	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(ctx, e.cfg, auth, baseModel, req.Model, e.Identifier(), from, opts, req, false)
	if err != nil {
		return resp, err
	}
	body := prepared.Body
	originalPayload := prepared.OriginalPayload
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, from, req, opts, body)
	if errReplay != nil {
		return resp, errReplay
	}
	reporter.setTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	var lastErr error
	for attempt := range 2 {
		httpReq, upstreamBody, identityState, errReq := e.cacheHelper(
			ctx, from, url, auth, req, originalPayload, body, opts.Headers,
		)
		if errReq != nil {
			return resp, errReq
		}
		applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
		applyModelHeaderOverrides(httpReq.Header, baseModel)
		applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
		recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), upstreamBody, e.Identifier(), auth)
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			return resp, errDo
		}
		data, readErr := func() ([]byte, error) {
			defer func() {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close response body error: %v", errClose)
				}
			}()
			recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
				b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
				b = applyCodexIdentityConfuseResponsePayload(b, identityState)
				appendAPIResponseChunk(ctx, e.cfg, b)
				logWithRequestID(ctx).Debugf(
					"request error, error status: %d, error message: %s", httpResp.StatusCode,
					summarizeErrorBody(httpResp.Header.Get("Content-Type"), b),
				)
				return nil, newCodexStatusErr(httpResp.StatusCode, b, httpResp.Header)
			}
			payload, errRead := readCodexResponseBody(httpResp.Body, maxCodexResponseBytes)
			if errRead != nil {
				recordAPIResponseError(ctx, e.cfg, errRead)
			}
			return payload, errRead
		}()
		if readErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return resp, errCtx
			}
			if statusCode := statusErrCode(readErr); statusCode != 0 {
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(
					ctx,
					replayScope,
					statusCode,
					[]byte(readErr.Error()),
				); errClearReplay != nil {
					return resp, errClearReplay
				}
				if statusCode == http.StatusRequestTimeout && attempt == 0 {
					lastErr = readErr
					continue
				}
				return resp, readErr
			}
		}
		data = helps.RestoreCodexMultiAgentV2Response(data, prepared.OptimizedMultiAgentV2)
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		appendAPIResponseChunk(ctx, e.cfg, data)

		recovered := newCodexNonStreamRecovery(req.Model)
		var streamErr error
		var terminalPayload []byte
		terminalType := ""
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		remaining := data
		for len(remaining) > 0 {
			frameLen := sseFrameLen(remaining)
			if frameLen == 0 {
				if len(bytes.TrimSpace(remaining)) > 0 {
					streamErr = statusErr{
						code: http.StatusBadGateway,
						msg:  "codex executor: upstream stream ended with incomplete SSE frame",
					}
				}
				break
			}
			frame := remaining[:frameLen]
			remaining = remaining[frameLen:]
			eventData, hasData, valid := sseDataPayload(frame)
			if !hasData {
				continue
			}
			if !valid {
				streamErr = statusErr{
					code: http.StatusBadGateway,
					msg:  "codex executor: upstream returned malformed stream data",
				}
				break
			}
			if bytes.Equal(eventData, []byte("[DONE]")) {
				continue
			}

			eventData = normalizeCodexCompletionEvent(eventData)
			eventData = helps.RestoreCodexMultiAgentV2Response(eventData, prepared.OptimizedMultiAgentV2)
			recovered.addEvent(eventData)
			if errEvent, ok := parseCodexEventError(eventData); ok {
				streamErr = errEvent
				break
			}
			eventType := gjson.GetBytes(eventData, "type").String()
			switch eventType {
			case "response.output_item.done":
				collectCodexOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.incomplete":
				terminalType = eventType
				terminalPayload = patchCodexCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
			}
		}
		if streamErr != nil {
			if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(
				ctx,
				replayScope,
				statusErrCode(streamErr),
				[]byte(streamErr.Error()),
			); errClearReplay != nil {
				return resp, errClearReplay
			}
			return resp, streamErr
		}
		if len(terminalPayload) > 0 {
			if terminalType == "response.completed" {
				cacheCodexReasoningReplayFromCompleted(ctx, replayScope, terminalPayload)
			}
			if detail, ok := parseCodexUsage(terminalPayload); ok {
				reporter.publish(ctx, detail)
			}
			var param any
			out := sdktranslator.TranslateNonStream(
				ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, originalPayload, body,
				applyCodexIdentityExposeResponsePayload(terminalPayload, identityState), &param,
			)
			reporter.ensurePublished(ctx)
			resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
			return resp, nil
		}
		if recoveredPayload, ok := recovered.completedPayload(); ok {
			cacheCodexReasoningReplayFromCompleted(ctx, replayScope, recoveredPayload)
			var param any
			out := sdktranslator.TranslateNonStream(
				ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, originalPayload, body,
				applyCodexIdentityExposeResponsePayload(recoveredPayload, identityState), &param,
			)
			reporter.ensurePublished(ctx)
			resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
			return resp, nil
		}
		lastErr = newCodexIncompleteStreamError()
	}
	if lastErr != nil {
		return resp, lastErr
	}
	return resp, newCodexIncompleteStreamError()
}

func (e *codexExecutor) executeCompact(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	isCompat := helps.APIKeyModelIsCompat(req)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		originalPayload,
		false,
		isCompat,
	)
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		req.Payload,
		false,
		isCompat,
	)

	body, err = helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "stream")
	body = normalizeCodexInstructions(body)
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(
		ctx,
		"codex compact",
		body,
		isCompat,
	)
	body = normalizeCodexParallelToolCalls(body, opts.Headers)
	body, optimizedMultiAgentV2 := helps.OptimizeCodexMultiAgentV2Request(
		ctx,
		opts.Headers,
		body,
		e.cfg,
		isCompat,
	)
	reporter.setTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	// noinspection DuplicatedCode
	httpReq, upstreamBody, identityState, err := e.cacheHelper(
		ctx, from, url, auth, req, originalPayload, body, opts.Headers,
	)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), upstreamBody, e.Identifier(), auth)
	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		b = applyCodexIdentityConfuseResponsePayload(b, identityState)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), b),
		)
		err = newCodexStatusErr(httpResp.StatusCode, b, httpResp.Header)
		return resp, err
	}
	data, err := readCodexResponseBody(httpResp.Body, maxCodexResponseBytes)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	data = helps.RestoreCodexMultiAgentV2Response(data, optimizedMultiAgentV2)
	data = applyCodexIdentityConfuseResponsePayload(data, identityState)
	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publish(ctx, parseOpenAIUsage(data))
	reporter.ensurePublished(ctx)
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, originalPayload, body,
		applyCodexIdentityExposeResponsePayload(data, identityState), &param,
	)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *codexExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImageStream(ctx, auth, req, opts)
	}
	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(ctx, e.cfg, auth, baseModel, req.Model, e.Identifier(), from, opts, req, true)
	if err != nil {
		return nil, err
	}
	body := prepared.Body
	originalPayload := prepared.OriginalPayload
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, from, req, opts, body)
	if errReplay != nil {
		return nil, errReplay
	}
	reporter.setTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, upstreamBody, identityState, err := e.cacheHelper(
		ctx, from, url, auth, req, originalPayload, body, opts.Headers,
	)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), upstreamBody, e.Identifier(), auth)

	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			readErr = markResponseStarted(readErr)
			recordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(
			ctx,
			replayScope,
			httpResp.StatusCode,
			data,
		); errClearReplay != nil {
			return nil, errClearReplay
		}
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newCodexStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		scanner.Split(scanSSEFrame)
		var param any
		recovered := newCodexNonStreamRecovery(req.Model)
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		var terminalUsagePayload []byte
		sawTerminal := false
		for scanner.Scan() {
			frame := applyCodexIdentityConfuseResponsePayload(bytes.Clone(scanner.Bytes()), identityState)
			if len(bytes.TrimSpace(frame)) == 0 {
				continue
			}
			appendAPIResponseChunk(ctx, e.cfg, frame)
			if !isCompleteSSEFrame(frame) {
				errIncomplete := statusErr{
					code: http.StatusBadGateway,
					msg:  "codex executor: upstream stream ended with incomplete SSE frame",
				}
				recordAPIResponseError(ctx, e.cfg, errIncomplete)
				reporter.publishFailure(ctx, errIncomplete)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errIncomplete})
				return
			}

			eventData, hasData, valid := sseDataPayload(frame)
			if hasData {
				if !valid {
					errMalformed := statusErr{
						code: http.StatusBadGateway,
						msg:  "codex executor: upstream returned malformed stream data",
					}
					recordAPIResponseError(ctx, e.cfg, errMalformed)
					reporter.publishFailure(ctx, errMalformed)
					_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errMalformed})
					return
				}
				if !bytes.Equal(eventData, []byte("[DONE]")) {
					eventData = normalizeCodexCompletionEvent(eventData)
					eventData = helps.RestoreCodexMultiAgentV2Response(eventData, prepared.OptimizedMultiAgentV2)
					recovered.addEvent(eventData)
					if errEvent, ok := parseCodexEventError(eventData); ok {
						if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(
							ctx,
							replayScope,
							statusErrCode(errEvent),
							[]byte(errEvent.Error()),
						); errClearReplay != nil {
							recordAPIResponseError(ctx, e.cfg, errClearReplay)
							reporter.publishFailure(ctx, errClearReplay)
							_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errClearReplay})
							return
						}
						recordAPIResponseError(ctx, e.cfg, errEvent)
						reporter.publishFailure(ctx, errEvent)
						_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errEvent})
						return
					}
					eventType := gjson.GetBytes(eventData, "type").String()
					switch eventType {
					case "response.output_item.done":
						collectCodexOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
					case "response.completed", "response.incomplete":
						sawTerminal = true
						eventData = patchCodexCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
						if eventType == "response.completed" {
							cacheCodexReasoningReplayFromCompleted(ctx, replayScope, eventData)
						}
						terminalUsagePayload = bytes.Clone(eventData)
					}
				}
				frame = encodeCodexHTTPEvent(eventData)
			}

			chunks := sdktranslator.TranslateStream(
				ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, originalPayload, body,
				applyCodexIdentityExposeResponsePayload(frame, identityState), &param,
			)
			for i := range chunks {
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					errSend := ctx.Err()
					if errSend == nil {
						errSend = context.Canceled
					}
					recordAPIResponseError(ctx, e.cfg, errSend)
					reporter.publishFailure(ctx, errSend)
					return
				}
			}
			if sawTerminal {
				if detail, ok := parseCodexUsage(terminalUsagePayload); ok {
					reporter.publish(ctx, detail)
				} else {
					reporter.ensurePublished(ctx)
				}
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if ctx.Err() != nil {
				return
			}
			recordAPIResponseError(ctx, e.cfg, errScan)
			streamErr := newCodexIncompleteStreamError()
			reporter.publishFailure(ctx, streamErr)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: streamErr})
			return
		}
		if !sawTerminal {
			if recoveredPayload, ok := recovered.completedPayload(); ok {
				cacheCodexReasoningReplayFromCompleted(ctx, replayScope, recoveredPayload)
				translated := sdktranslator.TranslateStream(
					ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, originalPayload, body,
					applyCodexIdentityExposeResponsePayload(encodeCodexHTTPEvent(recoveredPayload), identityState),
					&param,
				)
				for i := range translated {
					if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: translated[i]}) {
						errSend := ctx.Err()
						if errSend == nil {
							errSend = context.Canceled
						}
						recordAPIResponseError(ctx, e.cfg, errSend)
						reporter.publishFailure(ctx, errSend)
						return
					}
				}
				reporter.ensurePublished(ctx)
				return
			}
			streamEOFErr := newCodexIncompleteStreamError()
			recordAPIResponseError(ctx, e.cfg, streamEOFErr)
			reporter.publishFailure(ctx, streamEOFErr)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: streamEOFErr})
			return
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *codexExecutor) CountTokens(
	ctx context.Context,
	_ *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		req.Payload,
		false,
		helps.APIKeyModelIsCompat(req),
	)

	body, err := helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, req.Payload, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body = helps.SetBoolIfDifferent(body, "stream", false)
	body = normalizeCodexInstructions(body)

	enc, err := tokenizerForCodexModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: tokenizer init failed: %w", err)
	}

	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(
		`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count,
	)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func tokenizerForCodexModel(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	default:
		return tokenizer.Get(tokenizer.Cl100kBase)
	}
}

func countCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	var segments []string

	if inst := strings.TrimSpace(root.Get("instructions").String()); inst != "" {
		segments = append(segments, inst)
	}

	inputItems := root.Get("input")
	if inputItems.IsArray() {
		arr := inputItems.Array()
		for i := range arr {
			item := arr[i]
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					parts := content.Array()
					for j := range parts {
						part := parts[j]
						if text := strings.TrimSpace(part.Get("text").String()); text != "" {
							segments = append(segments, text)
						}
					}
				}
			case "function_call":
				if name := strings.TrimSpace(item.Get("name").String()); name != "" {
					segments = append(segments, name)
				}
				if args := strings.TrimSpace(item.Get("arguments").String()); args != "" {
					segments = append(segments, args)
				}
			case "function_call_output":
				if out := strings.TrimSpace(item.Get("output").String()); out != "" {
					segments = append(segments, out)
				}
			default:
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					segments = append(segments, text)
				}
			}
		}
	}

	tools := root.Get("tools")
	if tools.IsArray() {
		toolArray := tools.Array()
		for i := range toolArray {
			tool := toolArray[i]
			if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
				segments = append(segments, name)
			}
			if desc := strings.TrimSpace(tool.Get("description").String()); desc != "" {
				segments = append(segments, desc)
			}
			if params := tool.Get("parameters"); params.Exists() {
				val := params.Raw
				if params.Type == gjson.String {
					val = params.String()
				}
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					segments = append(segments, trimmed)
				}
			}
		}
	}

	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		if name := strings.TrimSpace(textFormat.Get("name").String()); name != "" {
			segments = append(segments, name)
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			val := schema.Raw
			if schema.Type == gjson.String {
				val = schema.String()
			}
			if trimmed := strings.TrimSpace(val); trimmed != "" {
				segments = append(segments, trimmed)
			}
		}
	}

	text := strings.Join(segments, "\n")
	if text == "" {
		return 0, nil
	}

	count, err := enc.Count(text)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (e *codexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := codexauth.NewAuth(e.cfg)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

type codexIdentityConfuseState struct {
	enabled                bool
	authID                 string
	originalPromptCacheKey string
	promptCacheKey         string
	turnIDs                []codexIdentityReplacement
}

type codexIdentityReplacement struct {
	original string
	confused string
}

func applyCodexIdentityConfuseBody(
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	userPayload []byte,
	rawJSON []byte,
) ([]byte, codexIdentityConfuseState) {
	if !codexIdentityConfuseEnabled(cfg) || auth == nil || strings.TrimSpace(auth.ID) == "" || len(rawJSON) == 0 {
		return rawJSON, codexIdentityConfuseState{}
	}

	state := codexIdentityConfuseState{enabled: true, authID: strings.TrimSpace(auth.ID)}
	if promptCacheKey := cliproxysession.NormalizeExplicitID(
		gjson.GetBytes(
			userPayload, "prompt_cache_key",
		).String(),
	); promptCacheKey != "" {
		state.originalPromptCacheKey = promptCacheKey
		state.promptCacheKey = codexIdentityConfuseUUID(auth.ID, "prompt-cache", promptCacheKey)
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", state.promptCacheKey)
	}
	if installationID := strings.TrimSpace(
		gjson.GetBytes(
			userPayload, "client_metadata.x-codex-installation-id",
		).String(),
	); installationID != "" {
		rawJSON, _ = sjson.SetBytes(
			rawJSON, "client_metadata.x-codex-installation-id",
			codexIdentityConfuseUUID(auth.ID, "installation", installationID),
		)
	}
	if turnMetadata := strings.TrimSpace(
		gjson.GetBytes(
			rawJSON, "client_metadata.x-codex-turn-metadata",
		).String(),
	); turnMetadata != "" {
		rawJSON, _ = sjson.SetBytes(
			rawJSON, "client_metadata.x-codex-turn-metadata",
			applyCodexTurnMetadataIdentityConfuse(turnMetadata, &state),
		)
	}
	if state.promptCacheKey != "" {
		if windowID := strings.TrimSpace(
			gjson.GetBytes(
				rawJSON, "client_metadata.x-codex-window-id",
			).String(),
		); windowID != "" {
			rawJSON, _ = sjson.SetBytes(rawJSON, "client_metadata.x-codex-window-id", state.promptCacheKey+":0")
		}
	}

	return rawJSON, state
}

func applyCodexIdentityConfuseHeaders(headers http.Header, state *codexIdentityConfuseState) {
	if headers == nil || state == nil || !state.enabled {
		return
	}
	if rawTurnMetadata := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); rawTurnMetadata != "" {
		headers.Set("X-Codex-Turn-Metadata", applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata, state))
	}
	if state.promptCacheKey == "" {
		return
	}
	headers.Set("Session_id", state.promptCacheKey)
	if headerValueCaseInsensitive(headers, "Conversation_id") != "" {
		headers.Set("Conversation_id", state.promptCacheKey)
	}
	headers.Set("X-Client-Request-Id", state.promptCacheKey)
	headers.Set("Thread-Id", state.promptCacheKey)
	headers.Set("X-Codex-Window-Id", state.promptCacheKey+":0")
}

func applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata string, state *codexIdentityConfuseState) string {
	updatedTurnMetadata := rawTurnMetadata
	if state == nil || !state.enabled {
		return updatedTurnMetadata
	}
	if state.promptCacheKey != "" && gjson.Get(rawTurnMetadata, "prompt_cache_key").Exists() {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "prompt_cache_key", state.promptCacheKey)
	} else if state.promptCacheKey != "" && state.originalPromptCacheKey != "" {
		updatedTurnMetadata = strings.ReplaceAll(
			updatedTurnMetadata, state.originalPromptCacheKey, state.promptCacheKey,
		)
	}
	if turnID := strings.TrimSpace(gjson.Get(rawTurnMetadata, "turn_id").String()); turnID != "" {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "turn_id", state.confuseTurnID(turnID))
	}
	if state.promptCacheKey != "" && gjson.Get(rawTurnMetadata, "window_id").Exists() {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "window_id", state.promptCacheKey+":0")
	}
	return updatedTurnMetadata
}

func applyCodexIdentityConfuseResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	payload = replaceCodexIdentityResponsePayload(payload, state.originalPromptCacheKey, state.promptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.original, turnID.confused)
	}
	return payload
}

func applyCodexIdentityExposeResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	payload = replaceCodexIdentityResponsePayload(payload, state.promptCacheKey, state.originalPromptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.confused, turnID.original)
	}
	return payload
}

func (state *codexIdentityConfuseState) confuseTurnID(turnID string) string {
	turnID = strings.TrimSpace(turnID)
	if state == nil || !state.enabled || strings.TrimSpace(state.authID) == "" || turnID == "" {
		return turnID
	}
	for _, replacement := range state.turnIDs {
		if replacement.original == turnID || replacement.confused == turnID {
			return replacement.confused
		}
	}
	confusedTurnID := codexIdentityConfuseUUID(state.authID, "turn", turnID)
	state.turnIDs = append(state.turnIDs, codexIdentityReplacement{original: turnID, confused: confusedTurnID})
	return confusedTurnID
}

func replaceCodexIdentityResponsePayload(payload []byte, from string, to string) []byte {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if len(payload) == 0 || from == "" || to == "" || from == to || !bytes.Contains(payload, []byte(from)) {
		return payload
	}
	return bytes.ReplaceAll(payload, []byte(from), []byte(to))
}

func codexIdentityConfuseEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Codex.IdentityConfuse {
		return false
	}
	strategy := strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy))
	return cfg.Routing.SessionAffinity || strategy == "fill-first" || strategy == "fillfirst" || strategy == "ff"
}

func codexIdentityConfuseUUID(authID string, kind string, value string) string {
	name := strings.Join(
		[]string{
			"cli-proxy-api", "codex", "identity-confuse", kind, strings.TrimSpace(authID), strings.TrimSpace(value),
		}, ":",
	)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func (e *codexExecutor) cacheHelper(
	ctx context.Context,
	from sdktranslator.Format,
	url string,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	userPayload []byte,
	rawJSON []byte,
	headerSets ...http.Header,
) (*http.Request, []byte, codexIdentityConfuseState, error) {
	var requestHeaders http.Header
	if len(headerSets) > 0 {
		requestHeaders = headerSets[0]
	}
	var cache codexCache
	if shouldUseImplicitCodexConversationCache(from, req.Model) {
		modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
		if modelName == "" {
			modelName = req.Model
		}
		if cached, ok := codexPromptCacheFromClaudeRequest(ctx, modelName, req.Payload, requestHeaders); ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		cache.ID = cliproxysession.NormalizeExplicitID(gjson.GetBytes(req.Payload, "prompt_cache_key").String())
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		cache.ID = cliproxysession.NormalizeExplicitID(gjson.GetBytes(req.Payload, "prompt_cache_key").String())
		if cache.ID == "" {
			cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
		}
		if cache.ID == "" {
			if cached, ok := codexPromptCacheFromOpenAIAPIKey(ctx); ok {
				cache = cached
			}
		}
	}
	if cache.ID == "" {
		cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
	}

	if cache.ID != "" {
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", cache.ID)
	}
	rawJSON = helps.SanitizeCodexInputItemIDs(rawJSON)
	var identityState codexIdentityConfuseState
	rawJSON, identityState = applyCodexIdentityConfuseBody(e.cfg, auth, userPayload, rawJSON)
	if identityState.promptCacheKey != "" {
		cache.ID = identityState.promptCacheKey
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, nil, codexIdentityConfuseState{}, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Conversation_id", cache.ID)
		httpReq.Header.Set("Session_id", cache.ID)
	}
	return httpReq, rawJSON, identityState, nil
}

func applyModelHeaderOverrides(headers http.Header, modelName string) {
	if headers == nil {
		return
	}
	overrides := registry.ModelOverrideHeaders(modelName)
	if len(overrides) == 0 {
		return
	}
	for key, value := range overrides {
		headers.Set(key, value)
	}
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") && strings.TrimSpace(headers.Get("Session_id")) == "" {
		headers.Set("Session_id", uuid.NewString())
	}
}

var imageGenToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func isImageGenerationFunctionTool(tool gjson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("name").String() == "image_gen.imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		tools := tool.Get("tools")
		if !tools.IsArray() {
			return false
		}
		for _, nestedTool := range tools.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func normalizeCodexParallelToolCalls(body []byte, headers http.Header) []byte {
	if isCodexResponsesLiteRequest(body, headers) {
		body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
		return body
	}
	return normalizeCodexParallelToolCallsForTools(body)
}

func normalizeCodexParallelToolCallsForTools(body []byte) []byte {
	if !gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		return body
	}
	body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	return body
}

func isCodexResponsesLiteRequest(body []byte, headers http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	value := gjson.GetBytes(body, codexResponsesLiteMetadata)
	if !value.Exists() {
		return false
	}
	return value.Type == gjson.True || value.Type == gjson.String && strings.EqualFold(
		strings.TrimSpace(value.String()), "true",
	)
}

func ensureImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	if isCodexResponsesLiteRequest(body, headers) {
		return body
	}
	if strings.HasSuffix(baseModel, "spark") {
		return body
	}
	if isCodexFreePlanAuth(auth) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", imageGenToolArrayJSON)
		return body
	}
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(tool) {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tools.-1", imageGenToolJSON)
	return body
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config) {
	var ginHeaders http.Header
	if ginCtx, ok := util.GinContextValue(r.Context()).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = trustedCodexClientHeaders(ginCtx.Request.Header)
	}
	applyCodexHeadersFromSources(r, auth, token, stream, cfg, ginHeaders)
}

// applyCodexDirectImageHeaders sets Codex upstream headers for direct /images/* calls.
// Downstream client User-Agent values are not forwarded to reduce Cloudflare 1010 blocks.
func applyCodexDirectImageHeaders(
	r *http.Request,
	auth *cliproxyauth.Auth,
	token string,
	stream bool,
	cfg *config.Config,
) {
	var ginHeaders http.Header
	if ginCtx, ok := util.GinContextValue(r.Context()).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = trustedCodexClientHeaders(ginCtx.Request.Header)
		if ginHeaders != nil {
			ginHeaders = ginHeaders.Clone()
			ginHeaders.Del("User-Agent")
		}
	}
	applyCodexHeadersFromSources(r, auth, token, stream, cfg, ginHeaders)
}

func applyCodexHeadersFromSources(
	r *http.Request,
	auth *cliproxyauth.Auth,
	token string,
	stream bool,
	cfg *config.Config,
	ginHeaders http.Header,
) {
	misc.ScrubProxyAndFingerprintHeaders(r)
	if userAgent := strings.TrimSpace(r.Header.Get("User-Agent")); userAgent != "" && !isCodexUserAgent(userAgent) {
		r.Header.Del("User-Agent")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
	misc.EnsureHeader(r.Header, ginHeaders, "Version", codexClientVersion)
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)
	if strings.Contains(r.Header.Get("User-Agent"), "Mac OS") {
		ensureHeaderWithTargetPrecedence(r.Header, ginHeaders, "Session_id", uuid.NewString())
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		r.Header.Set("Originator", originator)
	} else if !isAPIKey {
		r.Header.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				if trimmed := strings.TrimSpace(accountID); trimmed != "" {
					setHeaderCasePreserved(r.Header, "ChatGPT-Account-ID", trimmed)
				}
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	applyCodexCloakingHeaders(r.Header, cfg)
}

func applyCodexCloakingHeaders(headers http.Header, cfg *config.Config) {
	if headers == nil || cfg == nil || cfg.Codex.DisableCodexCloaking {
		return
	}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Originator", codexOriginator)
}

func codexHeaderDefaults(cfg *config.Config, auth *cliproxyauth.Auth) (string, string) {
	if cfg == nil || auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		if strings.TrimSpace(auth.Attributes["api_key"]) != "" {
			return "", ""
		}
	}
	return strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent), strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

func ensureHeaderWithPriority(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func ensureHeaderWithTargetPrecedence(target http.Header, source http.Header, key, fallbackValue string) {
	if target == nil || strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func ensureHeaderWithConfigPrecedence(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func trustedCodexClientHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	if userAgent != "" {
		if isCodexUserAgent(userAgent) {
			return headers
		}
		return nil
	}
	if isCodexOriginator(headers.Get("Originator")) {
		return headers
	}
	return nil
}

func isCodexUserAgent(userAgent string) bool {
	normalized := strings.ToLower(strings.TrimSpace(userAgent))
	return strings.HasPrefix(normalized, "codex_cli_rs/") || strings.HasPrefix(normalized, "codex-tui/")
}

func isCodexOriginator(originator string) bool {
	normalized := strings.ToLower(strings.TrimSpace(originator))
	return normalized == codexOriginator || normalized == "codex_cli_rs"
}

func newCodexStatusErr(statusCode int, body []byte, headers ...http.Header) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if len(headers) > 0 {
		err = err.withUpstreamResponse(headers[0])
	}
	if retryAfter := parseCodexRetryAfter(statusCode, body, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	return err
}

func statusErrCode(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(interface{ StatusCode() int }); ok {
		return se.StatusCode()
	}
	return 0
}

func parseCodexEventError(payload []byte) (error, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil, false
	}
	root := gjson.ParseBytes(payload)
	eventType := strings.TrimSpace(root.Get("type").String())
	if eventType != "error" && eventType != "response.failed" {
		return nil, false
	}
	errNode := root.Get("error")
	if !errNode.Exists() || errNode.Type == gjson.Null {
		errNode = root.Get("response.error")
	}
	status := codexEventFailureStatus(root, errNode)
	body := []byte(`{}`)
	if errNode.Exists() && errNode.Type != gjson.Null && strings.TrimSpace(errNode.Raw) != "" {
		body, _ = sjson.SetRawBytes(body, "error", []byte(errNode.Raw))
	} else {
		msg := strings.TrimSpace(root.Get("message").String())
		if msg == "" {
			msg = strings.TrimSpace(root.Get("response.error.message").String())
		}
		if msg == "" {
			msg = "upstream stream failed without error details"
		}
		body, _ = sjson.SetBytes(body, "error.type", "server_error")
		body, _ = sjson.SetBytes(body, "error.message", msg)
	}
	err := newCodexStatusErr(status, body, nil)
	err.dispatchPhase = cliproxyexecutor.DispatchPhaseResponseStarted
	return err, true
}

func codexEventFailureStatus(root, errNode gjson.Result) int {
	errorCode := strings.ToLower(strings.TrimSpace(errNode.Get("code").String()))
	if errorCode == "cyber_policy" {
		return http.StatusBadRequest
	}
	for _, path := range []string{"status", "status_code", "response.status_code"} {
		if status := int(root.Get(path).Int()); status >= 400 && status <= 599 {
			return status
		}
	}
	for _, path := range []string{"status_code", "status"} {
		if status := int(errNode.Get(path).Int()); status >= 400 && status <= 599 {
			return status
		}
	}

	errorType := strings.ToLower(strings.TrimSpace(errNode.Get("type").String()))
	switch {
	case errorType == "invalid_request_error", errorType == "bad_request_error":
		return http.StatusBadRequest
	case errorType == "authentication_error", errorCode == "invalid_api_key", errorCode == "unauthorized":
		return http.StatusUnauthorized
	case errorType == "permission_error", errorCode == "forbidden", errorCode == "permission_denied":
		return http.StatusForbidden
	case errorType == "not_found_error", errorCode == "not_found", errorCode == "model_not_found":
		return http.StatusNotFound
	case errorType == "rate_limit_error", errorCode == "rate_limit_exceeded":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func parseCodexRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 {
		return nil
	}
	if strings.TrimSpace(gjson.GetBytes(errorBody, "error.type").String()) != "usage_limit_reached" {
		return nil
	}
	if resetsAt := gjson.GetBytes(errorBody, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			return new(resetAtTime.Sub(now))
		}
	}
	if resetsInSeconds := gjson.GetBytes(errorBody, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		return new(time.Duration(resetsInSeconds) * time.Second)
	}
	return nil
}

type codexRecoveredToolCall struct {
	callID    string
	name      string
	arguments string
}

type codexNonStreamRecovery struct {
	defaultModel string
	responseID   string
	model        string
	createdAt    int64
	reasoning    strings.Builder
	text         strings.Builder
	toolCalls    []codexRecoveredToolCall
}

func newCodexNonStreamRecovery(model string) *codexNonStreamRecovery {
	return &codexNonStreamRecovery{defaultModel: strings.TrimSpace(model)}
}

func (r *codexNonStreamRecovery) addEvent(payload []byte) {
	if r == nil || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "response.created":
		if id := strings.TrimSpace(root.Get("response.id").String()); id != "" {
			r.responseID = id
		}
		if model := strings.TrimSpace(root.Get("response.model").String()); model != "" {
			r.model = model
		}
		if createdAt := root.Get("response.created_at").Int(); createdAt > 0 {
			r.createdAt = createdAt
		}
	case "response.reasoning_summary_text.delta":
		r.reasoning.WriteString(root.Get("delta").String())
	case "response.output_text.delta":
		r.text.WriteString(root.Get("delta").String())
	case "response.output_item.added":
		item := root.Get("item")
		if item.Get("type").String() == "function_call" {
			r.upsertToolCall(item.Get("call_id").String(), item.Get("name").String(), item.Get("arguments").String())
		}
	case "response.function_call_arguments.delta":
		r.appendToolArguments(root.Get("delta").String())
	case "response.function_call_arguments.done":
		r.completeToolArguments(root.Get("arguments").String())
	case "response.output_item.done":
		item := root.Get("item")
		switch item.Get("type").String() {
		case "function_call":
			r.upsertToolCall(item.Get("call_id").String(), item.Get("name").String(), item.Get("arguments").String())
		case "message":
			if r.text.Len() == 0 {
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(
						func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								r.text.WriteString(part.Get("text").String())
							}
							return true
						},
					)
				}
			}
		}
	}
}

func (r *codexNonStreamRecovery) upsertToolCall(callID string, name string, arguments string) {
	if r == nil {
		return
	}
	callID = strings.TrimSpace(callID)
	name = strings.TrimSpace(name)
	for i := range r.toolCalls {
		if callID != "" && r.toolCalls[i].callID == callID {
			if name != "" {
				r.toolCalls[i].name = name
			}
			if arguments != "" {
				r.toolCalls[i].arguments = arguments
			}
			return
		}
	}
	r.toolCalls = append(r.toolCalls, codexRecoveredToolCall{callID: callID, name: name, arguments: arguments})
}

func (r *codexNonStreamRecovery) appendToolArguments(delta string) {
	if r == nil || len(r.toolCalls) == 0 || delta == "" {
		return
	}
	r.toolCalls[len(r.toolCalls)-1].arguments += delta
}

func (r *codexNonStreamRecovery) completeToolArguments(arguments string) {
	if r == nil || len(r.toolCalls) == 0 || arguments == "" {
		return
	}
	last := &r.toolCalls[len(r.toolCalls)-1]
	if last.arguments == "" {
		last.arguments = arguments
	}
}

func (r *codexNonStreamRecovery) completedPayload() ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	outputCount := 0
	payload := []byte(`{"type":"response.completed","response":{"id":"","model":"","created_at":0,"status":"completed","output":[]}}`)
	responseID := strings.TrimSpace(r.responseID)
	if responseID == "" {
		responseID = "resp_" + uuid.NewString()
	}
	payload, _ = sjson.SetBytes(payload, "response.id", responseID)
	model := strings.TrimSpace(r.model)
	if model == "" {
		model = thinking.ParseSuffix(r.defaultModel).ModelName
		if model == "" {
			model = r.defaultModel
		}
	}
	payload, _ = sjson.SetBytes(payload, "response.model", model)
	createdAt := r.createdAt
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	payload, _ = sjson.SetBytes(payload, "response.created_at", createdAt)
	if r.reasoning.Len() > 0 {
		reasoning := []byte(`{"type":"reasoning","summary":[{"type":"summary_text","text":""}]}`)
		reasoning, _ = sjson.SetBytes(reasoning, "summary.0.text", r.reasoning.String())
		payload, _ = sjson.SetRawBytes(payload, "response.output.-1", reasoning)
		outputCount++
	}
	if r.text.Len() > 0 {
		message := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}`)
		message, _ = sjson.SetBytes(message, "content.0.text", r.text.String())
		payload, _ = sjson.SetRawBytes(payload, "response.output.-1", message)
		outputCount++
	}
	for i := range r.toolCalls {
		tool := r.toolCalls[i]
		if strings.TrimSpace(tool.name) == "" {
			continue
		}
		call := []byte(`{"type":"function_call","call_id":"","name":"","arguments":""}`)
		call, _ = sjson.SetBytes(call, "call_id", tool.callID)
		call, _ = sjson.SetBytes(call, "name", tool.name)
		call, _ = sjson.SetBytes(call, "arguments", tool.arguments)
		payload, _ = sjson.SetRawBytes(payload, "response.output.-1", call)
		outputCount++
	}
	if outputCount == 0 {
		return nil, false
	}
	if len(r.toolCalls) > 0 {
		payload, _ = sjson.SetBytes(payload, "response.stop_reason", "tool_use")
	}
	return payload, true
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func normalizeCodexInstructions(body []byte) []byte {
	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	return body
}
