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
