package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func audit68OriginalPerformImagesEndpointRequest(
	t *testing.T,
	endpointPath string,
	contentType string,
	body io.Reader,
	handler gin.HandlerFunc,
) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(endpointPath, handler)

	req := httptest.NewRequest(http.MethodPost, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func audit68OriginalAssertUnsupportedImagesModelResponse(
	t *testing.T,
	resp *httptest.ResponseRecorder,
	model string,
) {
	t.Helper()

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}

	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model " +
		model +
		" is not supported on " +
		imagesGenerationsPath +
		" or " +
		imagesEditsPath +
		((". Use one of " +
			"gpt-image-1.5," +
			" gpt-image-2, " +
			"gpt-image-2.5-" +
			"flare, gpt-ima" +
			"g") +
			"e-2.5-flare-2026-09-08, gpt-image-2.5-sunburst, gpt-image-2.5-sunburst" +
			"-2026-09-08, grok-imagine-image, grok-imagine-image-2.0, grok-imagine-" +
			"image-quality, or a configured openai-compatibility image model.")
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
	if errorType := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); errorType != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", errorType)
	}
}

func TestImagesModelValidationAllowsGPTImageAndXAIModels(t *testing.T) {
	for _, model := range []string{
		"gpt-image-1.5",
		"codex/gpt-image-1.5",
		"gpt-image-2",
		"codex/gpt-image-2",
		"gpt-image-2.5-flare",
		"codex/gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"codex/gpt-image-2.5-sunburst",
		"gpt-image-2.5",
		"codex/gpt-image-2.5",
		"grok-imagine-image",
		"xai/grok-imagine-image",
		"grok-imagine-image-quality",
		"xai/grok-imagine-image-quality",
		"grok-imagine-image-2.0",
		"xai/grok-imagine-image-2.0",
	} {
		wantSupported := model != "gpt-image-2.5" && model != "codex/gpt-image-2.5"
		if got := isSupportedImagesModel(model); got != wantSupported {
			t.Fatalf("isSupportedImagesModel(%q) = %v, want %v", model, got, wantSupported)
		}
	}
	if isSupportedImagesModel("gpt-5.4-mini") {
		t.Fatal("expected gpt-5.4-mini to be rejected")
	}
	if isSupportedImagesModel("codex/grok-imagine-image") {
		t.Fatal("expected codex/grok-imagine-image to be rejected")
	}
}

func TestImagesModelValidationAllowsOpenAICompatImageModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-openai-compat-image-model-validation"
	modelRegistry.RegisterClient(
		clientID, "openai-compatibility", []*registry.ModelInfo{
			{ID: "compat-image-model", Object: "model", OwnedBy: "compat", Type: registry.OpenAIImageModelType},
			{ID: "compat-chat-model", Object: "model", OwnedBy: "compat", Type: "openai-compatibility"},
		},
	)
	t.Cleanup(
		func() {
			modelRegistry.UnregisterClient(clientID)
		},
	)

	if !isSupportedImagesModel("compat-image-model") {
		t.Fatal("expected configured openai-compatibility image model to be supported")
	}
	if isSupportedImagesModel("compat-chat-model") {
		t.Fatal("expected non-image openai-compatibility model to be rejected")
	}
}

func TestCanonicalXAIImagesModelPreservesImage20(t *testing.T) {
	for _, model := range []string{
		"grok-imagine-image-2.0", "xai/grok-imagine-image-2.0", "XAI/Grok-Imagine-Image-2.0",
	} {
		if got := canonicalXAIImagesModel(model); got != xaiImages20Model {
			t.Fatalf("canonicalXAIImagesModel(%q) = %q, want %s", model, got, xaiImages20Model)
		}
	}
}

func TestBuildXAIImagesGenerationsRequest(t *testing.T) {
	rawJSON := []byte((`{"model":"xai/grok-imagine-image-quality","prompt":"abstract art","asp` +
		`ect_ratio":"landscape","resolution":"2k","n":2,"response_format":"url"` +
		`}`))

	req := buildXAIImagesGenerationsRequest(rawJSON, "xai/grok-imagine-image-quality", "url")

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image-quality" {
		t.Fatalf("model = %q, want grok-imagine-image-quality", got)
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "abstract art" {
		t.Fatalf("prompt = %q, want abstract art", got)
	}
	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
	if got := gjson.GetBytes(req, "response_format").String(); got != "url" {
		t.Fatalf("response_format = %q, want url", got)
	}
	if got := gjson.GetBytes(req, "n").Int(); got != 2 {
		t.Fatalf("n = %d, want 2", got)
	}
}

