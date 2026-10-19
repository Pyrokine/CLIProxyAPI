package helps

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestBuildDevinWireFiltersAutomationAndSanitizesDescriptions(t *testing.T) {
	request := BuildDevinGetChatMessageRequest(
		"test-token", "seed", "swe-2", "system", nil,
		[]DevinTool{
			{Name: "mcp__codex_app__automation_update", Description: "Recurring automations"},
			{Name: "exec_command", Description: "returning output or a session ID for ongoing interaction"},
			{Name: "write_stdin", Description: "Writes characters to an existing unified exec session and returns recent output. Takes a task_id parameter identifying the task"},
		},
		nil, 1000, "session-1", "cascade-1", nil,
	)
	tools := testProtoBytesFields(t, request, 10)
	if len(tools) != 2 {
		t.Fatalf("wire tool count = %d, want 2", len(tools))
	}
	for index, want := range []struct{ name, description string }{
		{"exec_command", "returning output or an session ID for ongoing interaction"},
		{"write_stdin", "Writes characters to a existing unified exec session and returns recent output. Takes a taskId parameter identifying the task"},
	} {
		name := testProtoBytesFields(t, tools[index], 1)
		description := testProtoBytesFields(t, tools[index], 2)
		if len(name) != 1 || len(description) != 1 || string(name[0]) != want.name || string(description[0]) != want.description {
			t.Fatalf("wire tool %d = name %q description %q, want %q/%q", index, name, description, want.name, want.description)
		}
	}
}

type repeatedByteReader byte

func (reader repeatedByteReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = byte(reader)
	}
	return len(buffer), nil
}

func TestReadConnectFrameValidatesFramingAndCompression(t *testing.T) {
	payload := []byte("hello devin")
	flag, decoded, err := ReadConnectFrame(bytes.NewReader(WrapConnectEnvelope(payload)))
	if err != nil {
		t.Fatalf("ReadConnectFrame(data): %v", err)
	}
	if flag != ConnectFlagData || !bytes.Equal(decoded, payload) {
		t.Fatalf("ReadConnectFrame(data) = 0x%02x/%q, want 0x00/%q", flag, decoded, payload)
	}

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err = writer.Write(payload); err != nil {
		t.Fatalf("gzip.Write(): %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("gzip.Close(): %v", err)
	}
	flag, decoded, err = ReadConnectFrame(bytes.NewReader(
		WrapConnectEnvelopeWithFlag(ConnectFlagCompressed, compressed.Bytes()),
	))
	if err != nil {
		t.Fatalf("ReadConnectFrame(gzip): %v", err)
	}
	if flag != ConnectFlagCompressed || !bytes.Equal(decoded, payload) {
		t.Fatalf("ReadConnectFrame(gzip) = 0x%02x/%q, want 0x01/%q", flag, decoded, payload)
	}
}

func TestReadConnectFrameRejectsMalformedFrames(t *testing.T) {
	oversized := make([]byte, 5)
	oversized[0] = ConnectFlagData
	binary.BigEndian.PutUint32(oversized[1:], maxConnectFrameSize+1)

	truncatedBody := make([]byte, 7)
	truncatedBody[0] = ConnectFlagData
	binary.BigEndian.PutUint32(truncatedBody[1:], 4)
	copy(truncatedBody[5:], "ab")

	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "short header", frame: []byte{ConnectFlagData, 0}},
		{name: "invalid flag", frame: WrapConnectEnvelopeWithFlag(0x04, nil)},
		{name: "oversized body", frame: oversized},
		{name: "truncated body", frame: truncatedBody},
		{name: "invalid gzip", frame: WrapConnectEnvelopeWithFlag(ConnectFlagCompressed, []byte("not gzip"))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := ReadConnectFrame(bytes.NewReader(test.frame)); err == nil {
				t.Fatal("ReadConnectFrame() error = nil")
			}
		})
	}
}

