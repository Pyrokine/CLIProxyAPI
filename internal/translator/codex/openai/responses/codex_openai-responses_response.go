package responses

import (
	"bytes"
	"context"
	"fmt"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertCodexResponseToOpenAIResponses converts OpenAI Chat Completions streaming chunks
// to OpenAI Responses SSE events (response.*).

type convertCodexToOpenAIResponsesParams struct {
	terminalEmitted bool
}

func ConvertCodexResponseToOpenAIResponses(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	if param != nil {
		if *param == nil {
			*param = &convertCodexToOpenAIResponsesParams{}
		}
		if state, ok := (*param).(*convertCodexToOpenAIResponsesParams); ok {
			if state.terminalEmitted {
				return nil
			}
			switch codexResponsesEventType(rawJSON) {
			case "response.completed", "response.done", "response.incomplete":
				state.terminalEmitted = true
			}
		}
	}
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
		rawJSON = setResponsesModel(rawJSON, modelName, originalRequestRawJSON, requestRawJSON)
		out := make([]byte, 0, len(rawJSON)+len("data: \n\n"))
		out = append(out, []byte("data: ")...)
		out = append(out, rawJSON...)
		out = append(out, '\n', '\n')
		return [][]byte{out}
	}
	rawJSON = bytes.TrimSpace(rawJSON)
	if len(rawJSON) == 0 {
		return nil
	}
	rawJSON = setResponsesModel(rawJSON, modelName, originalRequestRawJSON, requestRawJSON)
	out := append([]byte(nil), rawJSON...)
	out = append(out, '\n', '\n')
	return [][]byte{out}
}

func codexResponsesEventType(rawJSON []byte) string {
	trimmed := bytes.TrimSpace(rawJSON)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		return gjson.GetBytes(trimmed, "type").String()
	}
	for line := range bytes.SplitSeq(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimSuffix(line, []byte("\r")))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		return gjson.GetBytes(bytes.TrimSpace(line[len("data:"):]), "type").String()
	}
	return ""
}

func setResponsesModel(rawJSON []byte, modelName string, originalRequestRawJSON, requestRawJSON []byte) []byte {
	eventType := gjson.GetBytes(rawJSON, "type").String()
	if eventType != "response.created" && eventType != "response.in_progress" {
		return rawJSON
	}
	if gjson.GetBytes(rawJSON, "response.model").Exists() {
		return rawJSON
	}

	requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
	if requestModelName == "" {
		requestModelName = modelName
	}
	if requestModelName == "" {
		return rawJSON
	}

	updated, errSet := sjson.SetBytes(rawJSON, "response.model", requestModelName)
	if errSet != nil {
		return rawJSON
	}
	return updated
}

// ConvertCodexResponseToOpenAIResponsesNonStream builds a single Responses JSON
// from a non-streaming OpenAI Chat Completions response.
func ConvertCodexResponseToOpenAIResponsesNonStream(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) []byte {
	root := gjson.ParseBytes(rawJSON)
	responseType := root.Get("type").String()
	response := rawJSON
	if responseType != "" {
		if responseType != "response.completed" && responseType != "response.incomplete" {
			return nil
		}
		response = []byte(root.Get("response").Raw)
	} else if !root.Get("output").IsArray() {
		return nil
	}
	for index, item := range gjson.GetBytes(response, "output").Array() {
		if item.Get("type").String() != "reasoning" || len(item.Get("summary").Array()) > 0 {
			continue
		}
		var summary, remaining [][]byte
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() != "reasoning_text" {
				remaining = append(remaining, []byte(part.Raw))
				continue
			}
			entry := []byte(`{"type":"summary_text","text":""}`)
			entry, _ = sjson.SetBytes(entry, "text", part.Get("text").String())
			summary = append(summary, entry)
		}
		if len(summary) == 0 {
			continue
		}
		raw := []byte(item.Raw)
		raw, _ = sjson.SetRawBytes(raw, "summary", translatorcommon.JoinRawArray(summary))
		if len(remaining) == 0 {
			raw, _ = sjson.DeleteBytes(raw, "content")
		} else {
			raw, _ = sjson.SetRawBytes(raw, "content", translatorcommon.JoinRawArray(remaining))
		}
		response, _ = sjson.SetRawBytes(response, fmt.Sprintf("output.%d", index), raw)
	}
	return response
}
