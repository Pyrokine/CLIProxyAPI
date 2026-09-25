package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	codexresponses "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/codex/openai/responses"
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
	serviceTier  string
	model        string
	payload      []byte
}

type xaiImageCaptureExecutor struct {
	*imageCaptureExecutor
}

func (e *xaiImageCaptureExecutor) Identifier() string { return "xai" }

type repeatByteReader struct{}

const testPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

func (repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func setTestImageArtifactDeps(t *testing.T, base *handlers.BaseAPIHandler) *testImageArtifactStore {
	t.Helper()
	store := &testImageArtifactStore{}
	base.SetImageArtifactStore(store)
	base.SetImageArtifactURLBuilder(testImageArtifactURLBuilder{})
	return store
}

type testImageArtifactStore struct {
	records []handlers.ImageArtifactRecord
}

func (s *testImageArtifactStore) PutBytes(
	_ context.Context,
	data []byte,
	mimeType string,
) (handlers.ImageArtifactRecord, error) {
	if s == nil {
		return handlers.ImageArtifactRecord{}, errors.New("image artifact store is nil")
	}
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	ext := ".png"
	if mimeType == "" {
		mimeType = "image/png"
	}
	switch mimeType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	default:
		return handlers.ImageArtifactRecord{}, errors.New("unsupported test image MIME")
	}
	hashBytes := sha256.Sum256(data)
	hash := hex.EncodeToString(hashBytes[:])
	record := handlers.ImageArtifactRecord{
		ID:       "img_" + hash + ext,
		SHA256:   hash,
		MIMEType: mimeType,
		Size:     int64(len(data)),
	}
	s.records = append(s.records, record)
	return record, nil
}

func (s *testImageArtifactStore) PutDataURL(ctx context.Context, raw string) (handlers.ImageArtifactRecord, error) {
	header, encoded, ok := strings.Cut(strings.TrimSpace(raw), ",")
	if !ok {
		return handlers.ImageArtifactRecord{}, errors.New("invalid test data URL")
	}
	mediaType := strings.TrimPrefix(strings.Split(header, ";")[0], "data:")
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return handlers.ImageArtifactRecord{}, err
	}
	return s.PutBytes(ctx, data, mediaType)
}

func (s *testImageArtifactStore) PutRemoteImage(ctx context.Context, _ string) (
	handlers.ImageArtifactRecord,
	[]byte,
	error,
) {
	data, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		return handlers.ImageArtifactRecord{}, nil, err
	}
	record, err := s.PutBytes(ctx, data, "image/png")
	return record, data, err
}

type testImageArtifactURLBuilder struct{}