func TestReadConnectFrameRejectsDecompressedPayloadOverLimit(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := io.CopyN(writer, repeatedByteReader('x'), maxDecompressedFrameSize+1); err != nil {
		t.Fatalf("gzip payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip.Close(): %v", err)
	}

	frame := WrapConnectEnvelopeWithFlag(ConnectFlagCompressed, compressed.Bytes())
	if _, _, err := ReadConnectFrame(bytes.NewReader(frame)); err == nil ||
		!strings.Contains(err.Error(), "decompressed frame size") {
		t.Fatalf("ReadConnectFrame() error = %v, want decompressed size error", err)
	}
}

func TestBuildDevinGetChatMessageRequestLimitsSensitiveWordsToSystemPrompt(t *testing.T) {
	const sensitive = "SECRET_TOKEN"
	matcher := BuildSensitiveWordMatcher([]string{sensitive})
	prompts := []DevinPrompt{{
		MessageID: "assistant-1",
		Source:    2,
		Content:   "ordinary message with " + sensitive,
		ToolCalls: []DevinToolCall{{
			ID:        "call-1",
			Name:      "run_task",
			Arguments: `{"task_id":"` + sensitive + `"}`,
		}},
	}}
	tools := []DevinTool{{
		Name:        "run_task",
		Description: "accepts task_id exactly; " + sensitive,
		Parameters:  []byte(`{"type":"object","properties":{"task_id":{"type":"string"}}}`),
	}}

	sessionID := "sensitive-word-scope"
	ResetDevinSessionTurnIndex(sessionID)
	defer ResetDevinSessionTurnIndex(sessionID)
	request := BuildDevinGetChatMessageRequest(
		"session-token",
		"device-seed",
		"model-uid",
		"keep this system line\nremove "+sensitive+" line",
		prompts,
		tools,
		nil,
		1024,
		sessionID,
		"cascade-1",
		matcher,
	)

	systemFields := testProtoBytesFields(t, request, 2)
	if len(systemFields) != 1 || string(systemFields[0]) != "keep this system line" {
		t.Fatalf("system fields = %q, want one retained line", systemFields)
	}
	for _, unchanged := range []string{
		"ordinary message with " + sensitive,
		`{"task_id":"` + sensitive + `"}`,
		"accepts task_id exactly; " + sensitive,
		`{"type":"object","properties":{"task_id":{"type":"string"}}}`,
	} {
		if !bytes.Contains(request, []byte(unchanged)) {
			t.Fatalf("request changed non-system value %q", unchanged)
		}
	}
}

func TestBuildDevinGetChatMessageRequestPreservesExplicitZeroTemperature(t *testing.T) {
	zero := 0.0
	request := BuildDevinGetChatMessageRequest(
		"token",
		"seed",
		"model",
		"",
		nil,
		nil,
		&zero,
		777,
		"temperature-zero",
		"cascade",
		nil,
	)
	completionFields := testProtoBytesFields(t, request, 8)
	if len(completionFields) != 1 {
		t.Fatalf("completion field count = %d, want 1", len(completionFields))
	}
	var temperature float64
	var maxTokens uint64
	for position := 0; position < len(completionFields[0]); {
		number, wireType, consumed := protowire.ConsumeTag(completionFields[0][position:])
		if consumed <= 0 {
			t.Fatalf("consume completion tag at %d: %v", position, protowire.ParseError(consumed))
		}
		position += consumed
		switch {
		case number == 2 && wireType == protowire.VarintType:
			value, size := protowire.ConsumeVarint(completionFields[0][position:])
			if size <= 0 {
				t.Fatalf("consume max tokens: %v", protowire.ParseError(size))
			}
			maxTokens = value
			position += size
		case number == 5 && wireType == protowire.Fixed64Type:
			value, size := protowire.ConsumeFixed64(completionFields[0][position:])
			if size <= 0 {
				t.Fatalf("consume temperature: %v", protowire.ParseError(size))
			}
			temperature = math.Float64frombits(value)
			position += size
		default:
			size := protowire.ConsumeFieldValue(number, wireType, completionFields[0][position:])
			if size <= 0 {
				t.Fatalf("consume completion field %d: %v", number, protowire.ParseError(size))
			}
			position += size
		}
	}
	if temperature != 0 || maxTokens != 777 {
		t.Fatalf("completion config = temperature %v/max tokens %d, want 0/777", temperature, maxTokens)
	}
}

