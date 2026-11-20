package openai_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestPublishedResponsesHandlerNamesUseCurrentImplementation(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(*handlers.BaseAPIHandler) *openai.OpenAIResponsesAPIHandler
	}{
		{name: "published constructor", new: openai.NewOpenAIResponsesAPIHandler},
		{name: "current constructor", new: openai.NewResponsesAPIHandler},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
				var current = tc.new(base)
				var published = current
				if published == nil || published.BaseAPIHandler != base {
					t.Fatal("constructor changed the supplied base handler")
				}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(
					http.MethodPost, "/v1/responses", iotest.ErrReader(io.ErrUnexpectedEOF),
				)
				published.Responses(ctx)
				var body handlers.ErrorResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil ||
					recorder.Code != http.StatusBadRequest || body.Error.Type != "invalid_request_error" {
					t.Fatalf("published handler HTTP=%d body=%s error=%v", recorder.Code, recorder.Body.String(), err)
				}
			},
		)
	}
}