func (testImageArtifactURLBuilder) ImageArtifactContentURL(
	_ *gin.Context,
	record handlers.ImageArtifactRecord,
) (string, error) {
	return "https://cpa.test/v1/artifacts/" + record.ID + "/content", nil
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
	if raw, ok := opts.Metadata[coreexecutor.ServiceTierMetadataKey].(string); ok {
		e.serviceTier = raw
	}
	return coreexecutor.Response{Payload: []byte(`{"created":1,"data":[{"b64_json":"` + testPNGBase64 + `"}]}`)}, nil
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
	setTestImageArtifactDeps(t, base)
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

func TestCodexImagesToolModelClassification(t *testing.T) {
	for _, model := range []string{
		"gpt-image-1.5",
		"gpt-image-2",
		"GPT-IMAGE-2.5-FLARE",
		"codex/gpt-image-2.5-flare-2026-09-08",
		"openai/gpt-image-2.5-sunburst",
		"gpt-image-2.5-sunburst-2026-09-08",
	} {
		if !isCodexImagesToolModel(model) {
			t.Errorf("isCodexImagesToolModel(%q) = false, want true", model)
		}
	}
	if isCodexImagesToolModel("gpt-image-2.5") {
		t.Fatal("plain gpt-image-2.5 alias must remain unsupported")
	}
}

func TestImagesEndpointsNormalizeBuiltinModelRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: "auth-builtin-image-normalization", Provider: executor.Identifier(), Status: coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{
			{ID: "gpt-image-2", Type: registry.OpenAIImageModelType},
			{ID: "gpt-image-2.5-flare", Type: registry.OpenAIImageModelType},
			{ID: "gpt-image-2.5-flare-2026-09-08", Type: registry.OpenAIImageModelType},
			{ID: "gpt-image-2.5-sunburst", Type: registry.OpenAIImageModelType},
			{ID: "gpt-image-2.5-sunburst-2026-09-08", Type: registry.OpenAIImageModelType},
			{ID: "grok-imagine-image-quality", Type: registry.OpenAIImageModelType},
			{ID: "grok-imagine-image-2.0", Type: registry.OpenAIImageModelType},
		},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)
	router.POST("/v1/images/edits", h.ImagesEdits)

	tests := []struct {
		name        string
		model       string
		wantModel   string
		wantQuality string
		request     func(t *testing.T) *http.Request
	}{
		{
			name:      "generations provider prefix case and suffix",
			model:     "Codex/GPT-IMAGE-2(high)",
			wantModel: "gpt-image-2",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/generations",
					strings.NewReader(`{"model":"Codex/GPT-IMAGE-2(high)","prompt":"draw"}`),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:        "GPT Image 2.5 Flare generations",
			model:       "OPENAI/GPT-IMAGE-2.5-FLARE",
			wantModel:   "gpt-image-2.5-flare",
			wantQuality: "xhigh",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/generations",
					strings.NewReader(
						`{"model":"OPENAI/GPT-IMAGE-2.5-FLARE","prompt":"draw","quality":"xhigh"}`,
					),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:        "GPT Image 2.5 Sunburst snapshot generations",
			model:       "Codex/GPT-IMAGE-2.5-SUNBURST-2026-09-08",
			wantModel:   "gpt-image-2.5-sunburst-2026-09-08",
			wantQuality: "auto",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/generations",
					strings.NewReader(
						`{"model":"Codex/GPT-IMAGE-2.5-SUNBURST-2026-09-08","prompt":"draw","quality":"auto"}`,
					),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:      "xai quality generations provider prefix case and suffix",
			model:     "XAI/Grok-Imagine-Image-Quality(HIGH)",
			wantModel: "grok-imagine-image-quality",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/generations",
					strings.NewReader(`{"model":"XAI/Grok-Imagine-Image-Quality(HIGH)","prompt":"draw"}`),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:      "xai image 2 generations preserve canonical ID",
			model:     "XAI/Grok-Imagine-Image-2.0",
			wantModel: "grok-imagine-image-2.0",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/generations",
					strings.NewReader(`{"model":"XAI/Grok-Imagine-Image-2.0","prompt":"draw"}`),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:      "xai image 2 JSON edits preserve canonical ID",
			model:     "XAI/Grok-Imagine-Image-2.0",
			wantModel: "grok-imagine-image-2.0",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/edits",
					strings.NewReader(
						`{"model":"XAI/Grok-Imagine-Image-2.0","prompt":"edit","images":[{"image_url":"data:image/png;base64,iVBORw0KGgo="}]}`,
					),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:      "xai image 2 multipart edits preserve canonical ID",
			model:     "grok-imagine-image-2.0",
			wantModel: "grok-imagine-image-2.0",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				body := &bytes.Buffer{}
				writer := multipart.NewWriter(body)
				if err := writer.WriteField("model", "grok-imagine-image-2.0"); err != nil {
					t.Fatalf("WriteField model: %v", err)
				}
				if err := writer.WriteField("prompt", "edit"); err != nil {
					t.Fatalf("WriteField prompt: %v", err)
				}
				part, err := writer.CreateFormFile("image", "input.png")
				if err != nil {
					t.Fatalf("CreateFormFile: %v", err)
				}
				image, err := base64.StdEncoding.DecodeString(testPNGBase64)
				if err != nil {
					t.Fatalf("DecodeString: %v", err)
				}
				if _, err = part.Write(image); err != nil {
					t.Fatalf("write multipart image: %v", err)
				}
				if err = writer.Close(); err != nil {
					t.Fatalf("Close multipart writer: %v", err)
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
				req.Header.Set("Content-Type", writer.FormDataContentType())
				return req
			},
		},
		{
			name:        "GPT Image 2.5 Sunburst JSON edits",
			model:       "gpt-image-2.5-sunburst",
			wantModel:   "gpt-image-2.5-sunburst",
			wantQuality: "max",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/edits",
					strings.NewReader(
						`{"model":"gpt-image-2.5-sunburst","prompt":"edit","quality":"max","images":[{"file_id":"file-abc"}]}`,
					),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:      "JSON edits provider prefix case and suffix",
			model:     "CODEX/GPT-IMAGE-2(HIGH)",
			wantModel: "gpt-image-2",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				req := httptest.NewRequest(
					http.MethodPost,
					"/v1/images/edits",
					strings.NewReader(
						`{"model":"CODEX/GPT-IMAGE-2(HIGH)","prompt":"edit","images":[{"file_id":"file-abc"}]}`,
					),
				)
				req.Header.Set("Content-Type", "application/json")
				return req
			},
		},
		{
			name:        "GPT Image 2.5 Flare snapshot multipart edits",
			model:       "Codex/GPT-IMAGE-2.5-FLARE-2026-09-08",
			wantModel:   "gpt-image-2.5-flare-2026-09-08",
			wantQuality: "low",
			request: func(t *testing.T) *http.Request {
				t.Helper()
				body := &bytes.Buffer{}
				writer := multipart.NewWriter(body)
				if err := writer.WriteField("model", "Codex/GPT-IMAGE-2.5-FLARE-2026-09-08"); err != nil {
					t.Fatalf("WriteField model: %v", err)
				}
				if err := writer.WriteField("prompt", "edit"); err != nil {
					t.Fatalf("WriteField prompt: %v", err)
				}
				if err := writer.WriteField("quality", "low"); err != nil {
					t.Fatalf("WriteField quality: %v", err)
				}
				part, err := writer.CreateFormFile("image", "input.png")
				if err != nil {
					t.Fatalf("CreateFormFile: %v", err)
				}
				image, err := base64.StdEncoding.DecodeString(testPNGBase64)
				if err != nil {
					t.Fatalf("DecodeString: %v", err)
				}
				if _, err = part.Write(image); err != nil {
					t.Fatalf("write multipart image: %v", err)
				}
				if err = writer.Close(); err != nil {
					t.Fatalf("Close multipart writer: %v", err)
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
				req.Header.Set("Content-Type", writer.FormDataContentType())
				return req
			},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				executor.model = ""
				executor.payload = nil
				resp := httptest.NewRecorder()
				router.ServeHTTP(resp, tt.request(t))

				if resp.Code != http.StatusOK {
					t.Fatalf(
						"model %q status = %d, want %d: %s", tt.model, resp.Code, http.StatusOK, resp.Body.String(),
					)
				}
				if executor.model != tt.wantModel {
					t.Fatalf("executor model = %q, want %q; payload=%s", executor.model, tt.wantModel, executor.payload)
				}
				if tt.wantQuality != "" {
					quality := gjson.GetBytes(executor.payload, "quality").String()
					multipartValue := []byte("\r\n" + tt.wantQuality + "\r\n")
					if quality != tt.wantQuality && !bytes.Contains(executor.payload, multipartValue) {
						t.Fatalf("quality = %q, want %q; payload=%s", quality, tt.wantQuality, executor.payload)
					}
				}
			},
		)
	}
}

func TestImagesGenerationsPreservesCustomXAIPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	capture := &imageCaptureExecutor{}
	executor := &xaiImageCaptureExecutor{imageCaptureExecutor: capture}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-prefixed-xai-image",
		Provider: executor.Identifier(),
		Prefix:   "TeamA",
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{{ID: "TeamA/grok-imagine-image-quality"}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/images/generations",
		strings.NewReader(`{"model":"teama/GROK-IMAGINE-IMAGE-QUALITY(HIGH)","prompt":"draw"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if capture.model != "grok-imagine-image-quality" {
		t.Fatalf("executor model = %q, want bare XAI model", capture.model)
	}
	if got := gjson.GetBytes(capture.payload, "model").String(); got != "grok-imagine-image-quality" {
		t.Fatalf("payload model = %q, want canonical XAI model; payload=%s", got, capture.payload)
	}
}

func TestImagesGenerationsChecksCustomXAIPrefixAccessRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	capture := &imageCaptureExecutor{}
	executor := &xaiImageCaptureExecutor{imageCaptureExecutor: capture}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-blocked-prefixed-xai-image",
		Provider: executor.Identifier(),
		Prefix:   "TeamA",
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{{ID: "TeamA/grok-imagine-image-2.0"}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{
			APIKeyRules: map[string]internalconfig.APIKeyRule{
				"sk-test": {BlockedModels: []string{"TeamA/grok-imagine-image-2.0"}},
			},
		},
		manager,
	)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)
	router := gin.New()
	router.Use(
		func(c *gin.Context) {
			c.Set("apiKey", "sk-test")
			c.Next()
		},
	)
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/images/generations",
		strings.NewReader(`{"model":"teama/GROK-IMAGINE-IMAGE-2.0","prompt":"draw"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusForbidden, resp.Body.String())
	}
	if capture.model != "" {
		t.Fatalf("blocked request reached executor with model %q", capture.model)
	}
}

