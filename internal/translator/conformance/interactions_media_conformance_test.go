package conformance_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsMediaURIConformance(t *testing.T) {
	for _, target := range []struct {
		format sdktranslator.Format
		path   string
	}{
		{sdktranslator.FormatGemini, "contents.0.parts.0.fileData"},
		{sdktranslator.FormatAntigravity, "request.contents.0.parts.0.fileData"},
	} {
		for _, stream := range []bool{false, true} {
			for _, media := range []struct{ typ, mime string }{
				{"image", "image/png"}, {"audio", "audio/wav"}, {"video", "video/mp4"}, {"document", "application/pdf"},
			} {
				for _, source := range []string{
					`"uri":"gs://bucket/file"`, `"file_uri":"gs://bucket/file"`, `"fileUri":"gs://bucket/file"`,
					`"file_uri":"  ","fileUri":"","uri":"gs://bucket/file"`,
				} {
					t.Run(fmt.Sprintf("%s/%t/%s/%s", target.format, stream, media.typ, source), func(t *testing.T) {
						input := []byte(fmt.Sprintf(`{"input":[{"type":"user_input","content":[{"type":%q,"mime_type":%q,%s}]}]}`, media.typ, media.mime, source))
						body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, target.format, "test-model", input, stream)
						file := gjson.GetBytes(body, target.path)
						if err != nil || file.Get("fileUri").String() != "gs://bucket/file" || file.Get("mimeType").String() != media.mime {
							t.Fatalf("media URI = (%s, %v), want retained %s", body, err, media.mime)
						}
					})
				}
			}
		}
	}
}

func TestInteractionsGeminiHistoryConformance(t *testing.T) {
	const first = "EgYKBAECAwQ="
	const second = "EgYKBAEEAwI="
	for _, target := range []struct {
		format sdktranslator.Format
		path   string
	}{
		{sdktranslator.FormatGemini, "contents"},
		{sdktranslator.FormatAntigravity, "request.contents"},
	} {
		for _, stream := range []bool{false, true} {
			for _, shape := range []struct{ name, input string }{
				{"flat", `[{"type":"thought","signature":"` + first + `"},{"type":"function_call","name":"f","call_id":"a","arguments":{}},{"type":"function_call","name":"f","call_id":"b","arguments":{}},{"type":"function_result","name":"f","call_id":"a","result":1},{"type":"function_result","name":"f","call_id":"b","result":2}]`},
				{"interleaved model text", `[{"type":"function_call","name":"f","call_id":"a","signature":"` + first + `"},{"type":"model_output","content":[{"text":"explanation"}]},{"type":"function_call","name":"f","call_id":"b"},{"type":"function_result","name":"f","call_id":"a","result":1},{"type":"function_result","name":"f","call_id":"b","result":2}]`},
				{"unsigned thought between", `[{"type":"thought","signature":"` + first + `"},{"type":"thought","summary":[{"text":"plan"}]},{"type":"function_call","name":"f","call_id":"a"},{"type":"function_call","name":"f","call_id":"b"},{"type":"function_result","name":"f","call_id":"a","result":1},{"type":"function_result","name":"f","call_id":"b","result":2}]`},
				{"explicit clears carrier", `[{"type":"thought","signature":"` + first + `"},{"type":"function_call","name":"f","call_id":"a","signature":"` + first + `"},{"type":"function_call","name":"f","call_id":"b"},{"type":"function_result","name":"f","call_id":"a","result":1},{"type":"function_result","name":"f","call_id":"b","result":2}]`},
				{"distinct signatures", `[{"type":"thought","summary":[{"text":"plan"}],"signature":"` + second + `"},{"type":"thought","signature":"` + first + `"},{"type":"function_call","name":"f","call_id":"a"},{"type":"function_call","name":"f","call_id":"b"},{"type":"function_result","name":"f","call_id":"a","result":1},{"type":"function_result","name":"f","call_id":"b","result":2}]`},
			} {
				t.Run(fmt.Sprintf("%s/%t/%s", target.format, stream, shape.name), func(t *testing.T) {
					body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, target.format, "test-model", []byte(`{"input":`+shape.input+`}`), stream)
					contents := gjson.GetBytes(body, target.path).Array()
					if err != nil || len(contents) != 2 || contents[0].Get("role").String() != "model" || contents[1].Get("parts.#").Int() != 2 {
						t.Fatalf("parallel history = (%s, %v), want model and tool-result turns", body, err)
					}
					calls := 0
					for _, part := range contents[0].Get("parts").Array() {
						if part.Get("functionCall").Exists() {
							want := ""
							if calls == 0 {
								want = first
							}
							if part.Get("thoughtSignature").String() != want {
								t.Fatalf("call %d signature = %q, want %q; body=%s", calls, part.Get("thoughtSignature").String(), want, body)
							}
							calls++
						}
					}
					if calls != 2 || (shape.name == "distinct signatures" && !strings.Contains(string(body), second)) {
						t.Fatalf("call or signature lost: %s", body)
					}
				})
			}
		}
	}
}