func TestBuildXAIImagesGenerationsRequestPreservesQuality(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rawJSON     string
		wantQuality string
		hasQuality  bool
	}{
		{
			name:        "explicit medium",
			rawJSON:     `{"model":"grok-imagine-image-2.0","prompt":"circle","quality":"medium"}`,
			wantQuality: "medium",
			hasQuality:  true,
		},
		{
			name:        "explicit low",
			rawJSON:     `{"model":"grok-imagine-image-2.0","prompt":"circle","quality":"low"}`,
			wantQuality: "low",
			hasQuality:  true,
		},
		{
			name:        "trimmed whitespace",
			rawJSON:     `{"model":"grok-imagine-image-2.0","prompt":"circle","quality":"  high  "}`,
			wantQuality: "high",
			hasQuality:  true,
		},
		{
			name:       "empty quality omitted",
			rawJSON:    `{"model":"grok-imagine-image-2.0","prompt":"circle","quality":""}`,
			hasQuality: false,
		},
		{
			name:       "blank quality omitted",
			rawJSON:    `{"model":"grok-imagine-image-2.0","prompt":"circle","quality":"   "}`,
			hasQuality: false,
		},
		{
			name:       "omitted quality omitted",
			rawJSON:    `{"model":"grok-imagine-image-2.0","prompt":"circle"}`,
			hasQuality: false,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				req := buildXAIImagesGenerationsRequest(
					[]byte(tc.rawJSON),
					"grok-imagine-image-2.0",
					"b64_json",
				)
				q := gjson.GetBytes(req, "quality")
				if tc.hasQuality {
					if !q.Exists() || q.String() != tc.wantQuality {
						t.Fatalf(
							"quality = %q (exists=%v), want %q; req = %s",
							q.String(),
							q.Exists(),
							tc.wantQuality,
							string(req),
						)
					}
				} else {
					if q.Exists() {
						t.Fatalf("expected quality to be omitted, got %q; req = %s", q.String(), string(req))
					}
				}
			},
		)
	}
}