func TestCollectImagesFromResponsesStreamFallsBackToPartialImage(t *testing.T) {
	dataCh := make(chan []byte, 1)
	dataCh <- []byte("data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"" + testPNGBase64 + "\",\"output_format\":\"png\",\"size\":\"1024x1024\",\"quality\":\"low\",\"background\":\"opaque\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[]}}\n\n")
	close(dataCh)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)

	out, errMsg := h.collectImagesFromResponsesStream(context.Background(), c, dataCh, nil, "b64_json")
	if errMsg != nil {
		t.Fatalf("collectImagesFromResponsesStream error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != testPNGBase64 {
		t.Fatalf("b64_json = %q, want test PNG; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "data.0.content_url").String(); !strings.HasPrefix(
		got, "https://cpa.test/v1/artifacts/img_",
	) {
		t.Fatalf("content_url = %q, want CPA artifact URL; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "data.0.artifact_id").String(); !strings.HasPrefix(got, "img_") {
		t.Fatalf("artifact_id = %q, want img_ id; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "data.0.sha256").String(); got == "" {
		t.Fatalf("sha256 missing; body=%s", out)
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
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)
	err := h.forwardImagesStream(
		context.Background(),
		c,
		dataCh,
		nil,
		firstChunk,
		"b64_json",
		"image_generation",
		func(eventName string, dataJSON []byte) {
			events = append(events, eventName+":"+string(dataJSON))
		},
	)
	if err != nil {
		t.Fatalf("forwardImagesStream: %v", err)
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

func TestRewriteOpenAICompatImagesPayloadMaterializesURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	store := setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)

	out, err := h.rewriteOpenAICompatImagesPayload(
		context.Background(), c, []byte(`{"created":1,"data":[{"url":"https://provider.example/image.png"}]}`), true,
	)
	if err != nil {
		t.Fatalf("rewriteOpenAICompatImagesPayload: %v", err)
	}
	if got := gjson.GetBytes(out, "data.0.url").String(); !strings.HasPrefix(
		got, "https://cpa.test/v1/artifacts/img_",
	) {
		t.Fatalf("rewritten url = %q, want CPA artifact URL; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "data.0.artifact_id").String(); !strings.HasPrefix(got, "img_") {
		t.Fatalf("artifact_id = %q, want img_ id; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "data.0.sha256").String(); got == "" {
		t.Fatalf("sha256 missing; body=%s", out)
	}
	if len(store.records) != 1 {
		t.Fatalf("stored records = %d, want 1", len(store.records))
	}
}

func TestRewriteOpenAICompatSSEFrameMaterializesURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)

	frame := []byte("event: image\ndata: {\"data\":[{\"url\":\"https://provider.example/image.png\"}]}\n\n")
	out, err := h.rewriteOpenAICompatSSEFrame(context.Background(), c, frame)
	if err != nil {
		t.Fatalf("rewriteOpenAICompatSSEFrame: %v", err)
	}
	if !bytes.Contains(out, []byte("event: image")) {
		t.Fatalf("SSE event line was not preserved: %s", out)
	}
	payload := ""
	for line := range strings.SplitSeq(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "data:"); ok {
			payload = strings.TrimSpace(after)
			break
		}
	}
	if got := gjson.Get(payload, "data.0.url").String(); !strings.HasPrefix(got, "https://cpa.test/v1/artifacts/img_") {
		t.Fatalf("rewritten SSE url = %q, want CPA artifact URL; payload=%s", got, payload)
	}
}

func TestBuildImagesAPIResponseFromXAIMaterializesDataURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)

	payload := []byte(`{"created":1,"data":[{"url":"data:image/png;base64,` + testPNGBase64 + `"}]}`)
	out, err := h.buildImagesAPIResponseFromXAI(context.Background(), c, payload, "url")
	if err != nil {
		t.Fatalf("buildImagesAPIResponseFromXAI: %v", err)
	}
	if got := gjson.GetBytes(out, "data.0.url").String(); !strings.HasPrefix(
		got, "https://cpa.test/v1/artifacts/img_",
	) {
		t.Fatalf("xAI url = %q, want CPA artifact URL; body=%s", got, out)
	}
}

