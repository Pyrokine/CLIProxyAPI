package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConsumeDevinFramesMergesUsagePresence(t *testing.T) {
	firstUsage := testDevinVarintField(nil, 2, 10)
	secondUsage := testDevinVarintField(nil, 2, 0)
	secondUsage = testDevinVarintField(secondUsage, 3, 4)
	frame := testDevinBytesField(nil, 7, firstUsage)
	frame = testDevinBytesField(frame, 7, secondUsage)
	frame = testDevinBytesField(frame, 28, testExecutorDevinDimensionGroup(
		testExecutorDevinDimensionMetric("input_tokens", 99),
		testExecutorDevinDimensionMetric("output_tokens", 8),
		testExecutorDevinDimensionMetric("cached_input_tokens", 3),
	))
	stream := testDevinConnectStream(frame)

	response, logBody, _, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2")
	if err != nil {
		t.Fatalf("consumeDevinFrames() error = %v", err)
	}
	if got := gjson.GetBytes(response, "usage.total_input_tokens").Int(); got != 3 {
		t.Fatalf("total_input_tokens = %d, want 3", got)
	}
	if got := gjson.GetBytes(response, "usage.total_output_tokens").Int(); got != 4 {
		t.Fatalf("total_output_tokens = %d, want 4", got)
	}
	if got := gjson.GetBytes(response, "usage.total_cached_tokens").Int(); got != 3 {
		t.Fatalf("total_cached_tokens = %d, want 3", got)
	}
	if got := gjson.GetBytes(response, "usage.total_tokens").Int(); got != 7 {
		t.Fatalf("total_tokens = %d, want 7", got)
	}
	if !bytes.Contains(logBody, []byte(`"prompt_tokens": 0`)) ||
		!bytes.Contains(logBody, []byte(`"cached_tokens": 3`)) {
		t.Fatalf("usage log did not retain explicit zero/fallback presence: %s", logBody)
	}
}

func TestDevinResponseSeparatesCacheWritesFromInputUsage(t *testing.T) {
	usageField := testDevinVarintField(nil, 2, 7)
	usageField = testDevinVarintField(usageField, 3, 3)
	usageField = testDevinVarintField(usageField, 4, 11)
	usageField = testDevinVarintField(usageField, 5, 2)
	stream := testDevinConnectStream(testDevinBytesField(nil, 7, usageField))

	response, logBody, _, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2")
	if err != nil {
		t.Fatalf("consume Devin cache write usage: %v", err)
	}
	if gjson.GetBytes(response, "usage.total_input_tokens").Int() != 9 ||
		gjson.GetBytes(response, "usage.total_tokens").Int() != 12 ||
		gjson.GetBytes(response, "usage.cache_write_tokens").Int() != 11 {
		t.Fatalf("non-stream cache write usage = %s", response)
	}
	if !bytes.Contains(logBody, []byte(`"cache_write_tokens": 11`)) {
		t.Fatalf("cache write usage missing from log: %s", logBody)
	}

	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)
		executor.streamDevinFrames(ctx, bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatInteractions}, nil,
			newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
	}()
	var completed gjson.Result
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("stream cache write usage error: %v", chunk.Err)
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if !done && gjson.GetBytes(payload, "event_type").String() == "interaction.completed" {
			completed = gjson.ParseBytes(payload)
		}
	}
	if completed.Get("interaction.usage.total_input_tokens").Int() != 9 ||
		completed.Get("interaction.usage.total_tokens").Int() != 12 ||
		completed.Get("interaction.usage.cache_write_tokens").Int() != 11 {
		t.Fatalf("stream cache write usage = %s", completed.Raw)
	}
	record := testWaitForDevinUsageRecord(t, plugin.records)
	if record.Failed || record.Detail.InputTokens != 9 || record.Detail.TotalTokens != 12 ||
		record.Detail.CacheCreationTokens != 11 || record.Detail.CacheReadTokens != 2 ||
		!record.Detail.TokenBreakdown.Valid() ||
		record.Detail.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified {
		t.Fatalf("Devin cache write usage record = %#v", record)
	}
}

func TestDevinCacheWriteExplicitZeroAndFailureDetail(t *testing.T) {
	zeroUsage := testDevinVarintField(nil, 2, 0)
	zeroUsage = testDevinVarintField(zeroUsage, 3, 0)
	zeroUsage = testDevinVarintField(zeroUsage, 4, 0)
	zeroUsage = testDevinVarintField(zeroUsage, 5, 0)
	response, logBody, _, err := consumeDevinFrames(bytes.NewReader(testDevinConnectStream(
		testDevinBytesField(nil, 7, zeroUsage),
	)), "devin/swe-2")
	if err != nil {
		t.Fatalf("consume explicit zero cache writes: %v", err)
	}
	if !gjson.GetBytes(response, "usage.cache_write_tokens").Exists() ||
		gjson.GetBytes(response, "usage.cache_write_tokens").Int() != 0 ||
		!bytes.Contains(logBody, []byte(`"cache_write_tokens": 0`)) {
		t.Fatalf("explicit zero cache write usage response=%s log=%s", response, logBody)
	}

	withWrites := &helps.DevinUsage{
		PromptTokens:            7,
		PromptTokensPresent:     true,
		CompletionTokens:        3,
		CompletionTokensPresent: true,
		CachedTokens:            2,
		CachedTokensPresent:     true,
		CacheWriteTokens:        11,
		CacheWriteTokensPresent: true,
	}
	detail := devinUsageDetail(withWrites)
	if detail.InputTokens != 9 || detail.TotalTokens != 12 || detail.CacheCreationTokens != 11 ||
		detail.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified ||
		!detail.TokenBreakdown.Valid() {
		t.Fatalf("failed Devin usage detail = %+v", detail)
	}
	writeOnly := devinUsageDetail(&helps.DevinUsage{CacheWriteTokens: 11, CacheWriteTokensPresent: true})
	if writeOnly.TotalTokens != 0 || writeOnly.CacheCreationTokens != 11 ||
		writeOnly.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified ||
		!writeOnly.TokenBreakdown.Valid() {
		t.Fatalf("write-only usage detail = %+v", writeOnly)
	}
	reported := normalizeUsageDetailTotal(devinUsageWithCacheWrites(parseInteractionsUsage(
		[]byte(`{"interaction":{"usage":{"cache_write_tokens":11}}}`))), "devin", "DevinExecutor")
	if reported.TotalTokens != 0 || reported.CacheCreationTokens != 11 ||
		reported.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified {
		t.Fatalf("normalized write-only usage detail = %+v", reported)
	}
}

