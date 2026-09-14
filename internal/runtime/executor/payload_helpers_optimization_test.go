package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

type countingPayloadMarshaler struct {
	calls *int
	value string
}

func (m countingPayloadMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls = *m.calls + 1
	return json.Marshal(m.value)
}

func TestApplyPayloadConfigReusesCanonicalOverrides(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-test", Protocol: "openai"}},
					Params: map[string]any{"stream": true, "model": "gpt-test"},
				},
			},
			OverrideRaw: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-test", Protocol: "openai"}},
					Params: map[string]any{"metadata": `{"source":"executor"}`},
				},
			},
		},
	}
	input := []byte(`{"model":"gpt-test","stream":true,"metadata":{"source":"executor"},"messages":[]}`)
	output := applyPayloadConfigWithRoot(cfg, "gpt-test", "openai", "", input, nil, "")
	if &output[0] != &input[0] {
		t.Fatal("canonical payload overrides caused a payload copy")
	}
}

func TestApplyPayloadConfigNormalizesByteSliceOverride(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-test", Protocol: "openai"}},
					Params: map[string]any{"value": []byte("abc")},
				},
			},
		},
	}
	input := []byte(`{"value":"YWJj"}`)
	output := applyPayloadConfigWithRoot(cfg, "gpt-test", "openai", "", input, nil, "")
	value := gjson.GetBytes(output, "value")
	if value.Type != gjson.String || value.String() != "abc" {
		t.Fatalf("value = %s, want string abc", value.Raw)
	}
}

func TestSetPayloadValueIfDifferentUsesSJSONNumberEncoding(t *testing.T) {
	input := []byte(`{"value":1.2}`)
	output := setPayloadValueIfDifferent(input, "value", float32(1.2))
	if got := gjson.GetBytes(output, "value").Raw; got != "1.2000000476837158" {
		t.Fatalf("value = %s, want sjson float32 encoding", got)
	}
	canonical := []byte(`{"value":1.2000000476837158}`)
	reused := setPayloadValueIfDifferent(canonical, "value", float32(1.2))
	if &reused[0] != &canonical[0] {
		t.Fatal("canonical float32 encoding caused a payload copy")
	}
}

func TestSetPayloadValueIfDifferentCallsMarshalerOnce(t *testing.T) {
	for _, input := range [][]byte{[]byte(`{"value":"old"}`), []byte(`{"value":"new"}`)} {
		calls := 0
		value := countingPayloadMarshaler{calls: &calls, value: "new"}
		output := setPayloadValueIfDifferent(input, "value", value)
		if calls != 1 {
			t.Fatalf("MarshalJSON calls = %d, want 1", calls)
		}
		if got := gjson.GetBytes(output, "value").String(); got != "new" {
			t.Fatalf("value = %q, want new", got)
		}
	}
}

func TestRemoveToolTypeReusesArrayWithoutMatch(t *testing.T) {
	input := []byte(`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	output := removeToolTypeFromToolsArray(input, "tools", "image_generation")
	if &output[0] != &input[0] {
		t.Fatal("tool filtering without a match caused a payload copy")
	}
}

func TestStripVertexToolCallIDsReusesPayloadWithoutIDs(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"id":9007199254740993}}}]}]}`)
	output := stripVertexOpenAIResponsesToolCallIDs(input, "openai-response")
	if &output[0] != &input[0] {
		t.Fatal("payload without tool call IDs was copied")
	}
}

func TestStripVertexToolCallIDsRebuildsContentsOnce(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{"id":9007199254740993}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"lookup","response":{"id":"keep"}}}]}]}`)
	output := stripVertexOpenAIResponsesToolCallIDs(input, "openai-response")
	if gjson.GetBytes(output, "contents.0.parts.0.functionCall.id").Exists() {
		t.Fatal("functionCall.id was not removed")
	}
	if gjson.GetBytes(output, "contents.1.parts.0.functionResponse.id").Exists() {
		t.Fatal("functionResponse.id was not removed")
	}
	if got := gjson.GetBytes(output, "contents.1.parts.0.functionResponse.response.id").String(); got != "keep" {
		t.Fatalf("nested response id = %q, want keep", got)
	}
	if got := gjson.GetBytes(output, "contents.0.parts.0.functionCall.args.id").Raw; got != "9007199254740993" {
		t.Fatalf("large integer = %s, want exact original value", got)
	}
}

// noinspection GoUnusedGlobalVariable
var benchmarkVertexPayloadOutput []byte

func BenchmarkStripVertexToolCallIDsLargeNoopPayload(b *testing.B) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"` + strings.Repeat("x", 8<<20) + `"}]}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkVertexPayloadOutput = stripVertexOpenAIResponsesToolCallIDs(input, "openai-response")
	}
}