func TestForwardImagesStreamMaterializesCompletedImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	firstChunk := []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" + testPNGBase64 + "\",\"output_format\":\"png\"}]}}\n\n")
	dataCh := make(chan []byte)
	close(dataCh)

	var events []string
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)
	err := h.forwardImagesStream(
		context.Background(),
		c,
		dataCh,
		nil,
		firstChunk,
		"url",
		"image_generation",
		func(eventName string, dataJSON []byte) {
			events = append(events, eventName+":"+string(dataJSON))
		},
	)
	if err != nil {
		t.Fatalf("forwardImagesStream: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("events = %v, want one completed event", events)
	}
	if !strings.HasPrefix(events[0], "image_generation.completed:") {
		t.Fatalf("event = %q, want completed event", events[0])
	}
	_, eventPayload, _ := strings.Cut(events[0], ":")
	if got := gjson.Get(eventPayload, "url").String(); !strings.HasPrefix(got, "https://cpa.test/v1/artifacts/img_") {
		t.Fatalf("url = %q, want CPA artifact URL; event=%s", got, events[0])
	}
	if got := gjson.Get(eventPayload, "artifact_id").String(); !strings.HasPrefix(got, "img_") {
		t.Fatalf("artifact_id = %q, want img_ id; event=%s", got, events[0])
	}
}

func newImageStreamTestHandler(t *testing.T) (*APIHandler, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	setTestImageArtifactDeps(t, base)
	return NewAPIHandler(base), c, recorder
}

func TestInspectImageSSEFrameReadsNestedResponseFailure(t *testing.T) {
	info := inspectImageSSEFrame(
		[]byte("data: {\"type\":\"response.failed\",\"response\":{\"status_code\":429,\"error\":{\"message\":\"try later\"}}}\n\n"),
	)
	if info.err == nil {
		t.Fatal("inspectImageSSEFrame returned nil error")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(info.err, &statusErr) || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want status %d", info.err, http.StatusTooManyRequests)
	}
	if !strings.Contains(info.err.Error(), "try later") {
		t.Fatalf("error = %q, want nested response message", info.err.Error())
	}
}

func TestSSEFrameAccumulatorRejectsOversizedPendingFrame(t *testing.T) {
	acc := newBoundedSSEFrameAccumulator(8)
	if frames := acc.AddChunk([]byte("data: 123")); len(frames) != 0 {
		t.Fatalf("frames = %q, want none", frames)
	}
	if err := acc.Err(); err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("accumulator error = %v", err)
	}
}

func TestSSEFrameAccumulatorAllowsAggregateChunkOverLimit(t *testing.T) {
	frame := []byte("data: {}\n\n")
	acc := newResponsesSSEFrameAccumulator()
	acc.maxPending = len(frame)
	chunk := append(append([]byte(nil), frame...), frame...)
	frames := acc.AddChunk(chunk)
	if err := acc.Err(); err != nil {
		t.Fatalf("accumulator error = %v, want nil", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %q, want two complete frames", frames)
	}
}

func TestSSEFrameAccumulatorProcessesLargeAggregateWithoutPendingCopy(t *testing.T) {
	const frameCount = 4096
	frame := []byte("data: {}\n\n")
	acc := newResponsesSSEFrameAccumulator()
	acc.maxPending = len(frame)
	frames := acc.AddChunk(bytes.Repeat(frame, frameCount))
	if err := acc.Err(); err != nil {
		t.Fatalf("accumulator error = %v, want nil", err)
	}
	if len(frames) != frameCount {
		t.Fatalf("frame count = %d, want %d", len(frames), frameCount)
	}
	if len(acc.pending) != 0 || cap(acc.pending) != 0 {
		t.Fatalf("pending len/cap = %d/%d, want 0/0", len(acc.pending), cap(acc.pending))
	}
}

func TestSSEFrameAccumulatorRejectsOversizedWhitespaceChunk(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	acc.maxPending = 8
	if frames := acc.AddChunk([]byte("         ")); len(frames) != 0 {
		t.Fatalf("frames = %q, want none", frames)
	}
	if err := acc.Err(); err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("accumulator error = %v, want size error", err)
	}
}

func TestSSEFrameAccumulatorDoesNotRetainWhitespaceChunk(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	acc.maxPending = 1024
	if frames := acc.AddChunk(bytes.Repeat([]byte(" "), 1024)); len(frames) != 0 {
		t.Fatalf("frames = %q, want none", frames)
	}
	if err := acc.Err(); err != nil {
		t.Fatalf("accumulator error = %v, want nil", err)
	}
	if len(acc.pending) != 0 || cap(acc.pending) != 0 {
		t.Fatalf("pending len/cap = %d/%d, want 0/0", len(acc.pending), cap(acc.pending))
	}
}

