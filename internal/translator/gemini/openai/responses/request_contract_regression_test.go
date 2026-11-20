package responses_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	geminiresponses "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiResponsesOriginalMediaInputsAcrossLocations(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	//goland:noinspection SpellCheckingInspection
	fixtures := []struct {
		name, part, mime, data, uri string
		reject                      bool
	}{
		{
			name: "nested-audio-wav", part: `{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}`,
			mime: "audio/wav", data: "UklGRg==",
		},
		{
			name: "flat-audio-mp3", part: `{"type":"input_audio","data":"SUQzBA==","format":"mp3"}`, mime: "audio/mpeg",
			data: "SUQzBA==",
		},
		{
			name: "audio-data-url", part: `{"type":"input_audio","audio_url":"data:audio/ogg;base64,T2dnUw=="}`,
			mime: "audio/ogg", data: "T2dnUw==",
		},
		{
			name: "audio-alias-mp3", part: `{"type":"audio","audio":{"data":"SUQzBA==","format":"mp3"}}`,
			mime: "audio/mpeg", data: "SUQzBA==",
		},
		{
			name: "audio-alias-wav", part: `{"type":"audio","audio":{"data":"UklGRg==","format":"wav"}}`,
			mime: "audio/wav", data: "UklGRg==",
		},
		{
			name: "video-data-url", part: `{"type":"input_video","video_url":"data:video/mp4;base64,AAAAIGZ0eXBtcDQy"}`,
			mime: "video/mp4", data: "AAAAIGZ0eXBtcDQy",
		},
		{
			name: "nested-video-url",
			part: `{"type":"input_video","video_url":{"url":"data:video/webm;base64,GkXfo59ChoEBQveBAULygQ8="}}`,
			mime: "video/webm", data: "GkXfo59ChoEBQveBAULygQ8=",
		},
		{
			name: "video-file-name", part: `{"type":"input_file","filename":"sample.mp4","file_data":"AAAAIGZ0eXA="}`,
			mime: "video/mp4", data: "AAAAIGZ0eXA=",
		},
		{
			name: "video-alias", part: `{"type":"video","video":{"data":"GkXfo59ChoEBQveBAULygQ8=","format":"webm"}}`,
			mime: "video/webm", data: "GkXfo59ChoEBQveBAULygQ8=",
		},
		{
			name: "inline-audio-explicit-format",
			part: `{"type":"input_audio","data":"UklGRg==","format":"wav","filename":"download.bin"}`,
			mime: "audio/wav", data: "UklGRg==",
		},
		{
			name: "inline-video-explicit-format",
			part: `{"type":"input_video","data":"AAAAIGZ0eXA=","format":"mp4","filename":"download.bin"}`,
			mime: "video/mp4", data: "AAAAIGZ0eXA=",
		},
		{
			name: "remote-video-nested-format",
			part: `{"type":"input_video","video_url":"https://example.com/download.bin",` +
				`"input_video":{"format":` +
				`"mp4"}}`,
			mime: "video/mp4", uri: "https://example.com/download.bin",
		},
		{
			name: "remote-audio-nested-mime",
			part: `{"type":"input_audio","audio_url":"https://example.com/download.bin",` +
				`"input_audio":{"mime_type":"audio/` +
				`wav"}}`,
			mime: "audio/wav", uri: "https://example.com/download.bin",
		},
		{
			name: "remote-pdf-nested-format",
			part: `{"type":"input_file","file_url":"https://example.com/download.bin","file":{"format":"pdf"}}`,
			mime: "application/pdf", uri: "https://example.com/download.bin",
		},
		{
			name: "remote-jpeg-file-format",
			part: `{"type":"input_file","file_url":"https://example.com/download.bin","file":{"format":"jpeg"}}`,
			mime: "image/jpeg", uri: "https://example.com/download.bin",
		},
		{
			name: "remote-jpeg-image-format",
			part: `{"type":"input_image","image_url":"https://example.com/download.bin","image":{"format":"jpeg"}}`,
			mime: "image/jpeg", uri: "https://example.com/download.bin",
		},
		{
			name: "generic-video-nested-format",
			part: `{"type":"input_video","video_url":"https://example.com/download.bin","` +
				`mime_type":"application/octet-stream","input_video":{"format":"mp4"}}`,
			mime: "video/mp4", uri: "https://example.com/download.bin",
		},
		{
			name: "generic-audio-nested-format",
			part: `{"type":"input_audio","audio_url":"https://example.com/download.bin","` +
				`mime_type":"binary/octet-stream","input_audio":{"format":"wav"}}`,
			mime: "audio/wav", uri: "https://example.com/download.bin",
		},
		{
			name: "generic-file-nested-format",
			part: `{"type":"input_file","file_url":"https://example.com/download.bin","mi` +
				`me_type":"application/octet-stream","file":{"format":"pdf"}}`,
			mime: "application/pdf", uri: "https://example.com/download.bin",
		},
		{
			name: "remote-video-extension", part: `{"type":"input_video","video_url":"https://example.com/stream.mp4"}`,
			mime: "video/mp4", uri: "https://example.com/stream.mp4",
		},
		{
			name: "remote-audio-extension",
			part: `{"type":"input_audio","audio_url":"https://example.com/recording.wav"}`, mime: "audio/wav",
			uri: "https://example.com/recording.wav",
		},
		{
			name: "remote-image-extension", part: `{"type":"input_image","image_url":"https://example.com/photo.jpeg"}`,
			mime: "image/jpeg", uri: "https://example.com/photo.jpeg",
		},
		{
			name: "remote-image-explicit-format",
			part: `{"type":"input_image","image_url":"https://example.com/legacy.bin","format":"jpg"}`,
			mime: "image/jpeg", uri: "https://example.com/legacy.bin",
		},
		{
			name: "remote-video-generic-format",
			part: `{"type":"input_video","video_url":"https://example.com/clip.webm","format":"binary/octet-stream"}`,
			mime: "video/webm", uri: "https://example.com/clip.webm",
		},
		{
			name: "remote-audio-generic-mime",
			part: `{"type":"input_audio","audio_url":"https://example.com/recording.mp3",` +
				`"mime_type":"application/` +
				`octet-stream"}`,
			mime: "audio/mpeg", uri: "https://example.com/recording.mp3",
		},
		{
			name: "generic-data-url-image-mime",
			part: `{"type":"input_image","image_url":"data:;base64,AAAA","mime_type":"image/jpeg"}`, mime: "image/jpeg",
			data: "AAAA",
		},
		{
			name: "generic-data-url-file-mime",
			part: `{"type":"input_file","file_data":"data:;base64,BBBB","mime_type":"application/pdf"}`,
			mime: "application/pdf", data: "BBBB",
		},
		{
			name: "generic-data-url-image-name",
			part: `{"type":"input_image","image_url":"data:;base64,CCCC","filename":"photo.jpeg"}`, mime: "image/jpeg",
			data: "CCCC",
		},
		{
			name: "generic-data-url-file-format",
			part: `{"type":"input_file","file_data":"data:;base64,DDDD","format":"pdf"}`, mime: "application/pdf",
			data: "DDDD",
		},
		{
			name: "generic-data-url-no-extension",
			part: `{"type":"input_image","image_url":"data:;base64,EEEE","filename":"photo"}`, mime: "image/png",
			data: "EEEE",
		},
		{
			name: "generic-data-url-unknown-extension",
			part: `{"type":"input_image","image_url":"data:;base64,FFFF","filename":"photo.unknownext"}`,
			mime: "image/png", data: "FFFF",
		},
		{
			name: "generic-data-url-application",
			part: `{"type":"input_image","image_url":"data:application/octet-stream;` +
				`base64,GGGG","filename":` +
				`"photo.jpeg"}`,
			mime: "image/jpeg", data: "GGGG",
		},
		{
			name: "generic-data-url-binary",
			part: `{"type":"input_image","image_url":"data:binary/octet-stream;base64,HHHH","filename":"photo.jpeg"}`,
			mime: "image/jpeg", data: "HHHH",
		},
		{
			name: "generic-file-mime-name",
			part: `{"type":"input_file","file_data":"QUJD","filename":"report.pdf","mime_type":"binary/octet-stream"}`,
			mime: "application/pdf", data: "QUJD",
		},
		{
			name: "remote-audio-explicit-format",
			part: `{"type":"input_audio","audio_url":"https://example.com/download.bin","format":"wav"}`,
			mime: "audio/wav", uri: "https://example.com/download.bin",
		},
		{
			name: "remote-video-explicit-format",
			part: `{"type":"input_video","video_url":"https://example.com/stream.bin","format":"mp4"}`,
			mime: "video/mp4", uri: "https://example.com/stream.bin",
		},
		{
			name: "unknown-audio-extension",
			part: `{"type":"input_audio","audio_url":"https://example.com/download.bin",` +
				`"mime_type":"application/` +
				`octet-stream"}`,
			mime: "audio/wav", uri: "https://example.com/download.bin",
		},
		{
			name: "unknown-video-extension",
			part: `{"type":"input_video","video_url":"https://example.com/download.bin",` +
				`"mime_type":"application/` +
				`octet-stream"}`,
			mime: "video/mp4", uri: "https://example.com/download.bin",
		},
		{
			name:   "empty-audio-data-url",
			part:   `{"type":"input_audio","data":"data:audio/wav;base64,"}`,
			reject: true,
		},
		{
			name: "non-base64-video-data-url", part: `{"type":"input_video","video_url":"data:text/plain,hello"}`,
			reject: true,
		},
		{
			name: "corrupted-audio-data-url", part: `{"type":"input_audio","data":"data:audio/wav;base64,!!!"}`,
			reject: true,
		},
		{
			name:   "uppercase-audio-data-url",
			part:   `{"type":"input_audio","data":"DATA:audio/wav,hello"}`,
			reject: true,
		},
		{
			name: "whitespace-audio-data-url", part: `{"type":"input_audio","data":" DATA:audio/wav;base64,!!!"}`,
			reject: true,
		},
		{
			name: "source-invalid-data-url",
			part: `{"type":"input_image","source":{"type":"base64","data":" DATA:image/png;base64,!!!"}}`, reject: true,
		},
		{
			name: "video-url-alias", part: `{"type":"video_url","video_url":"http://example.com/stream.mp4"}`,
			mime: "video/mp4", uri: "http://example.com/stream.mp4",
		},
		{
			name: "file-alias-gs-uri",
			part: `{"type":"file","file_url":"gs://synthetic-bucket/report.pdf","mime_type":"application/pdf"}`,
			mime: "application/pdf", uri: "gs://synthetic-bucket/report.pdf",
		},
	}
	for _, mediaType := range []string{
		"input_image", "input_audio", "audio", "input_video", "video_url", "video", "input_file", "file",
	} {
		for _, invalid := range []struct{ name, fields string }{
			{name: "missing"},
			{name: "invalid-data-url", fields: `,"url":"data:application/octet-stream;base64,???"`},
			{name: "unsupported-file-id", fields: `,"file_id":"synthetic-file"`},
		} {
			fixtures = append(
				fixtures, struct {
					name, part, mime, data, uri string
					reject                      bool
				}{
					name: mediaType + "/" + invalid.name, part: fmt.Sprintf(`{"type":%q%s}`, mediaType, invalid.fields),
					reject: true,
				},
			)
		}
	}
	for _, fixture := range fixtures {
		for _, location := range []string{"input", "message", "tool", "tool-string-array", "tool-string-object"} {
			t.Run(
				fixture.name+"/"+location, func(t *testing.T) {
					input := fmt.Sprintf(`{"input":[%s]}`, fixture.part)
					if location == "message" {
						input = fmt.Sprintf(
							`{"input":[{"type":"message","role":"user","content":[%s]}]}`,
							fixture.part,
						)
					} else if strings.HasPrefix(location, "tool") {
						output := "[" + fixture.part + "]"
						if location == "tool-string-object" {
							output = fixture.part
						}
						if location != "tool" {
							encoded, err := json.Marshal(output)
							if err != nil {
								t.Fatal(err)
							}
							output = string(encoded)
						}
						input = fmt.Sprintf(
							`{"input":[{"type":"function_call","call_id":"media-call","name":"inspect","`+
								`arguments":"{}"},{"type":"function_call_output","call_id":"media-call","`+
								`output":%s}]}`,
							output,
						)
					}
					for _, stream := range []bool{false, true} {
						out, err := sdk.TranslateRequestChecked(
							t.Context(),
							sdk.FormatOpenAIResponse,
							sdk.FormatGemini,
							"gemini-2.5-flash",
							[]byte(input),
							stream,
						)
						legacy := geminiresponses.ConvertOpenAIResponsesRequestToGemini(
							"gemini-2.5-flash", []byte(input), stream,
						)
						if fixture.reject {
							if err == nil || len(out) != 0 || len(legacy) != 0 {
								t.Fatalf(
									"invalid media accepted: checked=%s byte=%s error=%v",
									out,
									legacy,
									err,
								)
							}
							continue
						}
						if err != nil || string(legacy) != string(out) {
							t.Fatalf("media entries disagree: checked=%s byte=%s error=%v", out, legacy, err)
						}
						prefix, inline, file := "contents.0.parts.0.", "inline_data.", "file_data."
						mime, data, uri := "mime_type", "data", "file_uri"
						if strings.HasPrefix(location, "tool") {
							prefix, inline, file = "contents.1.parts.0.functionResponse.parts.0.",
								"inlineData.", "fileData."
							mime, uri = "mimeType", "fileUri"
						}
						if fixture.uri != "" {
							if gjson.GetBytes(out, prefix+file+mime).String() != fixture.mime || gjson.GetBytes(
								out, prefix+file+uri,
							).String() != fixture.uri {
								t.Fatalf("media MIME or URI changed: %s", out)
							}
						} else if gjson.GetBytes(
							out, prefix+inline+mime,
						).String() != fixture.mime || gjson.GetBytes(out, prefix+inline+data).String() != fixture.data {
							t.Fatalf("media MIME or data changed: %s", out)
						}
					}
				},
			)
		}
	}
}

