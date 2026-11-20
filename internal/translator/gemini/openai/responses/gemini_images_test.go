package responses

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const nanoBananaImageResponse = `{"responseId":"banana","modelVersion":"gemini-nano-banana-2.1","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"before"},{"inlineData":{"mimeType":"image/webp","data":"QUFB"},"thoughtSignature":"EgYKBAECAwQ="},{"text":"after"},{"inline_data":{"mime_type":"image/png","data":"QkJC"},"thought_signature":"EgYKBAEGBwg="}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}`

func TestNanoBanana21ResponsesImagesAndReplay(t *testing.T) {
	request := []byte(`{"model":"gemini-nano-banana-2.1","input":"draw"}`)
	response := ConvertGeminiResponseToOpenAIResponsesNonStream(
		t.Context(), "gemini-nano-banana-2.1", request, request, []byte(nanoBananaImageResponse), nil,
	)
	if gjson.GetBytes(response, "status").String() != "completed" {
		t.Fatalf("image response status: %s", response)
	}
	var images []gjson.Result
	for _, item := range gjson.GetBytes(response, "output").Array() {
		if item.Get("type").String() == "image_generation_call" {
			images = append(images, item)
		}
	}
	if len(images) != 2 || images[0].Get("result").String() != "QUFB" || images[1].Get("result").String() != "QkJC" || images[0].Get("output_format").String() != "webp" || images[1].Get("thought_signature").String() != "EgYKBAEGBwg=" {
		t.Fatalf("image output lost: %s", response)
	}
	replay := []byte(fmt.Sprintf(
		`{"model":"gemini-nano-banana-2.1","input":[%s,%s,{"role":"user","content":[{"type":"input_text","text":"edit"}]}]}`,
		images[0].Raw, images[1].Raw,
	))
	translated, err := ConvertOpenAIResponsesRequestToGeminiChecked("gemini-nano-banana-2.1", replay, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"QUFB", "QkJC", "EgYKBAECAwQ=", "EgYKBAEGBwg=", "image/webp"} {
		if !bytes.Contains(translated, []byte(value)) {
			t.Fatalf("replay lost %s: %s", value, translated)
		}
	}
}

func TestNanoBanana21StreamingImagesReachTerminalOutput(t *testing.T) {
	request := []byte(`{"model":"gemini-nano-banana-2.1","input":"draw"}`)
	var state any
	frames := translateGeminiFrameWithEOF(
		t.Context(), "gemini-nano-banana-2.1", request, request, []byte(nanoBananaImageResponse), &state,
	)
	images := 0
	var completed gjson.Result
	for _, frame := range frames {
		for _, line := range strings.Split(string(frame), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			item := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if item.Get("type").String() == "response.output_item.done" && item.Get("item.type").String() == "image_generation_call" {
				images++
			}
			if item.Get("type").String() == "response.completed" {
				completed = item.Get("response")
			}
		}
	}
	if images != 2 || completed.Get("status").String() != "completed" || completed.Get("usage.total_tokens").Int() != 8 {
		t.Fatalf("images=%d completed=%s", images, completed.Raw)
	}
	terminalImages := 0
	for _, item := range completed.Get("output").Array() {
		if item.Get("type").String() == "image_generation_call" {
			terminalImages++
			if item.Get("result").String() == "" {
				t.Fatal("terminal image lost bytes")
			}
		}
	}
	if terminalImages != 2 {
		t.Fatalf("terminal lost image items: %s", completed.Raw)
	}
}

func TestNanoBanana21ImageOnlyResponseIsSuccessful(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"QUFB"}}]},"finishReason":"STOP"}]}`)
	result := ConvertGeminiResponseToOpenAIResponsesNonStream(
		t.Context(), "gemini-nano-banana-2.1", nil, nil, body, nil,
	)
	if gjson.GetBytes(result, "status").String() != "completed" || gjson.GetBytes(
		result, "output.0.result",
	).String() != "QUFB" {
		t.Fatalf("image-only response: %s", result)
	}
}

func TestNanoBanana21ImageConfigurationSurvivesOutputBudget(t *testing.T) {
	request := []byte(`{"input":"draw","generationConfig":{"imageConfig":{"imageSize":"4K","aspectRatio":"16:9"},"responseModalities":["IMAGE"],"thinkingConfig":{"thinkingLevel":"medium"}},"max_output_tokens":4096}`)
	result, err := ConvertOpenAIResponsesRequestToGeminiChecked("gemini-nano-banana-2.1", request, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(result, "generationConfig.imageConfig.imageSize").String() != "4K" || gjson.GetBytes(
		result, "generationConfig.imageConfig.aspectRatio",
	).String() != "16:9" || gjson.GetBytes(
		result, "generationConfig.responseModalities.0",
	).String() != "IMAGE" || gjson.GetBytes(
		result, "generationConfig.maxOutputTokens",
	).Int() != 4096 || gjson.GetBytes(result, "generationConfig.thinkingConfig.thinkingLevel").String() != "medium" {
		t.Fatalf("generation settings lost: %s", result)
	}
}
