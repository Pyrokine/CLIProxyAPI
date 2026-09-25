package executor

import (
	"bytes"
	"compress/gzip"
	"context"
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

	response, logBody, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2")
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

	response, _, err := consumeDevinFrames(bytes.NewReader(stream), "devin/swe-2")
	if err != nil {
		t.Fatalf("consumeDevinFrames() error = %v", err)
	}
	if got := gjson.GetBytes(response, "steps.0.content.0.text").String(); got != "你好" {
		t.Fatalf("content = %q, want 你好", got)
	}
	if got := gjson.GetBytes(response, "steps.1.id").String(); got != "call-1" {
		t.Fatalf("tool id = %q", got)
	}
	if got := gjson.GetBytes(response, "steps.1.name").String(); got != "read_file" {
		t.Fatalf("tool name = %q", got)
	}
	if got := gjson.GetBytes(response, "steps.1.arguments.path").String(); got != "README.md" {
		t.Fatalf("tool arguments path = %q", got)
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
			_, _, err := consumeDevinFrames(bytes.NewReader(test.stream), "devin/swe-2")
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
	if record.Failed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 4 ||
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
		toolStepIndices:  make(map[int]int),
		activeTextIndex:  -1,
		thoughtStepIndex: -1,
	}
	if err := state.processFrame(helps.DevinFrameResult{ToolCallDeltas: []helps.DevinToolCallDelta{{
		Index:     0,
		Arguments: `{"path":"`,
	}}}); err != nil {
		t.Fatalf("processFrame(arguments) error = %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("tool events before EOS = %d, want start and delta", len(out))
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
	if len(events) != 5 {
		t.Fatalf("tool events = %s", testDevinEventTypes(events))
	}
	if got := gjson.GetBytes(events[0], "step.id").String(); got != "" {
		t.Fatalf("initial tool id = %q, want empty until metadata arrives", got)
	}
	if got := gjson.GetBytes(events[1], "delta.arguments").String(); got != `{"path":"` {
		t.Fatalf("initial arguments delta = %q", got)
	}
	if got := gjson.GetBytes(events[2], "step.id").String(); got != "call-1" {
		t.Fatalf("updated tool id = %q", got)
	}
	if got := gjson.GetBytes(events[2], "step.name").String(); got != "read_file" {
		t.Fatalf("updated tool name = %q", got)
	}
	if got := gjson.GetBytes(events[3], "delta.arguments").String(); got != `README.md"}` {
		t.Fatalf("final arguments delta = %q", got)
	}
	if gjson.GetBytes(events[4], "event_type").String() != "step.stop" ||
		gjson.GetBytes(events[4], "index").Int() != 0 {
		t.Fatalf("tool stop event = %s", events[4])
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
	if signaturePosition < 0 || modelStopPosition < signaturePosition {
		t.Fatalf(
			"model output stopped before late signature: signature position=%d stop position=%d",
			signaturePosition,
			modelStopPosition,
		)
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