func TestGeminiResponsesOriginalToolOutputVariations(t *testing.T) {
	for _, tc := range []struct {
		name, input, callID, tool, result, mime, data string
	}{
		{
			name: "array with image",
			input: `{"model":"gemini-3.7-flash-high","input":[{"role":"user","content":[{"` +
				`type":"input_text","text":"Below is the image from tool. Reply IMAGE_S` +
				`EEN."}]},{"type":"function_call","id":"fc_test","call_id":"call_test",` +
				`"name":"read","arguments":"{}"},{"type":"function_call_output","call_i` +
				`d":"call_test","output":[{"type":"input_text","text":"Read image file ` +
				`[image/png]"},{"type":"input_image","detail":"auto","image_url":"data:` +
				`image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}]}]}`,
			callID: "call_test", tool: "read", result: `"Read image file [image/png]"`,
			mime: "image/png", data: "iVBORw0KGgoAAAANSUhEUg==",
		},
		{
			name: "stringified JSON array with image",
			input: `{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","cal` +
				`l_id":"call_1","name":"screenshot","arguments":"{}"},{"type":"function` +
				`_call_output","call_id":"call_1","output":"[{\"type\":\"input_text\",\` +
				`"text\":\"done\"},{\"type\":\"input_image\",\"image_url\":\"data:image` +
				`/jpeg;base64,/9j/4AAQSkZJRg==\"}]"}]}`,
			callID: "call_1", tool: "screenshot", result: `"done"`,
			mime: "image/jpeg", data: "/9j/4AAQSkZJRg==",
		},
		{
			name: "plain structured JSON array without images",
			input: `{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","cal` +
				`l_id":"call_1","name":"list_items","arguments":"{}"},{"type":"function` +
				`_call_output","call_id":"call_1","output":[{"id":1,"name":"first"},{"i` +
				`d":2,"name":"second"}]}]}`,
			callID: "call_1", tool: "list_items", result: `[{"id":1,"name":"first"},{"id":2,"name":"second"}]`,
		},
		{
			name: "plain string output",
			input: `{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","cal` +
				`l_id":"call_1","name":"echo","arguments":"{}"},{"type":"function_call_` +
				`output","call_id":"call_1","output":"plain string result"}]}`,
			callID: "call_1", tool: "echo", result: `"plain string result"`,
		},
		{
			name: "structured string with image_url stays string",
			input: `{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","cal` +
				`l_id":"call_1","name":"get_hero","arguments":"{}"},{"type":"function_c` +
				`all_output","call_id":"call_1","output":"{\"ok\":true,\"caption\":\"he` +
				`ro\",\"image_url\":\"https://example.com/hero.png\"}"}]}`,
			callID: "call_1", tool: "get_hero",
			result: `"{\"ok\":true,\"caption\":\"hero\",\"image_url\":\"https://example.com/hero.png\"}"`,
		},
		{
			name: "mixed array with text and structured object",
			input: `{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","call_id":"call_1","` +
				`name":"query","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":[` +
				`{"type":"input_text","text":"summary header"},{"id":1,"status":"active"}]}]}`,
			callID: "call_1", tool: "query",
			result: `[{"type":"input_text","text":"summary header"},{"id":1,"status":"active"}]`,
		},
		{
			name: "stringified non-media array stays string",
			input: `{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","cal` +
				`l_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_cal` +
				`l_output","call_id":"call_1","output":"[{\"id\":1}]"}]}`,
			callID: "call_1", tool: "lookup", result: `"[{\"id\":1}]"`,
		},
		{
			name: "nested image_url object with detail",
			input: `{"model":"gemini-3.7-flash-high","input":[{"type":"function_call","cal` +
				`l_id":"call_1","name":"photo","arguments":"{}"},{"type":"function_call` +
				`_output","call_id":"call_1","output":[{"type":"input_image","image_url` +
				`":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="},"detail":"h` +
				`igh"}]}]}`,
			callID: "call_1", tool: "photo", result: `""`,
			mime: "image/png", data: "iVBORw0KGgoAAAANSUhEUg==",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				for _, stream := range []bool{false, true} {
					out, err := sdk.TranslateRequestChecked(
						t.Context(), sdk.FormatOpenAIResponse, sdk.FormatGemini, "gemini-3.7-flash-high",
						[]byte(tc.input), stream,
					)
					if err != nil {
						t.Fatal(err)
					}
					if legacy := geminiresponses.ConvertOpenAIResponsesRequestToGemini(
						"gemini-3.7-flash-high", []byte(tc.input), stream,
					); string(legacy) != string(out) {
						t.Fatalf("byte and checked tool outputs disagree: byte=%s checked=%s", legacy, out)
					}
					var responses []gjson.Result
					for _, content := range gjson.GetBytes(out, "contents").Array() {
						for _, part := range content.Get("parts").Array() {
							if response := part.Get("functionResponse"); response.Exists() {
								responses = append(responses, response)
							}
						}
					}
					if len(responses) != 1 || responses[0].Get("id").String() != tc.callID ||
						responses[0].Get("name").String() != tc.tool {
						t.Fatalf("tool output ownership changed: %s", out)
					}
					response := responses[0]
					var want, got any
					if err := json.Unmarshal([]byte(tc.result), &want); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(response.Get("response.result").Raw), &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("tool result changed: got=%v want=%v output=%s", got, want, out)
					}
					media := response.Get("parts").Array()
					if tc.data == "" {
						if len(media) != 0 {
							t.Fatalf("ordinary result became media: %s", out)
						}
					} else if len(media) !=
						1 ||
						media[0].Get("inlineData.mimeType").String() !=
							tc.mime ||
						media[0].Get("inlineData.data").String() !=
							tc.data {
						t.Fatalf("tool image was lost or changed: %s", out)
					}
				}
			},
		)
	}
}

