package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type xaiReasoningReplayScope struct {
	modelName  string
	sessionKey string
}

func (s xaiReasoningReplayScope) valid() bool {
	return strings.TrimSpace(s.modelName) != "" && strings.TrimSpace(s.sessionKey) != ""
}

func applyXAIReasoningReplayCache(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
	websocketBaseURL ...string,
) ([]byte, xaiReasoningReplayScope) {
	scope := xaiReasoningReplayScopeFromRequest(ctx, auth, from, req, opts, body, websocketBaseURL...)
	if !scope.valid() {
		return body, scope
	}
	items, ok, err := internalcache.GetXAIReasoningReplayItemsRequired(ctx, scope.modelName, scope.sessionKey)
	if err != nil {
		log.WithContext(ctx).Warnf("xai reasoning replay cache read failed: %v", err)
		return body, scope
	}
	if !ok {
		return body, scope
	}
	cachedItems := items
	items = filterXAIReasoningReplayItemsForInput(body, cachedItems)
	if len(items) == 0 {
		return body, scope
	}
	updated, paired := xaiPairChatReasoningWithToolCalls(body, cachedItems, items)
	if paired {
		return updated, scope
	}
	if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		return body, scope
	}
	updated, ok = insertCodexReasoningReplayItems(body, items)
	if !ok {
		return body, scope
	}
	return updated, scope
}

func xaiPairChatReasoningWithToolCalls(body []byte, cachedItems, filtered [][]byte) ([]byte, bool) {
	inputResult := gjson.GetBytes(body, "input")
	if !inputResult.IsArray() {
		return body, false
	}
	input := inputResult.Array()
	callIndexes := make(map[string]int)
	outputs := make(map[string]bool)
	for index, item := range input {
		switch item.Get("type").String() {
		case "function_call", "custom_tool_call":
			for _, id := range codexReplayComparableCallIDs(item.Get("call_id").String()) {
				callIndexes[item.Get("type").String()+":"+id] = index
			}
		case "function_call_output", "custom_tool_call_output":
			for _, id := range codexReplayComparableCallIDs(item.Get("call_id").String()) {
				outputs[strings.TrimSuffix(item.Get("type").String(), "_output")+":"+id] = true
			}
		}
	}
	remaining := make(map[string]int)
	for _, raw := range filtered {
		if gjson.GetBytes(raw, "type").String() != "reasoning" {
			return body, false
		}
		remaining[gjson.GetBytes(raw, "encrypted_content").String()]++
	}
	insertions := make(map[int][][]byte)
	var pending [][]byte
	for _, raw := range cachedItems {
		item := gjson.ParseBytes(raw)
		switch item.Get("type").String() {
		case "reasoning":
			proof := item.Get("encrypted_content").String()
			if remaining[proof] > 0 {
				pending = append(pending, raw)
				remaining[proof]--
			}
		case "function_call", "custom_tool_call":
			if len(pending) == 0 {
				continue
			}
			anchor := -1
			for _, id := range codexReplayComparableCallIDs(item.Get("call_id").String()) {
				key := item.Get("type").String() + ":" + id
				if index, ok := callIndexes[key]; ok && outputs[key] {
					anchor = index
					break
				}
			}
			if anchor < 0 {
				return body, false
			}
			insertions[anchor] = append(insertions[anchor], pending...)
			pending = nil
		case "message":
			if len(pending) > 0 {
				return body, false
			}
		}
	}
	if len(pending) > 0 || len(insertions) == 0 {
		return body, false
	}
	merged := make([]string, 0, len(input)+len(filtered))
	for index, item := range input {
		for _, proof := range insertions[index] {
			merged = append(merged, string(proof))
		}
		merged = append(merged, item.Raw)
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(merged, ",")+"]"))
	return updated, err == nil
}

func xaiReasoningReplayScopeFromRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
	websocketBaseURL ...string,
) xaiReasoningReplayScope {
	if !xaiReasoningReplayEnabledForSource(from) {
		return xaiReasoningReplayScope{}
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) &&
		strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String()) != "" {
		return xaiReasoningReplayScope{}
	}
	if sourceFormatEqual(from, sdktranslator.FormatOpenAI) && !xaiChatHasExplicitReplaySession(ctx, req, opts, body) {
		return xaiReasoningReplayScope{}
	}
	sessionKey := xaiReasoningReplayIsolateSessionKey(ctx, codexReasoningReplaySessionKey(ctx, from, req, opts, body))
	token, _ := xaiCreds(auth)
	if sessionKey == "" || token == "" {
		return xaiReasoningReplayScope{}
	}
	plane := "cli"
	if xaiUsingAPI(auth) {
		plane = "api"
	}
	transport := "http"
	baseURL := xaiChatBaseURL(auth)
	if opts.Alt == "responses/compact" || xaiInputHasItemType(req.Payload, "compaction_trigger") {
		transport = "compact"
		baseURL = xaiCompactBaseURL(auth)
	}
	if len(websocketBaseURL) > 0 {
		transport = "websocket"
		baseURL = websocketBaseURL[0]
	}
	if strings.TrimSpace(baseURL) == "" {
		return xaiReasoningReplayScope{}
	}
	credential := sha256.Sum256([]byte(token + "\x00" + plane + "\x00" + transport + "\x00" + xaiNormalizeBaseURL(baseURL)))
	return xaiReasoningReplayScope{
		modelName:  thinking.ParseSuffix(req.Model).ModelName,
		sessionKey: "credential:" + hex.EncodeToString(credential[:16]) + ":" + sessionKey,
	}
}

