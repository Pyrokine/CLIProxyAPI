package gemini

import (
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToOpenAIToolCallIDsAreDeterministicAndExplicit(t *testing.T) {
	request := []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"thought":true,"functionCall":{"name":"hidden","args":{}}},
				{"functionCall":{"name":"first","args":{"value":1}}},
				{"functionCall":{"id":"explicit-id","name":"second","args":{}}},
				{"functionCall":{"call_id":"explicit-snake","name":"third","args":{}}},
				{"functionCall":{"callId":"explicit-camel","name":"fourth","args":{}}}
			]},
			{"role":"user","parts":[
				{"functionResponse":{"name":"first","response":{"result":"one"}}},
				{"functionResponse":{"id":"explicit-id","name":"second","response":{"result":"two"}}},
				{"functionResponse":{"call_id":"explicit-snake","name":"third","response":{"result":"three"}}},
				{"functionResponse":{"callId":"explicit-camel","name":"fourth","response":{"result":"four"}}}
			]}
		]
	}`)

	first := ConvertGeminiRequestToOpenAI("gpt-test", request, false)
	second := ConvertGeminiRequestToOpenAI("gpt-test", request, false)
	want := []string{
		"call_gemini_0000000000000001",
		"explicit-id",
		"explicit-snake",
		"explicit-camel",
	}
	if got := openAIGeminiToolCallIDs(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("tool call IDs = %#v, want %#v; output: %s", got, want, first)
	}
	if got := openAIGeminiToolResultIDs(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("tool result IDs = %#v, want %#v; output: %s", got, want, first)
	}
	if got := openAIGeminiToolCallIDs(second); !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated conversion IDs = %#v, want %#v", got, want)
	}
}

func TestConvertGeminiRequestToOpenAIMatchesToolResultsByExplicitIDNameAndFIFO(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	request := []byte(("{\n\t\t\"contents\":[\n\t\t\t{\"role\":\"model\",\"parts\":[\n\t\t\t\t{\"" +
		"functionCall\":{\"id\":\"alpha-one\",\"name\":\"alpha\",\"args\":{}}},\n\t\t\t\t{\"functionCall\":{" +
		"\"id\":\"beta-one\",\"name\":\"beta\",\"args\":{}}},\n\t\t\t\t{\"functionCall\":{\"id\":\"alpha-" +
		"two\",\"name\":\"alpha\",\"args\":{}}}\n\t\t\t]},\n\t\t\t{\"role\":\"user\",\"parts\":[\n\t\t\t\t{\"" +
		"functionResponse\":{\"id\":\"alpha-two\",\"name\":\"alpha\",\"response\":{\"result\":\"explicit\"}}}" +
		",\n\t\t\t\t{\"functionResponse\":{\"name\":\"beta\",\"response\":{\"result\":\"named\"}}},\n\t\t\t\t" +
		"{\"functionResponse\":{\"response\":{\"result\":\"fifo\"}}}\n\t\t\t]}\n\t\t]\n\t}"))

	out := ConvertGeminiRequestToOpenAI("gpt-test", request, false)
	want := []string{"alpha-two", "beta-one", "alpha-one"}
	if got := openAIGeminiToolResultIDs(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("tool result IDs = %#v, want %#v; output: %s", got, want, out)
	}
}

func TestConvertGeminiRequestToOpenAIUsesDeterministicOrphanResponseID(t *testing.T) {
	request := []byte(
		`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"orphan","response":{"result":"value"}}}]}]}`,
	)
	want := []string{"call_gemini_0000000000000001"}

	for run := range 2 {
		out := ConvertGeminiRequestToOpenAI("gpt-test", request, false)
		if got := openAIGeminiToolResultIDs(out); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d orphan result IDs = %#v, want %#v; output: %s", run, got, want, out)
		}
	}
}

func openAIGeminiToolCallIDs(out []byte) []string {
	var ids []string
	for _, message := range gjson.GetBytes(out, "messages").Array() {
		for _, toolCall := range message.Get("tool_calls").Array() {
			ids = append(ids, toolCall.Get("id").String())
		}
	}
	return ids
}

func openAIGeminiToolResultIDs(out []byte) []string {
	var ids []string
	for _, message := range gjson.GetBytes(out, "messages").Array() {
		if message.Get("role").String() == "tool" {
			ids = append(ids, message.Get("tool_call_id").String())
		}
	}
	return ids
}
