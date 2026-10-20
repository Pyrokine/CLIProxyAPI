package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

func isClineCompatEndpoint(baseURL string) bool {
	endpoint, err := url.Parse(baseURL)
	return err == nil && endpoint.Scheme == "https" &&
		strings.EqualFold(endpoint.Hostname(), "api.cline.bot") &&
		(endpoint.Port() == "" || endpoint.Port() == "443") &&
		strings.TrimSuffix(endpoint.EscapedPath(), "/") == "/api/v1" &&
		endpoint.User == nil && endpoint.RawQuery == "" && !endpoint.ForceQuery && endpoint.Fragment == ""
}

type clineResponseText struct{ strings.Builder }

func (text *clineResponseText) MarshalJSON() ([]byte, error) {
	return json.Marshal(text.String())
}

type clineResponseChoice struct {
	Index        int            `json:"index"`
	Message      map[string]any `json:"message"`
	FinishReason any            `json:"finish_reason"`
	tools        map[int]map[string]any
}

func (e *OpenAICompatExecutor) readClineChatCompletion(
	ctx context.Context, response *http.Response, reporter *usageReporter, observed *streamUsageBuffer,
) ([]byte, error) {
	reader := &io.LimitedReader{R: response.Body, N: maxStreamUsageLineBytes + 1}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	observe := func(body []byte) {
		observeResponseTokenEvent(ctx, body)
		reporter.observeResponseModel(body)
		observed.observeOpenAIStream(body)
	}
	if mediaType != "text/event-stream" {
		body, err := io.ReadAll(reader)
		appendAPIResponseChunk(ctx, e.cfg, body)
		if err != nil {
			return nil, markResponseStarted(err)
		}
		if len(body) > maxStreamUsageLineBytes {
			return nil, clineResponseError("response exceeds the size limit")
		}
		if err = ctx.Err(); err != nil {
			return nil, markResponseStarted(err)
		}
		observed.observeOpenAIStream(body)
		body, err = normalizeClineResponse(body)
		if err != nil {
			return nil, err
		}
		observe(body)
		return body, validateClineCompletion(body)
	}

	root := map[string]any{"object": "chat.completion"}
	choices := make(map[int]*clineResponseChoice)
	scanner := bufio.NewScanner(reader)
	scanner.Split(scanSSEFrame)
	scanner.Buffer(nil, maxStreamUsageLineBytes+1)
	bytesRead, done := 0, false
	for scanner.Scan() {
		frame := scanner.Bytes()
		bytesRead += len(frame)
		if bytesRead > maxStreamUsageLineBytes {
			return nil, clineResponseError("response exceeds the size limit")
		}
		appendAPIResponseChunk(ctx, e.cfg, frame)
		if err := ctx.Err(); err != nil {
			return nil, markResponseStarted(err)
		}
		data, hasData, valid := sseDataPayload(frame)
		if !hasData {
			for _, line := range bytes.Split(frame, []byte("\n")) {
				line = bytes.TrimSpace(line)
				if bytes.HasPrefix(line, []byte("{")) || bytes.HasPrefix(line, []byte("[")) {
					return nil, clineResponseError("expected an SSE data frame")
				}
			}
			continue
		}
		if !isCompleteSSEFrame(frame) {
			return nil, clineResponseError("invalid or truncated SSE frame")
		}
		if len(data) == 0 {
			continue
		}
		if !valid {
			return nil, clineResponseError("invalid or truncated SSE frame")
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			done = true
			break
		}
		observed.observeOpenAIStream(data)
		data, err := normalizeClineResponse(data)
		if err != nil {
			return nil, err
		}
		observe(data)
		if err = mergeClineResponseChunk(root, choices, data); err != nil {
			return nil, err
		}
	}
	if err := scannerOrContextError(ctx, scanner); err != nil {
		return nil, err
	}
	if reader.N == 0 {
		return nil, clineResponseError("response exceeds the size limit")
	}
	ordered := make([]*clineResponseChoice, 0, len(choices))
	for _, choice := range choices {
		if !done && choice.FinishReason == nil {
			return nil, clineResponseError("stream closed before a completed response")
		}
		indices := make([]int, 0, len(choice.tools))
		for index := range choice.tools {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			calls, _ := choice.Message["tool_calls"].([]any)
			choice.Message["tool_calls"] = append(calls, choice.tools[index])
		}
		ordered = append(ordered, choice)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	root["choices"] = ordered
	body, err := json.Marshal(root)
	if err != nil {
		return nil, markResponseStarted(err)
	}
	if len(body) > maxStreamUsageLineBytes {
		return nil, clineResponseError("completion exceeds the size limit")
	}
	return body, validateClineCompletion(body)
}

func normalizeClineResponse(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, clineResponseError("invalid JSON response")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, clineResponseError("expected a JSON object")
	}
	if success := root.Get("success"); success.Exists() {
		if success.Type == gjson.False {
			return nil, clineResponseError("upstream returned a failed response: " + root.Get("error").Raw)
		}
		if success.Type != gjson.True || !root.Get("data").IsObject() {
			return nil, clineResponseError("invalid success/data response")
		}
		body = []byte(root.Get("data").Raw)
		root = gjson.ParseBytes(body)
	}
	if upstreamError := root.Get("error"); upstreamError.Exists() && upstreamError.Type != gjson.Null {
		return nil, clineResponseError("upstream returned an error: " + upstreamError.Raw)
	}
	return body, nil
}

