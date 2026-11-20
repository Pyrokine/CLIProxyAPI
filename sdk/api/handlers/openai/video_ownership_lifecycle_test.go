package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	apihandlers "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func performOwnedVideoRequest(h *APIHandler, method, path, key, body string) *httptest.ResponseRecorder {
	engine := gin.New()
	engine.Use(
		func(c *gin.Context) {
			c.Set("apiKey", key)
			c.Request = c.Request.WithContext(
				coreusage.WithAPIKeyIdentity(
					c.Request.Context(), coreusage.HashAPIKeyIdentity(key),
				),
			)
			c.Next()
		},
	)
	for _, prefix := range []string{"/v1/videos", "/openai/v1/videos"} {
		engine.POST(prefix, h.VideosCreate)
		engine.GET(prefix+"/:video_id", h.VideosRetrieve)
		engine.GET(prefix+"/:video_id/content", h.VideosContent)
	}
	engine.GET("/native/:request_id", h.XAIVideosRetrieve)
	engine.POST("/native/generations", h.XAIVideosGenerations)
	engine.POST("/native/edits", h.XAIVideosEdits)
	engine.POST("/native/extensions", h.XAIVideosExtensions)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	return response
}

func TestVideoOwnershipSurvivesCacheLifecycle(t *testing.T) {
	for _, mode := range []string{"expired", "evicted", "handler recreated"} {
		t.Run(
			mode, func(t *testing.T) {
				executor := &videoAuthCaptureExecutor{
					requestID: "persistent-video", contentURL: "http://127.0.0.1:1/video.mp4",
				}
				h := newVideoAuthBindingTestHandler(t, executor)
				created := performOwnedVideoRequest(
					h, http.MethodPost, "/v1/videos", "creator", `{"model":"sora-2","prompt":"test"}`,
				)
				if created.Code != http.StatusOK {
					t.Fatalf("create=%d %s", created.Code, created.Body.String())
				}
				cacheKey := coreusage.HashAPIKeyIdentity("creator") + "\x00persistent-video"
				switch mode {
				case "expired":
					h.videoAuthBindings.mu.Lock()
					binding := h.videoAuthBindings.entries[cacheKey]
					binding.expiresAt = time.Now().Add(-time.Second)
					h.videoAuthBindings.entries[cacheKey] = binding
					h.videoAuthBindings.mu.Unlock()
				case "evicted":
					h.videoAuthBindings.mu.Lock()
					for i := range maxVideoAuthBindingEntries {
						h.videoAuthBindings.entries[fmt.Sprintf("unrelated-%d", i)] = videoAuthBinding{
							authID: "other", expiresAt: time.Now().Add(time.Hour),
							lastUsed: time.Now().Add(time.Minute),
						}
					}
					h.videoAuthBindings.evictOverflowLocked()
					_, retained := h.videoAuthBindings.entries[cacheKey]
					h.videoAuthBindings.mu.Unlock()
					if retained {
						t.Fatal("target was not evicted")
					}
				case "handler recreated":
					h = NewAPIHandler(h.BaseAPIHandler)
				}
				before := len(executor.AuthIDs())
				for _, path := range []string{
					"/v1/videos/persistent-video", "/v1/videos/persistent-video/content",
					"/openai/v1/videos/persistent-video", "/openai/v1/videos/persistent-video/content",
					"/native/persistent-video",
				} {
					response := performOwnedVideoRequest(h, http.MethodGet, path, "other-key", "")
					if response.Code != http.StatusNotFound {
						t.Errorf("other key %s=%d %s", path, response.Code, response.Body.String())
					}
				}
				if got := len(executor.AuthIDs()); got != before {
					t.Fatalf("other key made %d upstream calls", got-before)
				}
				for _, path := range []string{
					"/v1/videos/persistent-video", "/openai/v1/videos/persistent-video", "/native/persistent-video",
				} {
					response := performOwnedVideoRequest(h, http.MethodGet, path, "creator", "")
					if response.Code != http.StatusOK {
						t.Fatalf("owner %s=%d %s", path, response.Code, response.Body.String())
					}
				}
			},
		)
	}
}

