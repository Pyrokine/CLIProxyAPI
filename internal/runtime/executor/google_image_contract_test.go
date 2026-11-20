package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const googleMediaImageResponse = `{"responseId":"nano-output","modelVersion":"gemini-nano-banana-2.1","candidates":[{"content":{"role":"model","parts":[{"text":"synthetic image"},{"inlineData":{"mimeType":"image/webp","data":"QUFB"},"thoughtSignature":"synthetic-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}`

func TestNanoBanana21ExistingGoogleExecutorChannels(t *testing.T) {
	for _, channel := range []string{"gemini", "vertex", "gemini-cli", "antigravity"} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", channel, stream), func(t *testing.T) {
					cfg := &config.Config{}
					var executor coreauth.ProviderExecutor
					switch channel {
					case "gemini":
						executor = NewGeminiExecutor(cfg)
					case "vertex":
						executor = NewGeminiVertexExecutor(cfg)
					case "gemini-cli":
						executor = NewGeminiCLIExecutor(cfg)
					case "antigravity":
						executor = NewAntigravityExecutor(cfg)
					}
					auth := &coreauth.Auth{
						ID: t.Name(), Provider: channel, Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://synthetic-google.invalid",
						}, Metadata: map[string]any{
							"access_token": "synthetic-access", "token_type": "Bearer",
							"project_id": "synthetic-project", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
							"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
						},
					}
					request := []byte(`{"model":"gemini-nano-banana-2.1","input":[{"role":"user","content":[{"type":"input_text","text":"edit synthetic"},{"type":"input_image","image_url":"data:image/png;base64,QkJC"}]}],"generationConfig":{"imageConfig":{"imageSize":"4K","aspectRatio":"16:9"},"responseModalities":["TEXT","IMAGE"]},"reasoning":{"effort":"medium"},"max_output_tokens":4096}`)
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(r *http.Request) (*http.Response, error) {
								calls++
								body, err := io.ReadAll(r.Body)
								if err != nil {
									return nil, err
								}
								root := gjson.ParseBytes(body)
								if root.Get("request").Exists() {
									root = root.Get("request")
								}
								if root.Get("generationConfig.imageConfig.imageSize").String() != "4K" || root.Get("generationConfig.imageConfig.aspectRatio").String() != "16:9" || root.Get("generationConfig.responseModalities.1").String() != "IMAGE" || !bytes.Contains(
									body, []byte("QkJC"),
								) {
									t.Fatalf("%s lost request media: %s", channel, body)
								}
								if channel == "antigravity" && gjson.GetBytes(
									body, "requestType",
								).String() != "image_gen" {
									t.Fatalf("Nano Banana classified as text: %s", body)
								}
								wire := googleMediaImageResponse
								if channel == "antigravity" || channel == "gemini-cli" {
									wire = `{"response":` + wire + `}`
								}
								mime := "application/json"
								if stream {
									mime = "text/event-stream"
									wire = "data: " + wire + "\n\n"
								}
								return &http.Response{
									StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {mime}},
									Body: io.NopCloser(strings.NewReader(wire)),
								}, nil
							},
						),
					)
					req := core.Request{Model: "gemini-nano-banana-2.1", Payload: request}
					opts := core.Options{
						SourceFormat: translator.FormatOpenAIResponse, OriginalRequest: request, Stream: stream,
					}
					var output []byte
					if stream {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							output = append(output, chunk.Payload...)
						}
					} else {
						result, err := executor.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						output = result.Payload
					}
					if calls != 1 || !bytes.Contains(
						output, []byte(`"image_generation_call"`),
					) || !bytes.Contains(output, []byte("QUFB")) || !bytes.Contains(
						output, []byte("synthetic-signature"),
					) {
						t.Fatalf("%s media output lost: %s (calls=%d)", channel, output, calls)
					}
				},
			)
		}
	}
}

func TestNanoBanana21AIStudioRelayPayloadAndOutput(t *testing.T) {
	request := []byte(`{"model":"gemini-nano-banana-2.1","input":"draw synthetic","generationConfig":{"imageConfig":{"imageSize":"4K","aspectRatio":"16:9"},"responseModalities":["TEXT","IMAGE"]},"max_output_tokens":4096}`)
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	for _, stream := range []bool{false, true} {
		prepared, err := executor.prepareRequest(
			t.Context(), &coreauth.Auth{ID: "synthetic-relay", Provider: "aistudio"},
			core.Request{Model: "gemini-nano-banana-2.1", Payload: request},
			core.Options{SourceFormat: translator.FormatOpenAIResponse, OriginalRequest: request}, stream,
		)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(
			prepared.wsReq.Body, "generationConfig.imageConfig.imageSize",
		).String() != "4K" || !strings.Contains(prepared.wsReq.URL, "gemini-nano-banana-2.1:") {
			t.Fatalf("relay request: %+v", prepared.wsReq)
		}
		var state any
		output := translator.TranslateNonStream(
			t.Context(), prepared.body.toFormat, translator.FormatOpenAIResponse, "gemini-nano-banana-2.1", request,
			prepared.translatedReq, []byte(googleMediaImageResponse), &state,
		)
		if !bytes.Contains(output, []byte("QUFB")) || !bytes.Contains(output, []byte("image_generation_call")) {
			t.Fatalf("relay response lost image: %s", output)
		}
	}
}