func TestResponsesSSEFrameAccumulatorRejectsDelimiterlessCompletion(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	chunk := []byte(`event: response.completed
data: {"type":"response.completed"}`)
	if frames := acc.AddChunk(chunk); len(frames) != 0 {
		t.Fatalf("AddChunk frames = %q, want none", frames)
	}
	if frames := acc.Finalize(); len(frames) != 0 {
		t.Fatalf("Finalize frames = %q, want none", frames)
	}
	if err := acc.Err(); err == nil || !strings.Contains(err.Error(), "malformed data") {
		t.Fatalf("accumulator error = %v, want malformed data", err)
	}
}

func TestCodexResponsesTranslatorProducesCompleteSSEFrame(t *testing.T) {
	chunks := codexresponses.ConvertCodexResponseToOpenAIResponses(
		context.Background(),
		"gpt-5-codex",
		nil,
		nil,
		[]byte(`data: {"type":"response.completed"}`),
		nil,
	)
	if len(chunks) != 1 {
		t.Fatalf("translator chunks = %q, want one chunk", chunks)
	}

	acc := newResponsesSSEFrameAccumulator()
	frames := acc.AddChunk(chunks[0])
	if err := acc.Err(); err != nil {
		t.Fatalf("AddChunk error = %v, want nil", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %q, want one complete frame", frames)
	}
	if !acc.sawResponsesTerminal {
		t.Fatal("translated response.completed was not recognized as terminal")
	}
	if finalFrames := acc.Finalize(); len(finalFrames) != 0 {
		t.Fatalf("Finalize frames = %q, want none", finalFrames)
	}
	if err := acc.Err(); err != nil {
		t.Fatalf("Finalize error = %v, want nil", err)
	}
}

func TestImageSSEFrameAccumulatorRejectsDelimiterlessCompletion(t *testing.T) {
	acc := newBoundedSSEFrameAccumulator(1024)
	chunk := []byte(`data: {"type":"response.completed"}`)
	if frames := acc.AddChunk(chunk); len(frames) != 0 {
		t.Fatalf("AddChunk frames = %q, want none", frames)
	}
	if frames := acc.Flush(); len(frames) != 0 {
		t.Fatalf("Flush frames = %q, want none", frames)
	}
	if err := acc.Err(); err == nil || !strings.Contains(err.Error(), "malformed data") {
		t.Fatalf("accumulator error = %v, want malformed data", err)
	}
}

func TestResponsesSSEFrameAccumulatorRejectsOversizedPendingFrame(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	if acc.maxPending != maxResponsesSSEFrameBytes {
		t.Fatalf("maxPending = %d, want %d", acc.maxPending, maxResponsesSSEFrameBytes)
	}
	acc.maxPending = 8
	if frames := acc.AddChunk([]byte("data: 123")); len(frames) != 0 {
		t.Fatalf("frames = %q, want none", frames)
	}
	if err := acc.Err(); err == nil || !strings.Contains(err.Error(), "Responses SSE frame exceeds") {
		t.Fatalf("accumulator error = %v", err)
	}
}

func TestImageSSEFrameAccumulatorRejectsMultilineDataSplitAcrossChunks(t *testing.T) {
	acc := newBoundedSSEFrameAccumulator(1024)
	first := []byte(`data: {"type":"response.image_generation_call.partial_image","partial_image_b64":"abc"}`)
	if frames := acc.AddChunk(first); len(frames) != 0 {
		t.Fatalf("first chunk frames = %q, want none", frames)
	}
	frames := acc.AddChunk([]byte("\ndata: {}\n\n"))
	if len(frames) != 1 {
		t.Fatalf("frames = %q, want one complete event", frames)
	}
	info := inspectImageSSEFrame(frames[0])
	if info.err == nil || !strings.Contains(info.err.Error(), "invalid image SSE data JSON") {
		t.Fatalf("frame error = %v, want invalid combined data", info.err)
	}
}

func TestInspectImageSSEFrameAcceptsValidMultilineData(t *testing.T) {
	info := inspectImageSSEFrame([]byte("data: {\"type\":\ndata: \"response.completed\"}\n\n"))
	if info.err != nil {
		t.Fatalf("frame error = %v, want nil", info.err)
	}
	if !info.sawPayload || !info.terminal {
		t.Fatalf("frame info = %#v, want terminal payload", info)
	}
}

func TestInspectImageSSEFrameAcceptsResponseIncomplete(t *testing.T) {
	info := inspectImageSSEFrame([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n"))
	if info.err != nil {
		t.Fatalf("frame error = %v, want nil", info.err)
	}
	if !info.sawPayload || !info.terminal {
		t.Fatalf("frame info = %#v, want incomplete terminal payload", info)
	}
}

func TestExtractImagesFromResponsesIncomplete(t *testing.T) {
	results, _, usage, metadata, err := extractImagesFromResponsesCompleted([]byte(`{
		"type":"response.incomplete",
		"response":{
			"status":"incomplete",
			"output":[{"type":"image_generation_call","result":"AA==","output_format":"png"}],
			"tool_usage":{"image_gen":{"generated_images":1}}
		}
	}`))
	if err != nil {
		t.Fatalf("extractImagesFromResponsesCompleted() error = %v", err)
	}
	if len(results) != 1 || results[0].Result != "AA==" {
		t.Fatalf("results = %#v, want incomplete image output", results)
	}
	if metadata.OutputFormat != "png" {
		t.Fatalf("output format = %q, want png", metadata.OutputFormat)
	}
	if got := gjson.GetBytes(usage, "generated_images").Int(); got != 1 {
		t.Fatalf("generated_images = %d, want 1; usage=%s", got, usage)
	}
}

func TestForwardRawImageStreamRequiresTerminalEvent(t *testing.T) {
	h, c, recorder := newImageStreamTestHandler(t)
	err := h.forwardRawImageStream(
		context.Background(),
		c,
		nil,
		nil,
		[]byte("data: {\"type\":\"image_generation.partial\",\"value\":1}\n\n"),
	)
	if err == nil || !strings.Contains(err.Error(), "disconnected before completion") {
		t.Fatalf("forwardRawImageStream error = %v", err)
	}
	if !strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatalf("response missing error event: %s", recorder.Body.String())
	}
}

func TestForwardRawImageStreamLateErrorSuppressesTerminalSuccess(t *testing.T) {
	h, c, recorder := newImageStreamTestHandler(t)
	dataCh := make(chan []byte, 1)
	dataCh <- []byte("data: {\"type\":\"response.completed\"}\n\n")
	close(dataCh)
	errCh := make(chan *handlers.ErrorMessage, 1)
	errCh <- &handlers.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("late upstream failure")}
	close(errCh)

	err := h.forwardRawImageStream(
		context.Background(),
		c,
		dataCh,
		errCh,
		[]byte("data: {\"type\":\"image_generation.partial\",\"value\":1}\n\n"),
	)
	if err == nil || !strings.Contains(err.Error(), "late upstream failure") {
		t.Fatalf("forwardRawImageStream error = %v", err)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "response.completed") {
		t.Fatalf("terminal success was emitted before late error: %s", body)
	}
	if !strings.Contains(body, "late upstream failure") {
		t.Fatalf("response missing late error: %s", body)
	}
}

func TestForwardRawImageStreamRejectsResponseFailedEvent(t *testing.T) {
	h, c, recorder := newImageStreamTestHandler(t)
	err := h.forwardRawImageStream(
		context.Background(),
		c,
		nil,
		nil,
		[]byte("data: {\"type\":\"response.failed\",\"response\":{\"status_code\":503,\"error\":{\"message\":\"provider unavailable\"}}}\n\n"),
	)
	if err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("forwardRawImageStream error = %v", err)
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want status %d", err, http.StatusServiceUnavailable)
	}
	if strings.Contains(recorder.Body.String(), "response.failed") {
		t.Fatalf("failed upstream event was forwarded as data: %s", recorder.Body.String())
	}
}