func TestConsumeDevinFramesHandlesCompressedUTF8AndTools(t *testing.T) {
	encoded := []byte("你好")
	toolFirst := testDevinBytesField(nil, 3, []byte(`{"path":"`))
	toolFirst = testDevinVarintField(toolFirst, 4, 0)
	toolLast := testDevinBytesField(nil, 1, []byte("call-1"))
	toolLast = testDevinBytesField(toolLast, 2, []byte("read_file"))
	toolLast = testDevinBytesField(toolLast, 3, []byte(`README.md"}`))
	toolLast = testDevinVarintField(toolLast, 4, 0)

	first := testDevinBytesField(nil, 3, encoded[:2])
	first = testDevinBytesField(first, 6, toolFirst)
	second := testDevinBytesField(nil, 3, encoded[2:])
	second = testDevinBytesField(second, 6, toolLast)
	stream := append(testCompressedDevinFrame(t, helps.ConnectFlagCompressed, first),
		testCompressedDevinFrame(t, helps.ConnectFlagCompressed, second)...)
	stream = append(stream, testCompressedDevinFrame(t,
		helps.ConnectFlagCompressed|helps.ConnectFlagEndStream, []byte(`{}`))...)

	response, _, _, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2")
	if err != nil {
		t.Fatalf("consumeDevinFrames() error = %v", err)
	}
	if got := gjson.GetBytes(response, "steps.0.id").String(); got != "call-1" {
		t.Fatalf("tool id = %q", got)
	}
	if got := gjson.GetBytes(response, "steps.0.name").String(); got != "read_file" {
		t.Fatalf("tool name = %q", got)
	}
	if got := gjson.GetBytes(response, "steps.0.arguments.path").String(); got != "README.md" {
		t.Fatalf("tool arguments path = %q", got)
	}
	if got := gjson.GetBytes(response, "steps.1.content.0.text").String(); got != "你好" {
		t.Fatalf("post-tool content = %q, want 你好", got)
	}
}

func TestDevinSequentialAndInterleavedToolCallsPreserveIdentity(t *testing.T) {
	toolFrame := func(id, name, arguments string) []byte {
		tool := testDevinBytesField(nil, 1, []byte(id))
		if name != "" {
			tool = testDevinBytesField(tool, 2, []byte(name))
		}
		tool = testDevinBytesField(tool, 3, []byte(arguments))
		tool = testDevinVarintField(tool, 4, 0)
		return testDevinBytesField(nil, 6, tool)
	}
	stream := testDevinConnectStream(
		toolFrame("title_0", "title", `{"title":"`),
		toolFrame("bash_1", "bash", `{"command":"2>&1"}`),
		toolFrame("title_0", "", `work"}`),
	)
	want := map[string]string{
		"title_0": `{"title":"work"}`,
		"bash_1":  `{"command":"2>&1"}`,
	}

	response, _, _, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2")
	if err != nil {
		t.Fatalf("non-stream consumeDevinFrames: %v", err)
	}
	steps := gjson.GetBytes(response, "steps").Array()
	if len(steps) != 2 {
		t.Fatalf("non-stream tool steps = %s, want two", response)
	}
	for i, id := range []string{"title_0", "bash_1"} {
		if steps[i].Get("id").String() != id || steps[i].Get("arguments").Raw != want[id] {
			t.Fatalf("non-stream tool step %d = %s, want %s/%s", i, steps[i].Raw, id, want[id])
		}
	}

	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 32)
	go func() {
		defer close(out)
		executor.streamDevinFrames(ctx, bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatInteractions}, nil,
			newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
	}()

	startIndex := make(map[string]int64)
	arguments := make(map[int64]string)
	stops := make(map[int64]int)
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("stream tool error: %v", chunk.Err)
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if done {
			continue
		}
		event := gjson.ParseBytes(payload)
		switch event.Get("event_type").String() {
		case "step.start":
			if event.Get("step.type").String() == "function_call" && event.Get("step.id").String() != "" {
				id := event.Get("step.id").String()
				if _, exists := startIndex[id]; exists {
					t.Fatalf("duplicate tool start %q: %s", id, payload)
				}
				startIndex[id] = event.Get("index").Int()
			}
		case "step.delta":
			if event.Get("delta.type").String() == "arguments_delta" {
				index := event.Get("index").Int()
				arguments[index] += event.Get("delta.arguments").String()
				if event.Get("delta.arguments").String() == `{"command":"2>&1"}` &&
					!bytes.Contains(payload, []byte("2>&1")) {
					t.Fatalf("HTML characters escaped in tool argument delta: %s", payload)
				}
			}
		case "step.stop":
			stops[event.Get("index").Int()]++
		}
	}
	if len(startIndex) != 2 {
		t.Fatalf("stream tool calls = %#v, want both IDs", startIndex)
	}
	for id, value := range want {
		index, exists := startIndex[id]
		if !exists || arguments[index] != value || stops[index] != 1 {
			t.Fatalf("tool %s index=%d args=%q stop=%d, want %q and one stop", id, index, arguments[index], stops[index], value)
		}
	}
}