func TestGeminiResponsesOriginalTopLevelTextAndVideoStayTogether(t *testing.T) {
	input := []byte((`{"model":"gemini-2.5-flash","input":[{"type":"input_text","text":"Check video"},{"type":` +
		`"input_video","video_url":"data:video/mp4;base64,AAAAIGZ0eXA="}]}`))
	for _, stream := range []bool{false, true} {
		out, err := sdk.TranslateRequestChecked(
			t.Context(), sdk.FormatOpenAIResponse, sdk.FormatGemini, "gemini-2.5-flash", input, stream,
		)
		if err != nil {
			t.Fatal(err)
		}
		if legacy := geminiresponses.ConvertOpenAIResponsesRequestToGemini(
			"gemini-2.5-flash", input, stream,
		); string(legacy) != string(out) {
			t.Fatalf("byte and checked top-level media disagree: byte=%s checked=%s", legacy, out)
		}
		contents := gjson.GetBytes(out, "contents").Array()
		//goland:noinspection SpellCheckingInspection
		if len(contents) != 1 || contents[0].Get("role").String() != "user" ||
			contents[0].Get("parts.#").Int() != 2 ||
			contents[0].Get("parts.0.text").String() != "Check video" ||
			contents[0].Get("parts.1.inline_data.mime_type").String() != "video/mp4" ||
			contents[0].Get("parts.1.inline_data.data").String() != "AAAAIGZ0eXA=" {
			t.Fatalf("top-level text and video order changed: %s", out)
		}
	}
}

