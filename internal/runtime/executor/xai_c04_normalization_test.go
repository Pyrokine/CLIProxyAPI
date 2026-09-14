package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestNormalizeXAIObjectFunctionSchemaRootUnions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		schema     string
		union      string
		wantBranch string
	}{
		{
			name:       "anyOf keeps the object branch and removes nullable branch",
			schema:     `{"anyOf":[{"properties":{"query":{"type":"string"}},"required":["query"]},{"type":"null"}]}`,
			union:      "anyOf",
			wantBranch: "query",
		},
		{
			name:       "oneOf narrows an object nullable type",
			schema:     `{"oneOf":[{"type":["object","null"],"properties":{"path":{"type":"string"}}},{"type":"string"}]}`,
			union:      "oneOf",
			wantBranch: "path",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				normalized, changed, err := normalizeXAIObjectFunctionSchema([]byte(tt.schema))
				if err != nil {
					t.Fatalf("normalizeXAIObjectFunctionSchema() error = %v", err)
				}
				if !changed {
					t.Fatal("normalizeXAIObjectFunctionSchema() changed = false")
				}
				if got := gjson.GetBytes(normalized, "type").String(); got != "object" {
					t.Fatalf("root type = %q, want object; schema=%s", got, normalized)
				}
				if got := gjson.GetBytes(normalized, tt.union+".#").Int(); got != 1 {
					t.Fatalf("%s branch count = %d, want 1; schema=%s", tt.union, got, normalized)
				}
				if got := gjson.GetBytes(normalized, tt.union+".0.type").String(); got != "object" {
					t.Fatalf("%s.0.type = %q, want object; schema=%s", tt.union, got, normalized)
				}
				if !gjson.GetBytes(normalized, tt.union+".0.properties."+tt.wantBranch).Exists() {
					t.Fatalf("object branch property %q missing; schema=%s", tt.wantBranch, normalized)
				}
			},
		)
	}
}

func TestNormalizeXAIObjectFunctionSchemaRejectsInvalidRoots(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		schema string
		want   string
	}{
		{name: "non-object root", schema: `{"type":"string"}`, want: "no valid object type"},
		{
			name: "anyOf without object branch", schema: `{"anyOf":[{"type":"string"},{"type":"null"}]}`,
			want: "anyOf has no valid object branch",
		},
		{
			name: "oneOf without object branch", schema: `{"oneOf":[{"type":"number"},false]}`,
			want: "oneOf has no valid object branch",
		},
		{name: "empty union", schema: `{"type":"object","anyOf":[]}`, want: "anyOf must be a non-empty array"},
	} {
		t.Run(
			tt.name, func(t *testing.T) {
				_, _, err := normalizeXAIObjectFunctionSchema([]byte(tt.schema))
				if err == nil {
					t.Fatal("normalizeXAIObjectFunctionSchema() error = nil")
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %q, want substring %q", err, tt.want)
				}
			},
		)
	}
}

func TestXAIRequestPreparationPreservesControlsAndPathParity(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"grok-4.3",
		"messages":[{"role":"user","content":"hello"}],
		"max_completion_tokens":64,
		"max_tokens":128,
		"temperature":0,
		"top_p":0.25,
		"top_k":7,
		"stop":["END"],
		"response_format":{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}}}}},
		"text":{"verbosity":"low"},
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"oneOf":[{"properties":{"query":{"type":"string"}}},{"type":"null"}]}}}]
	}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}

	httpPrepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(req, opts, true)
	if err != nil {
		t.Fatalf("HTTP prepare error = %v", err)
	}
	wsPrepared, err := newXAIWebsocketsExecutor(&config.Config{}).prepareResponsesWebsocketRequest(
		context.Background(), req, opts,
	)
	if err != nil {
		t.Fatalf("WebSocket prepare error = %v", err)
	}
	if !bytes.Equal(httpPrepared.body, wsPrepared.body) {
		t.Fatalf("HTTP/WS prepared bodies differ:\nHTTP=%s\nWS=%s", httpPrepared.body, wsPrepared.body)
	}

	for path, want := range map[string]string{
		"max_output_tokens":               "64",
		"temperature":                     "0",
		"top_p":                           "0.25",
		"top_k":                           "7",
		"text.verbosity":                  "low",
		"text.format.type":                "json_schema",
		"tools.0.parameters.type":         "object",
		"tools.0.parameters.oneOf.0.type": "object",
	} {
		if got := gjson.GetBytes(httpPrepared.body, path).Raw; got != want && gjson.GetBytes(
			httpPrepared.body, path,
		).String() != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, httpPrepared.body)
		}
	}
	if gjson.GetBytes(httpPrepared.body, "stop").Exists() {
		t.Fatalf("stop should be removed; body=%s", httpPrepared.body)
	}
	if gjson.GetBytes(httpPrepared.body, "verbosity").Exists() {
		t.Fatalf("root verbosity should be removed; body=%s", httpPrepared.body)
	}
}