func TestInteractionsMediaSignatureRoundTripConformance(t *testing.T) {
	const signature = "EgYKBAECAwQ="
	for _, target := range []struct {
		format sdktranslator.Format
		path   string
	}{
		{sdktranslator.FormatGemini, "contents"},
		{sdktranslator.FormatAntigravity, "request.contents"},
	} {
		for _, part := range []string{
			`{"text":"answer","thoughtSignature":"` + signature + `"}`,
			`{"text":"plan","thought":true,"thoughtSignature":"` + signature + `"}`,
			`{"thought":true,"thoughtSignature":"` + signature + `"}`,
			`{"functionCall":{"name":"f","id":"a","args":{}},"thoughtSignature":"` + signature + `"}`,
			`{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="},"thoughtSignature":"` + signature + `"}`,
			`{"inline_data":{"mime_type":"application/pdf","data":"aGVsbG8="},"thoughtSignature":"` + signature + `"}`,
		} {
			t.Run(fmt.Sprintf("%s/%s", target.format, part), func(t *testing.T) {
				response := []byte(`{"candidates":[{"content":{"role":"model","parts":[` + part + `]}}]}`)
				if target.format == sdktranslator.FormatAntigravity {
					response = []byte(`{"response":` + string(response) + `}`)
				}
				translated := sdktranslator.TranslateNonStream(context.Background(), target.format, sdktranslator.FormatInteractions, "test-model", nil, nil, response, nil)
				if gjson.GetBytes(translated, "steps.0.signature").String() != signature {
					t.Fatalf("response signature lost: %s", translated)
				}
				body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatInteractions, target.format, "test-model", []byte(`{"input":{"role":"model","steps":`+gjson.GetBytes(translated, "steps").Raw+`}}`), false)
				if err != nil || !strings.Contains(string(body), signature) || gjson.GetBytes(body, target.path+".0.parts.#").Int() != 1 {
					t.Fatalf("signature replay = (%s, %v)", body, err)
				}
				if strings.Contains(part, "aGVsbG8=") && !strings.Contains(string(body), "aGVsbG8=") {
					t.Fatalf("signed media bytes lost: response=%s; request=%s", translated, body)
				}
				if strings.Contains(part, `"text":"answer"`) {
					var state any
					frames := sdktranslator.TranslateStream(context.Background(), target.format, sdktranslator.FormatInteractions, "test-model", nil, nil, response, &state)
					var output strings.Builder
					for _, frame := range frames {
						output.Write(frame)
					}
					if !strings.Contains(output.String(), signature) || !strings.Contains(output.String(), "answer") {
						t.Fatalf("stream lost text or signature: %s", output.String())
					}
				}
			})
		}
	}
}

func TestGeminiInteractionsRejectedByteRequestConformance(t *testing.T) {
	for _, stream := range []bool{false, true} {
		input := []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf"}}]}]}`)
		body, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatGemini, sdktranslator.FormatInteractions, "test-model", input, stream)
		if err == nil || len(body) != 0 {
			t.Fatalf("checked request = (%s, %v), want rejected empty body", body, err)
		}
		if body := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatInteractions, "test-model", input, stream); len(body) != 0 {
			t.Fatalf("byte request retained rejected body: %s", body)
		}
	}
}