func TestBuildDevinGetChatMessageRequestTracksSessionTurns(t *testing.T) {
	sessionID := "session-turn-counter"
	ResetDevinSessionTurnIndex(sessionID)
	defer ResetDevinSessionTurnIndex(sessionID)
	prompts := []DevinPrompt{{MessageID: "user-1", Source: 1, Content: "hello"}}

	first := BuildDevinGetChatMessageRequest(
		"token", "seed", "model", "", prompts, nil, nil, 100, sessionID, "cascade", nil,
	)
	firstSession := testProtoBytesFields(t, first, 15)
	if len(firstSession) != 1 {
		t.Fatalf("first request field 15 count = %d, want 1", len(firstSession))
	}
	firstValues := testProtoVarintFields(t, firstSession[0])
	if _, present := firstValues[2]; present {
		t.Fatalf("first request field 15.2 = %d, want omitted", firstValues[2])
	}
	if firstValues[3] != 4 || firstValues[4] != 14 {
		t.Fatalf("first request field 15 = %v, want 3:4 and 4:14", firstValues)
	}

	second := BuildDevinGetChatMessageRequest(
		"token", "seed", "model", "", prompts, nil, nil, 100, sessionID, "cascade", nil,
	)
	secondSession := testProtoBytesFields(t, second, 15)
	secondValues := testProtoVarintFields(t, secondSession[0])
	if secondValues[2] != 1 {
		t.Fatalf("second request field 15.2 = %d, want 1", secondValues[2])
	}
}

func TestParseDevinFrameDecodesContentToolAndUsage(t *testing.T) {
	tool := appendDevinStringField(nil, 1, "call-1")
	tool = appendDevinStringField(tool, 2, "read_file")
	tool = appendDevinStringField(tool, 3, `{"path":"README.md"}`)
	tool = appendDevinVarintField(tool, 4, 2)

	usage := appendDevinVarintField(nil, 2, 0)
	usage = appendDevinVarintField(usage, 3, 5)
	usage = appendDevinVarintField(usage, 5, 0)

	payload := appendDevinStringField(nil, 1, "output-1")
	payload = appendDevinStringField(payload, 3, "hello ")
	payload = appendDevinStringField(payload, 3, "world")
	payload = appendDevinBytesField(payload, 6, tool)
	payload = appendDevinBytesField(payload, 7, usage)
	payload = appendDevinStringField(payload, 9, "thinking")
	payload = appendDevinBytesField(payload, 10, []byte("signature"))
	payload = appendDevinStringField(payload, 17, "message-1")
	payload = appendDevinStringField(payload, 21, "anthropic")

	result, err := ParseDevinFrame(payload)
	if err != nil {
		t.Fatalf("ParseDevinFrame(): %v", err)
	}
	if result.OutputID != "output-1" || result.ContentText != "hello world" ||
		result.ThinkingText != "thinking" || result.MessageID != "message-1" {
		t.Fatalf("ParseDevinFrame() = %+v", result)
	}
	if len(result.ToolCallDeltas) != 1 || result.ToolCallDeltas[0].Index != 2 ||
		result.ToolCallDeltas[0].Name != "read_file" {
		t.Fatalf("tool call deltas = %+v", result.ToolCallDeltas)
	}
	if result.Usage == nil || !result.Usage.PromptTokensPresent ||
		!result.Usage.CachedTokensPresent || result.Usage.PromptTokens != 0 ||
		result.Usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v, want explicit zero prompt/cache and completion 5", result.Usage)
	}
}

