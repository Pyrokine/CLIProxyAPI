package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

const mediaPrivateError = `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"` +
	`invalid field; Authorization: Bearer fixture-private-bearer; endpoint http://10.23.4.5:8317/private","` +
	`access_token":"fixture-private-access","password":"fixture-private-password"}}`

type mediaRegressionExecutor struct {
	coreauth.ProviderExecutor
	provider string
	payload  []byte
	err      error
}

func (e *mediaRegressionExecutor) Identifier() string { return e.provider }

func (e *mediaRegressionExecutor) Execute(
	ctx context.Context,
	_ *coreauth.Auth,
	request coreexecutor.Request,
	_ coreexecutor.Options,
) (coreexecutor.Response, error) {
	if e.err == nil {
		coreusage.DeferRecord(ctx, coreusage.Record{Provider: e.provider, Model: request.Model, Completed: true})
	}
	return coreexecutor.Response{Payload: e.payload}, e.err
}

func (e *mediaRegressionExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	return nil, e.err
}

func newMediaRegressionHandler(t *testing.T, executor coreauth.ProviderExecutor, model string) *APIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model, Type: registry.OpenAIImageModelType}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	setTestImageArtifactDeps(t, base)
	return NewAPIHandler(base)
}

func assertMediaErrorPrivacy(t *testing.T, body string) {
	t.Helper()
	for _, private := range []string{
		"fixture-private-bearer", "fixture-private-access", "fixture-private-password", "10.23.4.5", "/private",
	} {
		if strings.Contains(body, private) {
			t.Errorf("upstream private value %q reached client: %s", private, body)
		}
	}
	if !strings.Contains(body, "invalid field") || !strings.Contains(body, "invalid_value") {
		t.Errorf("actionable message/classification lost: %s", body)
	}
}

func TestImageConsumersSanitizeUpstreamErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{
		"image JSON", "image first stream error", "responses collected image", "responses first stream error",
	} {
		t.Run(
			scenario, func(t *testing.T) {
				const model = "fixture-image-privacy"
				executor := &mediaRegressionExecutor{
					provider: "fixture-image-provider",
					err: &imageHandlerStatusError{
						statusCode: http.StatusBadRequest,
						err:        errors.New(mediaPrivateError),
					},
				}
				handler := newMediaRegressionHandler(t, executor, model)
				router := gin.New()
				router.POST(
					imagesGenerationsPath, func(c *gin.Context) {
						switch scenario {
						case "image JSON", "image first stream error":
							handler.ImagesGenerations(c)
						case "responses collected image":
							handler.collectImagesFromResponses(c, []byte(`{"model":"`+model+`"}`), "url")
						case "responses first stream error":
							handler.streamImagesFromResponses(
								c, []byte(`{"model":"`+model+`"}`), "url", "image_generation",
							)
						}
					},
				)
				stream := "false"
				if strings.Contains(scenario, "first stream") {
					stream = "true"
				}
				req := httptest.NewRequest(
					http.MethodPost, imagesGenerationsPath,
					strings.NewReader(`{"model":"`+model+`","prompt":"fixture","stream":`+stream+`}`),
				)
				req.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, req)
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("error status = %d: %s", recorder.Code, recorder.Body.String())
				}
				assertMediaErrorPrivacy(t, recorder.Body.String())
			},
		)
	}
}

type mediaRegressionTransport func(*http.Request) (*http.Response, error)

func (transport mediaRegressionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestVideoContentConsumerSanitizesDownloadError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const videoID = "fixture-video-privacy"
	executor := &videoAuthCaptureExecutor{requestID: videoID, contentURL: "https://8.8.8.8/fixture-video"}
	handler := newVideoAuthBindingTestHandler(t, executor)
	owner := coreusage.HashAPIKeyIdentity("video-test-key")
	ownerCtx := coreusage.WithAPIKeyIdentity(t.Context(), owner)
	if err := handler.bindVideoAuthID(ownerCtx, videoID, videoID+"-auth-a", defaultXAIVideosModel); err != nil {
		t.Fatal(err)
	}
	hits := 0
	transport := mediaRegressionTransport(
		func(request *http.Request) (*http.Response, error) {
			hits++
			return &http.Response{
				StatusCode: http.StatusBadRequest, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(mediaPrivateError)), Request: request,
			}, nil
		},
	)
	router := gin.New()
	router.GET(openAIVideosPath+"/:video_id/content", handler.VideosContent)
	req := httptest.NewRequest(http.MethodGet, openAIVideosPath+"/"+videoID+"/content", nil)
	req = req.WithContext(util.WithRoundTripper(ownerCtx, transport))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || hits != 1 {
		t.Fatalf("download status/transport hits = %d/%d: %s", recorder.Code, hits, recorder.Body.String())
	}
	assertMediaErrorPrivacy(t, recorder.Body.String())
	if authIDs := executor.AuthIDs(); len(authIDs) != 1 || authIDs[0] != videoID+"-auth-a" {
		t.Fatalf("download lookup lost the creating credential: %v", authIDs)
	}
}

