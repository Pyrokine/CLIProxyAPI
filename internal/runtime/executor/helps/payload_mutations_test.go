package helps

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSetStringIfDifferentReusesCanonicalValue(t *testing.T) {
	input := []byte(`{"model":"gpt-test","messages":[]}`)
	output := SetStringIfDifferent(input, "model", "gpt-test")
	if &output[0] != &input[0] {
		t.Fatal("canonical string caused a payload copy")
	}
}

func TestSetStringIfDifferentNormalizesWrongType(t *testing.T) {
	input := []byte(`{"model":123}`)
	original := bytes.Clone(input)
	output := SetStringIfDifferent(input, "model", "123")
	model := gjson.GetBytes(output, "model")
	if model.Type != gjson.String || model.String() != "123" {
		t.Fatalf("model = %s, want string 123", model.Raw)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("input payload was modified in place")
	}
}

func TestSetBoolIfDifferentReusesCanonicalValue(t *testing.T) {
	input := []byte(`{"stream":true,"input":[]}`)
	output := SetBoolIfDifferent(input, "stream", true)
	if &output[0] != &input[0] {
		t.Fatal("canonical boolean caused a payload copy")
	}
}

func TestSetBoolIfDifferentNormalizesWrongType(t *testing.T) {
	input := []byte(`{"stream":"true"}`)
	output := SetBoolIfDifferent(input, "stream", true)
	if stream := gjson.GetBytes(output, "stream"); stream.Type != gjson.True {
		t.Fatalf("stream = %s, want boolean true", stream.Raw)
	}
}

func TestSetRawIfDifferentReusesIdenticalRawValue(t *testing.T) {
	input := []byte(`{"metadata":{"source":"executor"},"input":[]}`)
	output := setRawIfDifferent(input, "metadata", []byte(`{"source":"executor"}`))
	if &output[0] != &input[0] {
		t.Fatal("identical raw value caused a payload copy")
	}
}

func TestSetRawIfDifferentUpdatesDifferentRawValue(t *testing.T) {
	input := []byte(`{"metadata":"executor"}`)
	output := setRawIfDifferent(input, "metadata", []byte(`{"source":"executor"}`))
	metadata := gjson.GetBytes(output, "metadata")
	if !metadata.IsObject() || metadata.Get("source").String() != "executor" {
		t.Fatalf("metadata = %s, want object", metadata.Raw)
	}
}

// noinspection GoUnusedGlobalVariable
var benchmarkPayloadMutationOutput []byte

func BenchmarkSetStringIfDifferentLargeCanonicalPayload(b *testing.B) {
	input := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"` + strings.Repeat("x", 8<<20) + `"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkPayloadMutationOutput = SetStringIfDifferent(input, "model", "gpt-test")
	}
}