func TestDevinResponsesStreamSeparatesSequentialToolCalls(t *testing.T) {
	toolFrame := func(id, name, arguments string) []byte {
		tool := testDevinBytesField(nil, 1, []byte(id))
		tool = testDevinBytesField(tool, 2, []byte(name))
		tool = testDevinBytesField(tool, 3, []byte(arguments))
		tool = testDevinVarintField(tool, 4, 0)
		return testDevinBytesField(nil, 6, tool)
	}
	stream := testDevinConnectStream(
		toolFrame("title_0", "title", `{"title":"triage"}`),
		toolFrame("bash_1", "bash", `{"command":"2>&1 <work>"}`),
	)
	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 64)
	go func() {
		defer close(out)
		executor.streamDevinFrames(ctx, bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatOpenAIResponse}, nil,
			newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
	}()

	var toolIDs []string
	var completed gjson.Result
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("Responses stream tool error: %v", chunk.Err)
		}
		_, payload, ok := bytes.Cut(chunk.Payload, []byte("data: "))
		if !ok {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		event := gjson.ParseBytes(payload)
		switch event.Get("type").String() {
		case "response.output_item.done":
			if event.Get("item.type").String() != "function_call" {
				continue
			}
			toolIDs = append(toolIDs, event.Get("item.call_id").String())
			if event.Get("item.call_id").String() == "bash_1" && !bytes.Contains(payload, []byte(`2>&1 <work>`)) {
				t.Fatalf("Responses tool arguments escaped: %s", payload)
			}
		case "response.completed":
			completed = event
		}
	}
	if len(toolIDs) != 2 || toolIDs[0] != "title_0" || toolIDs[1] != "bash_1" {
		t.Fatalf("Responses tool IDs = %#v, want title_0 then bash_1", toolIDs)
	}
	if got := completed.Get("response.output").Array(); len(got) != 2 ||
		got[0].Get("call_id").String() != "title_0" || got[1].Get("call_id").String() != "bash_1" {
		t.Fatalf("Responses completed output = %s", completed.Raw)
	}
}

func TestDevinPostToolTextFollowsCompletedToolCall(t *testing.T) {
	tool := testDevinBytesField(nil, 1, []byte("call-1"))
	tool = testDevinBytesField(tool, 2, []byte("read_file"))
	tool = testDevinBytesField(tool, 3, []byte(`{"path":"README.md"}`))
	stream := testDevinConnectStream(
		testDevinBytesField(nil, 3, []byte("before")),
		append(testDevinBytesField(nil, 6, tool), testDevinBytesField(nil, 3, []byte("after"))...),
	)
	response, _, _, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2")
	if err != nil {
		t.Fatalf("non-stream post-tool content: %v", err)
	}
	steps := gjson.GetBytes(response, "steps").Array()
	if len(steps) != 3 || steps[0].Get("content.0.text").String() != "before" ||
		steps[1].Get("type").String() != "function_call" || steps[1].Get("call_id").String() != "call-1" ||
		steps[2].Get("content.0.text").String() != "after" {
		t.Fatalf("non-stream post-tool step ordering = %s", response)
	}

	for _, format := range []sdktranslator.Format{sdktranslator.FormatInteractions, sdktranslator.FormatOpenAIResponse} {
		t.Run(string(format), func(t *testing.T) {
			ctx := context.Background()
			executor := NewDevinExecutor(&config.Config{})
			out := make(chan cliproxyexecutor.StreamChunk, 32)
			go func() {
				defer close(out)
				executor.streamDevinFrames(ctx, bytes.NewReader(stream),
					cliproxyexecutor.Request{Model: "devin/swe-2"},
					cliproxyexecutor.Options{ResponseFormat: format}, nil,
					newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
			}()
			toolDone := -1
			postToolStart := -1
			position := 0
			for chunk := range out {
				if chunk.Err != nil {
					t.Fatalf("post-tool stream error: %v", chunk.Err)
				}
				_, payload, ok := bytes.Cut(chunk.Payload, []byte("data: "))
				if !ok || bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
					continue
				}
				event := gjson.ParseBytes(bytes.TrimSpace(payload))
				if format == sdktranslator.FormatInteractions {
					if event.Get("event_type").String() == "step.stop" && event.Get("index").Int() == 1 {
						toolDone = position
					}
					if event.Get("event_type").String() == "step.start" && event.Get("step.type").String() == "model_output" && event.Get("index").Int() > 1 {
						postToolStart = position
					}
				} else {
					if event.Get("type").String() == "response.output_item.done" && event.Get("item.type").String() == "function_call" {
						toolDone = position
					}
					if event.Get("type").String() == "response.output_item.added" && event.Get("item.type").String() == "message" && event.Get("output_index").Int() > 1 {
						postToolStart = position
					}
				}
				position++
			}
			if toolDone < 0 || postToolStart <= toolDone {
				t.Fatalf("post-tool stream ordering: tool done=%d post-tool start=%d", toolDone, postToolStart)
			}
		})
	}
}

func TestDevinCustomToolOutputFailsWithoutToolBridge(t *testing.T) {
	tool := testDevinBytesField(nil, 1, []byte("call-1"))
	tool = testDevinBytesField(tool, 2, []byte("apply_patch"))
	tool = testDevinBytesField(tool, 4, []byte("*** Begin Patch\n*** End Patch"))
	tool = testDevinVarintField(tool, 6, 1)
	usageField := testDevinVarintField(nil, 2, 7)
	usageField = testDevinVarintField(usageField, 3, 3)
	usageField = testDevinVarintField(usageField, 4, 11)
	usageField = testDevinVarintField(usageField, 5, 2)
	stream := testDevinConnectStream(testDevinBytesField(nil, 7, usageField), testDevinBytesField(nil, 6, tool))
	if _, _, _, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2"); err == nil || testExecutorStatusCode(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "custom tool") {
		t.Fatalf("non-stream unsupported custom tool error = %v, want explicit 502", err)
	}

	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)
		executor.streamDevinFrames(ctx, bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatInteractions}, nil,
			newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
	}()
	var failure error
	for chunk := range out {
		if chunk.Err != nil {
			failure = chunk.Err
			continue
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if done || gjson.GetBytes(payload, "event_type").String() == "interaction.completed" {
			t.Fatalf("unsupported custom tool emitted success terminal: %s", chunk.Payload)
		}
	}
	if failure == nil || testExecutorStatusCode(failure) != http.StatusBadGateway || !strings.Contains(failure.Error(), "custom tool") {
		t.Fatalf("stream unsupported custom tool error = %v, want explicit 502", failure)
	}
	record := testWaitForDevinUsageRecord(t, plugin.records)
	if !record.Failed || record.Detail.InputTokens != 9 || record.Detail.TotalTokens != 12 ||
		record.Detail.CacheCreationTokens != 11 ||
		record.Detail.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified {
		t.Fatalf("failed custom tool usage record = %#v", record)
	}
}

