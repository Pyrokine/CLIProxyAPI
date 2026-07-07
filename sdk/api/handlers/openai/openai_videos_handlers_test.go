package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	apihandlers "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func performVideosEndpointRequest(
	t *testing.T,
	method string,
	endpointPath string,
	contentType string,
	body io.Reader,
	handler gin.HandlerFunc,
) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	switch method {
	case http.MethodGet:
		router.GET(endpointPath, handler)
	default:
		router.POST(endpointPath, handler)
	}

	req := httptest.NewRequest(method, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func performVideosRouteRequest(
	t *testing.T,
	method string,
	routePath string,
	requestPath string,
	contentType string,
	body io.Reader,
	handler gin.HandlerFunc,
) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	switch method {
	case http.MethodGet:
		router.GET(routePath, handler)
	default:
		router.POST(routePath, handler)
	}

	req := httptest.NewRequest(method, requestPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

type videoAuthCaptureExecutor struct {
	mu         sync.Mutex
	requestID  string
	contentURL string
	authIDs    []string
	models     []string
}

func (e *videoAuthCaptureExecutor) Identifier() string { return "xai" }

func (e *videoAuthCaptureExecutor) Execute(
	_ context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	_ coreexecutor.Options,
) (coreexecutor.Response, error) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	e.authIDs = append(e.authIDs, authID)
	e.models = append(e.models, req.Model)
	e.mu.Unlock()

	requestID := strings.TrimSpace(gjson.GetBytes(req.Payload, "request_id").String())
	if requestID == "" {
		requestID = e.requestID
	}
	contentURL := strings.TrimSpace(e.contentURL)
	if contentURL == "" {
		contentURL = "https://vidgen.x.ai/video.mp4"
	}
	payload := []byte(
		`{"request_id":` + strconv.Quote(requestID) +
			`,"status":"completed","progress":100,"video":{"url":` + strconv.Quote(contentURL) + `,"duration":4}}`,
	)
	return coreexecutor.Response{Payload: payload}, nil
}

func (e *videoAuthCaptureExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *videoAuthCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *videoAuthCaptureExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *videoAuthCaptureExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

func (e *videoAuthCaptureExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.authIDs))
	copy(out, e.authIDs)
	return out
}

func (e *videoAuthCaptureExecutor) Models() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.models))
	copy(out, e.models)
	return out
}

func resetVideoAuthBindingsForTest(t *testing.T) {
	t.Helper()
	previous := videoAuthBindings
	videoAuthBindings = newVideoAuthBindingStore()
	t.Cleanup(
		func() {
			videoAuthBindings = previous
		},
	)
}