func TestVideoUnknownOwnershipAndStorageFailuresStopUpstream(t *testing.T) {
	executor := &videoAuthCaptureExecutor{requestID: "unknown-video"}
	h := newVideoAuthBindingTestHandler(t, executor)
	for _, path := range []string{
		"/v1/videos/unknown-video", "/v1/videos/unknown-video/content", "/native/unknown-video",
	} {
		response := performOwnedVideoRequest(h, http.MethodGet, path, "first-reader", "")
		if response.Code != http.StatusNotFound {
			t.Errorf("unknown %s=%d", path, response.Code)
		}
	}
	if got := len(executor.AuthIDs()); got != 0 {
		t.Fatalf("unknown task made %d calls", got)
	}
	store := h.VideoTaskStoreSnapshot().(*memoryVideoTaskStore)
	if _, found, err := store.LoadVideoTask(context.Background(), "xai", "unknown-video"); err != nil || found {
		t.Fatalf("query claimed unknown task: found=%v err=%v", found, err)
	}
	h.SetVideoTaskStore(&memoryVideoTaskStore{err: errors.New("storage unavailable")})
	for _, path := range []string{"/v1/videos", "/native/generations", "/native/edits", "/native/extensions"} {
		response := performOwnedVideoRequest(
			h, http.MethodPost, path, "creator", `{"model":"grok-imagine-video","prompt":"test"}`,
		)
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("unavailable storage %s=%d %s", path, response.Code, response.Body.String())
		}
	}
	if got := len(executor.AuthIDs()); got != 0 {
		t.Fatalf("unavailable store made %d upstream calls", got)
	}
	h.SetVideoTaskStore(store)
	response := performOwnedVideoRequest(
		h,
		http.MethodPost,
		"/v1/videos",
		"",
		`{"model":"sora-2","prompt":"test"}`,
	)
	if response.Code != http.StatusUnauthorized || len(executor.AuthIDs()) != 0 {
		t.Fatalf("missing identity=%d calls=%d", response.Code, len(executor.AuthIDs()))
	}
}

func TestVideoOwnershipPersistenceFailureDoesNotReturnSuccess(t *testing.T) {
	executor := &videoAuthCaptureExecutor{requestID: "existing-video"}
	h := newVideoAuthBindingTestHandler(t, executor)
	store := h.VideoTaskStoreSnapshot().(*memoryVideoTaskStore)
	task := apihandlers.VideoTask{
		ID: "existing-video", Provider: "xai", OwnerIdentity: coreusage.HashAPIKeyIdentity("original"),
		AuthID: "original-upstream", Model: defaultXAIVideosModel,
	}
	if err := store.SaveVideoTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	response := performOwnedVideoRequest(
		h, http.MethodPost, "/v1/videos", "other", `{"model":"sora-2","prompt":"test"}`,
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("conflicting create=%d %s", response.Code, response.Body.String())
	}
	if _, found := h.videoAuthBindings.getBinding(coreusage.HashAPIKeyIdentity("other") + "\x00existing-video"); found {
		t.Fatal("failed persistence populated cache")
	}
	got, _, err := store.LoadVideoTask(context.Background(), "xai", "existing-video")
	if err != nil || got != task {
		t.Fatalf("original ownership changed: %+v err=%v", got, err)
	}
}

func TestVideoReadEnforcesCurrentKeyPolicy(t *testing.T) {
	for _, test := range []struct {
		name string
		rule internalconfig.APIKeyRule
		want int
	}{
		{
			name: "model blocked", rule: internalconfig.APIKeyRule{BlockedModels: []string{defaultXAIVideosModel}},
			want: http.StatusForbidden,
		},
		{
			name: "provider blocked", rule: internalconfig.APIKeyRule{Providers: []string{"codex"}},
			want: http.StatusForbidden,
		},
		{
			name: "quota storage unavailable",
			rule: internalconfig.APIKeyRule{Limits: []internalconfig.APIKeyPolicyLimit{{Window: "1h", Requests: 1}}},
			want: http.StatusServiceUnavailable,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				executor := &videoAuthCaptureExecutor{requestID: "policy-video"}
				h := newVideoAuthBindingTestHandler(t, executor)
				response := performOwnedVideoRequest(
					h, http.MethodPost, "/v1/videos", "creator", `{"model":"sora-2","prompt":"test"}`,
				)
				if response.Code != http.StatusOK {
					t.Fatalf("create=%d %s", response.Code, response.Body.String())
				}
				h.UpdateClients(
					&sdkconfig.SDKConfig{
						APIKeyRules: map[string]internalconfig.APIKeyRule{"creator": test.rule},
					},
				)
				before := len(executor.AuthIDs())
				for _, path := range []string{
					"/v1/videos/policy-video", "/v1/videos/policy-video/content", "/native/policy-video",
				} {
					response := performOwnedVideoRequest(h, http.MethodGet, path, "creator", "")
					if response.Code != test.want {
						t.Errorf("%s=%d %s", path, response.Code, response.Body.String())
					}
				}
				if got := len(executor.AuthIDs()); got != before {
					t.Fatalf("policy refusal made %d upstream calls", got-before)
				}
			},
		)
	}
}

func TestVideoNativeMutationsCheckReferencedOwnership(t *testing.T) {
	executor := &videoAuthCaptureExecutor{requestID: "source-video"}
	h := newVideoAuthBindingTestHandler(t, executor)
	response := performOwnedVideoRequest(
		h, http.MethodPost, "/native/generations", "creator", `{"model":"grok-imagine-video","prompt":"test"}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("create=%d %s", response.Code, response.Body.String())
	}
	before := len(executor.AuthIDs())
	for _, path := range []string{"/native/edits", "/native/extensions"} {
		response := performOwnedVideoRequest(
			h, http.MethodPost, path, "other",
			`{"model":"grok-imagine-video","prompt":"test","video_id":"source-video"}`,
		)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s=%d %s", path, response.Code, response.Body.String())
		}
	}
	if got := len(executor.AuthIDs()); got != before {
		t.Fatalf("unauthorized mutation made %d calls", got-before)
	}
}