func TestParseDevinFrameRecognizesFinalCustomToolWireFields(t *testing.T) {
	tool := appendDevinStringField(nil, 1, "call-1")
	tool = appendDevinStringField(tool, 2, "apply_patch")
	tool = appendDevinStringField(tool, 4, "*** Begin Patch\n*** End Patch")
	tool = appendDevinStringField(tool, 5, "not JSON")
	tool = appendDevinVarintField(tool, 6, 1)
	frame, err := ParseDevinFrame(appendDevinBytesField(nil, 6, tool))
	if err != nil || len(frame.ToolCallDeltas) != 1 {
		t.Fatalf("ParseDevinFrame custom tool = %+v, %v", frame, err)
	}
	call := frame.ToolCallDeltas[0]
	if call.ID != "call-1" || call.Name != "apply_patch" ||
		call.InvalidJSONStr != "*** Begin Patch\n*** End Patch" ||
		call.InvalidJSONErr != "not JSON" || !call.IsCustomToolCall {
		t.Fatalf("custom tool fields = %+v", call)
	}
}

func TestParseDevinFrameRejectsMalformedProtobuf(t *testing.T) {
	wrongWireType := appendDevinVarintField(nil, 3, 1)
	truncatedBytes := protowire.AppendTag(nil, 3, protowire.BytesType)
	truncatedBytes = protowire.AppendVarint(truncatedBytes, 4)
	truncatedBytes = append(truncatedBytes, 'a')

	for _, payload := range [][]byte{wrongWireType, truncatedBytes, {0x80}} {
		if _, err := ParseDevinFrame(payload); err == nil {
			t.Fatalf("ParseDevinFrame(%x) error = nil", payload)
		}
	}
}

func TestParseDevinTrailerErrorIsStrict(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantStatus int
		wantError  bool
	}{
		{name: "empty", payload: "", wantStatus: 0},
		{name: "success", payload: "{}", wantStatus: 0},
		{name: "malformed JSON", payload: "{", wantStatus: http.StatusBadGateway, wantError: true},
		{name: "missing error", payload: `{"metadata":{}}`, wantStatus: http.StatusBadGateway, wantError: true},
		{name: "missing code", payload: `{"error":{"message":"failed"}}`, wantStatus: http.StatusBadGateway, wantError: true},
		{name: "quota", payload: `{"error":{"code":"failed_precondition","message":"monthly ACU quota exhausted"}}`, wantStatus: http.StatusTooManyRequests, wantError: true},
		{name: "unauthenticated", payload: `{"error":{"code":"unauthenticated","message":"expired"}}`, wantStatus: http.StatusUnauthorized, wantError: true},
		{name: "high demand", payload: `{"error":{"code":"permission_denied","message":"The model is currently in high demand; retry soon"}}`, wantStatus: http.StatusTooManyRequests, wantError: true},
		{name: "high demand case-insensitive", payload: `{"error":{"code":"permission_denied","message":"HIGH DEMAND: retry soon"}}`, wantStatus: http.StatusTooManyRequests, wantError: true},
		{name: "genuine permission denial", payload: `{"error":{"code":"permission_denied","message":"model access denied"}}`, wantStatus: http.StatusForbidden, wantError: true},
		{name: "resource exhausted", payload: `{"error":{"code":"resource_exhausted","message":"rate limit"}}`, wantStatus: http.StatusTooManyRequests, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, err := ParseDevinTrailerError([]byte(test.payload))
			if status != test.wantStatus || (err != nil) != test.wantError {
				t.Fatalf("ParseDevinTrailerError() = %d/%v, want %d/error=%t", status, err, test.wantStatus, test.wantError)
			}
		})
	}
}