func TestGeminiResponsesOriginalCustomToolReasoningHistory(t *testing.T) {
	const signature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"
	input := []byte((`{"model":"gemini-3.6-flash-high","input":[{"type":"additional_tools","tools":[{"type":"` +
		`namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},{"type":"message","role":` +
		`"user","content":[{"type":"input_text","text":"Run pwd"}]},{"type":"reasoning","encrypted_content":"`) +
		signature + (`","summary":[{"type":"summary_text","text":"executing pwd"}]},{"type":"` +
		`custom_tool_call","call_id":"call_1","name":"exec","namespace":"functions","input":"pwd"},{"` +
		`type":"custom_tool_call_output","call_id":"call_1","output":"/workspace"}]}`))
	for _, stream := range []bool{false, true} {
		out, err := sdk.TranslateRequestChecked(
			t.Context(), sdk.FormatOpenAIResponse, sdk.FormatGemini, "gemini-3.6-flash-high", input, stream,
		)
		if err != nil {
			t.Fatal(err)
		}
		if legacy := geminiresponses.ConvertOpenAIResponsesRequestToGemini(
			"gemini-3.6-flash-high", input, stream,
		); string(legacy) != string(out) {
			t.Fatalf("byte and checked custom-tool history disagree: byte=%s checked=%s", legacy, out)
		}
		contents := gjson.GetBytes(out, "contents").Array()
		if len(contents) != 3 || contents[0].Get("role").String() != "user" ||
			contents[1].Get("role").String() != "model" ||
			contents[2].Get("role").String() != "user" {
			t.Fatalf("custom-tool history roles changed: %s", out)
		}
		modelParts := contents[1].Get("parts").Array()
		if len(modelParts) != 2 || !modelParts[0].Get("thought").Bool() ||
			modelParts[0].Get("text").String() != "executing pwd" ||
			modelParts[1].Get("functionCall.name").String() != "functions__exec" ||
			modelParts[1].Get("thoughtSignature").String() != signature {
			t.Fatalf("custom-tool thought, identity or signature changed: %s", out)
		}
		response := contents[2].Get("parts").Array()
		if len(response) != 1 || response[0].Get("functionResponse.name").String() != "functions__exec" ||
			response[0].Get("functionResponse.response.result").String() != "/workspace" {
			t.Fatalf("custom-tool output changed: %s", out)
		}
	}
}

