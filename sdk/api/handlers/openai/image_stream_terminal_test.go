package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestImageStreamsRejectEventsAfterCompletion(t *testing.T) {
	for _, translated := range []bool{false, true} {
		for _, combined := range []bool{false, true} {
			name := "raw/separate chunks"
			if translated {
				name = "responses/separate chunks"
			}
			if combined {
				name = strings.ReplaceAll(name, "separate chunks", "same chunk")
			}
			t.Run(
				name, func(t *testing.T) {
					handler, ctx, recorder := newImageStreamTestHandler(t)
					completion := "data: {\"type\":\"response.completed\"}\n\n"
					late := "data: {\"type\":\"image_generation.partial\",\"value\":\"fixture-after-completion\"}\n\n"
					if translated {
						completion = "data: {\"type\":\"response.completed\",\"response\":{\"output\":" +
							"[{\"type\":\"image_generation_call\",\"result\":" +
							"\"" +
							testPNGBase64 +
							"\",\"output_format\":\"png\"}]}}\n\n"
						late = "data: {\"type\":\"response.image_generation_call.partial_image\",\"par" +
							"tial_image_b64\":\"fixture-after-completion\"}\n\n"
					}
					data := make(chan coreexecutor.StreamChunk, 3)
					var lateAcknowledgement error
					if combined {
						data <- coreexecutor.StreamChunk{Payload: []byte(completion + late)}
					} else {
						data <- coreexecutor.StreamChunk{Payload: []byte(completion)}
						data <- coreexecutor.StreamChunk{
							Payload: []byte(late), Acknowledge: func(err error) { lateAcknowledgement = err },
						}
					}
					data <- coreexecutor.StreamChunk{Terminal: true}
					close(data)
					var err error
					if translated {
						err = handler.forwardImagesStreamChunks(
							context.Background(), ctx, data, nil, "url", "image_generation",
							func(name string, payload []byte) error {
								return handlers.WriteStreamBytes(
									ctx.Writer, []byte("event: "+name+"\ndata: "), payload, []byte("\n\n"),
								)
							},
						)
					} else {
						err = handler.forwardRawImageStreamChunks(context.Background(), ctx, data, nil)
					}
					if err == nil || (!combined && lateAcknowledgement == nil) {
						t.Errorf("post-completion error/ack = %v/%v", err, lateAcknowledgement)
					}
					body := recorder.Body.String()
					if strings.Contains(body, "fixture-after-completion") || strings.Contains(
						body, "event: error",
					) || strings.Count(body, `"type":"response.completed"`) > 1 {
						t.Errorf("output after completion: %s", body)
					}
					if !recorder.Flushed {
						t.Error("completion was not flushed")
					}
				},
			)
		}
	}
}

func TestImageStreamsRecognizeEventNameAndNestedErrors(t *testing.T) {
	for _, frame := range []string{
		"event: error\ndata: {\"message\":\"fixture protocol failure\"}\n\n",
		"event: response.failed\ndata: {\"message\":\"fixture protocol failure\"}\n\n",
		"data: {\"type\":\"image_generation.progress\",\"error\":{\"message\":\"fixture protocol failure\"}}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"error\":" +
			"{\"message\":\"fixture protocol " +
			"failure\"}}}\n\n",
	} {
		t.Run(
			frame, func(t *testing.T) {
				handler, ctx, recorder := newImageStreamTestHandler(t)
				data := make(chan coreexecutor.StreamChunk, 2)
				data <- coreexecutor.StreamChunk{Payload: []byte(frame)}
				data <- coreexecutor.StreamChunk{Terminal: true}
				close(data)
				if err := handler.forwardRawImageStreamChunks(
					t.Context(), ctx, data, nil,
				); err == nil || !strings.Contains(err.Error(), "fixture protocol failure") {
					t.Fatalf("protocol failure = %v, body=%s", err, recorder.Body.String())
				}
				if strings.Count(recorder.Body.String(), "event: error") != 1 {
					t.Fatalf("terminal error response = %s", recorder.Body.String())
				}
			},
		)
	}
}