func TestDevinResponseAccumulatorBoundsSparseToolCalls(t *testing.T) {
	state := newDevinResponseAccumulator()
	if err := state.applyToolDelta(helps.DevinToolCallDelta{
		Index: 127,
		ID:    "call-127",
		Name:  "empty_arguments",
	}); err != nil {
		t.Fatalf("applyToolDelta(127) error = %v", err)
	}
	if err := state.validateTools(); err != nil {
		t.Fatalf("validateTools() error = %v", err)
	}
	calls := state.toolCalls()
	if len(calls) != 1 || calls[0].ID != "call-127" || calls[0].Name != "empty_arguments" || calls[0].Arguments != "" {
		t.Fatalf("sparse tool calls = %#v", calls)
	}
	if err := state.applyToolDelta(helps.DevinToolCallDelta{Index: 128, Name: "out_of_range"}); err == nil {
		t.Fatal("applyToolDelta(128) error = nil")
	}
	if err := state.applyToolDelta(helps.DevinToolCallDelta{Index: 0, Name: "other"}); err != nil {
		t.Fatalf("applyToolDelta(0) error = %v", err)
	}
	if calls := state.toolCalls(); len(calls) != 2 || calls[1].Name != "other" {
		t.Fatalf("different unnamed tool indices were merged: %#v", calls)
	}
}

func TestDevinResponseLogIsBounded(t *testing.T) {
	state := newDevinResponseAccumulator()
	state.content.WriteString(strings.Repeat("\x00", maxDevinDecodedLogSource+1))
	state.thinking.WriteString(strings.Repeat("t", maxDevinDecodedLogSource))
	state.signature = bytes.Repeat([]byte{'s'}, maxDevinDecodedLogSource)
	if err := state.applyToolDelta(helps.DevinToolCallDelta{
		Index:     0,
		ID:        "call-1",
		Name:      "run",
		Arguments: strings.Repeat("a", maxDevinDecodedLogSource),
	}); err != nil {
		t.Fatalf("applyToolDelta() error = %v", err)
	}
	for field := 1; field <= maxDevinLoggedFields+10; field++ {
		state.recordUnknownFields([]int{field})
	}

	logged := state.logBytes("completed")
	if len(logged) > 8<<20 {
		t.Fatalf("log length = %d, want at most 8 MiB", len(logged))
	}
	payload := logged[bytes.IndexByte(logged, '\n')+1:]
	if !gjson.GetBytes(payload, "truncated").Bool() {
		t.Fatalf("truncated = false in %d-byte log", len(logged))
	}
	if len(state.unknownFields) != maxDevinLoggedFields {
		t.Fatalf("unknown field count = %d, want %d", len(state.unknownFields), maxDevinLoggedFields)
	}
}

func TestDevinStreamSkipsDecodedLogBodyWhenRequestLoggingDisabled(t *testing.T) {
	response := newDevinStreamingResponseAccumulator(false)
	state := &devinStreamState{
		executor:         NewDevinExecutor(&config.Config{}),
		ctx:              context.Background(),
		responseFormat:   sdktranslator.FormatInteractions,
		out:              make(chan cliproxyexecutor.StreamChunk, 16),
		interactionID:    "interaction-test",
		response:         response,
		activeTextIndex:  -1,
		thoughtStepIndex: -1,
	}
	fields := make([]int, maxDevinLoggedFields+10)
	for index := range fields {
		fields[index] = index + 1
	}
	if err := state.processFrame(helps.DevinFrameResult{
		ThinkingText:        "reason",
		ContentText:         "answer",
		DeltaSignature:      []byte("signature"),
		DeltaSignatureType:  "anthropic",
		UnknownFieldNumbers: fields,
	}); err != nil {
		t.Fatalf("processFrame() error = %v", err)
	}
	if response.thinking.Len() != 0 || response.content.Len() != 0 || len(response.signature) != 0 ||
		len(response.unknownFields) != 0 {
		t.Fatalf(
			"disabled log accumulation = thinking:%d content:%d signature:%d unknown:%d",
			response.thinking.Len(),
			response.content.Len(),
			len(response.signature),
			len(response.unknownFields),
		)
	}
	if err := response.finishText(); err != nil {
		t.Fatalf("finishText() error = %v", err)
	}

	bounded := newDevinStreamingResponseAccumulator(true)
	bounded.captureLogText(&bounded.thinking, strings.Repeat("t", maxDevinDecodedLogSource))
	bounded.captureLogText(&bounded.content, "overflow")
	bounded.captureLogSignature([]byte("overflow"))
	if bounded.logBodyBytes != maxDevinDecodedLogSource || !bounded.logBodyTruncated ||
		bounded.thinking.Len()+bounded.content.Len()+len(bounded.signature) > maxDevinDecodedLogSource {
		t.Fatalf(
			"bounded log accumulation = bytes:%d truncated:%t thinking:%d content:%d signature:%d",
			bounded.logBodyBytes,
			bounded.logBodyTruncated,
			bounded.thinking.Len(),
			bounded.content.Len(),
			len(bounded.signature),
		)
	}
}