func newVideoAuthBindingTestHandler(t *testing.T, executor *videoAuthCaptureExecutor) *APIHandler {
	t.Helper()

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	authIDs := []string{executor.requestID + "-auth-a", executor.requestID + "-auth-b"}
	for _, authID := range authIDs {
		auth := &coreauth.Auth{
			ID:       authID,
			Provider: "xai",
			Status:   coreauth.StatusActive,
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("manager.Register(%s): %v", authID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(
			authID, auth.Provider, []*registry.ModelInfo{{ID: defaultXAIVideosModel}},
		)
		manager.RefreshSchedulerEntry(authID)
	}
	t.Cleanup(
		func() {
			for _, authID := range authIDs {
				registry.GetGlobalRegistry().UnregisterClient(authID)
			}
		},
	)

	base := apihandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	return NewAPIHandler(base)
}

func TestXAIVideosPreviewModelIsSupported(t *testing.T) {
	model := "xai/grok-imagine-video-1.5-preview"

	if !isXAIVideosModel(model) {
		t.Fatalf("isXAIVideosModel(%q) = false", model)
	}
	if got := canonicalXAIVideosModel(model); got != xaiVideos15PreviewModel {
		t.Fatalf("canonicalXAIVideosModel(%q) = %q, want %q", model, got, xaiVideos15PreviewModel)
	}

	payload, meta, err := buildXAIVideosCreateRequest([]byte(`{"prompt":"make a clip","seconds":"5"}`), model)
	if err != nil {
		t.Fatalf("buildXAIVideosCreateRequest() error = %v", err)
	}
	if got := gjson.GetBytes(payload, "model").String(); got != xaiVideos15PreviewModel {
		t.Fatalf("payload model = %q, want %q; payload=%s", got, xaiVideos15PreviewModel, payload)
	}
	if meta.Model != xaiVideos15PreviewModel {
		t.Fatalf("meta.Model = %q, want %q", meta.Model, xaiVideos15PreviewModel)
	}
}

func TestVideosModelValidationAllowsSoraWrapper(t *testing.T) {
	if !isSupportedVideosModel("sora-2") {
		t.Fatal("expected sora-2 to be supported by the OpenAI video wrapper")
	}
	if isXAIVideosModel("sora-2") {
		t.Fatal("expected sora-2 not to be treated as a native xAI video model")
	}
}

func TestBuildXAIVideosCreateRequestMapsSoraModelToXAIBackend(t *testing.T) {
	rawJSON := []byte(`{"model":"sora-2","prompt":"a cat playing piano","seconds":"8"}`)

	req, meta, err := buildXAIVideosCreateRequest(rawJSON, "sora-2")
	if err != nil {
		t.Fatalf("buildXAIVideosCreateRequest() error = %v", err)
	}
	if got := gjson.GetBytes(req, "model").String(); got != defaultXAIVideosModel {
		t.Fatalf("upstream model = %q, want %s", got, defaultXAIVideosModel)
	}
	if meta.Model != defaultXAIVideosModel {
		t.Fatalf("response model = %q, want %s", meta.Model, defaultXAIVideosModel)
	}
	if meta.UpstreamModel != defaultXAIVideosModel {
		t.Fatalf("upstream meta model = %q, want %s", meta.UpstreamModel, defaultXAIVideosModel)
	}
}

func TestBuildVideosRetrieveAPIResponseFromXAI(t *testing.T) {
	payload := []byte(`{"object":"video","id":"91989464-273f-95df-8197-703b4fefd40e","model":"grok-imagine-video","status":"completed","progress":100,"seconds":"4","video":{"url":"https://vidgen.x.ai/xai-video.mp4","duration":4,"respect_moderation":true},"usage":{"cost_in_usd_ticks":2800000000}}`)

	out, err := buildVideosRetrieveAPIResponseFromXAI(
		"91989464-273f-95df-8197-703b4fefd40e", payload, defaultOpenAIVideosModel,
	)
	if err != nil {
		t.Fatalf("buildVideosRetrieveAPIResponseFromXAI() error = %v", err)
	}
	if got := gjson.GetBytes(out, "model").String(); got != defaultXAIVideosModel {
		t.Fatalf("model = %q, want %s", got, defaultXAIVideosModel)
	}
	if got := gjson.GetBytes(out, "video_url").String(); got != "https://vidgen.x.ai/xai-video.mp4" {
		t.Fatalf("video_url = %q", got)
	}
	if gjson.GetBytes(out, "video").Exists() {
		t.Fatalf("video field must not be exposed in OpenAI retrieve response: %s", string(out))
	}
	if gjson.GetBytes(out, "usage").Exists() {
		t.Fatalf("usage field must not be exposed in OpenAI retrieve response: %s", string(out))
	}
}

func TestBuildVideosRetrieveAPIResponseFromXAINormalizesErrors(t *testing.T) {
	payload := []byte(`{"status":"failed","error":{"message":"The request was rejected by the safety system.","type":"invalid_request_error","code":"content_policy_violation"}}`)

	out, err := buildVideosRetrieveAPIResponseFromXAI("video_123", payload, defaultOpenAIVideosModel)
	if err != nil {
		t.Fatalf("buildVideosRetrieveAPIResponseFromXAI() error = %v", err)
	}
	if got := gjson.GetBytes(out, "error.code").String(); got != "content_policy_violation" {
		t.Fatalf("error.code = %q, want content_policy_violation", got)
	}
	if got := gjson.GetBytes(out, "error.message").String(); got != "The request was rejected by the safety system." {
		t.Fatalf("error.message = %q", got)
	}
	if gjson.GetBytes(out, "error.type").Exists() {
		t.Fatalf("error.type must not be present: %s", string(out))
	}
}

func TestXAIVideoContentURLFromPayload(t *testing.T) {
	payload := []byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4","duration":6}}`)

	got, err := xaiVideoContentURLFromPayload(payload)
	if err != nil {
		t.Fatalf("xaiVideoContentURLFromPayload() error = %v", err)
	}
	if got != "https://vidgen.x.ai/video.mp4" {
		t.Fatalf("url = %q, want https://vidgen.x.ai/video.mp4", got)
	}
}

func TestVideosCreateRejectsUnsupportedModel(t *testing.T) {
	handler := &APIHandler{}
	body := strings.NewReader(`{"model":"not-a-video-model","prompt":"make a video"}`)

	resp := performVideosEndpointRequest(
		t, http.MethodPost, openAIVideosPath, "application/json", body, handler.VideosCreate,
	)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "object").String(); got != "video" {
		t.Fatalf("object = %q, want video", got)
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "status").String(); got != "failed" {
		t.Fatalf("status = %q, want failed", got)
	}
	expectedMessage := "Model not-a-video-model is not supported on " + openAIVideosPath + ". Use " + defaultOpenAIVideosModel + "."
	if got := gjson.GetBytes(resp.Body.Bytes(), "error.message").String(); got != expectedMessage {
		t.Fatalf("error.message = %q, want %q", got, expectedMessage)
	}
}

func TestXAIVideosNativeRejectsUnsupportedModel(t *testing.T) {
	handler := &APIHandler{}
	body := strings.NewReader(`{"model":"sora-2","prompt":"make a video"}`)

	resp := performVideosEndpointRequest(
		t, http.MethodPost, xaiVideosGenerationsAPI, "application/json", body, handler.XAIVideosGenerations,
	)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model sora-2 is not supported on " + xaiVideosGenerationsAPI + ", " + xaiVideosEditsAPI + ", or " + xaiVideosExtensionsAPI + ". Use " + defaultXAIVideosModel + "."
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
}

func TestVideosCreateBindsRetrieveToSelectedAuth(t *testing.T) {
	resetVideoAuthBindingsForTest(t)
	executor := &videoAuthCaptureExecutor{requestID: "video-openai-bound"}
	handler := newVideoAuthBindingTestHandler(t, executor)

	createResp := performVideosEndpointRequest(
		t, http.MethodPost, openAIVideosPath, "application/json",
		strings.NewReader(`{"model":"sora-2","prompt":"make a video"}`), handler.VideosCreate,
	)
	if createResp.Code != http.StatusOK {
		t.Fatalf("create status = %d, want %d: %s", createResp.Code, http.StatusOK, createResp.Body.String())
	}
	videoID := gjson.GetBytes(createResp.Body.Bytes(), "id").String()
	if videoID != executor.requestID {
		t.Fatalf("created video id = %q, want %q", videoID, executor.requestID)
	}

	retrieveResp := performVideosRouteRequest(
		t, http.MethodGet, openAIVideosPath+"/:video_id", openAIVideosPath+"/"+videoID, "", nil, handler.VideosRetrieve,
	)
	if retrieveResp.Code != http.StatusOK {
		t.Fatalf("retrieve status = %d, want %d: %s", retrieveResp.Code, http.StatusOK, retrieveResp.Body.String())
	}

	authIDs := executor.AuthIDs()
	if len(authIDs) != 2 {
		t.Fatalf("authIDs = %v, want two calls", authIDs)
	}
	if authIDs[1] != authIDs[0] {
		t.Fatalf("retrieve auth = %q, want create auth %q; sequence=%v", authIDs[1], authIDs[0], authIDs)
	}
}

func TestXAIVideosNativeCreateBindsRetrieveToSelectedAuth(t *testing.T) {
	resetVideoAuthBindingsForTest(t)
	executor := &videoAuthCaptureExecutor{requestID: "video-xai-bound"}
	handler := newVideoAuthBindingTestHandler(t, executor)

	createResp := performVideosEndpointRequest(
		t, http.MethodPost, xaiVideosGenerationsAPI, "application/json",
		strings.NewReader(`{"model":"grok-imagine-video","prompt":"make a video"}`), handler.XAIVideosGenerations,
	)
	if createResp.Code != http.StatusOK {
		t.Fatalf("create status = %d, want %d: %s", createResp.Code, http.StatusOK, createResp.Body.String())
	}
	videoID := gjson.GetBytes(createResp.Body.Bytes(), "request_id").String()
	if videoID != executor.requestID {
		t.Fatalf("created request_id = %q, want %q", videoID, executor.requestID)
	}

	retrieveResp := performVideosRouteRequest(
		t, http.MethodGet, videosPath+"/:request_id", videosPath+"/"+videoID, "", nil, handler.XAIVideosRetrieve,
	)
	if retrieveResp.Code != http.StatusOK {
		t.Fatalf("retrieve status = %d, want %d: %s", retrieveResp.Code, http.StatusOK, retrieveResp.Body.String())
	}

	authIDs := executor.AuthIDs()
	if len(authIDs) != 2 {
		t.Fatalf("authIDs = %v, want two calls", authIDs)
	}
	if authIDs[1] != authIDs[0] {
		t.Fatalf("retrieve auth = %q, want create auth %q; sequence=%v", authIDs[1], authIDs[0], authIDs)
	}
}

func TestXAIVideosNativeRetrieveUsesBoundModel(t *testing.T) {
	resetVideoAuthBindingsForTest(t)
	executor := &videoAuthCaptureExecutor{requestID: "video-xai-preview-bound"}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	authModels := []struct {
		authID string
		model  string
	}{
		{authID: "video-xai-preview-default-auth", model: defaultXAIVideosModel},
		{authID: "video-xai-preview-auth", model: xaiVideos15PreviewModel},
	}
	for _, entry := range authModels {
		auth := &coreauth.Auth{
			ID:       entry.authID,
			Provider: "xai",
			Status:   coreauth.StatusActive,
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("manager.Register(%s): %v", entry.authID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(
			entry.authID, auth.Provider, []*registry.ModelInfo{{ID: entry.model}},
		)
		manager.RefreshSchedulerEntry(entry.authID)
	}
	t.Cleanup(
		func() {
			for _, entry := range authModels {
				registry.GetGlobalRegistry().UnregisterClient(entry.authID)
			}
		},
	)

	base := apihandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler := NewAPIHandler(base)

	createResp := performVideosEndpointRequest(
		t, http.MethodPost, xaiVideosGenerationsAPI, "application/json",
		strings.NewReader(`{"model":"grok-imagine-video-1.5-preview","prompt":"make a video"}`),
		handler.XAIVideosGenerations,
	)
	if createResp.Code != http.StatusOK {
		t.Fatalf("create status = %d, want %d: %s", createResp.Code, http.StatusOK, createResp.Body.String())
	}
	videoID := gjson.GetBytes(createResp.Body.Bytes(), "request_id").String()
	if videoID != executor.requestID {
		t.Fatalf("created request_id = %q, want %q", videoID, executor.requestID)
	}

	retrieveResp := performVideosRouteRequest(
		t, http.MethodGet, videosPath+"/:request_id", videosPath+"/"+videoID, "", nil, handler.XAIVideosRetrieve,
	)
	if retrieveResp.Code != http.StatusOK {
		t.Fatalf("retrieve status = %d, want %d: %s", retrieveResp.Code, http.StatusOK, retrieveResp.Body.String())
	}

	authIDs := executor.AuthIDs()
	if len(authIDs) != 2 {
		t.Fatalf("authIDs = %v, want two calls", authIDs)
	}
	if authIDs[0] != "video-xai-preview-auth" || authIDs[1] != authIDs[0] {
		t.Fatalf("authIDs = %v, want both calls to use video-xai-preview-auth", authIDs)
	}
	models := executor.Models()
	if len(models) != 2 {
		t.Fatalf("models = %v, want two calls", models)
	}
	if models[0] != xaiVideos15PreviewModel || models[1] != xaiVideos15PreviewModel {
		t.Fatalf("models = %v, want both calls to use %s", models, xaiVideos15PreviewModel)
	}
	binding, ok := videoAuthBindings.getBinding(videoID)
	if !ok {
		t.Fatal("video auth binding was not stored")
	}
	if binding.authID != "video-xai-preview-auth" || binding.model != xaiVideos15PreviewModel {
		t.Fatalf(
			"binding = {authID:%q model:%q}, want {authID:%q model:%q}", binding.authID, binding.model,
			"video-xai-preview-auth", xaiVideos15PreviewModel,
		)
	}
}

func TestVideosContentRejectsPrivateDownloadURL(t *testing.T) {
	resetVideoAuthBindingsForTest(t)

	upstream := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write([]byte("video-bytes"))
			},
		),
	)
	defer upstream.Close()

	var proxyMu sync.Mutex
	proxyHits := 0
	globalProxy := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				proxyMu.Lock()
				proxyHits++
				proxyMu.Unlock()
				http.Error(w, "unexpected proxy", http.StatusBadGateway)
			},
		),
	)
	defer globalProxy.Close()

	videoID := "video-content-selected"
	authID := "video-content-selected-auth"
	executor := &videoAuthCaptureExecutor{
		requestID:  videoID,
		contentURL: upstream.URL + "/video.mp4",
	}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "xai",
		Status:   coreauth.StatusActive,
		ProxyURL: "direct",
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("manager.Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(
		authID, auth.Provider, []*registry.ModelInfo{{ID: defaultXAIVideosModel}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(authID)
		},
	)

	base := apihandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{ProxyURL: globalProxy.URL}, manager)
	handler := NewAPIHandler(base)

	resp := performVideosRouteRequest(
		t, http.MethodGet, openAIVideosPath+"/:video_id/content", openAIVideosPath+"/"+videoID+"/content", "", nil,
		handler.VideosContent,
	)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("content status = %d, want %d: %s", resp.Code, http.StatusBadGateway, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "private/internal") {
		t.Fatalf("content body = %q, want private URL rejection", resp.Body.String())
	}
	authIDs := executor.AuthIDs()
	if len(authIDs) != 1 || authIDs[0] != authID {
		t.Fatalf("authIDs = %v, want [%s]", authIDs, authID)
	}
	if boundAuthID, ok := videoAuthBindings.get(videoID); !ok || boundAuthID != authID {
		t.Fatalf("bound auth = %q ok=%v, want %s", boundAuthID, ok, authID)
	}
	proxyMu.Lock()
	gotProxyHits := proxyHits
	proxyMu.Unlock()
	if gotProxyHits != 0 {
		t.Fatalf("global proxy hits = %d, want 0", gotProxyHits)
	}
}