func TestXAIResponsesPreparationNormalizesDeepSpecialKeyImages(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"grok-4.3",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[{"type":"input_text","text":"hello"}],
			"metadata.with.dot":{"back\\slash":{"":{"image":{"image_url":"https://example.com/deep.png"}}}}
		}]
	}`)
	prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(
		cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		true,
	)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(prepared.body, &body); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	input := body["input"].([]any)[0].(map[string]any)
	image := input["metadata.with.dot"].(map[string]any)["back\\slash"].(map[string]any)[""].(map[string]any)["image"].(map[string]any)
	if got := image["url"]; got != "https://example.com/deep.png" {
		t.Fatalf("deep image url = %#v", got)
	}
	if _, exists := image["image_url"]; exists {
		t.Fatalf("deep image_url was not removed: %#v", image)
	}
}

func TestXAIRequestPreparationRejectsInvalidSchemaAcrossHTTPAndWebSocket(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"grok-4.3","input":"hello","tools":[{"type":"function","name":"bad","parameters":{"anyOf":[{"type":"string"},{"type":"null"}]}}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}

	_, httpErr := newXAIExecutor(&config.Config{}).prepareResponsesRequest(req, opts, true)
	_, wsErr := newXAIWebsocketsExecutor(&config.Config{}).prepareResponsesWebsocketRequest(
		context.Background(), req, opts,
	)
	for path, err := range map[string]error{"HTTP": httpErr, "WebSocket": wsErr} {
		if err == nil {
			t.Fatalf("%s prepare error = nil", path)
		}
		status, ok := err.(interface{ StatusCode() int })
		if !ok || status.StatusCode() != http.StatusBadRequest {
			t.Fatalf("%s prepare error = %T %v, want status 400", path, err, err)
		}
		if !strings.Contains(err.Error(), "no valid object branch") {
			t.Fatalf("%s prepare error = %q", path, err)
		}
	}
}

func TestNormalizeXAIImageRefsDeepSpecialKeys(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"metadata.with.dot":{"back\\slash":{"":{"items":[{"image":{"image_url":"https://example.com/a.png"}}]}}},
		"images":[{"image_url":{"url":"https://example.com/b.png"}}],
		"content":[{"type":"image_url","image_url":{"url":"https://example.com/keep.png"}}]
	}`)
	normalized := normalizeXAIImageRefs(input)
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	deep := payload["metadata.with.dot"].(map[string]any)["back\\slash"].(map[string]any)[""].(map[string]any)["items"].([]any)[0].(map[string]any)["image"].(map[string]any)
	if got := deep["url"]; got != "https://example.com/a.png" {
		t.Fatalf("deep image url = %#v", got)
	}
	if _, exists := deep["image_url"]; exists {
		t.Fatalf("deep image_url was not removed: %#v", deep)
	}
	content := payload["content"].([]any)[0].(map[string]any)
	if _, exists := content["url"]; exists {
		t.Fatalf("chat content image_url was rewritten: %#v", content)
	}
}

func TestXAIMediaPathsShareImageNormalization(t *testing.T) {
	t.Parallel()

	var bodies [][]byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				bodies = append(bodies, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[]}`))
			},
		),
	)
	defer server.Close()

	exec := newXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": server.URL}}
	payload := []byte(`{"nested.with.dot":{"image":{"image_url":"https://example.com/media.png"}}}`)
	requests := []struct {
		format sdktranslator.Format
		path   string
	}{
		{format: sdktranslator.FromString(xaiImageHandlerType), path: "/v1/images/edits"},
		{format: sdktranslator.FromString(xaiVideoHandlerType), path: "/v1/videos/edits"},
	}
	for _, request := range requests {
		_, err := exec.Execute(
			context.Background(), auth, cliproxyexecutor.Request{Model: "grok-media", Payload: payload},
			cliproxyexecutor.Options{
				SourceFormat: request.format,
				Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: request.path},
			},
		)
		if err != nil {
			t.Fatalf("Execute(%s) error = %v", request.path, err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("captured body count = %d, want 2", len(bodies))
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("image/video normalized bodies differ:\nimage=%s\nvideo=%s", bodies[0], bodies[1])
	}
	var body map[string]any
	if err := json.Unmarshal(bodies[0], &body); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	image := body["nested.with.dot"].(map[string]any)["image"].(map[string]any)
	if got := image["url"]; got != "https://example.com/media.png" {
		t.Fatalf("media image url = %#v", got)
	}
	if _, exists := image["image_url"]; exists {
		t.Fatalf("media image_url was not removed: %#v", image)
	}
}