type mediaUsageRecorder chan coreusage.Record

func (records mediaUsageRecorder) HandleUsage(_ context.Context, record coreusage.Record) {
	records <- record
}

func TestImageDeliveryFailureControlsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"compat JSON", "xAI JSON", "xAI SSE"} {
		for _, failure := range []string{"none", "write", "short write", "flush"} {
			t.Run(
				mode+"/"+failure, func(t *testing.T) {
					model, provider := "fixture-delivery-image", "fixture-image-provider"
					if mode != "compat JSON" {
						model, provider = defaultXAIImagesModel, "xai"
					}
					executor := &mediaRegressionExecutor{
						provider: provider,
						payload:  []byte(`{"created":1,"data":[{"b64_json":"` + testPNGBase64 + `"}]}`),
					}
					handler := newMediaRegressionHandler(t, executor, model)
					records := make(mediaUsageRecorder, 1)
					usageManager := coreusage.NewManager(8)
					usageManager.Register(records)
					t.Cleanup(usageManager.Stop)
					handler.UsageManager = usageManager
					outcome := internallogging.NewRequestOutcomeState()
					events := make([]string, 0, 4)
					router := gin.New()
					router.POST(
						imagesGenerationsPath, func(c *gin.Context) {
							writer := &openAIStreamDeliveryWriter{ResponseWriter: c.Writer, events: &events}
							switch failure {
							case "write":
								writer.writeErr = errors.New("fixture client write failure")
							case "short write":
								writer.shortWrite = true
							case "flush":
								writer.flushErr = errors.New("fixture client flush failure")
							}
							c.Writer = writer
							handler.ImagesGenerations(c)
						},
					)
					stream := "false"
					if mode == "xAI SSE" {
						stream = "true"
					}
					req := httptest.NewRequest(
						http.MethodPost, imagesGenerationsPath,
						strings.NewReader(`{"model":"`+model+`","prompt":"fixture","stream":`+stream+`}`),
					)
					req.Header.Set("Content-Type", "application/json")
					req = req.WithContext(internallogging.WithRequestOutcomeState(t.Context(), outcome))
					router.ServeHTTP(httptest.NewRecorder(), req)
					usageManager.Stop()
					select {
					case record := <-records:
						wantFailure := failure != "none"
						if record.Failed != wantFailure || record.Completed == wantFailure ||
							outcome.Snapshot().Failed != wantFailure {
							t.Errorf(
								"delivery %s: failed/completed/outcome = %t/%t/%#v", failure, record.Failed,
								record.Completed, outcome.Snapshot(),
							)
						}
						if wantFailure && record.Fail.Body == "" {
							t.Error("delivery failure reason was lost")
						}
					default:
						t.Fatal("deferred usage was not published")
					}
				},
			)
		}
	}
}

func TestSanitizedUpstreamErrorPreservesDeliveryContract(t *testing.T) {
	cause := errors.New(mediaPrivateError)
	acknowledged := false
	original := &handlers.ErrorMessage{
		StatusCode: http.StatusBadRequest, Error: cause, Headers: http.Header{"Retry-After": {"3"}},
		Acknowledge: func(error) { acknowledged = true },
	}
	cleaned := sanitizeOpenAIUpstreamError(original)
	if cleaned == original || cleaned.StatusCode != original.StatusCode || !errors.Is(
		cleaned.Error, cause,
	) || cleaned.Headers.Get("Retry-After") != "3" {
		t.Fatalf("error contract changed: %#v", cleaned)
	}
	cleaned.Acknowledge(nil)
	if !acknowledged {
		t.Fatal("error acknowledgement was lost")
	}
	assertMediaErrorPrivacy(t, cleaned.Error.Error())
	original.DirectResponse = true
	if sanitizeOpenAIUpstreamError(original) != original {
		t.Fatal("explicit direct response was changed")
	}
}