func clineResponseError(message string) error {
	return markResponseStarted(statusErr{code: http.StatusBadGateway, msg: "cline response: " + message})
}

func mergeClineResponseChunk(root map[string]any, choices map[int]*clineResponseChoice, body []byte) error {
	var chunk map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&chunk); err != nil {
		return clineResponseError("invalid completion chunk")
	}
	for key, value := range chunk {
		if key == "choices" || key == "object" || value == nil || value == "" {
			continue
		}
		if key == "usage" && !openAIStyleUsageTokenFieldsExist(gjson.GetBytes(body, "usage")) {
			continue
		}
		root[key] = value
	}
	items, ok := chunk["choices"].([]any)
	if !ok {
		return clineResponseError("missing completion choices")
	}
	for _, item := range items {
		incoming, ok := item.(map[string]any)
		if !ok {
			return clineResponseError("invalid completion choice")
		}
		index, err := clineResponseIndex(incoming["index"])
		if err != nil {
			return err
		}
		choice := choices[index]
		if choice == nil {
			choice = &clineResponseChoice{Index: index, Message: map[string]any{"role": "assistant"}, tools: make(map[int]map[string]any)}
			choices[index] = choice
		}
		delta, ok := incoming["delta"].(map[string]any)
		if !ok {
			return clineResponseError("missing completion delta")
		}
		for key, value := range delta {
			if choice.FinishReason != nil && key != "provider_metadata" && key != "role" && value != nil && value != "" {
				return clineResponseError("content received after a finished choice")
			}
			switch key {
			case "role":
				if value != nil && value != "assistant" {
					return clineResponseError("invalid assistant role")
				}
			case "tool_calls":
				if err = mergeClineToolCalls(choice, value); err != nil {
					return err
				}
			default:
				if err = appendClineResponseField(choice.Message, key, value); err != nil {
					return err
				}
			}
		}
		if reason := incoming["finish_reason"]; reason != nil {
			if text, valid := reason.(string); !valid || text == "" || (choice.FinishReason != nil && choice.FinishReason != reason) {
				return clineResponseError("invalid completion finish reason")
			}
			choice.FinishReason = reason
		}
	}
	return nil
}

func clineResponseIndex(value any) (int, error) {
	number, ok := value.(json.Number)
	if ok {
		index, err := strconv.Atoi(number.String())
		if err == nil && index >= 0 {
			return index, nil
		}
	}
	return 0, clineResponseError("invalid completion index")
}

