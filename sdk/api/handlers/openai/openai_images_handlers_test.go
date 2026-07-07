package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type imageCaptureExecutor struct {
	sourceFormat string
	requestPath  string
	model        string
	payload      []byte
}

type repeatByteReader struct{}

func (repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func (e *imageCaptureExecutor) Identifier() string { return "test-openai-compat" }

func (e *imageCaptureExecutor) Execute(
	_ context.Context,
	_ *coreauth.Auth,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (coreexecutor.Response, error) {
	e.sourceFormat = opts.SourceFormat.String()
	e.model = req.Model
	e.payload = append([]byte(nil), req.Payload...)
	if raw, ok := opts.Metadata[coreexecutor.RequestPathMetadataKey].(string); ok {
		e.requestPath = raw
	}
	return coreexecutor.Response{Payload: []byte(`{"created":1,"data":[{"b64_json":"abc"}]}`)}, nil
}

func (e *imageCaptureExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *imageCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *imageCaptureExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *imageCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestImagesGenerationsRoutesConfiguredOpenAICompatImageModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: "auth-openai-compat-image", Provider: executor.Identifier(), Status: coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "alias-image", Type: registry.OpenAIImageModelType}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(
		http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"alias-image","prompt":"draw"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.sourceFormat != "openai-image" {
		t.Fatalf("source format = %q, want openai-image", executor.sourceFormat)
	}
	if executor.requestPath != "/v1/images/generations" {
		t.Fatalf("request path = %q, want /v1/images/generations", executor.requestPath)
	}
	if executor.model != "alias-image" {
		t.Fatalf("model = %q, want alias-image", executor.model)
	}
	if got := gjson.GetBytes(executor.payload, "model").String(); got != "alias-image" {
		t.Fatalf("payload model = %q, want alias-image; body=%s", got, executor.payload)
	}
	if gjson.GetBytes(executor.payload, "stream").Exists() {
		t.Fatalf("payload stream should be removed: %s", executor.payload)
	}
}

func TestCollectImagesFromResponsesStreamFallsBackToPartialImage(t *testing.T) {
	dataCh := make(chan []byte, 1)
	dataCh <- []byte("data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"abc\",\"output_format\":\"png\",\"size\":\"1024x1024\",\"quality\":\"low\",\"background\":\"opaque\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[]}}\n\n")
	close(dataCh)

	out, errMsg := collectImagesFromResponsesStream(context.Background(), dataCh, nil, "b64_json")
	if errMsg != nil {
		t.Fatalf("collectImagesFromResponsesStream error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "abc" {
		t.Fatalf("b64_json = %q, want abc; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "output_format").String(); got != "png" {
		t.Fatalf("output_format = %q, want png; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "size").String(); got != "1024x1024" {
		t.Fatalf("size = %q, want 1024x1024; body=%s", got, out)
	}
}

func TestForwardImagesStreamIgnoresEmptyCompletedAfterPartialImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	firstChunk := []byte("data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"abc\",\"output_format\":\"png\",\"partial_image_index\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[]}}\n\n")
	dataCh := make(chan []byte)
	close(dataCh)

	var events []string
	var cancelled bool
	h := &APIHandler{}
	h.forwardImagesStream(
		c,
		func(err error) {
			cancelled = true
			if err != nil {
				t.Fatalf("cancel error: %v", err)
			}
		},
		dataCh,
		nil,
		firstChunk,
		"b64_json",
		"image_generation",
		func(eventName string, dataJSON []byte) {
			events = append(events, eventName+":"+string(dataJSON))
		},
	)

	if !cancelled {
		t.Fatalf("cancel was not called")
	}
	if len(events) != 1 {
		t.Fatalf("events = %v, want one partial image event", events)
	}
	if !strings.HasPrefix(events[0], "image_generation.partial_image:") {
		t.Fatalf("event = %q, want partial image event", events[0])
	}
	_, eventPayload, _ := strings.Cut(events[0], ":")
	if got := gjson.Get(eventPayload, "b64_json").String(); got != "abc" {
		t.Fatalf("b64_json = %q, want abc; event=%s", got, events[0])
	}
}

func TestImagesEditsRejectsOversizedMultipartBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: "auth-image-edit", Provider: executor.Identifier(), Status: coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "alias-image-edit", Type: registry.OpenAIImageModelType}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatalf("WriteField model: %v", err)
	}
	if err := writer.WriteField("prompt", "edit"); err != nil {
		t.Fatalf("WriteField prompt: %v", err)
	}
	part, err := writer.CreateFormFile("image", "large.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err = io.Copy(part, io.LimitReader(repeatByteReader{}, maxImagesEditMultipartBodyBytes)); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("Close multipart writer: %v", err)
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/edits", h.ImagesEdits)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestImagesEditsRoutesConfiguredOpenAICompatImageModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: "auth-openai-compat-image-edit", Provider: executor.Identifier(), Status: coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "alias-image-edit", Type: registry.OpenAIImageModelType}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/edits", h.ImagesEdits)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/images/edits",
		strings.NewReader(`{"model":"alias-image-edit","prompt":"edit","image":"data:image/png;base64,abc"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.sourceFormat != "openai-image" {
		t.Fatalf("source format = %q, want openai-image", executor.sourceFormat)
	}
	if executor.requestPath != "/v1/images/edits" {
		t.Fatalf("request path = %q, want /v1/images/edits", executor.requestPath)
	}
	if got := gjson.GetBytes(executor.payload, "model").String(); got != "alias-image-edit" {
		t.Fatalf("payload model = %q, want alias-image-edit; body=%s", got, executor.payload)
	}
}

func TestImagesGenerationsRoutesGPTImage15ToOpenAIImageExecutor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: "auth-gpt-image-15", Provider: executor.Identifier(), Status: coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-image-1.5", Type: registry.OpenAIImageModelType}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1.5","prompt":"draw"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.sourceFormat != "openai-image" {
		t.Fatalf("source format = %q, want openai-image", executor.sourceFormat)
	}
	if executor.requestPath != "/v1/images/generations" {
		t.Fatalf("request path = %q, want /v1/images/generations", executor.requestPath)
	}
	if executor.model != "gpt-image-1.5" {
		t.Fatalf("model = %q, want gpt-image-1.5", executor.model)
	}
	if got := gjson.GetBytes(executor.payload, "model").String(); got != "gpt-image-1.5" {
		t.Fatalf("payload model = %q, want gpt-image-1.5; body=%s", got, executor.payload)
	}
}

func TestImagesEditsRoutesCodexImageModelToOpenAIImageExecutor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: "auth-codex-image-edit", Provider: executor.Identifier(), Status: coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-image-2", Type: registry.OpenAIImageModelType}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/edits", h.ImagesEdits)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/images/edits",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"edit","images":[{"file_id":"file-abc"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.sourceFormat != "openai-image" {
		t.Fatalf("source format = %q, want openai-image", executor.sourceFormat)
	}
	if executor.requestPath != "/v1/images/edits" {
		t.Fatalf("request path = %q, want /v1/images/edits", executor.requestPath)
	}
	if got := gjson.GetBytes(executor.payload, "model").String(); got != "gpt-image-2" {
		t.Fatalf("payload model = %q, want gpt-image-2; body=%s", got, executor.payload)
	}
	if got := gjson.GetBytes(executor.payload, "images.0.file_id").String(); got != "file-abc" {
		t.Fatalf("images.0.file_id = %q, want file-abc; body=%s", got, executor.payload)
	}
}
