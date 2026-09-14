package responses

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertAntigravityResponseToOpenAIResponsesNonStreamUnwrapsRequestsIndependently(t *testing.T) {
	openAIRequest := `{
		"model":"gemini-test",
		"input":"Call the weather tool for Tokyo.",
		"tools":[{
			"type":"function",
			"name":"get weather",
			"description":"Get weather for a city",
			"parameters":{
				"type":"object",
				"properties":{"city":{"type":"string"}},
				"required":["city"]
			}
		}]
	}`
	translatedRequest := []byte(`{
		"request":{
			"model":"gemini-test",
			"tools":[{
				"functionDeclarations":[{
					"name":"get_weather",
					"description":"Get weather for a city",
					"parameters":{
						"type":"OBJECT",
						"properties":{"city":{"type":"STRING"}},
						"required":["city"]
					}
				}]
			}]
		}
	}`)
	geminiResponse := `{
		"responseId":"antigravity-tool-response",
		"candidates":[{
			"content":{"parts":[{
				"functionCall":{
					"name":"get_weather",
					"args":{"city":"Tokyo"}
				}
			}]},
			"finishReason":"STOP"
		}]
	}`

	tests := []struct {
		name            string
		originalRequest []byte
		rawResponse     []byte
	}{
		{
			name:            "response wrapped request unwrapped",
			originalRequest: []byte(openAIRequest),
			rawResponse:     []byte(`{"response":` + geminiResponse + `}`),
		},
		{
			name:            "request wrapped response unwrapped",
			originalRequest: []byte(`{"request":` + openAIRequest + `}`),
			rawResponse:     []byte(geminiResponse),
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				output := convertAntigravityResponseToOpenAIResponsesNonStream(
					context.Background(),
					"gemini-test",
					test.originalRequest,
					translatedRequest,
					test.rawResponse,
					nil,
				)

				if !gjson.ValidBytes(output) {
					t.Fatalf("converter returned invalid JSON: %s", output)
				}
				if got := gjson.GetBytes(output, "tools.0.type").String(); got != "function" {
					t.Fatalf("tools.0.type = %q, want function; output=%s", got, output)
				}
				if got := gjson.GetBytes(output, "tools.0.name").String(); got != "get weather" {
					t.Fatalf("tools.0.name = %q, want get weather; output=%s", got, output)
				}
				if gjson.GetBytes(output, "tools.0.functionDeclarations").Exists() {
					t.Fatalf("OpenAI response contains Gemini functionDeclarations: %s", output)
				}
				if got := gjson.GetBytes(output, "output.0.type").String(); got != "function_call" {
					t.Fatalf("output.0.type = %q, want function_call; output=%s", got, output)
				}
				if got := gjson.GetBytes(output, "output.0.name").String(); got != "get weather" {
					t.Fatalf("output.0.name = %q, want get weather; output=%s", got, output)
				}
				arguments := gjson.GetBytes(output, "output.0.arguments").String()
				if !gjson.Valid(arguments) || gjson.Get(arguments, "city").String() != "Tokyo" {
					t.Fatalf(
						"output.0.arguments = %q, want JSON arguments with city Tokyo; output=%s", arguments, output,
					)
				}
			},
		)
	}
}