func appendClineResponseField(target map[string]any, key string, value any) error {
	if value == nil {
		return nil
	}
	switch incoming := value.(type) {
	case string:
		builder, ok := target[key].(*clineResponseText)
		if !ok {
			if target[key] != nil {
				return clineResponseError("inconsistent completion field " + key)
			}
			builder = &clineResponseText{}
			target[key] = builder
		}
		builder.WriteString(incoming)
	case []any:
		previous, ok := target[key].([]any)
		if !ok && target[key] != nil {
			return clineResponseError("inconsistent completion field " + key)
		}
		target[key] = append(previous, incoming...)
	case map[string]any:
		switch key {
		case "reasoning_content", "reasoning", "reasoning_text", "reasoning_details":
			return appendClineResponseField(target, key, []any{incoming})
		}
		if _, ok := target[key].(map[string]any); !ok && target[key] != nil {
			return clineResponseError("inconsistent completion field " + key)
		}
		target[key] = incoming
	default:
		return clineResponseError("unsupported completion field " + key)
	}
	return nil
}

func mergeClineToolCalls(choice *clineResponseChoice, value any) error {
	if value == nil {
		return nil
	}
	calls, ok := value.([]any)
	if !ok {
		return clineResponseError("invalid tool calls")
	}
	for _, value := range calls {
		call, ok := value.(map[string]any)
		if !ok {
			return clineResponseError("invalid tool call")
		}
		index, err := clineResponseIndex(call["index"])
		if err != nil {
			return err
		}
		tool := choice.tools[index]
		if tool == nil {
			tool = map[string]any{"type": "function", "function": map[string]any{}}
			choice.tools[index] = tool
		}
		if kind := call["type"]; kind != nil && kind != "function" {
			return clineResponseError("unsupported tool call type")
		}
		if err = appendClineResponseField(tool, "id", call["id"]); err != nil {
			return err
		}
		if value := call["function"]; value != nil {
			function, ok := value.(map[string]any)
			if !ok {
				return clineResponseError("invalid tool function")
			}
			target := tool["function"].(map[string]any)
			for _, field := range []string{"name", "arguments"} {
				if err = appendClineResponseField(target, field, function[field]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func hasClineResponseText(node gjson.Result) bool {
	if node.Type == gjson.String {
		return node.String() != ""
	}
	if node.IsObject() {
		return node.Get("text").Type == gjson.String && node.Get("text").String() != ""
	}
	if node.IsArray() {
		for _, part := range node.Array() {
			if hasClineResponseText(part) {
				return true
			}
		}
	}
	return false
}

func validateClineCompletion(body []byte) error {
	root := gjson.ParseBytes(body)
	choices := root.Get("choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		return clineResponseError("missing completion choices")
	}
	for _, choice := range choices.Array() {
		message := choice.Get("message")
		if !message.IsObject() {
			return clineResponseError("missing assistant message")
		}
		if role := message.Get("role"); role.Exists() && role.String() != "assistant" {
			return clineResponseError("invalid assistant role")
		}
		content := message.Get("content")
		hasContent := content.Type == gjson.String && content.String() != ""
		if content.IsArray() {
			for _, part := range content.Array() {
				kind := part.Get("type").String()
				if (kind != "text" && kind != "reasoning") || part.Get("text").Type != gjson.String {
					return clineResponseError("unsupported assistant content block")
				}
				hasContent = hasContent || part.Get("text").String() != ""
			}
		} else if content.Exists() && content.Type != gjson.Null && content.Type != gjson.String {
			return clineResponseError("invalid assistant content")
		}
		for _, key := range []string{"reasoning_content", "reasoning", "reasoning_text", "reasoning_details"} {
			hasContent = hasContent || hasClineResponseText(message.Get(key))
		}
		if calls := message.Get("tool_calls"); calls.Exists() && calls.Type != gjson.Null {
			if !calls.IsArray() {
				return clineResponseError("invalid tool calls")
			}
			for _, call := range calls.Array() {
				arguments := call.Get("function.arguments")
				if kind := call.Get("type"); kind.Exists() && kind.String() != "function" {
					return clineResponseError("unsupported tool call type")
				}
				if call.Get("id").Type != gjson.String || call.Get("id").String() == "" ||
					call.Get("function.name").Type != gjson.String || call.Get("function.name").String() == "" || arguments.Type != gjson.String ||
					!gjson.Valid(arguments.String()) || !gjson.Parse(arguments.String()).IsObject() {
					return clineResponseError("incomplete tool call")
				}
				hasContent = true
			}
		}
		if !hasContent {
			return clineResponseError(fmt.Sprintf("assistant choice %s contains no output", choice.Get("index").Raw))
		}
	}
	return nil
}