func TestGeminiResponsesSDKUnsignedReasoningDoesNotCreateSignatures(t *testing.T) {
	for _, model := range []string{"gemini-3-flash", "configured-model-alias"} {
		for _, summary := range []string{"", "unsigned reasoning"} {
			for _, visible := range []string{"", "visible answer"} {
				for _, signed := range []bool{false, true} {
					t.Run(
						fmt.Sprintf(
							"%s/summary=%t/visible=%t/signed=%t",
							model,
							summary != "",
							visible != "",
							signed,
						),
						func(t *testing.T) {
							signature := ""
							if signed {
								signature = "gemini#CgQBBQYH"
							}
							input := []byte(fmt.Sprintf(
								`{"input":[{"role":"user","content":"start"},{"type":"reasoning","summa`+
									`ry":[{"type":"summary_text","text":%q}],"encrypted_content":%q},{"role`+
									`":"assistant","content":%q},{"role":"user","content":"continue"}]}`,
								summary, signature, visible,
							))
							for _, stream := range []bool{false, true} {
								out, err := sdk.TranslateRequestChecked(
									t.Context(),
									sdk.FormatOpenAIResponse,
									sdk.FormatGemini,
									model,
									input,
									stream,
								)
								if err != nil {
									t.Fatal(err)
								}
								if legacy := geminiresponses.ConvertOpenAIResponsesRequestToGemini(
									model, input, stream,
								); string(legacy) != string(out) {
									t.Fatalf(
										"byte and checked entries disagree: byte=%s checked=%s",
										legacy,
										out,
									)
								}
								var summaries, answers, signatures int
								for _, content := range gjson.GetBytes(out, "contents").Array() {
									for _, part := range content.Get("parts").Array() {
										if got := part.Get("thoughtSignature"); got.Exists() {
											signatures++
											//goland:noinspection SpellCheckingInspection
											if !signed || got.String() != "CgQBBQYH" {
												t.Fatalf("unsigned or changed model-part signature: %s", out)
											}
										}
										if text := part.Get("text").String(); text == "unsigned reasoning" {
											summaries++
										} else if text == "visible answer" {
											answers++
										}
										if !signed && content.Get("role").String() == "model" &&
											part.Get("text").String() == "" {
											t.Fatalf(
												"empty unsigned model part survived before the next user: %s",
												out,
											)
										}
									}
								}
								if (summary != "") != (summaries == 1) || (visible != "") != (answers == 1) ||
									signed && signatures != 1 {
									t.Fatalf(
										"history changed: summaries=%d answers=%d signatures=%d; output=%s",
										summaries,
										answers,
										signatures,
										out,
									)
								}
							}
						},
					)
				}
			}
		}
	}
}

