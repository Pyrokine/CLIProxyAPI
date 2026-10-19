package responses

import (
	"bytes"
	"context"

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
	rootResult := gjson.ParseBytes(rawJSON)
	responseType := rootResult.Get("type").String()
	if responseType == "" && rootResult.Get("output").IsArray() {
		return rawJSON
	}
	if responseType != "response.completed" && responseType != "response.incomplete" {
		return []byte{}
	}
	responseResult := rootResult.Get("response")
	return []byte(responseResult.Raw)
}
