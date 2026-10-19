package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const googleInteractionSnapshot = `{"id":"contract-complete","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}],"usage":{"total_input_tokens":2,"total_output_tokens":3,"total_tokens":5}}`

func googleInteractionFrame() string {
	return "event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":" + googleInteractionSnapshot + "}\n\n"
}

type googleInteractionHooks struct{ calls int }

func (h *googleInteractionHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	h.calls++
	out, err := sjson.SetBytes(body, "translation_call", h.calls)
	if err != nil {
		panic(err)
	}
	return out
}
func (*googleInteractionHooks) TranslateRequest(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, bool) ([]byte, bool) {
	return nil, false
}
func (*googleInteractionHooks) NormalizeResponseBefore(_ context.Context, _, _ sdktranslator.Format, _ string, _, _, body []byte, _ bool) []byte {
	return body
}
func (*googleInteractionHooks) TranslateResponse(context.Context, sdktranslator.Format, sdktranslator.Format, string, []byte, []byte, []byte, bool) ([]byte, bool) {
	return nil, false
}
func (*googleInteractionHooks) NormalizeResponseAfter(_ context.Context, _, _ sdktranslator.Format, _ string, _, _, body []byte, _ bool) []byte {
	return body
}

func TestGeminiInteractionsTranslationPair(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, originalKind := range []string{"nil", "same", "equal-copy", "different"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, originalKind), func(t *testing.T) {
				hooks := &googleInteractionHooks{}
				ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
				calls := 0
				var sent []byte
				ctx = util.WithRoundTripper(ctx, googlePayloadTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if !strings.HasSuffix(req.URL.Path, "/interactions") {
						return nil, fmt.Errorf("unexpected URL %s", req.URL)
					}
					var err error
					sent, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					response := googleInteractionSnapshot
					if stream {
						response = googleInteractionFrame()
					}
					return googlePayloadResponse(req, response), nil
				}))
				payload := []byte(`{"messages":[{"role":"user","content":"working"}]}`)
				before := bytes.Clone(payload)
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatInteractions}
				wantHooks := 1
				switch originalKind {
				case "same":
					opts.OriginalRequest = payload
				case "equal-copy":
					opts.OriginalRequest = bytes.Clone(payload)
				case "different":
					opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"original"}]}`)
					wantHooks = 2
				}
				// The source baseline must retain the first translation result, including its hook output.
				cfg := &config.Config{Payload: config.PayloadConfig{Default: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "interactions"}}, Params: map[string]any{"translation_call": 99}}}}}
				exec := NewGeminiInteractionsExecutor(cfg)
				req := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: payload}
				var err error
				if stream {
					var result *cliproxyexecutor.StreamResult
					result, err = exec.ExecuteStream(ctx, googlePayloadAuth("gemini-interactions"), req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = exec.Execute(ctx, googlePayloadAuth("gemini-interactions"), req, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				if hooks.calls != wantHooks || calls != 1 || gjson.GetBytes(sent, "translation_call").Int() != int64(wantHooks) {
					t.Fatalf("hook=%d HTTP=%d body=%s", hooks.calls, calls, sent)
				}
				if !bytes.Equal(before, payload) || bytes.Contains(sent, []byte(`"original"`)) || !bytes.Contains(sent, []byte("working")) {
					t.Fatalf("working payload changed: %s", sent)
				}
			})
		}
	}
}

type googleInteractionBrokenBody struct {
	io.Reader
	err error
}

func (body googleInteractionBrokenBody) Read(p []byte) (int, error) {
	n, err := body.Reader.Read(p)
	if err == io.EOF {
		return n, body.err
	}
	return n, err
}
func (googleInteractionBrokenBody) Close() error { return nil }

func TestGeminiInteractionsCompletedEOF(t *testing.T) {
	for _, provider := range []string{"gemini-interactions", "vertex"} {
		for _, tc := range []struct {
			name, wire string
			readError  error
			wantError  bool
		}{
			{name: "complete", wire: googleInteractionFrame()},
			{name: "complete without separator", wire: strings.TrimRight(googleInteractionFrame(), "\n")},
			{name: "delta only", wire: "data: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"text\":\"partial\"}}\n\n", wantError: true},
			{name: "malformed completed", wire: "event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"x\",\"status\":\"completed\"}\n\n", wantError: true},
			{name: "missing snapshot", wire: "data: {\"event_type\":\"interaction.completed\"}\n\n", wantError: true},
			{name: "incomplete snapshot", wire: "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"x\",\"status\":\"in_progress\",\"steps\":[]}}\n\n", wantError: true},
			{name: "transport error", wire: googleInteractionFrame(), readError: io.ErrUnexpectedEOF, wantError: true},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				calls, closes := 0, 0
				ctx := util.WithRoundTripper(t.Context(), googlePayloadTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					resp := googlePayloadResponse(req, tc.wire)
					var reader io.Reader = strings.NewReader(tc.wire)
					if tc.readError != nil {
						reader = googleInteractionBrokenBody{Reader: reader, err: tc.readError}
					}
					resp.Body = &googleInteractionClosingBody{Reader: reader, closed: &closes}
					return resp, nil
				}))
				var exec cliproxyauth.ProviderExecutor = NewGeminiInteractionsExecutor(&config.Config{})
				if provider == "vertex" {
					exec = NewGeminiVertexExecutor(&config.Config{})
				}
				stream, err := exec.ExecuteStream(ctx, googlePayloadAuth(provider), cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"input":"hi"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions})
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				for chunk := range stream.Chunks {
					output.Write(chunk.Payload)
					if chunk.Err != nil {
						err = chunk.Err
					}
				}
				if (err != nil) != tc.wantError || calls != 1 || closes != 1 {
					t.Fatalf("err=%v wantError=%t HTTP=%d close=%d", err, tc.wantError, calls, closes)
				}
				if !tc.wantError && (!bytes.Contains(output.Bytes(), []byte("contract-complete")) || bytes.Contains(output.Bytes(), []byte("[DONE]"))) {
					t.Fatalf("complete snapshot changed: %s", output.Bytes())
				}
				if tc.readError != nil && !errors.Is(err, tc.readError) {
					t.Fatalf("read error lost: %v", err)
				}
			})
		}
	}
}

type googleInteractionClosingBody struct {
	io.Reader
	closed *int
}

func (body *googleInteractionClosingBody) Close() error { *body.closed++; return nil }

func TestGeminiInteractionsRejectsUnsupportedContent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			ctx := util.WithRoundTripper(t.Context(), googlePayloadTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected HTTP request")
			}))
			req := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"not-stored"}}]}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatInteractions}
			var err error
			if stream {
				_, err = NewGeminiInteractionsExecutor(&config.Config{}).ExecuteStream(ctx, googlePayloadAuth("gemini-interactions"), req, opts)
			} else {
				_, err = NewGeminiInteractionsExecutor(&config.Config{}).Execute(ctx, googlePayloadAuth("gemini-interactions"), req, opts)
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusBadRequest || calls != 0 {
				t.Fatalf("error=%v HTTP=%d", err, calls)
			}
		})
	}
}
