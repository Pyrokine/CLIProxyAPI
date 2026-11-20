package executor

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestMetaImageResponsesKeepsNativeToolContract(t *testing.T) {
	for _, stream := range []bool{false, true} {
		request := []byte(`{"model":"muse-image-1.0","input":"synthetic","safety_identifier":"opaque-id","store":false,"tools":[{"type":"image_generation","enable_image_search":false,"enable_web_search":false,"enable_shell":false,"reasoning_strength":"low","size":"1792x1024","output_format":"webp"}]}`)
		ctx := util.WithRoundTripper(
			t.Context(), devinTestRoundTripper(
				func(r *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if r.URL.Path != "/v1/responses" || gjson.GetBytes(
						body, "model",
					).String() != "muse-image-1.0" || gjson.GetBytes(
						body, "safety_identifier",
					).String() != "opaque-id" || gjson.GetBytes(
						body, "tools.0.reasoning_strength",
					).String() != "low" || !gjson.GetBytes(
						body, "tools.0.enable_image_search",
					).Exists() || gjson.GetBytes(body, "tools.0.enable_image_search").Bool() {
						t.Fatalf("Meta Responses contract changed: %s", body)
					}
					wire := "data: " + `{"type":"response.completed","response":{"id":"resp_image","object":"response","status":"completed","model":"muse-image-1.0","output":[{"type":"image_generation_call","id":"ig_synthetic","status":"completed","result":"QUFB"}]}}` + "\n\n"
					return &http.Response{
						StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(wire)),
					}, nil
				},
			),
		)
		executor := NewMetaExecutor(&config.Config{})
		auth := &coreauth.Auth{
			Provider: "meta", Attributes: map[string]string{
				"auth_kind": "apikey", "api_key": "synthetic-meta", "base_url": "https://api.meta.ai/v1",
			},
		}
		req := core.Request{Model: "muse-image-1.0", Payload: request}
		opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Stream: stream}
		var payload []byte
		if stream {
			result, err := executor.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				payload = append(payload, chunk.Payload...)
			}
		} else {
			result, err := executor.Execute(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			payload = result.Payload
		}
		if !bytes.Contains(payload, []byte("image_generation_call")) || !bytes.Contains(payload, []byte("QUFB")) {
			t.Fatalf("Meta image Responses output lost: %s", payload)
		}
	}
}

func TestMetaTranscriptionErrorsRemainFailures(t *testing.T) {
	body, contentType := mediaSyntheticAudio(t)
	auth := &coreauth.Auth{
		Provider: "meta", Attributes: map[string]string{
			"auth_kind": "apikey", "api_key": "synthetic-meta", "base_url": "https://api.meta.ai/v1",
		},
	}
	opts := core.Options{
		SourceFormat: translator.FromString(metaAudioFormat),
		Headers:      http.Header{"Content-Type": {contentType}, "Accept": {"application/json"}},
	}
	req := core.Request{Model: "muse-voice-transcribe-1.0", Payload: body}
	for _, code := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusInternalServerError} {
		ctx := util.WithRoundTripper(
			t.Context(), devinTestRoundTripper(
				func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: code, Request: r,
						Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"5"}},
						Body:   io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic provider rejection"}}`)),
					}, nil
				},
			),
		)
		if _, err := NewMetaExecutor(&config.Config{}).Execute(
			ctx, auth, req, opts,
		); err == nil || statusErrCode(err) != code {
			t.Fatalf("status %d returned error %v", code, err)
		}
	}
	ctx := util.WithRoundTripper(
		t.Context(), devinTestRoundTripper(
			func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(
						&auditImageFinalRead{
							payload: []byte(`{"transcript":"partial"`), err: io.ErrUnexpectedEOF,
						},
					),
				}, nil
			},
		),
	)
	if _, err := NewMetaExecutor(&config.Config{}).Execute(ctx, auth, req, opts); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial transcription became success: %v", err)
	}
}