func TestConsumeDevinFramesRejectsProtocolFailures(t *testing.T) {
	truncatedBody := make([]byte, 7)
	truncatedBody[0] = helps.ConnectFlagData
	binary.BigEndian.PutUint32(truncatedBody[1:], 4)
	copy(truncatedBody[5:], "ab")

	invalidUTF8 := testDevinBytesField(nil, 3, []byte{0xff})
	truncatedUTF8 := testDevinBytesField(nil, 3, []byte{0xe4, 0xbd})
	malformedDimension := testDevinBytesField(nil, 28, []byte{0x80})
	missingEOS := helps.WrapConnectEnvelope(testDevinBytesField(nil, 3, []byte("partial")))
	badTool := testDevinBytesField(nil, 6, testDevinBytesField(nil, 2, []byte("tool")))
	badTool = testDevinBytesField(badTool, 6, testDevinBytesField(nil, 3, []byte("{")))

	tests := []struct {
		name       string
		stream     []byte
		wantStatus int
		contains   string
	}{
		{name: "short header", stream: []byte{0}, wantStatus: http.StatusBadGateway, contains: "read Devin Connect frame"},
		{name: "truncated body", stream: truncatedBody, wantStatus: http.StatusBadGateway, contains: "read Devin Connect frame"},
		{name: "invalid flag", stream: helps.WrapConnectEnvelopeWithFlag(0x04, nil), wantStatus: http.StatusBadGateway, contains: "invalid connect frame flag"},
		{name: "invalid protobuf", stream: testDevinConnectStream([]byte{0x80}), wantStatus: http.StatusBadGateway, contains: "parse Devin response frame"},
		{name: "malformed field 28", stream: testDevinConnectStream(malformedDimension), wantStatus: http.StatusBadGateway, contains: "dimension usage"},
		{name: "invalid UTF-8", stream: testDevinConnectStream(invalidUTF8), wantStatus: http.StatusBadGateway, contains: "invalid UTF-8"},
		{name: "truncated UTF-8", stream: testDevinConnectStream(truncatedUTF8), wantStatus: http.StatusBadGateway, contains: "truncated UTF-8"},
		{name: "missing EOS", stream: missingEOS, wantStatus: http.StatusBadGateway, contains: "before the Connect EOS"},
		{name: "invalid tool arguments", stream: testDevinConnectStream(badTool), wantStatus: http.StatusBadGateway, contains: "invalid JSON arguments"},
		{
			name: "trailer authentication error",
			stream: helps.WrapConnectEnvelopeWithFlag(
				helps.ConnectFlagEndStream,
				[]byte(`{"error":{"code":"unauthenticated","message":"expired"}}`),
			),
			wantStatus: http.StatusUnauthorized,
			contains:   "unauthenticated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := consumeDevinFrames(bytes.NewReader(test.stream), "devin/swe-2")
			if err == nil {
				t.Fatal("consumeDevinFrames() error = nil")
			}
			if got := testExecutorStatusCode(err); got != test.wantStatus {
				t.Fatalf("status = %d, want %d, error = %v", got, test.wantStatus, err)
			}
			if !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %q, want substring %q", err, test.contains)
			}
		})
	}
}

func TestDevinStreamLifecycleAndUsageExactlyOnce(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)

	thinking := testDevinBytesField(nil, 9, []byte("reason"))
	signature := testDevinBytesField(nil, 10, []byte("sig"))
	signature = testDevinBytesField(signature, 21, []byte("anthropic"))
	content := testDevinBytesField(nil, 3, []byte("answer"))
	toolArguments := testDevinBytesField(nil, 3, []byte(`{"x":`))
	toolArguments = testDevinVarintField(toolArguments, 4, 0)
	toolMetadata := testDevinBytesField(nil, 1, []byte("call-1"))
	toolMetadata = testDevinBytesField(toolMetadata, 2, []byte("run"))
	toolMetadata = testDevinBytesField(toolMetadata, 3, []byte(`1}`))
	toolMetadata = testDevinVarintField(toolMetadata, 4, 0)
	usageField := testDevinVarintField(nil, 2, 2)
	usageField = testDevinVarintField(usageField, 3, 4)
	usageField = testDevinVarintField(usageField, 5, 3)
	usageField = testDevinBytesField(usageField, 9, []byte("devin/swe-3"))
	usageFrame := testDevinBytesField(nil, 7, usageField)
	stream := testDevinConnectStream(thinking, signature, content,
		testDevinBytesField(nil, 6, toolArguments),
		testDevinBytesField(nil, 6, toolMetadata), usageFrame)

	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	reporter := newUsageReporter(ctx, executor, "devin/swe-2", nil)
	out := make(chan cliproxyexecutor.StreamChunk, 32)
	go func() {
		defer close(out)
		executor.streamDevinFrames(
			ctx,
			bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatInteractions},
			nil,
			reporter,
			out,
		)
	}()

	var events [][]byte
	var doneCount int
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		if chunk.Terminal {
			t.Fatal("executor emitted transport Terminal=true")
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if done {
			doneCount++
			continue
		}
		if len(payload) > 0 {
			events = append(events, payload)
		}
	}
	if doneCount != 1 {
		t.Fatalf("[DONE] count = %d, want 1", doneCount)
	}
	if testDevinEventCount(events, "interaction.created") != 1 ||
		testDevinEventCount(events, "interaction.completed") != 1 {
		t.Fatalf("interaction lifecycle events = %s", testDevinEventTypes(events))
	}
	if testDevinEventCount(events, "response.failed") != 0 {
		t.Fatalf("unexpected failure events = %s", testDevinEventTypes(events))
	}

	startByType := make(map[string]int)
	stopByIndex := make(map[int64]int)
	var toolArgumentsDelta string
	var toolID string
	var toolName string
	for _, event := range events {
		switch gjson.GetBytes(event, "event_type").String() {
		case "step.start":
			stepType := gjson.GetBytes(event, "step.type").String()
			startByType[stepType]++
			if stepType == "function_call" {
				if value := gjson.GetBytes(event, "step.id").String(); value != "" {
					toolID = value
				}
				if value := gjson.GetBytes(event, "step.name").String(); value != "" {
					toolName = value
				}
			}
		case "step.delta":
			if gjson.GetBytes(event, "delta.type").String() == "arguments_delta" {
				toolArgumentsDelta += gjson.GetBytes(event, "delta.arguments").String()
			}
		case "step.stop":
			stopByIndex[gjson.GetBytes(event, "index").Int()]++
		}
	}
	if startByType["thought"] != 1 || startByType["model_output"] != 1 || startByType["function_call"] != 2 {
		t.Fatalf("step starts = %#v", startByType)
	}
	if toolID != "call-1" || toolName != "run" {
		t.Fatalf("updated tool metadata = %q/%q", toolID, toolName)
	}
	if len(stopByIndex) != 3 {
		t.Fatalf("step stops = %#v, want three indices", stopByIndex)
	}
	for index, count := range stopByIndex {
		if count != 1 {
			t.Fatalf("step %d stop count = %d, want 1", index, count)
		}
	}
	if toolArgumentsDelta != `{"x":1}` {
		t.Fatalf("arguments delta = %q", toolArgumentsDelta)
	}

	record := testWaitForDevinUsageRecord(t, plugin.records)
	if record.Failed || record.Model != "devin/swe-2" || record.ResponseModel != "devin/swe-3" ||
		record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 4 ||
		record.Detail.CachedTokens != 3 || record.Detail.TotalTokens != 9 {
		t.Fatalf("usage record = %#v", record)
	}
}