func TestCollectImagesFromResponsesStreamLateErrorWins(t *testing.T) {
	h, c, _ := newImageStreamTestHandler(t)
	dataCh := make(chan []byte, 1)
	dataCh <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" + testPNGBase64 + "\",\"output_format\":\"png\"}]}}\n\n")
	close(dataCh)
	errCh := make(chan *handlers.ErrorMessage, 1)
	errCh <- &handlers.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("late responses failure")}
	close(errCh)

	out, errMsg := h.collectImagesFromResponsesStream(context.Background(), c, dataCh, errCh, "url")
	if errMsg == nil || errMsg.Error == nil || !strings.Contains(errMsg.Error.Error(), "late responses failure") {
		t.Fatalf("collectImagesFromResponsesStream = (%s, %#v)", out, errMsg)
	}
	if out != nil {
		t.Fatalf("output = %s, want nil on late error", out)
	}
}

func TestForwardImagesStreamLateErrorSuppressesCompletedEvent(t *testing.T) {
	h, c, _ := newImageStreamTestHandler(t)
	errCh := make(chan *handlers.ErrorMessage, 1)
	errCh <- &handlers.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("late responses failure")}
	close(errCh)
	var events []string

	err := h.forwardImagesStream(
		context.Background(),
		c,
		nil,
		errCh,
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\""+testPNGBase64+"\",\"output_format\":\"png\"}]}}\n\n"),
		"url",
		"image_generation",
		func(eventName string, dataJSON []byte) {
			events = append(events, eventName+":"+string(dataJSON))
		},
	)
	if err == nil || !strings.Contains(err.Error(), "late responses failure") {
		t.Fatalf("forwardImagesStream error = %v", err)
	}
	if len(events) != 1 || !strings.HasPrefix(events[0], "error:") {
		t.Fatalf("events = %v, want only late error", events)
	}
}

