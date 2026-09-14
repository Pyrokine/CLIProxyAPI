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

func sanitizeOpenAIResponsesReasoningEncryptedContent(
	ctx context.Context,
	provider string,
	body []byte,
	preserveEmptyEncryptedContent ...bool,
) []byte {
	preserveEmpty := len(preserveEmptyEncryptedContent) > 0 && preserveEmptyEncryptedContent[0]
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

		if !encryptedContent.Exists() {
			if stripOrphanReasoningIDs && item.Get("id").Exists() {
				nextItem, err := sjson.Delete(item.Raw, "id")
				if err != nil {
					logWithRequestID(ctx).Debugf(
						"%s: failed to drop orphan reasoning id at input[%d]: %v", provider, index, err,
					)
					keep(item.Raw)
					continue
				}
				startRebuild(index)
				keep(nextItem)
				logWithRequestID(ctx).Debugf(
					"%s: dropped orphan reasoning id at input[%d] item_id=%q reason=missing encrypted_content with store disabled",
					provider, index, itemID,
				)
				continue
			}
			keep(item.Raw)
			continue
		}

		reason := ""
		switch encryptedContent.Type {
		case gjson.String:
			rawSignature := encryptedContent.String()
			if preserveEmpty && rawSignature == "" {
				keep(item.Raw)
				continue
			}
			if rawSignature != strings.TrimSpace(rawSignature) {
				reason = "encrypted_content has leading or trailing whitespace"
			} else if _, err := signature.InspectGPTReasoningSignature(rawSignature); err != nil {
				reason = err.Error()
			}
		case gjson.Null:
			reason = "encrypted_content is null"
		default:
			reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
		}
		if reason == "" {
			keep(item.Raw)
			continue
		}

		nextItem, err := sjson.Delete(item.Raw, "encrypted_content")
		if err != nil {
			logWithRequestID(ctx).Debugf(
				"%s: failed to drop invalid reasoning encrypted_content at input[%d]: %v", provider, index, err,
			)
			keep(item.Raw)
			continue
		}
		if stripOrphanReasoningIDs && item.Get("id").Exists() {
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