func TestDevinStreamEmitsToolDeltasBeforeEOS(t *testing.T) {
	out := make(chan cliproxyexecutor.StreamChunk, 8)
	state := &devinStreamState{
		executor:         NewDevinExecutor(&config.Config{}),
		ctx:              context.Background(),
		responseFormat:   sdktranslator.FormatInteractions,
		out:              out,
		interactionID:    "interaction-test",
		response:         newDevinStreamingResponseAccumulator(false),
		toolStepIndices:  make(map[*devinToolAccumulator]int),
		activeTextIndex:  -1,
		thoughtStepIndex: -1,
	}
	if err := state.processFrame(helps.DevinFrameResult{ToolCallDeltas: []helps.DevinToolCallDelta{{
		Index:     0,
		Arguments: `{"path":"`,
	}}}); err != nil {
		t.Fatalf("processFrame(arguments) error = %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("tool events before EOS = %d, want interaction creation, start and delta", len(out))
	}

	if err := state.processFrame(helps.DevinFrameResult{ToolCallDeltas: []helps.DevinToolCallDelta{{
		Index:     0,
		ID:        "call-1",
		Name:      "read_file",
		Arguments: `README.md"}`,
	}}}); err != nil {
		t.Fatalf("processFrame(metadata) error = %v", err)
	}
	if !state.flushTools() {
		t.Fatal("flushTools() = false")
	}

	var events [][]byte
	for len(out) > 0 {
		chunk := <-out
		if chunk.Err != nil {
			t.Fatalf("tool stream error = %v", chunk.Err)
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if done {
			t.Fatal("tool delta emitted [DONE] before completion")
		}
		events = append(events, payload)
	}
	if len(events) != 6 || gjson.GetBytes(events[0], "event_type").String() != "interaction.created" {
		t.Fatalf("tool events = %s", testDevinEventTypes(events))
	}
	if got := gjson.GetBytes(events[1], "step.id").String(); got != "" {
		t.Fatalf("initial tool id = %q, want empty until metadata arrives", got)
	}
	if got := gjson.GetBytes(events[2], "delta.arguments").String(); got != `{"path":"` {
		t.Fatalf("initial arguments delta = %q", got)
	}
	if got := gjson.GetBytes(events[3], "step.id").String(); got != "call-1" {
		t.Fatalf("updated tool id = %q", got)
	}
	if got := gjson.GetBytes(events[3], "step.name").String(); got != "read_file" {
		t.Fatalf("updated tool name = %q", got)
	}
	if got := gjson.GetBytes(events[4], "delta.arguments").String(); got != `README.md"}` {
		t.Fatalf("final arguments delta = %q", got)
	}
	if gjson.GetBytes(events[5], "event_type").String() != "step.stop" ||
		gjson.GetBytes(events[5], "index").Int() != 0 {
		t.Fatalf("tool stop event = %s", events[5])
	}
}

func TestDevinResponsesStreamWaitsForToolIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		id     string
		wantID string
	}{
		{name: "late identity", id: "call-1", wantID: "call-1"},
		{name: "missing upstream id", wantID: "call_0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := testDevinBytesField(nil, 3, []byte(`{"path":"`))
			first = testDevinVarintField(first, 4, 0)
			second := testDevinBytesField(nil, 2, []byte("read_file"))
			if test.id != "" {
				second = testDevinBytesField(second, 1, []byte(test.id))
			}
			second = testDevinBytesField(second, 3, []byte(`README.md"}`))
			second = testDevinVarintField(second, 4, 0)
			stream := testDevinConnectStream(testDevinBytesField(nil, 6, first), testDevinBytesField(nil, 6, second))

			ctx := context.Background()
			executor := NewDevinExecutor(&config.Config{})
			out := make(chan cliproxyexecutor.StreamChunk, 32)
			go func() {
				defer close(out)
				executor.streamDevinFrames(ctx, bytes.NewReader(stream),
					cliproxyexecutor.Request{Model: "devin/swe-2"},
					cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatOpenAIResponse}, nil,
					newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
			}()

			var added, completed bool
			var arguments string
			for chunk := range out {
				if chunk.Err != nil {
					t.Fatalf("stream error = %v", chunk.Err)
				}
				if bytes.Contains(chunk.Payload, []byte("data: [DONE]")) {
					continue
				}
				marker := []byte("data: ")
				index := bytes.Index(chunk.Payload, marker)
				if index < 0 {
					t.Fatalf("missing SSE data: %s", chunk.Payload)
				}
				payload := bytes.TrimSpace(chunk.Payload[index+len(marker):])
				switch gjson.GetBytes(payload, "type").String() {
				case "response.output_item.added", "response.output_item.done":
					if gjson.GetBytes(payload, "item.type").String() != "function_call" {
						continue
					}
					if got := gjson.GetBytes(payload, "item.call_id").String(); got != test.wantID {
						t.Fatalf("tool call_id = %q, want %q in %s", got, test.wantID, payload)
					}
					if got := gjson.GetBytes(payload, "item.name").String(); got != "read_file" {
						t.Fatalf("tool name = %q, want read_file in %s", got, payload)
					}
					if gjson.GetBytes(payload, "type").String() == "response.output_item.added" {
						added = true
					} else {
						completed = true
					}
				case "response.function_call_arguments.delta":
					arguments += gjson.GetBytes(payload, "delta").String()
				}
			}
			if !added || !completed || arguments != `{"path":"README.md"}` {
				t.Fatalf("tool events added=%t completed=%t arguments=%q", added, completed, arguments)
			}
		})
	}
}