func TestUTF8SplitBufferValidatesStreamText(t *testing.T) {
	var buffer UTF8SplitBuffer
	encoded := []byte("你好")
	if got := buffer.Feed(encoded[:2]); got != "" {
		t.Fatalf("Feed(first fragment) = %q, want empty", got)
	}
	if got := buffer.Feed(encoded[2:5]); got != "你" {
		t.Fatalf("Feed(second fragment) = %q, want 你", got)
	}
	if got := buffer.Feed(encoded[5:]); got != "好" {
		t.Fatalf("Feed(third fragment) = %q, want 好", got)
	}
	if err := buffer.Finish(); err != nil {
		t.Fatalf("Finish(valid): %v", err)
	}

	var truncated UTF8SplitBuffer
	truncated.Feed([]byte{0xe4, 0xbd})
	if err := truncated.Finish(); err == nil {
		t.Fatal("Finish(truncated) error = nil")
	}

	var invalid UTF8SplitBuffer
	if got := invalid.Feed([]byte{0xff}); got != string(utf8.RuneError) {
		t.Fatalf("Feed(invalid) = %q, want replacement rune", got)
	}
	if err := invalid.Finish(); err == nil {
		t.Fatal("Finish(invalid) error = nil")
	}
}

func TestGenerateDevinIdentityValues(t *testing.T) {
	first := GenerateDevinDeviceFingerprint("stable-seed")
	second := GenerateDevinDeviceFingerprint("stable-seed")
	if first != second || len(first) != DevinFingerprintHexLen {
		t.Fatalf("deterministic fingerprint lengths/equality = %d/%t", len(first), first == second)
	}

	parts := strings.Split(GenerateDevinSentryTrace(), "-")
	if len(parts) != 3 || len(parts[0]) != 32 || len(parts[1]) != 16 || parts[2] != "1" {
		t.Fatalf("Sentry trace = %q, want 32hex-16hex-1", strings.Join(parts, "-"))
	}
}

func testProtoBytesFields(t *testing.T, data []byte, wanted protowire.Number) [][]byte {
	t.Helper()
	var values [][]byte
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			t.Fatalf("consume tag at %d: %v", position, protowire.ParseError(consumed))
		}
		position += consumed
		if number == wanted && wireType == protowire.BytesType {
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				t.Fatalf("consume bytes at %d: %v", position, protowire.ParseError(size))
			}
			values = append(values, append([]byte(nil), value...))
			position += size
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			t.Fatalf("consume field %d at %d: %v", number, position, protowire.ParseError(size))
		}
		position += size
	}
	return values
}

func testProtoVarintFields(t *testing.T, data []byte) map[protowire.Number]uint64 {
	t.Helper()
	values := make(map[protowire.Number]uint64)
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			t.Fatalf("consume tag at %d: %v", position, protowire.ParseError(consumed))
		}
		position += consumed
		if wireType == protowire.VarintType {
			value, size := protowire.ConsumeVarint(data[position:])
			if size <= 0 {
				t.Fatalf("consume varint at %d: %v", position, protowire.ParseError(size))
			}
			values[number] = value
			position += size
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			t.Fatalf("consume field %d at %d: %v", number, position, protowire.ParseError(size))
		}
		position += size
	}
	return values
}

func TestBuildDevinClientMetadataCurrentFieldLayout(t *testing.T) {
	for _, osName := range []string{"linux", "windows"} {
		t.Run(osName, func(t *testing.T) {
			metadata := BuildDevinClientMetadataBytes("synthetic-session", "synthetic-seed", osName)
			for _, field := range []struct {
				number   protowire.Number
				expected string
			}{
				{1, DevinDefaultClientName},
				{2, DevinDefaultClientVersion},
				{3, "synthetic-session"},
				{5, osName},
				{7, DevinDefaultClientVersion},
				{12, DevinDefaultClientName},
			} {
				values := testProtoBytesFields(t, metadata, field.number)
				if len(values) != 1 || string(values[0]) != field.expected {
					t.Errorf("metadata field %d = %q, want %q", field.number, values, field.expected)
				}
			}
			if values := testProtoBytesFields(t, metadata, 28); len(values) != 0 {
				t.Errorf("obsolete metadata field 28 remains: %q", values)
			}
			if values := testProtoBytesFields(t, metadata, 31); len(values) != 1 || len(values[0]) != DevinFingerprintHexLen {
				t.Errorf("device fingerprint changed: %q", values)
			}
		})
	}
}
