package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestNanoBanana21ChatImagesKeepIndicesAndReplaySignatures(t *testing.T) {
	for _, channel := range []translator.Format{
		translator.FormatGemini, translator.FormatGeminiCLI, translator.FormatAntigravity,
	} {
		t.Run(
			channel.String(), func(t *testing.T) {
				var state any
				var images []json.RawMessage
				for index, signature := range []string{"EgYKBAECAwQ=", "EgYKBAEGBwg="} {
					wire := fmt.Sprintf(
						`{"responseId":"synthetic","candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/webp","data":"QUFB"},"thoughtSignature":%q}]}%s}]}`,
						signature, []string{"", `,"finishReason":"STOP"`}[index],
					)
					if channel != translator.FormatGemini {
						wire = `{"response":` + wire + `}`
					}
					chunks := translator.TranslateStream(
						t.Context(), channel, translator.FormatOpenAI, "gemini-nano-banana-2.1", nil, nil, []byte(wire),
						&state,
					)
					for _, chunk := range chunks {
						root := gjson.ParseBytes(bytes.TrimSpace(bytes.TrimPrefix(chunk, []byte("data:"))))
						for _, image := range root.Get("choices.0.delta.images").Array() {
							if image.Get("index").Int() != int64(index) || image.Get("extra_content.google.thought_signature").String() != signature {
								t.Fatalf("image index/signature lost: %s", chunk)
							}
							images = append(images, json.RawMessage(image.Raw))
						}
					}
				}
				if len(images) != 2 {
					t.Fatalf("stream images=%d", len(images))
				}
				encoded, err := json.Marshal(images)
				if err != nil {
					t.Fatal(err)
				}
				replay := []byte(fmt.Sprintf(
					`{"model":"gemini-nano-banana-2.1","messages":[{"role":"user","content":"draw"},{"role":"assistant","content":"result","images":%s},{"role":"user","content":"edit"}]}`,
					encoded,
				))
				body, err := translator.TranslateRequestChecked(
					t.Context(), translator.FormatOpenAI, channel, "gemini-nano-banana-2.1", replay, false,
				)
				if err != nil {
					t.Fatal(err)
				}
				for _, signature := range []string{"EgYKBAECAwQ=", "EgYKBAEGBwg="} {
					if !bytes.Contains(body, []byte(signature)) {
						t.Fatalf("image replay lost signature %s: %s", signature, body)
					}
				}
				root := gjson.ParseBytes(body)
				if root.Get("request").Exists() {
					root = root.Get("request")
				}
				parts := root.Get("contents.1.parts").Array()
				if len(parts) != 3 {
					t.Fatalf("assistant text and image replay: %s", body)
				}
			},
		)
	}
}