func TestDevinStreamLateSignatureTargetsOriginalThoughtStep(t *testing.T) {
	thinking := testDevinBytesField(nil, 9, []byte("reason"))
	content := testDevinBytesField(nil, 3, []byte("answer"))
	signature := testDevinBytesField(nil, 10, []byte("late-signature"))
	signature = testDevinBytesField(signature, 21, []byte("anthropic"))
	stream := testDevinConnectStream(thinking, content, signature)

	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	reporter := newUsageReporter(ctx, executor, "devin/swe-2", nil)
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)
		executor.streamDevinFrames(
			ctx,
			bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatInteractions},
			nil,
			reporter,
			out,
		)
	}()

	var events [][]byte
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if !done && len(payload) > 0 {
			events = append(events, payload)
		}
	}

	thoughtIndex := int64(-1)
	modelIndex := int64(-1)
	signatureIndex := int64(-1)
	signaturePosition := -1
	modelStopPosition := -1
	thoughtStarts := 0
	thoughtStopPosition := -1
	for position, event := range events {
		switch gjson.GetBytes(event, "event_type").String() {
		case "step.start":
			switch gjson.GetBytes(event, "step.type").String() {
			case "thought":
				thoughtStarts++
				thoughtIndex = gjson.GetBytes(event, "index").Int()
			case "model_output":
				modelIndex = gjson.GetBytes(event, "index").Int()
			}
		case "step.delta":
			if gjson.GetBytes(event, "delta.type").String() == "thought_signature" {
				signatureIndex = gjson.GetBytes(event, "index").Int()
				signaturePosition = position
				if got := gjson.GetBytes(event, "delta.signature_type").String(); got != "anthropic" {
					t.Fatalf("signature_type = %q, want anthropic", got)
				}
			}
		case "step.stop":
			if gjson.GetBytes(event, "index").Int() == thoughtIndex {
				thoughtStopPosition = position
			}
			if gjson.GetBytes(event, "index").Int() == modelIndex {
				modelStopPosition = position
			}
		}
	}
	if thoughtStarts != 1 {
		t.Fatalf("thought step starts = %d, want 1; events=%s", thoughtStarts, testDevinEventTypes(events))
	}
	if thoughtIndex < 0 || modelIndex < 0 || thoughtIndex == modelIndex {
		t.Fatalf("thought index = %d, model index = %d", thoughtIndex, modelIndex)
	}
	if signatureIndex != thoughtIndex {
		t.Fatalf("late signature index = %d, want thought index %d", signatureIndex, thoughtIndex)
	}
	if signaturePosition < 0 || thoughtStopPosition < signaturePosition || modelStopPosition < signaturePosition {
		t.Fatalf(
			"thought or model output stopped before late signature: signature=%d thought stop=%d model stop=%d",
			signaturePosition,
			thoughtStopPosition,
			modelStopPosition,
		)
	}
}

func TestDevinStreamDefersToolStartUntilLateSignature(t *testing.T) {
	tool := testDevinBytesField(nil, 1, []byte("call-1"))
	tool = testDevinBytesField(tool, 2, []byte("read_file"))
	tool = testDevinBytesField(tool, 3, []byte(`{"path":"README.md"}`))
	stream := testDevinConnectStream(
		testDevinBytesField(nil, 9, []byte("reason")),
		testDevinBytesField(nil, 6, tool),
		testDevinBytesField(nil, 10, []byte("late-signature")),
	)
	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)
		executor.streamDevinFrames(ctx, bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatInteractions}, nil,
			newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
	}()
	thoughtStop, signatureDelta, toolStart := -1, -1, -1
	position := 0
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("late-signature tool stream error: %v", chunk.Err)
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if done {
			continue
		}
		event := gjson.ParseBytes(payload)
		switch {
		case event.Get("event_type").String() == "step.stop" && event.Get("index").Int() == 0:
			thoughtStop = position
		case event.Get("delta.type").String() == "thought_signature":
			signatureDelta = position
		case event.Get("step.type").String() == "function_call":
			toolStart = position
		}
		position++
	}
	if signatureDelta < 0 || thoughtStop <= signatureDelta || toolStart <= thoughtStop {
		t.Fatalf("signature/tool order: signature=%d thought stop=%d tool start=%d", signatureDelta, thoughtStop, toolStart)
	}
}

func TestDevinResponsesStreamPreservesSplitLateSignature(t *testing.T) {
	signatureBytes := make([]byte, 1+8+16+16+32)
	signatureBytes[0] = 0x80
	signature := base64.RawURLEncoding.EncodeToString(signatureBytes)
	stream := testDevinConnectStream(
		testDevinBytesField(nil, 9, []byte("reason")),
		testDevinBytesField(nil, 3, []byte("answer")),
		testDevinBytesField(nil, 10, []byte(signature[:len(signature)/2])),
		testDevinBytesField(nil, 10, []byte(signature[len(signature)/2:])),
	)
	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 24)
	go func() {
		defer close(out)
		executor.streamDevinFrames(ctx, bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatOpenAIResponse}, nil,
			newUsageReporter(ctx, executor, "devin/swe-2", nil), out)
	}()

	var completed gjson.Result
	var reasoningDone gjson.Result
	textDeltas := 0
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("Devin Responses stream error: %v", chunk.Err)
		}
		_, payload, ok := bytes.Cut(chunk.Payload, []byte("data: "))
		if !ok || bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			continue
		}
		event := gjson.ParseBytes(bytes.TrimSpace(payload))
		switch event.Get("type").String() {
		case "response.output_text.delta":
			if event.Get("delta").String() == "answer" {
				textDeltas++
			}
		case "response.output_item.done":
			if event.Get("item.type").String() == "reasoning" {
				reasoningDone = event
			}
		case "response.completed":
			completed = event
		}
	}
	if textDeltas != 1 || reasoningDone.Get("item.encrypted_content").String() != signature {
		t.Fatalf("late signature in Responses item: text deltas=%d item=%s", textDeltas, reasoningDone.Raw)
	}
	for _, item := range completed.Get("response.output").Array() {
		if item.Get("type").String() == "reasoning" {
			if item.Get("encrypted_content").String() != signature {
				t.Fatalf("completed reasoning signature = %q, want %q", item.Get("encrypted_content").String(), signature)
			}
			return
		}
	}
	t.Fatalf("completed response did not retain reasoning item: %s", completed.Raw)
}