func TestBuildXAIImagesGenerationsRequestNineByTwenty(t *testing.T) {
	rawJSON := []byte((`{"model":"grok-imagine-image-quality","prompt":"kitten","aspect_ratio"` +
		`:"9:20","resolution":"2k","n":1,"response_format":"b64_json"}`))

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "9:20" {
		t.Fatalf("aspect_ratio = %q, want 9:20", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestBuildXAIImagesGenerationsRequestNineByTwentyFromSize(t *testing.T) {
	rawJSON := []byte(
		`{"model":"grok-imagine-image-quality","prompt":"kitten","size":"9:20","resolution":"2k"}`,
	)

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "9:20" {
		t.Fatalf("aspect_ratio from size = %q, want 9:20", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestBuildXAIImagesGenerationsRequestTwentyByNine(t *testing.T) {
	rawJSON := []byte((`{"model":"grok-imagine-image-quality","prompt":"kitten","aspect_ratio"` +
		`:"20:9","resolution":"2k","n":1,"response_format":"b64_json"}`))

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "20:9" {
		t.Fatalf("aspect_ratio = %q, want 20:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestBuildXAIImagesGenerationsRequestTwentyByNineFromSize(t *testing.T) {
	rawJSON := []byte(
		`{"model":"grok-imagine-image-quality","prompt":"kitten","size":"20:9","resolution":"2k"}`,
	)

	req := buildXAIImagesGenerationsRequest(rawJSON, "grok-imagine-image-quality", "b64_json")

	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "20:9" {
		t.Fatalf("aspect_ratio from size = %q, want 20:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
}

func TestXAIImagesAspectRatioNineByTwenty(t *testing.T) {
	if got := xaiImagesAspectRatio("9:20", "1:1"); got != "9:20" {
		t.Fatalf("xaiImagesAspectRatio(9:20) = %q, want 9:20", got)
	}
	if got := xaiImagesAspectRatio("20:9", "1:1"); got != "20:9" {
		t.Fatalf("xaiImagesAspectRatio(20:9) = %q, want 20:9", got)
	}
	if got := xaiImagesAspectRatio("9:21", "1:1"); got != "1:1" {
		t.Fatalf("unknown ratio should keep fallback, got %q", got)
	}
	if got := xaiImagesAspectRatioFromSize("9:20", ""); got != "9:20" {
		t.Fatalf("xaiImagesAspectRatioFromSize(9:20) = %q, want 9:20", got)
	}
	if got := xaiImagesAspectRatioFromSize("20:9", ""); got != "20:9" {
		t.Fatalf("xaiImagesAspectRatioFromSize(20:9) = %q, want 20:9", got)
	}
}

func TestBuildXAIImagesEditRequest(t *testing.T) {
	req := buildXAIImagesEditRequest(
		"grok-imagine-image",
		"edit it",
		[]string{"data:image/png;base64,AA==", "https://example.com/image.png"},
		"b64_json",
		"3:2",
		"1k",
		"medium",
		0,
	)

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image" {
		t.Fatalf("model = %q, want grok-imagine-image", got)
	}
	if got := gjson.GetBytes(req, "quality").String(); got != "medium" {
		t.Fatalf("quality = %q, want medium", got)
	}
	if got := gjson.GetBytes(req, "images.0.type").String(); got != "image_url" {
		t.Fatalf("images.0.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "images.0.url").String(); got != "data:image/png;base64,AA==" {
		t.Fatalf("images.0.url = %q", got)
	}
	if got := gjson.GetBytes(req, "images.1.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("images.1.url = %q", got)
	}
	if gjson.GetBytes(req, "image").Exists() {
		t.Fatalf("multiple image edits must use images array: %s", string(req))
	}
}

func TestBuildXAIImagesEditRequestSingleImage(t *testing.T) {
	req := buildXAIImagesEditRequest(
		"grok-imagine-image", "edit it", []string{"https://example.com/image.png"}, "url", "", "", "", 0,
	)

	if got := gjson.GetBytes(req, "image.type").String(); got != "image_url" {
		t.Fatalf("image.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "image.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("image.url = %q", got)
	}
	if gjson.GetBytes(req, "quality").Exists() {
		t.Fatalf("empty quality should be omitted: %s", string(req))
	}
	if gjson.GetBytes(req, "images").Exists() {
		t.Fatalf("single image edit must use image object: %s", string(req))
	}
}

func TestXAIImagesEditOptionsFromJSONPreservesQuality(t *testing.T) {
	aspectRatio, resolution, quality, n := xaiImagesEditOptionsFromJSON(
		[]byte(`{"quality":"low","size":"1024x1024","resolution":"1k","n":1}`),
	)
	if quality != "low" {
		t.Fatalf("quality = %q, want low", quality)
	}
	if aspectRatio != "1:1" {
		t.Fatalf("aspectRatio = %q, want 1:1", aspectRatio)
	}
	if resolution != "1k" {
		t.Fatalf("resolution = %q, want 1k", resolution)
	}
	if n != 1 {
		t.Fatalf("n = %d, want 1", n)
	}

	_, _, emptyQuality, _ := xaiImagesEditOptionsFromJSON([]byte(`{"n":1}`))
	if emptyQuality != "" {
		t.Fatalf("emptyQuality = %q, want empty", emptyQuality)
	}
}

func TestBuildOpenAICompatImagesJSONRequestPreservesStreamForStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest(
		[]byte(`{"model":"compat-image","prompt":"draw","stream":false}`), "upstream-image", true,
	)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if !gjson.GetBytes(req, "stream").Bool() {
		t.Fatalf("stream flag missing: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestDropsStreamForNonStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest(
		[]byte(`{"model":"compat-image","prompt":"draw","stream":true}`), "upstream-image", false,
	)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if gjson.GetBytes(req, "stream").Exists() {
		t.Fatalf("stream flag should be removed from non-streaming request: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesMultipartRequestPreservesStreamAndFileContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
		t.Fatalf("write prompt field: %v", errWrite)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read source form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove source form files: %v", errRemove)
		}
	}()

	out, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, "upstream-image", true)
	if errBuild != nil {
		t.Fatalf("buildOpenAICompatImagesMultipartRequest error: %v", errBuild)
	}
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		t.Fatalf("parse content type: %v", errParse)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	rewrittenReader := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	rewrittenForm, errRead := rewrittenReader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read rewritten form: %v", errRead)
	}
	defer func() {
		if errRemove := rewrittenForm.RemoveAll(); errRemove != nil {
			t.Fatalf("remove rewritten form files: %v", errRemove)
		}
	}()
	if got := rewrittenForm.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model values = %#v, want upstream-image", got)
	}
	if got := rewrittenForm.Value["stream"]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("stream values = %#v, want true", got)
	}
	if got := rewrittenForm.Value["prompt"]; len(got) != 1 || got[0] != "edit" {
		t.Fatalf("prompt values = %#v, want edit", got)
	}
	if got := rewrittenForm.File["image"]; len(got) != 1 || got[0].Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image headers = %#v, want image/png", got)
	}
}

func TestBuildImagesAPIResponseFromXAI(t *testing.T) {
	payload := []byte((`{"created":123,"data":[{"b64_json":"AA==","revised_prompt":"refined","` +
		`mime_type":"image/png"}],"usage":{"total_tokens":0}}`))

	out, err := audit68OriginalBuildImagesAPIResponseFromXAI(t, payload, "b64_json")
	if err != nil {
		t.Fatalf("audit68OriginalBuildImagesAPIResponseFromXAI(t, ) error = %v", err)
	}

	if got := gjson.GetBytes(out, "created").Int(); got != 123 {
		t.Fatalf("created = %d, want 123", got)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("data.0.b64_json = %q, want AA==", got)
	}
	if got := gjson.GetBytes(out, "data.0.revised_prompt").String(); got != "refined" {
		t.Fatalf("data.0.revised_prompt = %q, want refined", got)
	}
	if !gjson.GetBytes(out, "usage").Exists() {
		t.Fatalf("usage missing: %s", string(out))
	}
}

func TestImagesUnavailableHandlerKeepsValidationBoundary(t *testing.T) {
	for _, handler := range []*APIHandler{nil, {}} {
		for _, format := range []string{"generation", "JSON edit", "multipart edit"} {
			for _, model := range []string{"gpt-5.4-mini", "gpt-image-1.5"} {
				name := "zero/"
				if handler == nil {
					name = "nil/"
				}
				t.Run(
					name+format+"/"+model, func(t *testing.T) {
						var body bytes.Buffer
						contentType := "application/json"
						endpoint, handle := imagesEditsPath, handler.ImagesEdits
						if format == "generation" {
							endpoint, handle = imagesGenerationsPath, handler.ImagesGenerations
						}
						if format == "multipart edit" {
							writer := multipart.NewWriter(&body)
							if err := writer.WriteField("model", model); err != nil {
								t.Fatal(err)
							}
							if err := writer.Close(); err != nil {
								t.Fatal(err)
							}
							contentType = writer.FormDataContentType()
						} else {
							body.WriteString(`{"model":"` + model + `","prompt":"synthetic"}`)
						}
						response := audit68OriginalPerformImagesEndpointRequest(
							t,
							endpoint,
							contentType,
							&body,
							handle,
						)
						wantStatus, wantType := http.StatusBadRequest, "invalid_request_error"
						if model == "gpt-image-1.5" {
							wantStatus, wantType = http.StatusInternalServerError, "server_error"
						}
						if response.Code != wantStatus || gjson.GetBytes(
							response.Body.Bytes(), "error.type",
						).String() != wantType {
							t.Fatalf(
								"status=%d body=%s, want %d/%s",
								response.Code,
								response.Body.String(),
								wantStatus,
								wantType,
							)
						}
					},
				)
			}
		}
	}
}

func TestLegacyImagesFailuresPreserveCauseAndDeliveryError(t *testing.T) {
	upstream := errors.New("token=image-secret")
	writeFailure := errors.New("synthetic legacy image writer failure")
	for _, path := range []string{"collector", "stream", "failed write"} {
		t.Run(
			path, func(t *testing.T) {
				h, ctx, _ := newImageStreamTestHandler(t)
				data := make(chan []byte)
				close(data)
				errs := make(chan *handlers.ErrorMessage, 1)
				errs <- &handlers.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: upstream}
				close(errs)
				wantCause := upstream
				var returned error
				if path == "collector" {
					_, message := h.collectImagesFromResponsesStream(
						context.Background(),
						ctx,
						data,
						errs,
						"b64_json",
					)
					if message == nil || message.StatusCode != http.StatusTooManyRequests {
						t.Fatalf("lost upstream error status: %#v", message)
					}
					returned = imageHandlerError(message)
				} else {
					if path == "failed write" {
						var events []string
						ctx.Writer = &openAIStreamDeliveryWriter{
							ResponseWriter: ctx.Writer, events: &events, writeErr: writeFailure,
						}
						wantCause = writeFailure
					}
					returned = h.forwardImagesStream(
						context.Background(), ctx, data, errs, nil, "b64_json", "image_generation",
						func(string, []byte) {},
					)
				}
				if !errors.Is(returned, wantCause) || strings.Contains(returned.Error(), "image-secret") {
					t.Fatalf("returned error=%v, want safe text with cause %v", returned, wantCause)
				}
			},
		)
	}
}

func TestImagesGenerationsRejectsUnsupportedModel(t *testing.T) {
	handler := &APIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := audit68OriginalPerformImagesEndpointRequest(
		t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations,
	)

	audit68OriginalAssertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsJSONRejectsUnsupportedModel(t *testing.T) {
	handler := &APIHandler{}
	body := strings.NewReader(
		`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`,
	)

	resp := audit68OriginalPerformImagesEndpointRequest(
		t, imagesEditsPath, "application/json", body, handler.ImagesEdits,
	)

	audit68OriginalAssertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsMultipartRejectsUnsupportedModel(t *testing.T) {
	handler := &APIHandler{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("prompt", "edit this"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := audit68OriginalPerformImagesEndpointRequest(
		t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits,
	)

	audit68OriginalAssertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesGenerations_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil,
	)
	handler := NewAPIHandler(base)
	body := strings.NewReader(`{"prompt":"draw a square"}`)

	resp := audit68OriginalPerformImagesEndpointRequest(
		t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations,
	)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil,
	)
	handler := NewAPIHandler(base)
	body := strings.NewReader(`{"prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := audit68OriginalPerformImagesEndpointRequest(
		t, imagesEditsPath, "application/json", body, handler.ImagesEdits,
	)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesGenerations_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil,
	)
	handler := NewAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := audit68OriginalPerformImagesEndpointRequest(
		t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations,
	)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil,
	)
	handler := NewAPIHandler(base)
	body := strings.NewReader(
		`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`,
	)

	resp := audit68OriginalPerformImagesEndpointRequest(
		t, imagesEditsPath, "application/json", body, handler.ImagesEdits,
	)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestSSEFrameAccumulatorRejectsDataOnlyFrame(t *testing.T) {
	accumulator := &sseFrameAccumulator{}
	chunk := []byte(`data: {"type":"image_generation.partial","partial_image_index":0}`)

	if frames := accumulator.AddChunk(chunk); len(frames) != 0 {
		t.Fatalf("AddChunk() emitted an unterminated data-only frame: %q", frames)
	}
	frames := accumulator.Flush()
	if len(frames) != 0 || accumulator.Err() == nil {
		t.Fatalf("Flush() frames = %q, error = %v, want rejected incomplete frame", frames, accumulator.Err())
	}
}

func TestCollectImagesRejectsPayloadErrorBeforeCompleted(t *testing.T) {
	data := make(chan []byte, 1)
	data <- []byte(("event: error\ndata: {\"type\":\"provider.error\",\"error\":{\"code\":\"failed\",\"" +
		"message\":\"token=image-secret\"}}\n\nevent: response.completed\ndata: {\"type\":\"response." +
		"completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}]}" +
		"}\n\n"))
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)

	out, errMsg := audit68OriginalCollectImagesFromResponsesStream(
		t,
		context.Background(),
		data,
		errs,
		"b64_json",
	)
	if len(out) != 0 || errMsg == nil || errMsg.Error == nil {
		t.Fatalf("payload error result out=%q err=%#v", out, errMsg)
	}
	if strings.Contains(errMsg.Error.Error(), "image-secret") ||
		!strings.Contains(errMsg.Error.Error(), "[REDACTED]") {
		t.Fatalf("payload error was not sanitized: %q", errMsg.Error.Error())
	}
}

func TestForwardImagesStreamCancelsWithPayloadError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	_, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin writer to implement http.Flusher")
	}
	data := make(chan []byte)
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)
	var canceled error
	firstChunk := []byte("event: error\ndata: {\"error\":{\"message\":\"token=image-secret\"}}\n\n")

	canceled = h.forwardImagesStream(
		context.Background(),
		c,
		data,
		errs,
		firstChunk,
		"b64_json",
		"image_generation",
		func(string, []byte) {},
	)
	if canceled == nil || strings.Contains(canceled.Error(), "image-secret") || !strings.Contains(
		canceled.Error(), "[REDACTED]",
	) {
		t.Fatalf("payload error cancel = %v body=%q", canceled, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatalf("payload error event missing: %q", recorder.Body.String())
	}
}

func TestForwardRawImageStreamPrefersPendingErrorOnClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	for i := range 100 {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		data := make(chan []byte)
		close(data)
		errs := make(chan *handlers.ErrorMessage, 1)
		errs <- &handlers.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("image upstream busy")}
		close(errs)
		canceled := h.forwardRawImageStream(context.Background(), c, data, errs, nil)
		if canceled == nil || !strings.Contains(canceled.Error(), "image upstream busy") {
			t.Fatalf("iteration %d: cancel=%v body=%q", i, canceled, recorder.Body.String())
		}
	}
}

func TestCollectImagesPrefersPendingErrorWhenDataChannelCloses(t *testing.T) {
	for i := range 100 {
		data := make(chan []byte)
		close(data)
		errs := make(chan *handlers.ErrorMessage, 1)
		want := &handlers.ErrorMessage{
			StatusCode:     http.StatusTooManyRequests,
			Error:          errors.New("image upstream busy"),
			DirectResponse: true,
			Headers:        http.Header{"Retry-After": []string{"9"}},
		}
		errs <- want
		close(errs)

		_, got := audit68OriginalCollectImagesFromResponsesStream(
			t,
			context.Background(),
			data,
			errs,
			"b64_json",
		)
		if got != want {
			t.Fatalf("iteration %d: pending error = %#v, want original %#v", i, got, want)
		}
	}
}

func TestCollectImagesAllowsMultilineSSEData(t *testing.T) {
	data := make(chan []byte, 1)
	data <- []byte("event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\n" +
		("data: \"response\":{\"created_at\":1,\"output\":[{\"type\":" +
			"\"image_generation_call\",\"result\":" +
			"\"aW1hZ2U=\"}]}}\n\n"))
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)

	out, errMsg := audit68OriginalCollectImagesFromResponsesStream(
		t,
		context.Background(),
		data,
		errs,
		"b64_json",
	)
	if errMsg != nil {
		t.Fatalf("audit68OriginalCollectImagesFromResponsesStream(t, ) error = %v", errMsg.Error)
	}
	if !strings.Contains(string(out), `"b64_json":"aW1hZ2U="`) {
		t.Fatalf("multiline image response = %q", out)
	}
}

func TestSSEFrameAccumulatorKeepsMultipleFramesDistinct(t *testing.T) {
	accumulator := &sseFrameAccumulator{}
	first := "event: first\ndata: {\"type\":\"first\"}\n\n"
	second := "event: second\ndata: {\"type\":\"second\"}\n\n"

	frames := accumulator.AddChunk([]byte(first + second))
	if len(frames) != 2 {
		t.Fatalf("AddChunk() returned %d frames, want 2: %q", len(frames), frames)
	}
	if string(frames[0]) != first || string(frames[1]) != second {
		t.Fatalf("frames were overwritten during buffer compaction: %q", frames)
	}
}

func audit68OriginalBuildImagesAPIResponseFromXAI(t *testing.T, payload []byte, format string) ([]byte, error) {
	h, c, _ := newImageStreamTestHandler(t)
	return h.buildImagesAPIResponseFromXAI(context.Background(), c, payload, format)
}

func audit68OriginalCollectImagesFromResponsesStream(
	t *testing.T,
	ctx context.Context,
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
	format string,
) ([]byte, *handlers.ErrorMessage) {
	h, c, _ := newImageStreamTestHandler(t)
	return h.collectImagesFromResponsesStream(ctx, c, data, errs, format)
}

func TestWriteImagesStreamErrorEventSanitizesPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	raw := `{"error":{"code":"upstream_failed","message":"token=image-secret"},"debug":"` + strings.Repeat(
		"x", 8192,
	) + `"}`
	c.Request = httptest.NewRequest(http.MethodPost, imagesGenerationsPath, nil)
	h := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	data := make(chan []byte)
	close(data)
	errs := make(chan *handlers.ErrorMessage, 1)
	errs <- &handlers.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New(raw)}
	close(errs)
	_ = h.forwardRawImageStream(context.Background(), c, data, errs, nil)

	body := recorder.Body.String()
	if strings.Contains(body, "image-secret") || len(body) > 4096 || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("image stream error was not safely bounded: len=%d body=%q", len(body), body)
	}
}