func TestGeminiResponsesSDKUnsignedReasoningKeepsFunctionCallBypass(t *testing.T) {
	for _, model := range []string{"gemini-3-flash", "configured-model-alias"} {
		for _, summary := range []string{"", "unsigned reasoning"} {
			input := []byte(fmt.Sprintf(
				`{"input":[{"role":"user","content":"start"},{"type":"reasoning","summa`+
					`ry":[{"type":"summary_text","text":%q}]},{"type":"function_call","call`+
					`_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call`+
					`_output","call_id":"call-1","output":"done"},{"role":"user","content":`+
					`"continue"}]}`,
				summary,
			))
			out, err := sdk.TranslateRequestChecked(
				t.Context(), sdk.FormatOpenAIResponse, sdk.FormatGemini, model, input, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			for _, content := range gjson.GetBytes(out, "contents").Array() {
				for _, part := range content.Get("parts").Array() {
					if part.Get("functionCall").Exists() {
						calls++
						if part.Get("thoughtSignature").String() != "skip_thought_signature_validator" {
							t.Fatalf("unsigned function-call bypass changed: %s", out)
						}
					} else if part.Get("thoughtSignature").Exists() {
						t.Fatalf("unsigned non-function part claimed a signature: %s", out)
					}
				}
			}
			if calls != 1 {
				t.Fatalf("function call count=%d, want one: %s", calls, out)
			}
		}
	}
}

func TestGeminiResponsesSDKReusedToolIDsStayInCallGroup(t *testing.T) {
	for _, secondName := range []string{"lookup", "second"} {
		body := []byte(fmt.Sprintf(
			`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function",`+
				`"name":"second","parameters":{"type":"object"}}],"input":[{"role":"user","content":"first"},`+
				`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"`+
				`function_call_output","call_id":"call_1","output":"first_result"},{"role":"user","content":"`+
				`second"},{"type":"function_call","call_id":"call_1","name":%q,"arguments":"{}"},{"type":"`+
				`function_call_output","call_id":"call_1","output":"second_result"}]}`,
			secondName,
		))
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAIResponse, sdk.FormatGemini, "gemini-test", body, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		var names, results []string
		for _, content := range gjson.GetBytes(out, "contents").Array() {
			for _, part := range content.Get("parts").Array() {
				if response := part.Get("functionResponse"); response.Exists() {
					names = append(names, response.Get("name").String())
					results = append(results, response.Get("response.result").String())
				}
			}
		}
		if len(results) != 2 || results[0] != "first_result" || results[1] != "second_result" ||
			names[0] != "lookup" ||
			names[1] != secondName {
			t.Fatalf("call groups crossed: names=%v results=%v body=%s", names, results, out)
		}
	}
}

func TestGeminiResponsesSDKPreservesToolOutputStrings(t *testing.T) {
	for _, value := range []string{`"null"`, `""`, `null`} {
		body := []byte((`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input"` +
			`:[{"type":"function_call","call_id":"call-a","name":"lookup","arguments":"{}"},{"type":"` +
			`function_call_output","call_id":"call-a","output":`) + value + `}]}`)
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAIResponse, sdk.FormatGemini, "gemini-test", body, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, content := range gjson.GetBytes(out, "contents").Array() {
			for _, part := range content.Get("parts").Array() {
				if part.Get("functionResponse").Exists() {
					found = true
					result := part.Get("functionResponse.response.result")
					if value != `null` && result.Raw != value {
						t.Fatalf("string changed: want %s body=%s", value, out)
					}
					if value == `null` && result.Type == gjson.String {
						t.Fatalf("typed null became string: %s", out)
					}
				}
			}
		}
		if !found {
			t.Fatalf("response missing: %s", out)
		}
	}
}

func TestGeminiResponsesSDKCustomToolNativeCarrier(t *testing.T) {
	for _, marked := range []bool{false, true} {
		//goland:noinspection SpellCheckingInspection
		signature := "CgQBBQYH"
		if marked {
			signature = "cpa-gemini-responses-carrier-v1:next:function:" + base64.RawStdEncoding.EncodeToString(
				[]byte(signature),
			)
		}
		body := []byte(fmt.Sprintf(
			`{"tools":[{"type":"custom","name":"exec"}],"input":[{"type":"reasoning`+
				`","summary":[],"encrypted_content":%q},{"type":"custom_tool_call","nam`+
				`e":"exec","call_id":"call-a","input":"command"},{"type":"custom_tool_c`+
				`all_output","call_id":"call-a","output":"done"}]}`,
			signature,
		))
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAIResponse, sdk.FormatGemini, "gemini-test", body, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, content := range gjson.GetBytes(out, "contents").Array() {
			for _, part := range content.Get("parts").Array() {
				if part.Get("functionCall").Exists() {
					found = true
					//goland:noinspection SpellCheckingInspection
					if part.Get("thoughtSignature").String() != "CgQBBQYH" {
						t.Fatalf("custom carrier signature lost marked=%v: %s", marked, out)
					}
				}
			}
		}
		if !found {
			t.Fatalf("custom call missing: %s", out)
		}
	}
}