func TestDevinImmediateTrailerErrorPrecedesResponseBody(t *testing.T) {
	stream := helps.WrapConnectEnvelopeWithFlag(
		helps.ConnectFlagEndStream,
		[]byte(`{"error":{"code":"resource_exhausted","message":"model is busy"}}`),
	)
	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 8)
	go func() {
		defer close(out)
		executor.streamDevinFrames(
			ctx, bytes.NewReader(stream), cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatOpenAIResponse}, nil,
			newUsageReporter(ctx, executor, "devin/swe-2", nil), out,
		)
	}()
	var chunks []cliproxyexecutor.StreamChunk
	for chunk := range out {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || chunks[0].Err == nil || len(chunks[0].Payload) > 0 ||
		testExecutorStatusCode(chunks[0].Err) != http.StatusTooManyRequests {
		t.Fatalf("immediate upstream failure chunks = %#v, want HTTP 429 error before response body", chunks)
	}
}

func TestDevinStreamFailureIsTerminalExactlyOnce(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)

	stream := helps.WrapConnectEnvelope(testDevinBytesField(nil, 3, []byte("partial")))
	ctx := context.Background()
	executor := NewDevinExecutor(&config.Config{})
	reporter := newUsageReporter(ctx, executor, "devin/swe-2", nil)
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)
		executor.streamDevinFrames(
			ctx,
			bytes.NewReader(stream),
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatInteractions},
			nil,
			reporter,
			out,
		)
	}()

	var events [][]byte
	var errorCount int
	var doneCount int
	for chunk := range out {
		if chunk.Terminal {
			t.Fatal("executor emitted transport Terminal=true")
		}
		if chunk.Err != nil {
			errorCount++
			continue
		}
		payload, done := testDevinSSEPayload(chunk.Payload)
		if done {
			doneCount++
			continue
		}
		if len(payload) > 0 {
			events = append(events, payload)
		}
	}
	if errorCount != 1 {
		t.Fatalf("stream error count = %d, want 1", errorCount)
	}
	if doneCount != 0 || testDevinEventCount(events, "interaction.completed") != 0 {
		t.Fatalf("failure emitted success terminal: events=%s done=%d", testDevinEventTypes(events), doneCount)
	}
	if testDevinEventCount(events, "response.failed") != 1 {
		t.Fatalf("failure events = %s", testDevinEventTypes(events))
	}
	failedEvent := testDevinFindEvent(events, "response.failed")
	if gjson.GetBytes(failedEvent, "response.error.code").String() != "missing_eos" ||
		gjson.GetBytes(failedEvent, "error.code").String() != "missing_eos" {
		t.Fatalf("failure event = %s", failedEvent)
	}

	record := testWaitForDevinUsageRecord(t, plugin.records)
	if !record.Failed {
		t.Fatalf("usage record = %#v, want failure", record)
	}
}

func testDevinConnectStream(frames ...[]byte) []byte {
	var stream []byte
	for _, frame := range frames {
		stream = append(stream, helps.WrapConnectEnvelope(frame)...)
	}
	return append(stream, helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{}`))...)
}

func testCompressedDevinFrame(t *testing.T, flag byte, payload []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("gzip.Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip.Close() error = %v", err)
	}
	return helps.WrapConnectEnvelopeWithFlag(flag, compressed.Bytes())
}

func testExecutorDevinDimensionGroup(metrics ...[]byte) []byte {
	group := testDevinBytesField(nil, 1, []byte("Token Usage"))
	for _, metric := range metrics {
		group = testDevinBytesField(group, 2, metric)
	}
	return group
}

func testExecutorDevinDimensionMetric(key string, value float32) []byte {
	dimension := protowire.AppendTag(nil, 2, protowire.Fixed32Type)
	dimension = protowire.AppendFixed32(dimension, math.Float32bits(value))
	metric := testDevinBytesField(nil, 4, dimension)
	return testDevinBytesField(metric, 5, []byte(key))
}

func testExecutorStatusCode(err error) int {
	if status, ok := err.(interface{ StatusCode() int }); ok {
		return status.StatusCode()
	}
	return 0
}

func testDevinSSEPayload(frame []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(frame)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return nil, true
	}
	return payload, false
}

func testDevinEventCount(events [][]byte, eventType string) int {
	count := 0
	for _, event := range events {
		if gjson.GetBytes(event, "event_type").String() == eventType {
			count++
		}
	}
	return count
}

func testDevinFindEvent(events [][]byte, eventType string) []byte {
	for _, event := range events {
		if gjson.GetBytes(event, "event_type").String() == eventType {
			return event
		}
	}
	return nil
}

func testDevinEventTypes(events [][]byte) string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, gjson.GetBytes(event, "event_type").String())
	}
	return strings.Join(types, ",")
}

func testWaitForDevinUsageRecord(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		select {
		case duplicate := <-records:
			t.Fatalf("duplicate usage record: %#v", duplicate)
		case <-time.After(25 * time.Millisecond):
		}
		return record
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Devin usage record")
		return usage.Record{}
	}
}
