package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func openaiResponsesReasoningSummaryIsEmpty(summary gjson.Result) bool {
	return !summary.Exists() || summary.Type == gjson.Null || summary.IsArray() && len(summary.Array()) == 0
}

func promoteOpenAIResponsesReasoningTextToSummary(itemRaw string, content gjson.Result) (string, error) {
	var summary strings.Builder
	summary.WriteByte('[')
	count := 0
	for _, part := range content.Array() {
		if part.Get("type").String() != "reasoning_text" || part.Get("text").String() == "" {
			continue
		}
		value, err := sjson.Set(`{"type":"summary_text"}`, "text", part.Get("text").String())
		if err != nil {
			return itemRaw, err
		}
		if count > 0 {
			summary.WriteByte(',')
		}
		summary.WriteString(value)
		count++
	}
	if count == 0 {
		return itemRaw, nil
	}
	summary.WriteByte(']')
	return sjson.SetRaw(itemRaw, "summary", summary.String())
}

func sanitizeOpenAIResponsesReasoningEncryptedContent(
	ctx context.Context,
	provider string,
	body []byte,
	isCompat ...bool,
) []byte {
	compat := len(isCompat) > 0 && isCompat[0]
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx, provider, body, compat, compat)
}

func sanitizeOpenAIResponsesReasoningEncryptedContentKeepForeign(ctx context.Context, provider string, body []byte) []byte {
	return sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx, provider, body, false, true)
}

func sanitizeOpenAIResponsesReasoningEncryptedContentWithOptions(ctx context.Context, provider string, body []byte, isCompat, keepForeign bool) []byte {
	inputResult := util.GetGJSONBytesNoCopy(body, "input")
	if !inputResult.Exists() || !inputResult.IsArray() {
		return body
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}

	stripOrphanReasoningIDs := !gjson.GetBytes(body, "store").Bool()
	items := inputResult.Array()

	var rebuilt []byte
	itemsWritten := 0
	keep := func(raw string) {
		if rebuilt == nil {
			return
		}
		if itemsWritten > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, raw...)
		itemsWritten++
	}
	startRebuild := func(index int) {
		if rebuilt != nil {
			return
		}
		rebuilt = make([]byte, 0, len(inputResult.Raw))
		rebuilt = append(rebuilt, '[')
		for i := range index {
			keep(items[i].Raw)
		}
	}

	for index, item := range items {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			keep(item.Raw)
			continue
		}

		encryptedContent := item.Get("encrypted_content")
		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}
		nextItem := item.Raw
		changed := false
		content := item.Get("content")
		if !isCompat && content.IsArray() && len(content.Array()) > 0 {
			if openaiResponsesReasoningSummaryIsEmpty(item.Get("summary")) {
				promoted, err := promoteOpenAIResponsesReasoningTextToSummary(nextItem, content)
				if err != nil {
					logWithRequestID(ctx).Debugf("%s: failed to promote reasoning text at input[%d]: %v", provider, index, err)
				} else {
					nextItem = promoted
				}
			}
			cleared, err := sjson.SetRaw(nextItem, "content", "[]")
			if err != nil {
				logWithRequestID(ctx).Debugf("%s: failed to clear reasoning content at input[%d]: %v", provider, index, err)
			} else {
				nextItem = cleared
				changed = true
			}
		}

		if !encryptedContent.Exists() {
			if !isCompat && stripOrphanReasoningIDs && item.Get("id").Exists() {
				withoutID, err := sjson.Delete(nextItem, "id")
				if err != nil {
					logWithRequestID(ctx).Debugf("%s: failed to drop orphan reasoning id at input[%d]: %v", provider, index, err)
				} else {
					nextItem = withoutID
					changed = true
				}
			}
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}

		reason := ""
		switch encryptedContent.Type {
		case gjson.String:
			rawSignature := encryptedContent.String()
			if isCompat && rawSignature == "" {
				keep(item.Raw)
				continue
			}
			if rawSignature != strings.TrimSpace(rawSignature) {
				reason = "encrypted_content has leading or trailing whitespace"
			} else if _, err := signature.InspectGPTReasoningSignature(rawSignature); err != nil {
				_, accepted := signature.CompatibleSignatureForProviderWithOptions(
					signature.ProviderGPT, rawSignature, signature.BlockKindGPTReasoning,
					signature.SignatureCompatibilityOptions{PreserveUnknownNonEmpty: true},
				)
				if !keepForeign || !accepted {
					reason = err.Error()
				}
			}
		case gjson.Null:
			reason = "encrypted_content is null"
		default:
			reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
		}
		if reason == "" {
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}

		withoutSignature, err := sjson.Delete(nextItem, "encrypted_content")
		if err != nil {
			logWithRequestID(ctx).Debugf(
				"%s: failed to drop invalid reasoning encrypted_content at input[%d]: %v", provider, index, err,
			)
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}
		nextItem = withoutSignature
		if !isCompat && stripOrphanReasoningIDs && item.Get("id").Exists() {
			if nextID, errID := sjson.Delete(nextItem, "id"); errID != nil {
				logWithRequestID(ctx).Debugf(
					"%s: failed to drop reasoning id after invalid encrypted_content at input[%d]: %v",
					provider, index, errID,
				)
			} else {
				nextItem = nextID
			}
		}

		startRebuild(index)
		keep(nextItem)
		logWithRequestID(ctx).Debugf(
			"%s: dropped invalid reasoning encrypted_content at input[%d] item_id=%q reason=%s",
			provider, index, itemID, reason,
		)
	}

	if rebuilt == nil {
		return body
	}
	rebuilt = append(rebuilt, ']')

	updated, err := sjson.SetRawBytes(body, "input", rebuilt)
	if err != nil {
		logWithRequestID(ctx).Debugf(
			"%s: failed to rebuild input array while sanitizing reasoning encrypted_content: %v", provider, err,
		)
		return body
	}
	return updated
}