func TestVideoAuthBindingTTLUsesConfig(t *testing.T) {
	base := apihandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{VideoResultAuthCacheTTL: "45m"}, nil)
	handler := NewAPIHandler(base)
	if got := handler.videoAuthBindingTTL(); got != 45*time.Minute {
		t.Fatalf("videoAuthBindingTTL() = %v, want 45m", got)
	}

	base = apihandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{VideoResultAuthCacheTTL: "invalid"}, nil)
	handler = NewAPIHandler(base)
	if got := handler.videoAuthBindingTTL(); got != defaultVideoAuthBindingTTL {
		t.Fatalf("invalid videoAuthBindingTTL() = %v, want %v", got, defaultVideoAuthBindingTTL)
	}
}

func TestVideoAuthBindingStoreEvictsOldestEntries(t *testing.T) {
	store := newVideoAuthBindingStore()
	base := time.Now()
	store.mu.Lock()
	for i := 0; i <= maxVideoAuthBindingEntries; i++ {
		store.entries[fmt.Sprintf("video-%d", i)] = videoAuthBinding{
			authID:    fmt.Sprintf("auth-%d", i),
			expiresAt: base.Add(time.Hour),
			lastUsed:  base.Add(time.Duration(i) * time.Second),
		}
	}
	store.evictOverflowLocked()
	store.mu.Unlock()

	if len(store.entries) != maxVideoAuthBindingEntries {
		t.Fatalf("binding entries = %d, want %d", len(store.entries), maxVideoAuthBindingEntries)
	}
	if _, exists := store.entries["video-0"]; exists {
		t.Fatal("oldest binding was not evicted")
	}
}

func TestVideoAuthBindingStoreExpiresEntries(t *testing.T) {
	store := newVideoAuthBindingStore()
	store.entries["video-expired"] = videoAuthBinding{
		authID:    "auth-expired",
		expiresAt: time.Now().Add(-time.Second),
	}

	if authID, ok := store.get("video-expired"); ok {
		t.Fatalf("expired binding returned authID=%q", authID)
	}
	if _, exists := store.entries["video-expired"]; exists {
		t.Fatal("expired binding was not removed")
	}
}
