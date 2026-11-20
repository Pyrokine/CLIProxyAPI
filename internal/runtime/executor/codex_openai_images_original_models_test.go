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
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorDirectOpenAIImage25Models(t *testing.T) {
	for _, test := range []struct {
		model  string
		direct bool
	}{
		{model: "gpt-image-2.5"},
		{model: "gpt-image-2.5-flare", direct: true},
		{model: "gpt-image-2.5-sunburst", direct: true},
		{model: "codex/gpt-image-2.5"},
		{model: "codex/gpt-image-2.5-flare", direct: true},
		{model: "codex/gpt-image-2.5-sunburst", direct: true},
		{model: "GPT-Image-2.5(medium)"},
		{model: "codex/GPT-Image-2.5-Flare(high)", direct: true},
	} {
		for _, operation := range []string{"generate", "edit", "stream"} {
			t.Run(
				operation+"/"+test.model, func(t *testing.T) {
					baseModel := codexOpenAIImageBaseModel(test.model)
					if got := codexIsDirectOpenAIImageModel(baseModel); got != test.direct {
						t.Fatalf(
							"direct model %q = %t, want %t under the registered catalog",
							baseModel,
							got,
							test.direct,
						)
					}
					path := codexImagesGenerationsPath
					wantPath := "/images/generations"
					payload := fmt.Sprintf(`{"model":%q,"prompt":"draw something"}`, test.model)
					if operation == "edit" {
						path, wantPath = codexImagesEditsPath, "/images/edits"
						payload = fmt.Sprintf(
							`{"model":%q,"prompt":"edit something","images":[{"file_id":"f1"}]}`, test.model,
						)
					} else if operation == "stream" {
						payload = fmt.Sprintf(`{"model":%q,"prompt":"stream something"}`, test.model)
					}
					opts := codexOpenAIImageTestOptions(path, operation == "stream")
					req := core.Request{Model: test.model, Payload: []byte(payload)}
					if !test.direct {
						if got := codexDirectOpenAIImageRouteModel(req, opts); got != "" {
							t.Fatalf("retired generic alias selected a direct image route: %q", got)
						}
						return
					}
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					t.Cleanup(cancel)
					calls := 0
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(r *http.Request) (*http.Response, error) {
								calls++
								body, err := io.ReadAll(r.Body)
								if err != nil {
									return nil, err
								}
								if r.URL.Path != wantPath ||
									gjson.GetBytes(body, "model").String() != baseModel {
									t.Errorf(
										"image route=%s model=%s, want %s / %s", r.URL.Path,
										gjson.GetBytes(body, "model").String(), wantPath, baseModel,
									)
								}
								contentType := "application/json"
								response := `{"created":1713833628,"data":[{"b64_json":"AA=="}],"usage":` +
									`{"total_tokens":` +
									`10}}`
								if operation == "stream" {
									contentType = "text/event-stream"
									response = "event: image_generation.completed\n" +
										`data: {"type":"image_generation.completed","b64_json":"BB=="}` + "\n\n"
								}
								return &http.Response{
									StatusCode: http.StatusOK, Request: r,
									Header: http.Header{"Content-Type": {contentType}},
									Body:   io.NopCloser(strings.NewReader(response)),
								}, nil
							},
						),
					)
					executor := newCodexExecutor(&config.Config{})
					auth := newCodexOpenAIImageTestAuth("https://original-codex-image.invalid")
					if operation == "stream" {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						var payload bytes.Buffer
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							payload.Write(chunk.Payload)
						}
						if !bytes.Contains(payload.Bytes(), []byte(`"b64_json":"BB=="`)) {
							t.Fatalf("image stream lost original output: %s", payload.Bytes())
						}
					} else {
						result, err := executor.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						if gjson.GetBytes(result.Payload, "data.0.b64_json").String() != "AA==" {
							t.Fatalf("image response lost original output: %s", result.Payload)
						}
					}
					if calls != 1 {
						t.Fatalf("image request count = %d, want 1", calls)
					}
				},
			)
		}
	}
}

func TestCodexDirectImageReadFailurePreservesObservedUsage(t *testing.T) {
	const wire = "data: " + `{"type":"image_generation.completed","b64_json":"AA==",` +
		`"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}}` + "\n\n"
	for _, readErr := range []error{io.EOF, io.ErrUnexpectedEOF} {
		t.Run(
			readErr.Error(), func(t *testing.T) {
				capture := newCodexContractUsageCapture(t)
				body := &auditImageBody{
					Reader: &auditImageFinalRead{payload: []byte(wire), err: readErr},
					closed: make(chan struct{}),
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = util.WithRoundTripper(
					ctx, devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							if request.URL.Path != "/images/generations" {
								t.Errorf("direct image path=%s", request.URL.Path)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Request: request, Body: body,
								Header: http.Header{"Content-Type": {"text/event-stream"}},
							}, nil
						},
					),
				)
				auth := newCodexOpenAIImageTestAuth("https://original-codex-image.invalid")
				auth.ID = capture.authID
				result, err := newCodexExecutor(&config.Config{}).ExecuteStream(
					ctx, auth, core.Request{
						Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"synthetic"}`),
					}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, true),
				)
				if err != nil {
					t.Fatal(err)
				}
				var payload bytes.Buffer
				var streamErr error
				for chunk := range result.Chunks {
					payload.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				if payload.String() != wire || (readErr == io.EOF && streamErr != nil) ||
					(readErr != io.EOF && !errors.Is(streamErr, readErr)) {
					t.Fatalf("payload=%q error=%v read-error=%v", payload.String(), streamErr, readErr)
				}
				record := capture.next(t)
				if record.Model != "gpt-image-1.5" || record.Failed != (readErr != io.EOF) ||
					record.Detail.InputTokens !=
						7 ||
					record.Detail.OutputTokens !=
						5 ||
					record.Detail.TotalTokens !=
						12 {
					t.Fatalf("image usage did not retain observed tokens and outcome: %+v", record)
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("direct image stream left response body open")
				}
			},
		)
	}
}