func xaiChatHasExplicitReplaySession(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte) bool {
	if metadataString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey) != "" ||
		metadataString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey) != "" ||
		codexReasoningReplaySessionKeyFromPayload(body) != "" ||
		codexReasoningReplaySessionKeyFromPayload(req.Payload) != "" ||
		codexReasoningReplaySessionKeyFromHeaders(opts.Headers) != "" {
		return true
	}
	if ctx != nil {
		if ginCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			return codexReasoningReplaySessionKeyFromHeaders(ginCtx.Request.Header) != ""
		}
	}
	return false
}

func xaiReasoningReplayIsolateSessionKey(ctx context.Context, sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	if strings.HasPrefix(sessionKey, "execution:") {
		return sessionKey
	}
	apiKey := strings.TrimSpace(apiKeyFromContext(ctx))
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return "caller:" + hex.EncodeToString(sum[:8]) + ":" + sessionKey
}

func xaiReasoningReplayEnabledForSource(from sdktranslator.Format) bool {
	return sourceFormatEqual(from, sdktranslator.FormatClaude) ||
		sourceFormatEqual(from, sdktranslator.FormatOpenAI) ||
		sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse)
}

func xaiInputHasReasoningEncryptedContent(inputItems []gjson.Result, encryptedContent string) bool {
	if encryptedContent == "" {
		return false
	}
	for _, item := range inputItems {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		inputEncryptedContent := item.Get("encrypted_content")
		if inputEncryptedContent.Type == gjson.String && inputEncryptedContent.String() == encryptedContent {
			return true
		}
	}
	return false
}

func filterXAIReasoningReplayItemsForInput(body []byte, items [][]byte) [][]byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil
	}

	inputItems := input.Array()
	lastAssistantMessage, hasLastAssistantMessage := xaiInputLastAssistantMessage(inputItems)
	cachedAssistantMessage, hasCachedAssistantMessage := xaiReplayAssistantMessage(items)
	assistantMessageMatches := hasLastAssistantMessage && hasCachedAssistantMessage &&
		xaiAssistantMessageContentEqual(lastAssistantMessage.Get("content"), cachedAssistantMessage.Get("content"))
	if hasLastAssistantMessage && hasCachedAssistantMessage && !assistantMessageMatches {
		return nil
	}

	existingCalls := make(map[string]bool)
	existingOutputs := make(map[string]bool)
	for _, inputItem := range inputItems {
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
	hasReplayAnchor := false
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "reasoning":
			if xaiInputHasReasoningEncryptedContent(inputItems, itemResult.Get("encrypted_content").String()) {
				continue
			}
			hasReplayAnchor = true
		case "message":
			if assistantMessageMatches {
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
			hasReplayAnchor = true
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	if !hasReplayAnchor {
		return nil
	}
	return filtered
}

func xaiInputLastAssistantMessage(inputItems []gjson.Result) (gjson.Result, bool) {
	for _, inputItem := range slices.Backward(inputItems) {

		itemType := strings.TrimSpace(inputItem.Get("type").String())
		if (itemType != "" && itemType != "message") ||
			!strings.EqualFold(strings.TrimSpace(inputItem.Get("role").String()), "assistant") {
			continue
		}
		return inputItem, true
	}
	return gjson.Result{}, false
}

func xaiReplayAssistantMessage(items [][]byte) (gjson.Result, bool) {
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		if strings.TrimSpace(itemResult.Get("type").String()) == "message" &&
			strings.EqualFold(strings.TrimSpace(itemResult.Get("role").String()), "assistant") {
			return itemResult, true
		}
	}
	return gjson.Result{}, false
}

type xaiAssistantMessagePart struct {
	partType string
	value    string
}

func xaiAssistantMessageContentEqual(left, right gjson.Result) bool {
	leftParts, leftOK := xaiAssistantMessageParts(left)
	rightParts, rightOK := xaiAssistantMessageParts(right)
	if !leftOK || !rightOK || len(leftParts) != len(rightParts) {
		return false
	}
	for i := range leftParts {
		if leftParts[i] != rightParts[i] {
			return false
		}
	}
	return true
}

