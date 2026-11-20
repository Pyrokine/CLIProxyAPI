package openai

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestNonStreamingPublicErrorsSanitizeOriginalEnvelope(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	const envelope = `{"type":"response.failed","sequence_number":51,"extensions":{"access_token":"fixture-` +
		`private-access"},"headers":{"Authorization":"Bearer fixture-private-bearer"},"response":{"error":{"` +
		`type":"invalid_request_error","code":"invalid_value","message":"invalid field; endpoint http://10.` +
		`23.4.5:8317/private","cause":{"api_key":"fixture-private-password"}}}}`
	for _, endpoint := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat", path: "/v1/chat/completions",
			body: `{"model":"privacy-model","messages":[{"role":"user","content":"synthetic"}]}`,
		},
		{name: "completions", path: "/v1/completions", body: `{"model":"privacy-model","prompt":"synthetic"}`},
		{name: "responses", path: "/v1/responses", body: `{"model":"privacy-model","input":"synthetic"}`},
		{name: "compact", path: "/v1/responses/compact", body: `{"model":"privacy-model","input":"synthetic"}`},
	} {
		t.Run(
			endpoint.name, func(t *testing.T) {
				api := newMediaRegressionHandler(
					t, &mediaRegressionExecutor{provider: "privacy-provider", err: errors.New(envelope)},
					"privacy-model",
				)
				responses := NewResponsesAPIHandler(api.BaseAPIHandler)
				router := gin.New()
				switch endpoint.name {
				case "chat":
					router.POST(endpoint.path, api.ChatCompletions)
				case "completions":
					router.POST(endpoint.path, api.Completions)
				case "responses":
					router.POST(endpoint.path, responses.Responses)
				case "compact":
					router.POST(endpoint.path, responses.Compact)
				}
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(endpoint.body))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusInternalServerError {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				assertMediaErrorPrivacy(t, recorder.Body.String())
				var result struct {
					SequenceNumber int `json:"sequence_number"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.SequenceNumber != 51 {
					t.Fatalf("error sequence lost: error=%v body=%s", err, recorder.Body.String())
				}
			},
		)
	}
}

func TestResponsesErrorsPreserveClassificationWithoutUpstreamSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, client := range []string{"initial HTTP", "standard SSE", "Codex SSE"} {
		for _, message := range []string{
			`{"error":{"type":"invalid_request_error","code":"invalid_value","message":"invalid field; ` +
				`Bearer fixture-private-bearer; backend http://10.23.4.5:8317/private","access_token":"` +
				`fixture-private-access","password":"fixture-private-password","max_tokens":42}}`,
			`invalid field; Authorization: Bearer fixture-private-bearer; endpoint http://10.23.4.5:8317/private`,
		} {
			t.Run(
				client+"/"+message[:7], func(t *testing.T) {
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					if client == "Codex SSE" {
						ctx.Request.Header.Set("Originator", "codex-tui")
					}
					handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
					acknowledged := false
					message := &handlers.ErrorMessage{
						StatusCode: http.StatusBadRequest,
						Error:      errors.New(message),
						Acknowledge: func(err error) {
							acknowledged = true
							if err != nil {
								t.Errorf("error delivery acknowledgement = %v", err)
							}
						},
					}
					if client == "initial HTTP" {
						if err := handler.writeInitialResponsesStreamError(
							ctx, ctx.Writer, func() { handlers.SetSSEHeaders(ctx) }, message,
						); err != nil {
							t.Fatal(err)
						}
						if !acknowledged || recorder.Code != http.StatusBadRequest {
							t.Fatalf("HTTP error status/acknowledgement = %d/%v", recorder.Code, acknowledged)
						}
					} else if err := writeResponsesStreamTerminalError(ctx, message); err != nil {
						t.Fatal(err)
					}
					body := recorder.Body.String()
					for _, private := range []string{
						"fixture-private-bearer", "fixture-private-access", "fixture-private-password", "10.23.4.5",
						"/private",
					} {
						if strings.Contains(body, private) {
							t.Errorf("upstream private value %q reached client: %s", private, body)
						}
					}
					if !strings.Contains(body, "invalid field") {
						t.Fatalf("actionable error message lost: %s", body)
					}
					if strings.HasPrefix(message.Error.Error(), "{") && !strings.Contains(body, "invalid_value") {
						t.Fatalf("error classification lost: %s", body)
					}
				},
			)
		}
	}
}