func TestForwardImagesStreamRequiresCompletion(t *testing.T) {
	h, c, _ := newImageStreamTestHandler(t)
	var events []string
	err := h.forwardImagesStream(
		context.Background(),
		c,
		nil,
		nil,
		[]byte("data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"abc\",\"partial_image_index\":0}\n\n"),
		"b64_json",
		"image_generation",
		func(eventName string, dataJSON []byte) {
			events = append(events, eventName+":"+string(dataJSON))
		},
	)
	if err == nil || !strings.Contains(err.Error(), "disconnected before completion") {
		t.Fatalf("forwardImagesStream error = %v", err)
	}
	if len(events) != 2 || !strings.HasPrefix(
		events[0], "image_generation.partial_image:",
	) || !strings.HasPrefix(events[1], "error:") {
		t.Fatalf("events = %v, want partial image followed by error", events)
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
	setTestImageArtifactDeps(t, base)
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
	setTestImageArtifactDeps(t, base)
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

func TestImagesEditsPropagatesMultipartServiceTier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID: "auth-image-edit-service-tier", Provider: executor.Identifier(), Status: coreauth.StatusActive,
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

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatalf("WriteField model: %v", err)
	}
	if err := writer.WriteField("prompt", "edit"); err != nil {
		t.Fatalf("WriteField prompt: %v", err)
	}
	if err := writer.WriteField("service_tier", "flex"); err != nil {
		t.Fatalf("WriteField service_tier: %v", err)
	}
	part, err := writer.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	image, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	if _, err = part.Write(image); err != nil {
		t.Fatalf("write multipart image: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("Close multipart writer: %v", err)
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	setTestImageArtifactDeps(t, base)
	h := NewAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/edits", h.ImagesEdits)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.serviceTier != "flex" {
		t.Fatalf("service tier = %q, want flex", executor.serviceTier)
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
	setTestImageArtifactDeps(t, base)
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

func TestImageErrorMessageMapsContextStatuses(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		fallback int
		want     int
	}{
		{
			name: "canceled", err: context.Canceled,
			fallback: http.StatusBadGateway, want: clienterror.StatusClientClosedRequest,
		},
		{
			name: "deadline", err: context.DeadlineExceeded,
			fallback: http.StatusBadGateway, want: http.StatusGatewayTimeout,
		},
		{
			name: "wrapped explicit status",
			err: fmt.Errorf(
				"wrapped: %w", &imageHandlerStatusError{
					statusCode: http.StatusTooManyRequests,
					err:        errors.New("rate limited"),
				},
			),
			fallback: http.StatusBadGateway,
			want:     http.StatusTooManyRequests,
		},
		{
			name: "fallback", err: errors.New("boom"),
			fallback: http.StatusServiceUnavailable, want: http.StatusServiceUnavailable,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				got := imageErrorMessage(test.err, test.fallback)
				// The constructor must retain the exact error value, not only an equivalent unwrap chain.
				// noinspection GoDirectComparisonOfErrors
				if got == nil || got.StatusCode != test.want || got.Error != test.err {
					t.Fatalf("imageErrorMessage() = %#v, want status %d and original error", got, test.want)
				}
			},
		)
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
	setTestImageArtifactDeps(t, base)
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