func xaiAssistantMessageParts(content gjson.Result) ([]xaiAssistantMessagePart, bool) {
	if content.Type == gjson.String {
		return []xaiAssistantMessagePart{{partType: "output_text", value: content.String()}}, true
	}
	if !content.IsArray() {
		return nil, false
	}
	parts := make([]xaiAssistantMessagePart, 0, len(content.Array()))
	for _, part := range content.Array() {
		partType := strings.TrimSpace(part.Get("type").String())
		switch partType {
		case "output_text":
			text := part.Get("text")
			if text.Type != gjson.String {
				return nil, false
			}
			parts = append(parts, xaiAssistantMessagePart{partType: partType, value: text.String()})
		case "refusal":
			refusal := part.Get("refusal")
			if refusal.Type != gjson.String {
				return nil, false
			}
			parts = append(parts, xaiAssistantMessagePart{partType: partType, value: refusal.String()})
		default:
			return nil, false
		}
	}
	return parts, len(parts) > 0
}

func cacheXAIReasoningReplayFromCompleted(
	ctx context.Context,
	scope xaiReasoningReplayScope,
	completedData []byte,
) {
	if !scope.valid() {
		return
	}
	output := gjson.GetBytes(completedData, "response.output")
	if !output.IsArray() {
		return
	}
	items := make([][]byte, 0, len(output.Array()))
	for _, item := range output.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "reasoning", "message", "function_call", "custom_tool_call":
			items = append(items, []byte(item.Raw))
		default:
			continue
		}
	}
	switch internalcache.StoreXAIReasoningReplayItems(
		ctx, scope.modelName, scope.sessionKey, items,
	) {
	case internalcache.XAIReasoningReplayStored:
		return
	case internalcache.XAIReasoningReplayNoReplayableState:
		if err := internalcache.DeleteXAIReasoningReplayItemRequired(
			ctx, scope.modelName, scope.sessionKey,
		); err != nil {
			log.Warnf("xai reasoning replay cache delete failed after non-replayable completed output: %v", err)
		}
	case internalcache.XAIReasoningReplayStoreBackendError:
		log.Debug("xai reasoning replay cache store backend error; retaining previous entry")
	default:
	}
}

func clearXAIReasoningReplayAfterCompaction(ctx context.Context, scope xaiReasoningReplayScope) {
	if !scope.valid() {
		return
	}
	if err := internalcache.DeleteXAIReasoningReplayItemRequired(
		ctx, scope.modelName, scope.sessionKey,
	); err != nil {
		log.Warnf("xai reasoning replay cache delete failed after successful compaction: %v", err)
	}
}

func clearXAIReasoningReplayOnInvalidSignature(
	ctx context.Context,
	scope xaiReasoningReplayScope,
	statusCode int,
	body []byte,
) {
	if !scope.valid() {
		return
	}
	code, _, ok := codexStatusErrorClassification(statusCode, body)
	if ok && code == "thinking_signature_invalid" {
		if err := internalcache.DeleteXAIReasoningReplayItemRequired(
			ctx, scope.modelName, scope.sessionKey,
		); err != nil {
			log.Warnf("xai reasoning replay cache delete failed after invalid signature: %v", err)
		}
	}
}

func sanitizeXAIInputEncryptedContent(
	ctx context.Context,
	body []byte,
	preserveEmptyEncryptedContent ...bool,
) []byte {
	preserveEmpty := len(preserveEmptyEncryptedContent) > 0 && preserveEmptyEncryptedContent[0]
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}

	updated := body
	items := input.Array()
	for index, item := range slices.Backward(items) {

		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType != "reasoning" && itemType != "compaction" {
			continue
		}
		encryptedContentPath := fmt.Sprintf("input.%d.encrypted_content", index)
		encryptedContent := gjson.GetBytes(updated, encryptedContentPath)
		if !encryptedContent.Exists() {
			continue
		}

		reason := ""
		switch encryptedContent.Type {
		case gjson.String:
			if preserveEmpty && encryptedContent.String() == "" {
				continue
			}
			if _, err := signature.InspectGrokEncryptedContent(encryptedContent.String()); err != nil {
				reason = err.Error()
			}
		case gjson.Null:
			reason = "encrypted_content is null"
		default:
			reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
		}
		if reason == "" {
			continue
		}

		var next []byte
		var err error
		if itemType == "compaction" {
			next, err = deleteXAIInputItem(updated, index)
		} else {
			next, err = sjson.DeleteBytes(updated, encryptedContentPath)
		}
		if err != nil {
			log.WithContext(ctx).Debugf(
				"xai: failed to drop invalid %s encrypted_content at input[%d]: %v", itemType, index, err,
			)
			continue
		}
		updated = next
		log.WithContext(ctx).Debugf(
			"xai: dropped invalid %s encrypted_content at input[%d] reason=%s", itemType, index, reason,
		)
	}
	return mergeAdjacentXAIInputReasoningSummaries(updated)
}

func deleteXAIInputItem(body []byte, removeIndex int) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	kept := 0
	for index, item := range input.Array() {
		if index == removeIndex {
			continue
		}
		if kept > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(item.Raw)
		kept++
	}
	buf.WriteByte(']')
	return sjson.SetRawBytes(body, "input", buf.Bytes())
}
