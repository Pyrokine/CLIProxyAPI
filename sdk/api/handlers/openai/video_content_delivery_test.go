package openai

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestVideoContentIOFailureReachesOutcomeAndAccessLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previousHooks) })
	for _, scenario := range []string{"success", "upstream read", "downstream write", "short write"} {
		t.Run(
			scenario, func(t *testing.T) {
				hook.Reset()
				const videoID = "fixture-video-delivery"
				executor := &videoAuthCaptureExecutor{requestID: videoID, contentURL: "https://8.8.8.8/fixture-video"}
				handler := newVideoAuthBindingTestHandler(t, executor)
				ownerCtx := coreusage.WithAPIKeyIdentity(t.Context(), coreusage.HashAPIKeyIdentity("video-test-key"))
				if err := handler.bindVideoAuthID(
					ownerCtx, videoID, videoID+"-auth-a", defaultXAIVideosModel,
				); err != nil {
					t.Fatal(err)
				}
				transport := mediaRegressionTransport(
					func(request *http.Request) (*http.Response, error) {
						var body io.Reader = strings.NewReader("video-fragment")
						if scenario == "upstream read" {
							body = io.MultiReader(
								body,
								iotest.ErrReader(errors.New("fixture read failure from http://10.23.4.5/private")),
							)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"video/mp4"}},
							Body: io.NopCloser(body), Request: request,
						}, nil
					},
				)
				var outcome internallogging.RequestOutcome
				events := make([]string, 0, 2)
				router := gin.New()
				router.Use(internallogging.GinLogrusLogger())
				router.GET(
					openAIVideosPath+"/:video_id/content", func(c *gin.Context) {
						writer := &openAIStreamDeliveryWriter{ResponseWriter: c.Writer, events: &events}
						if scenario == "downstream write" {
							writer.writeErr = errors.New("fixture write failure")
						}
						writer.shortWrite = scenario == "short write"
						c.Writer = writer
						handler.VideosContent(c)
						outcome = internallogging.RequestOutcomeStateFromContext(c.Request.Context()).Snapshot()
					},
				)
				req := httptest.NewRequest(
					http.MethodGet, openAIVideosPath+"/"+videoID+"/content", nil,
				).WithContext(util.WithRoundTripper(ownerCtx, transport))
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, req)
				wantFailure := scenario != "success"
				if recorder.Code != http.StatusOK || outcome.Failed != wantFailure {
					t.Errorf("HTTP status/outcome = %d/%#v", recorder.Code, outcome)
				}
				entry := hook.LastEntry()
				if entry == nil || (wantFailure && !strings.Contains(
					entry.Message, "failure",
				) && !strings.Contains(entry.Message, "short write")) {
					t.Errorf("access log lost I/O failure: %#v", entry)
				}
				if entry != nil && (strings.Contains(entry.Message, "10.23.4.5") || strings.Contains(
					entry.Message, "/private",
				)) {
					t.Errorf("access log leaked upstream address: %s", entry.Message)
				}
			},
		)
	}
}
