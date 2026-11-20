package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apihandlers "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

type memoryVideoTaskStore struct {
	mu    sync.Mutex
	tasks map[string]apihandlers.VideoTask
	err   error
}

func (s *memoryVideoTaskStore) CheckVideoTaskStorage(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *memoryVideoTaskStore) SaveVideoTask(_ context.Context, task apihandlers.VideoTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.tasks == nil {
		s.tasks = make(map[string]apihandlers.VideoTask)
	}
	key := task.Provider + "\x00" + task.ID
	if existing, found := s.tasks[key]; found && existing != task {
		return errors.New("video ownership conflict")
	}
	s.tasks[key] = task
	return nil
}

func (s *memoryVideoTaskStore) LoadVideoTask(_ context.Context, provider, id string) (
	apihandlers.VideoTask,
	bool,
	error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, found := s.tasks[provider+"\x00"+id]
	return task, found, s.err
}

func TestVideoTaskRejectsAnotherAPIKeyBeforeUpstream(t *testing.T) {
	executor := &videoAuthCaptureExecutor{requestID: "private-video", contentURL: "http://127.0.0.1:1/video.mp4"}
	handler := newVideoAuthBindingTestHandler(t, executor)
	engine := gin.New()
	engine.Use(
		func(c *gin.Context) {
			key := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
			c.Set("apiKey", key)
			c.Request = c.Request.WithContext(
				coreusage.WithAPIKeyIdentity(
					c.Request.Context(), coreusage.HashAPIKeyIdentity(key),
				),
			)
			c.Next()
		},
	)
	engine.POST("/v1/videos", handler.VideosCreate)
	engine.GET("/v1/videos/:video_id", handler.VideosRetrieve)
	engine.GET("/v1/videos/:video_id/content", handler.VideosContent)
	engine.GET("/native/:request_id", handler.XAIVideosRetrieve)
	request := func(method, path, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		return response
	}
	created := request(http.MethodPost, "/v1/videos", "creator", `{"model":"sora-2","prompt":"private prompt"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	calls := len(executor.AuthIDs())
	for _, path := range []string{
		"/v1/videos/private-video", "/v1/videos/private-video/content", "/native/private-video",
	} {
		response := request(http.MethodGet, path, "other-client", "")
		if response.Code != http.StatusNotFound {
			t.Errorf("%s status=%d body=%s, want 404", path, response.Code, response.Body.String())
		}
	}
	if got := len(executor.AuthIDs()); got != calls {
		t.Fatalf("non-owner made %d upstream calls", got-calls)
	}
	owned := request(http.MethodGet, "/v1/videos/private-video", "creator", "")
	if owned.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", owned.Code, owned.Body.String())
	}
}
